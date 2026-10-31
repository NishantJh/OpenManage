package main

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	packageSetBodyLimit    = 32 << 10
	packageSetMaxItems     = 512
	packageSetMaxSources   = 100
	packageSetHistoryLimit = 50
)

type packageSetSourceInput struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"`
	SourceID *int64 `json:"source_id,omitempty"`
}

type packageSetRequest struct {
	OfficialPackages []string                `json:"official_packages"`
	RemovedDefaults  []string                `json:"removed_defaults"`
	Sources          []packageSetSourceInput `json:"sources"`
}

type packageSetSource struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"`
	SourceID *int64 `json:"source_id,omitempty"`
	Version  string `json:"version,omitempty"`
}

type packageSetRevision struct {
	Revision         int                `json:"revision"`
	OfficialPackages []string           `json:"official_packages"`
	RemovedDefaults  []string           `json:"removed_defaults"`
	Sources          []packageSetSource `json:"sources"`
	CreatedBy        string             `json:"created_by,omitempty"`
	CreatedAt        time.Time          `json:"created_at"`
	BasedOnRevision  *int               `json:"based_on_revision,omitempty"`
}

type packageSetResponse struct {
	DeviceID        string               `json:"device_id"`
	CurrentRevision int                  `json:"current_revision"`
	Current         *packageSetRevision  `json:"current,omitempty"`
	Revisions       []packageSetRevision `json:"revisions"`
}

