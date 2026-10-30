package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// After a flash, the packages the official feeds do not offer (typically third-party themes)
// are not in the new image. This file finds them and says which confirmed package source,
// if any, could put each back. It only reports. Installing a source stays a separate request
// that an administrator makes and confirms (installPackageSource): a flash never causes an
// install by itself.

type reinstallCandidate struct {
	Name string `json:"name"`
	// SourceID, Version and Format describe the confirmed source that fits the device; they
	// are empty when none does, and Reason says why.
	SourceID *int64 `json:"source_id"`
	Version  string `json:"version,omitempty"`
	Format   string `json:"format,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// sourceFits reports whether a source of this format and architecture can be installed on
// a device with this manifest (the same rules the install preflight applies).
func sourceFits(format, arch string, m firmwareManifest) (bool, string) {
	f, ok := installFormats[format]
	switch {
	case !ok || f.manager != m.PackageManager:
		return false, fmt.Sprintf("the only source is an .%s file and the device now uses %s", format, m.PackageManager)
	case arch != "all" && arch != "noarch" && arch != m.Arch:
		return false, fmt.Sprintf("the only source is for %s and the device is %s", arch, m.Arch)
	}
	return true, ""
}

// reinstallCandidates lists the wanted packages the device lacks after a flash, each with the
// newest confirmed source that fits it.
func (a *app) reinstallCandidates(ctx context.Context, wanted []string, m firmwareManifest) ([]reinstallCandidate, error) {
	have := map[string]bool{}
	for _, p := range m.Packages {
		have[p.Name] = true
	}
	out := []reinstallCandidate{}
	for _, name := range wanted {
		if have[name] {
			continue
		}
		c := reinstallCandidate{Name: name, Reason: "no confirmed package source for this name"}
		rows, err := a.db.Query(ctx, `SELECT id, version, format, arch FROM package_sources
			WHERE name=$1 AND status='confirmed' ORDER BY confirmed_at DESC, id DESC`, name)
		if err != nil {
			return nil, err
		}
		first := true
		for rows.Next() {
			var id int64
			var version, format, arch string
			if err := rows.Scan(&id, &version, &format, &arch); err != nil {
				rows.Close()
				return nil, err
			}
			if ok, why := sourceFits(format, arch, m); ok {
				c.SourceID, c.Version, c.Format, c.Reason = &id, version, format, ""
				break
			} else if first {
				// Sources are newest first; the newest one's problem is the one to report.
				c.Reason = why
			}
			first = false
		}
		rows.Close()
		out = append(out, c)
	}
	return out, nil
}

// reinstallCheck summarises the candidates as one post-flash check. It is a warning when any
// package is missing, so the upgrade ends "done with warnings" and the person is told.
func reinstallCheck(candidates []reinstallCandidate) upgradeCheck {
	if len(candidates) == 0 {
		return upgradeCheck{"Third-party packages", "ok", "every package from outside the official feeds is still installed"}
	}
	parts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c.SourceID != nil {
			parts = append(parts, fmt.Sprintf("%s can be reinstalled from source %d (%s)", c.Name, *c.SourceID, c.Version))
		} else {
			parts = append(parts, fmt.Sprintf("%s: %s", c.Name, c.Reason))
		}
	}
	return upgradeCheck{"Third-party packages", "warning", "not installed after the upgrade: " + strings.Join(parts, "; ") + ". Nothing was reinstalled; an administrator chooses what to install."}
}

// getPackageReinstalls lists what the newest finished upgrade left missing and which confirmed
// sources could restore it.
func (a *app) getPackageReinstalls(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	var id int64
	err := a.db.QueryRow(r.Context(), `SELECT id FROM firmware_upgrades WHERE device_id=$1
		AND status IN ('done','done_with_warnings','needs_attention') ORDER BY created_at DESC, id DESC LIMIT 1`, deviceID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, []reinstallCandidate{})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the upgrade")
		return
	}
	rec, err := a.loadUpgradeRecord(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the upgrade")
		return
	}
	var inv firmwareInventory
	err = scanFirmwareInventory(a.db.QueryRow(r.Context(), `SELECT `+firmwareInventoryColumns+`
		FROM firmware_inventories WHERE device_id=$1 ORDER BY captured_at DESC, id DESC LIMIT 1`, deviceID), &inv)
	if err != nil {
		writeError(w, http.StatusConflict, "read the device's firmware inventory first")
		return
	}
	candidates, err := a.reinstallCandidates(r.Context(), rec.Plan.Unavailable, inv.Manifest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not look up package sources")
		return
	}
	writeJSON(w, http.StatusOK, candidates)
}
