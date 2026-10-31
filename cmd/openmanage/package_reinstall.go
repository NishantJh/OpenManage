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
	SourceID   *int64 `json:"source_id"`
	Version    string `json:"version,omitempty"`
	Format     string `json:"format,omitempty"`
	Reason     string `json:"reason,omitempty"`
	SourceMode string `json:"source_mode,omitempty"`
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

// reinstallCandidates lists packages missing after a flash and remains as a compatibility
// wrapper for callers that do not have a desired package-set snapshot.
func (a *app) reinstallCandidates(ctx context.Context, wanted []string, m firmwareManifest) ([]reinstallCandidate, error) {
	return a.reinstallCandidatesWithDesired(ctx, wanted, nil, m)
}

// reinstallCandidatesWithDesired combines packages missing from official feeds with custom
// sources selected by the desired package-set revision. Source IDs are captured in the plan.
func (a *app) reinstallCandidatesWithDesired(ctx context.Context, wanted []string, desired []packageSetSource, m firmwareManifest) ([]reinstallCandidate, error) {
	have := map[string]bool{}
	for _, p := range m.Packages {
		have[p.Name] = true
	}
	desiredByName := make(map[string]packageSetSource, len(desired))
	for _, source := range desired {
		desiredByName[source.Name] = source
	}
	seen := map[string]bool{}
	names := make([]string, 0, len(wanted)+len(desired))
	for _, name := range wanted {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, source := range desired {
		if !seen[source.Name] {
			seen[source.Name] = true
			names = append(names, source.Name)
		}
	}
	out := []reinstallCandidate{}
	for _, name := range names {
		if have[name] {
			continue
		}
		c := reinstallCandidate{Name: name, Reason: "no confirmed package source for this name"}
		if selected, ok := desiredByName[name]; ok {
			c.SourceMode = selected.Mode
			if selected.SourceID == nil {
				c.Reason = "no confirmed source was available when this firmware plan was prepared"
				out = append(out, c)
				continue
			}
			var id int64
			var version, format, arch string
			err := a.db.QueryRow(ctx, `SELECT id, version, format, arch FROM package_sources WHERE id=$1 AND name=$2 AND status='confirmed'`, *selected.SourceID, name).Scan(&id, &version, &format, &arch)
			if errors.Is(err, pgx.ErrNoRows) {
				c.Reason = "the selected package source is no longer confirmed or available"
			} else if err != nil {
				return nil, err
			} else if selected.Version != "" && version != selected.Version {
				c.Reason = "the selected package source version no longer matches the firmware plan"
			} else if ok, why := sourceFits(format, arch, m); ok {
				c.SourceID, c.Version, c.Format, c.Reason = &id, version, format, ""
			} else {
				c.Reason = why
			}
			out = append(out, c)
			continue
		}
		rows, err := a.db.Query(ctx, `SELECT id, version, format, arch FROM package_sources WHERE name=$1 AND status='confirmed' ORDER BY confirmed_at DESC NULLS LAST, id DESC`, name)
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
				c.Reason = why
			}
			first = false
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
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