// accessCriticalPackage reports default packages that a firmware image must keep: without
// them the device may boot but OpenManage can no longer reach or configure it over SSH and
// rpcd. Swaps of wpad, dnsmasq and similar packages stay possible; these do not.
func accessCriticalPackage(name string) bool {
	for _, prefix := range []string{"kernel", "base-files", "busybox", "libc", "musl", "procd", "ubus", "uci", "rpcd", "netifd", "dropbear", "openssh", "apk", "opkg"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func normalizePackageSetNames(values []string) ([]string, error) {
	if len(values) > packageSetMaxItems {
		return nil, errors.New("too many package names")
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, name := range values {
		if !packageNamePattern.MatchString(name) || seen[name] {
			return nil, errors.New("package names must be valid and unique")
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func normalizePackageSetRequest(in packageSetRequest) (packageSetRequest, error) {
	var err error
	in.OfficialPackages, err = normalizePackageSetNames(in.OfficialPackages)
	if err != nil {
		return in, err
	}
	in.RemovedDefaults, err = normalizePackageSetNames(in.RemovedDefaults)
	if err != nil {
		return in, err
	}
	if len(in.Sources) > packageSetMaxSources {
		return in, errors.New("too many package sources")
	}
	official := make(map[string]bool, len(in.OfficialPackages))
	for _, name := range in.OfficialPackages {
		official[name] = true
	}
	removed := make(map[string]bool, len(in.RemovedDefaults))
	for _, name := range in.RemovedDefaults {
		removed[name] = true
	}
	for _, name := range in.RemovedDefaults {
		if accessCriticalPackage(name) {
			return in, errors.New(name + " is needed to reach and manage the device and cannot be removed from the image")
		}
	}
	for name := range official {
		if removed[name] {
			return in, errors.New("a package cannot be both added and removed")
		}
	}
	seen := map[string]bool{}
	for i := range in.Sources {
		s := &in.Sources[i]
		if !packageNamePattern.MatchString(s.Name) || seen[s.Name] || official[s.Name] {
			return in, errors.New("package source names must be valid, unique, and separate from official packages")
		}
		seen[s.Name] = true
		switch s.Mode {
		case "pinned":
			if s.SourceID == nil || *s.SourceID <= 0 {
				return in, errors.New("a pinned package source needs a source_id")
			}
		case "latest":
			if s.SourceID != nil {
				return in, errors.New("a latest package source must not specify source_id")
			}
		default:
			return in, errors.New("package source mode must be pinned or latest")
		}
	}
	sort.Slice(in.Sources, func(i, j int) bool { return in.Sources[i].Name < in.Sources[j].Name })
	if in.OfficialPackages == nil {
		in.OfficialPackages = []string{}
	}
	if in.RemovedDefaults == nil {
		in.RemovedDefaults = []string{}
	}
	if in.Sources == nil {
		in.Sources = []packageSetSourceInput{}
	}
	return in, nil
}

func (a *app) getDevicePackageSet(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, readOnlyRoles...) {
		return
	}
	var exists bool
	if err := a.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM devices WHERE id=$1)`, deviceID).Scan(&exists); err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the package set")
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	out, err := a.loadDevicePackageSet(r.Context(), deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the package set")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) putDevicePackageSet(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	if !validUUID(deviceID) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin") {
		return
	}
	var in packageSetRequest
	if err := decodeJSONBody(w, r, packageSetBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid package set")
		return
	}
	in, err := normalizePackageSetRequest(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the package set")
		return
	}
	defer tx.Rollback(r.Context())
	if err = appendPackageSetRevision(r.Context(), tx, deviceID, user.ID, in, nil, "device.package_set_revision"); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "device or confirmed package source not found")
		} else if strings.HasPrefix(err.Error(), "invalid package set:") {
			writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "invalid package set: "))
		} else {
			writeError(w, http.StatusInternalServerError, "could not save the package set")
		}
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the package set")
		return
	}
	out, err := a.loadDevicePackageSet(r.Context(), deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "package set was saved but could not be reloaded")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// appendPackageSetRevision creates a new immutable revision and moves the device's
// current pointer in the same transaction. It never contacts or changes the device.
func appendPackageSetRevision(ctx context.Context, tx pgx.Tx, deviceID, userID string, in packageSetRequest, basedOn *int, action string) error {
	var ignored string
	if err := tx.QueryRow(ctx, `SELECT name FROM devices WHERE id=$1 FOR UPDATE`, deviceID).Scan(&ignored); err != nil {
		return err
	}
	var setID int64
	var currentRevision *int
	err := tx.QueryRow(ctx, `SELECT s.id, r.revision FROM device_package_sets s
		LEFT JOIN device_package_set_revisions r ON r.id=s.current_revision_id
		WHERE s.device_id=$1 FOR UPDATE OF s`, deviceID).Scan(&setID, &currentRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO device_package_sets (device_id) VALUES ($1) RETURNING id`, deviceID).Scan(&setID)
	} else if err == nil && basedOn == nil && currentRevision != nil {
		basedOn = currentRevision
	}
	if err != nil {
		return err
	}
	var revision int
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM device_package_set_revisions WHERE package_set_id=$1`, setID).Scan(&revision); err != nil {
		return err
	}
	var revisionID int64
	err = tx.QueryRow(ctx, `INSERT INTO device_package_set_revisions
		(package_set_id, revision, official_packages, removed_defaults, created_by, based_on_revision)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		setID, revision, in.OfficialPackages, in.RemovedDefaults, userID, basedOn).Scan(&revisionID)
	if err != nil {
		return err
	}
	sourceAudit := make([]map[string]any, 0, len(in.Sources))
	for _, source := range in.Sources {
		var sourceID any
		version := ""
		switch source.Mode {
		case "pinned":
			if err = tx.QueryRow(ctx, `SELECT version FROM package_sources WHERE id=$1 AND name=$2 AND status='confirmed' FOR KEY SHARE`, *source.SourceID, source.Name).Scan(&version); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return errors.New("invalid package set: a pinned source must name a confirmed package source")
				}
				return err
			}
			sourceID = *source.SourceID
		case "latest":
			if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('device_package_set_source:' || $1))`, source.Name); err != nil {
				return err
			}
			var found bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM package_sources WHERE name=$1 AND status='confirmed')`, source.Name).Scan(&found); err != nil {
				return err
			}
			if !found {
				return errors.New("invalid package set: a latest source needs at least one confirmed package source")
			}
		default:
			return errors.New("invalid package set: package source mode must be pinned or latest")
		}
		_, err = tx.Exec(ctx, `INSERT INTO device_package_set_revision_sources
			(revision_id, package_name, tracking_mode, source_id, source_version)
			VALUES ($1,$2,$3,$4,$5)`, revisionID, source.Name, source.Mode, sourceID, version)
		if err != nil {
			return err
		}
		sourceAudit = append(sourceAudit, map[string]any{"name": source.Name, "mode": source.Mode, "version": version})
	}
	if _, err = tx.Exec(ctx, `UPDATE device_package_sets SET current_revision_id=$2, updated_at=NOW() WHERE id=$1`, setID, revisionID); err != nil {
		return err
	}
	details := map[string]any{"revision": revision, "based_on_revision": basedOn, "official_packages": in.OfficialPackages, "removed_defaults": in.RemovedDefaults, "sources": sourceAudit}
	return insertAuditEvent(ctx, tx, userID, action, "device", deviceID, details)
}

