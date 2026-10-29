package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The stock diff compares what a device has with the official image for its release and
// device profile: the profile's default package list from the build server's upstream
// profiles.json (the same list ASU and the ImageBuilder start from). "Added" is what the
// owner installed on top of that list; "removed" is what the list has and the device does
// not. Packages installed and later uninstalled appear in neither, and library packages
// pulled in as dependencies are not listed as removed, because the list names only what
// an image build is asked for.

type stockDiff struct {
	InventoryID  int64    `json:"inventory_id"`
	Release      string   `json:"release"`
	Target       string   `json:"target"`
	Profile      string   `json:"profile"`
	ProfileTitle string   `json:"profile_title"`
	StockCount   int      `json:"stock_count"`
	Added        []string `json:"added"`
	Removed      []string `json:"removed"`
	// Note explains a limit of the comparison, such as a device with no firmware image.
	Note string `json:"note,omitempty"`
}

// diffAgainstStock returns the owner's packages missing from the stock list and the stock
// packages the device does not have. A library installed under a dated name (libubox20240329)
// matches its plain stock name.
func diffAgainstStock(m firmwareManifest, stock []string) (added, removed []string) {
	inStock := map[string]bool{}
	for _, name := range stock {
		inStock[name] = true
	}
	installed := map[string]bool{}
	for _, p := range m.Packages {
		installed[p.Name] = true
		installed[abiFree(p.Name)] = true
	}
	added, removed = []string{}, []string{}
	for _, name := range m.UserInstalled {
		if !inStock[name] && !inStock[abiFree(name)] {
			added = append(added, name)
		}
	}
	for _, name := range stock {
		if !installed[name] {
			removed = append(removed, name)
		}
	}
	return added, removed
}

func stockDiffFor(ctx context.Context, serverURL string, inv firmwareInventory) (stockDiff, error) {
	m := inv.Manifest
	out := stockDiff{InventoryID: inv.ID, Release: m.Release, Target: m.Target, Added: []string{}, Removed: []string{}}
	// Extroot only blocks upgrades; the release's profile defaults can still be resolved.
	planned := m
	planned.Extroot = false
	plan, err := firmwarePlanFor(ctx, serverURL, planned, m.Release)
	if err != nil {
		return out, err
	}
	if len(plan.stock) == 0 {
		reason := "the build server has no stock package list for this device"
		if len(plan.Blockers) > 0 {
			reason = plan.Blockers[0]
		}
		return out, errors.New(reason)
	}
	out.Profile, out.ProfileTitle, out.StockCount = plan.Profile, plan.ProfileTitle, len(plan.stock)
	out.Added, out.Removed = diffAgainstStock(m, plan.stock)
	if !m.ROMKnown {
		out.Note = "This device has no read-only firmware image, so the added list may include packages that came with it."
	}
	return out, nil
}

// getFirmwareStockDiff compares the device's newest inventory with the official image for
// the release it runs. It needs the build server turned on, as the upgrade plan does.
func (a *app) getFirmwareStockDiff(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*buildServerTimeout)
	defer cancel()
	serverURL, err := a.firmwareBuildServer(ctx)
	if err != nil {
		buildServerError(w, err)
		return
	}
	var inv firmwareInventory
	err = scanFirmwareInventory(a.db.QueryRow(ctx, `SELECT `+firmwareInventoryColumns+`
		FROM firmware_inventories WHERE device_id=$1 ORDER BY captured_at DESC, id DESC LIMIT 1`, deviceID), &inv)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "read the device's firmware inventory first")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the firmware inventory")
		return
	}
	diff, err := stockDiffFor(ctx, serverURL, inv)
	if err != nil {
		buildServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, diff)
}