func (a *app) restoreDevicePackageSetRevision(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	revision, err := strconv.Atoi(r.PathValue("revision"))
	if !validUUID(deviceID) || err != nil || revision <= 0 {
		writeError(w, http.StatusBadRequest, "invalid device or revision")
		return
	}
	if !requireAnyRole(w, r, "admin") {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not restore the package set")
		return
	}
	defer tx.Rollback(r.Context())
	var setID int64
	err = tx.QueryRow(r.Context(), `SELECT s.id FROM device_package_sets s JOIN devices d ON d.id=s.device_id
		WHERE s.device_id=$1 FOR UPDATE OF d,s`, deviceID).Scan(&setID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "package set revision not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not restore the package set")
		return
	}
	var in packageSetRequest
	err = tx.QueryRow(r.Context(), `SELECT official_packages, removed_defaults FROM device_package_set_revisions
		WHERE package_set_id=$1 AND revision=$2`, setID, revision).Scan(&in.OfficialPackages, &in.RemovedDefaults)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "package set revision not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not restore the package set")
		return
	}
	rows, err := tx.Query(r.Context(), `SELECT package_name, tracking_mode, source_id FROM device_package_set_revision_sources
		WHERE revision_id=(SELECT id FROM device_package_set_revisions WHERE package_set_id=$1 AND revision=$2)
		ORDER BY package_name`, setID, revision)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not restore the package set")
		return
	}
	in.Sources = []packageSetSourceInput{}
	for rows.Next() {
		var source packageSetSourceInput
		if err = rows.Scan(&source.Name, &source.Mode, &source.SourceID); err != nil {
			break
		}
		in.Sources = append(in.Sources, source)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not restore the package set")
		return
	}
	in, err = normalizePackageSetRequest(in)
	if err != nil {
		writeError(w, http.StatusConflict, "the selected revision is no longer valid")
		return
	}
	basedOn := revision
	if err = appendPackageSetRevision(r.Context(), tx, deviceID, user.ID, in, &basedOn, "device.package_set_restore"); err != nil {
		if errors.Is(err, pgx.ErrNoRows) || strings.HasPrefix(err.Error(), "invalid package set:") {
			writeError(w, http.StatusConflict, "a package source in that revision is no longer available")
		} else {
			writeError(w, http.StatusInternalServerError, "could not restore the package set")
		}
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "could not restore the package set")
		return
	}
	out, err := a.loadDevicePackageSet(r.Context(), deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "package set was restored but could not be reloaded")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) loadDevicePackageSet(ctx context.Context, deviceID string) (packageSetResponse, error) {
	out := packageSetResponse{DeviceID: deviceID, Revisions: []packageSetRevision{}}
	var setID int64
	var currentID *int64
	err := a.db.QueryRow(ctx, `SELECT id, current_revision_id FROM device_package_sets WHERE device_id=$1`, deviceID).Scan(&setID, &currentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}

	// Load revisions and sources in one query. Nested queries while the history
	// cursor is open can exhaust the connection pool under concurrent requests.
	rows, err := a.db.Query(ctx, `SELECT r.id, r.revision, r.official_packages, r.removed_defaults,
		COALESCE(r.created_by::text,''), r.created_at, r.based_on_revision,
		COALESCE(s.package_name,''), COALESCE(s.tracking_mode,''), s.source_id,
		COALESCE(s.source_version,'')
		FROM (
			SELECT id, revision, official_packages, removed_defaults, created_by, created_at, based_on_revision
			FROM device_package_set_revisions
			WHERE package_set_id=$1
			ORDER BY revision DESC
			LIMIT $2
		) r
		LEFT JOIN device_package_set_revision_sources s ON s.revision_id=r.id
		ORDER BY r.revision DESC, s.package_name`, setID, packageSetHistoryLimit)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	var lastRevisionID int64
	var currentRevisionID int64
	if currentID != nil {
		currentRevisionID = *currentID
	}
	for rows.Next() {
		var revisionID int64
		var rev packageSetRevision
		var sourceName, sourceMode, sourceVersion string
		var sourceID *int64
		if err = rows.Scan(&revisionID, &rev.Revision, &rev.OfficialPackages, &rev.RemovedDefaults,
			&rev.CreatedBy, &rev.CreatedAt, &rev.BasedOnRevision,
			&sourceName, &sourceMode, &sourceID, &sourceVersion); err != nil {
			return out, err
		}
		if revisionID != lastRevisionID {
			if rev.OfficialPackages == nil {
				rev.OfficialPackages = []string{}
			}
			if rev.RemovedDefaults == nil {
				rev.RemovedDefaults = []string{}
			}
			rev.Sources = []packageSetSource{}
			out.Revisions = append(out.Revisions, rev)
			lastRevisionID = revisionID
		}
		if sourceName != "" {
			idx := len(out.Revisions) - 1
			out.Revisions[idx].Sources = append(out.Revisions[idx].Sources, packageSetSource{
				Name: sourceName, Mode: sourceMode, SourceID: sourceID, Version: sourceVersion,
			})
		}
		if currentID != nil && revisionID == currentRevisionID {
			copy := out.Revisions[len(out.Revisions)-1]
			out.Current = &copy
			out.CurrentRevision = copy.Revision
		}
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}
