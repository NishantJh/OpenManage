package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// The upgrade plan is the read-only second step of firmware upgrades
// (docs/firmware-upgrades.md, Phase 1). It lists every release the build server
// offers and, for one device and one chosen release, shows what an upgrade would
// request: the ASU profile, the packages the owner installed (renamed where the
// release renamed them), the defaults left out, and the packages the official feeds
// no longer have. Nothing is built and nothing changes on a device.
//
// Network targets: the configured build server (default sysupgrade.openwrt.org,
// the ASU JSON API) and the upstream download server it names (default
// downloads.openwrt.org, for profiles.json). Both are contacted only after an
// administrator turns the build server on. Requests carry only a release, target and
// profile in the URL; no package list leaves the server in this step.

const (
	defaultBuildServerURL = "https://sysupgrade.openwrt.org"
	buildServerTimeout    = 20 * time.Second
	buildServerBodyLimit  = 16 << 20
	buildServerCacheTTL   = time.Hour
	buildServerCacheSize  = 64
)

var (
	errBuildServerDisabled = errors.New("the firmware build server is turned off")
	releaseVersionPattern  = regexp.MustCompile(`^(SNAPSHOT|[0-9]{2}\.[0-9]{2}(-SNAPSHOT|\.[0-9]+(-rc[0-9]+)?))$`)
	releaseTargetPattern   = regexp.MustCompile(`^[a-z0-9_-]+/[a-z0-9_-]+$`)
	releaseArchPattern     = regexp.MustCompile(`^[a-z0-9_-]+$`)
	releaseDirPattern      = regexp.MustCompile(`^[a-z]+(/[A-Za-z0-9._-]+)?$`)
	releaseProfilePattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	revisionNumberPattern  = regexp.MustCompile(`^r([0-9]+)`)
	abiSuffixPattern       = regexp.MustCompile(`^(lib.*?[a-z-])[0-9]{8}$`)
)

// validBuildServerURL accepts https anywhere, or plain http only to a literal
// private IP (a self-hosted build server on the LAN). No credentials, query or
// fragment. It returns the URL without a trailing slash.
func validBuildServerURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Hostname() == "" || len(raw) > 256 {
		return "", false
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isPrivateDeviceIP(u.Hostname()) {
			return "", false
		}
	default:
		return "", false
	}
	return strings.TrimRight(u.String(), "/"), true
}

type firmwareSettings struct {
	BuildServerEnabled bool       `json:"build_server_enabled"`
	BuildServerURL     string     `json:"build_server_url"`
	UpdatedAt          *time.Time `json:"updated_at"`
}

func (a *app) loadFirmwareSettings(ctx context.Context) (firmwareSettings, error) {
	s := firmwareSettings{BuildServerURL: defaultBuildServerURL}
	err := a.db.QueryRow(ctx, `SELECT build_server_enabled, build_server_url, updated_at FROM firmware_settings`).
		Scan(&s.BuildServerEnabled, &s.BuildServerURL, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	return s, err
}

func (a *app) getFirmwareSettings(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, readOnlyRoles...) {
		return
	}
	s, err := a.loadFirmwareSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load firmware settings")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *app) updateFirmwareSettings(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	var in struct {
		BuildServerEnabled bool   `json:"build_server_enabled"`
		BuildServerURL     string `json:"build_server_url"`
	}
	if err := decodeJSONBody(w, r, monitoringBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid firmware settings")
		return
	}
	if in.BuildServerURL == "" {
		in.BuildServerURL = defaultBuildServerURL
	}
	serverURL, ok := validBuildServerURL(in.BuildServerURL)
	if !ok {
		writeError(w, http.StatusBadRequest, "the build server address must be an https URL, or http to a private IP address")
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	var out firmwareSettings
	tx, err := a.db.Begin(r.Context())
	if err == nil {
		defer tx.Rollback(r.Context())
		err = tx.QueryRow(r.Context(), `INSERT INTO firmware_settings (id, build_server_enabled, build_server_url, updated_by, updated_at)
			VALUES (TRUE, $1, $2, $3, NOW())
			ON CONFLICT (id) DO UPDATE SET build_server_enabled=EXCLUDED.build_server_enabled, build_server_url=EXCLUDED.build_server_url,
				updated_by=EXCLUDED.updated_by, updated_at=NOW()
			RETURNING build_server_enabled, build_server_url, updated_at`, in.BuildServerEnabled, serverURL, actor.ID).
			Scan(&out.BuildServerEnabled, &out.BuildServerURL, &out.UpdatedAt)
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, actor.ID, "firmware.settings_update", "firmware", "settings", map[string]any{
			"build_server_enabled": out.BuildServerEnabled, "build_server_url": out.BuildServerURL,
		})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save firmware settings")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// buildServerClient fetches JSON from the build and upstream servers with a size
// cap, a timeout, no cross-host redirects, and a small time-limited cache, since
// the same release metadata is read for every device.
type buildServerClient struct {
	http  *http.Client
	mu    sync.Mutex
	cache map[string]cachedBody
}

type cachedBody struct {
	at   time.Time
	body []byte
}

func refuseCrossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 || req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
		return errors.New("redirect to another host refused")
	}
	return nil
}

func newBuildServerClient() *buildServerClient {
	return &buildServerClient{
		http:  &http.Client{Timeout: buildServerTimeout, CheckRedirect: refuseCrossHostRedirect},
		cache: map[string]cachedBody{},
	}
}

var buildServer = newBuildServerClient()

// errBuildServerNotFound means the server answered 404: the release, target or
// profile does not exist there.
var errBuildServerNotFound = errors.New("not found on the build server")

func (c *buildServerClient) getJSON(ctx context.Context, rawURL string, out any) error {
	c.mu.Lock()
	hit, ok := c.cache[rawURL]
	c.mu.Unlock()
	if ok && time.Since(hit.at) < buildServerCacheTTL {
		return json.Unmarshal(hit.body, out)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "OpenManage")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("could not reach the build server")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errBuildServerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the build server answered HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, buildServerBodyLimit+1))
	if err != nil {
		return errors.New("could not read the build server's answer")
	}
	if len(body) > buildServerBodyLimit {
		return errors.New("the build server's answer is too large")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("the build server's answer is not valid JSON")
	}
	c.mu.Lock()
	if len(c.cache) >= buildServerCacheSize {
		oldest := ""
		for k, v := range c.cache {
			if oldest == "" || v.at.Before(c.cache[oldest].at) {
				oldest = k
			}
		}
		delete(c.cache, oldest)
	}
	c.cache[rawURL] = cachedBody{at: time.Now(), body: body}
	c.mu.Unlock()
	return nil
}

// asuOverview is the part of ASU's /json/v1/overview.json the plan uses.
type asuOverview struct {
	Latest      []string             `json:"latest"`
	UpstreamURL string               `json:"upstream_url"`
	Branches    map[string]asuBranch `json:"branches"`
}

type asuBranch struct {
	Enabled        *bool              `json:"enabled"`
	Path           string             `json:"path"`
	Versions       []string           `json:"versions"`
	Targets        map[string]string  `json:"targets"`
	PackageChanges []asuPackageChange `json:"package_changes"`
}

// asuPackageChange is a package renamed (Target set) or dropped (Target empty) as of
// a revision, as ASU and owut apply them.
type asuPackageChange struct {
	Source    string `json:"source"`
	Target    string `json:"target"`
	Revision  int    `json:"revision"`
	Mandatory bool   `json:"mandatory"`
}

// releaseInfo is one release the build server offers.
type releaseInfo struct {
	Version  string `json:"version"`
	Branch   string `json:"branch"`
	Snapshot bool   `json:"snapshot"`
	RC       bool   `json:"release_candidate"`
}

type releaseVersion struct {
	parts    [3]int
	rc       int // 0 for a final release
	snapshot bool
	branchSn bool // "24.10-SNAPSHOT"
}

func parseReleaseVersion(v string) (releaseVersion, bool) {
	var rv releaseVersion
	if !releaseVersionPattern.MatchString(v) {
		return rv, false
	}
	if v == "SNAPSHOT" {
		rv.snapshot = true
		rv.parts = [3]int{999, 99, 999}
		return rv, true
	}
	base, rc, hasRC := strings.Cut(v, "-rc")
	if hasRC {
		rv.rc, _ = strconv.Atoi(rc)
	}
	if b, ok := strings.CutSuffix(base, "-SNAPSHOT"); ok {
		base, rv.branchSn, rv.parts[2] = b, true, 999
	}
	for i, p := range strings.Split(base, ".") {
		rv.parts[i], _ = strconv.Atoi(p)
	}
	return rv, true
}

// compareReleases orders releases: by number, a release candidate before its final
// release, and a branch snapshot after every release of that branch.
func compareReleases(a, b string) int {
	ra, okA := parseReleaseVersion(a)
	rb, okB := parseReleaseVersion(b)
	if !okA || !okB {
		return strings.Compare(a, b)
	}
	for i := range ra.parts {
		if ra.parts[i] != rb.parts[i] {
			return ra.parts[i] - rb.parts[i]
		}
	}
	rank := func(r releaseVersion) int {
		if r.rc > 0 {
			return r.rc - 1000
		}
		return 0
	}
	return rank(ra) - rank(rb)
}

func releaseBranch(v string) string {
	if v == "SNAPSHOT" {
		return "SNAPSHOT"
	}
	if b, ok := strings.CutSuffix(v, "-SNAPSHOT"); ok {
		return b
	}
	if len(v) >= 5 {
		return v[:5]
	}
	return v
}

func (a *app) firmwareBuildServer(ctx context.Context) (string, error) {
	s, err := a.loadFirmwareSettings(ctx)
	if err != nil {
		return "", errors.New("could not load firmware settings")
	}
	if !s.BuildServerEnabled {
		return "", errBuildServerDisabled
	}
	serverURL, ok := validBuildServerURL(s.BuildServerURL)
	if !ok {
		return "", errors.New("the stored build server address is invalid")
	}
	return serverURL, nil
}

// fetchOverview returns the overview with only enabled branches and valid versions.
func fetchOverview(ctx context.Context, serverURL string) (asuOverview, error) {
	var ov asuOverview
	if err := buildServer.getJSON(ctx, serverURL+"/json/v1/overview.json", &ov); err != nil {
		return ov, err
	}
	for name, b := range ov.Branches {
		if (b.Enabled != nil && !*b.Enabled) || (!strings.Contains(b.Path, "{version}") && b.Path != "snapshots") {
			delete(ov.Branches, name)
			continue
		}
		versions := b.Versions[:0]
		for _, v := range b.Versions {
			if releaseVersionPattern.MatchString(v) {
				versions = append(versions, v)
			}
		}
		b.Versions = versions
		ov.Branches[name] = b
	}
	return ov, nil
}

func overviewReleases(ov asuOverview) []releaseInfo {
	seen := map[string]bool{}
	out := []releaseInfo{}
	for name, b := range ov.Branches {
		for _, v := range b.Versions {
			if seen[v] {
				continue
			}
			seen[v] = true
			rv, _ := parseReleaseVersion(v)
			out = append(out, releaseInfo{Version: v, Branch: name, Snapshot: rv.snapshot || rv.branchSn, RC: rv.rc > 0})
		}
	}
	sort.Slice(out, func(i, j int) bool { return compareReleases(out[i].Version, out[j].Version) > 0 })
	return out
}

func buildServerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBuildServerDisabled):
		writeError(w, http.StatusConflict, "the firmware build server is turned off; an administrator can turn it on in the firmware settings")
	case errors.Is(err, errBuildServerNotFound):
		writeError(w, http.StatusNotFound, "the build server has no such release, target or device profile")
	default:
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

// listFirmwareReleases returns every release the build server offers, newest first.
func (a *app) listFirmwareReleases(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), buildServerTimeout)
	defer cancel()
	serverURL, err := a.firmwareBuildServer(ctx)
	if err != nil {
		buildServerError(w, err)
		return
	}
	ov, err := fetchOverview(ctx, serverURL)
	if err != nil {
		buildServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": overviewReleases(ov), "latest": ov.Latest})
}

// asuProfiles is the upstream profiles.json for one release and target.
type asuProfiles struct {
	VersionCode     string   `json:"version_code"`
	VersionNumber   string   `json:"version_number"`
	ArchPackages    string   `json:"arch_packages"`
	DefaultPackages []string `json:"default_packages"`
	LinuxKernel     struct {
		Version string `json:"version"`
	} `json:"linux_kernel"`
	Profiles map[string]asuProfile `json:"profiles"`
}

type asuProfile struct {
	DevicePackages   []string `json:"device_packages"`
	SupportedDevices []string `json:"supported_devices"`
	Titles           []struct {
		Vendor  string `json:"vendor"`
		Model   string `json:"model"`
		Variant string `json:"variant"`
	} `json:"titles"`
}

// resolveProfile maps a board name to an image profile the way ASU and owut do:
// the board name with commas as underscores, then any profile listing it as a
// supported device, then "generic" for targets such as x86.
func resolveProfile(boardName string, p asuProfiles) (string, bool) {
	want := strings.ReplaceAll(boardName, ",", "_")
	if _, ok := p.Profiles[want]; ok && want != "" {
		return want, true
	}
	names := make([]string, 0, len(p.Profiles))
	for name := range p.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, alias := range p.Profiles[name].SupportedDevices {
			if want != "" && strings.ReplaceAll(alias, ",", "_") == want {
				return name, true
			}
		}
	}
	if _, ok := p.Profiles["generic"]; ok {
		return "generic", true
	}
	return "", false
}

// profileDefaults joins the target's default packages and the profile's device
// packages, where "-name" in the device list removes a target default.
func profileDefaults(p asuProfiles, profile string) map[string]bool {
	device := p.Profiles[profile].DevicePackages
	removed := map[string]bool{}
	for _, d := range device {
		if name, ok := strings.CutPrefix(d, "-"); ok {
			removed[name] = true
		}
	}
	out := map[string]bool{}
	for _, d := range p.DefaultPackages {
		if !removed[d] {
			out[d] = true
		}
	}
	for _, d := range device {
		if !strings.HasPrefix(d, "-") {
			out[d] = true
		}
	}
	return out
}

type packageRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type firmwarePlan struct {
	Version         string          `json:"version"`
	VersionCode     string          `json:"version_code"`
	Kernel          string          `json:"kernel"`
	Target          string          `json:"target"`
	Profile         string          `json:"profile"`
	ProfileTitle    string          `json:"profile_title"`
	PackageManager  string          `json:"package_manager"`
	Packages        []string        `json:"packages"`
	Kept            []string        `json:"kept"`
	Renamed         []packageRename `json:"renamed"`
	Dropped         []string        `json:"dropped"`
	Unavailable     []string        `json:"unavailable"`
	RemovedDefaults []string        `json:"removed_defaults"`
	Blockers        []string        `json:"blockers"`
	Warnings        []string        `json:"warnings"`
	// stock is the image's default package list for the device's profile; the stock
	// diff reads it, the plan response does not carry it.
	stock []string
}

// planPackages decides what a build request for the new release would carry: the
// owner's packages that the official feeds still offer (after renames), plus
// "-name" for each image default the owner removed that is still a default. The
// owner's packages missing from the feeds are reported, not requested; they are
// typically third-party themes reinstalled after the upgrade.
func planPackages(m firmwareManifest, defaults map[string]bool, available map[string]bool, changes []asuPackageChange, fromRevision, toRevision int, plan *firmwarePlan) {
	rename := map[string]string{}
	drop := map[string]bool{}
	for _, c := range changes {
		// A change applies to a device built before it (revision at most the change's)
		// and for a release built at or after it. A device already past the change that
		// still has the old name got it from somewhere else, such as a custom feed, so the
		// change says nothing about it. An unknown installed revision applies everything.
		if c.Revision > toRevision || (fromRevision > 0 && fromRevision > c.Revision) || !packageNamePattern.MatchString(c.Source) {
			continue
		}
		if c.Target == "" {
			drop[c.Source] = true
		} else if packageNamePattern.MatchString(c.Target) {
			rename[c.Source] = c.Target
		}
	}
	// feedName returns the name to request: the installed name, or its ABI-free form when
	// that is what the v2 package index lists (libubox20240329 is listed as libubox).
	feedName := func(name string) (string, bool) {
		if available[name] {
			return name, true
		}
		if m := abiSuffixPattern.FindStringSubmatch(name); m != nil && available[m[1]] {
			return m[1], true
		}
		return "", false
	}
	requested := map[string]bool{}
	for _, name := range m.UserInstalled {
		switch {
		case drop[name]:
			plan.Dropped = append(plan.Dropped, name)
		case rename[name] != "":
			plan.Renamed = append(plan.Renamed, packageRename{From: name, To: rename[name]})
			requested[rename[name]] = true
		default:
			if requestName, ok := feedName(name); ok {
				plan.Kept = append(plan.Kept, requestName)
				requested[requestName] = true
			} else {
				plan.Unavailable = append(plan.Unavailable, name)
			}
		}
	}
	// A default the owner removed stays removed under its new name when the release
	// renamed it; a default the release dropped needs no removal.
	left := map[string]bool{}
	for _, name := range m.RemovedDefaults {
		if drop[name] {
			continue
		}
		if to := rename[name]; to != "" {
			name = to
		}
		if defaults[name] && !requested[name] && !left[name] {
			left[name] = true
			plan.RemovedDefaults = append(plan.RemovedDefaults, name)
		}
	}
	for name := range requested {
		plan.Packages = append(plan.Packages, name)
	}
	sort.Strings(plan.Packages)
	sort.Strings(plan.RemovedDefaults)
	for _, name := range plan.RemovedDefaults {
		plan.Packages = append(plan.Packages, "-"+name)
	}
}

func revisionNumber(versionCode string) int {
	if m := revisionNumberPattern.FindStringSubmatch(versionCode); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// firmwarePlanFor builds the plan for one device's newest inventory and one release.
func firmwarePlanFor(ctx context.Context, serverURL string, m firmwareManifest, version string) (firmwarePlan, error) {
	plan := firmwarePlan{Version: version, Target: m.Target, Packages: []string{}, Kept: []string{}, Renamed: []packageRename{},
		Dropped: []string{}, Unavailable: []string{}, RemovedDefaults: []string{}, Blockers: []string{}, Warnings: []string{}}
	ov, err := fetchOverview(ctx, serverURL)
	if err != nil {
		return plan, err
	}
	branch, ok := ov.Branches[releaseBranch(version)]
	if !ok || !containsString(branch.Versions, version) {
		return plan, errBuildServerNotFound
	}
	rv, _ := parseReleaseVersion(version)
	plan.PackageManager = "apk"
	if !rv.snapshot && rv.parts[0] < 25 {
		plan.PackageManager = "opkg"
	}

	if m.Extroot {
		plan.Blockers = append(plan.Blockers, "Packages live on external storage (extroot); upgrades are not supported for this device.")
	}
	if m.BoardName == "" {
		plan.Blockers = append(plan.Blockers, "The device did not report its board name, so its firmware image cannot be chosen.")
	}
	if m.PackageManager == "apk" && plan.PackageManager == "opkg" {
		plan.Blockers = append(plan.Blockers, "Going back from an apk release to an opkg release is not supported.")
	}
	if len(plan.Blockers) > 0 {
		return plan, nil
	}
	if !releaseTargetPattern.MatchString(m.Target) {
		plan.Blockers = append(plan.Blockers, "The device reported an unusual target, so its firmware image cannot be chosen.")
		return plan, nil
	}
	if _, ok := branch.Targets[m.Target]; !ok && len(branch.Targets) > 0 {
		plan.Blockers = append(plan.Blockers, "OpenWrt "+version+" has no images for the "+m.Target+" target.")
		return plan, nil
	}
	dir := strings.ReplaceAll(branch.Path, "{version}", version)
	upstream, ok := validBuildServerURL(ov.UpstreamURL)
	if !ok || !releaseDirPattern.MatchString(dir) {
		return plan, errors.New("the build server returned an unusable download address")
	}

	var profiles asuProfiles
	if err := buildServer.getJSON(ctx, upstream+"/"+dir+"/targets/"+m.Target+"/profiles.json", &profiles); err != nil {
		return plan, err
	}
	plan.VersionCode, plan.Kernel = cleanInventoryField(profiles.VersionCode), cleanInventoryField(profiles.LinuxKernel.Version)
	profile, found := resolveProfile(m.BoardName, profiles)
	if !found || !releaseProfilePattern.MatchString(profile) {
		if m.BoardName != "" {
			plan.Blockers = append(plan.Blockers, "OpenWrt "+version+" has no image for this device ("+m.BoardName+").")
		}
		return plan, nil
	}
	plan.Profile = profile
	if t := profiles.Profiles[profile].Titles; len(t) > 0 {
		plan.ProfileTitle = cleanInventoryField(strings.TrimSpace(t[0].Vendor + " " + t[0].Model + " " + t[0].Variant))
	}
	if !releaseArchPattern.MatchString(profiles.ArchPackages) {
		return plan, errors.New("the build server returned an unusable package architecture")
	}

	var targetIndex struct {
		Packages map[string]string `json:"packages"`
	}
	if err := buildServer.getJSON(ctx, serverURL+"/json/v1/"+dir+"/targets/"+m.Target+"/index.json", &targetIndex); err != nil {
		return plan, err
	}
	var archIndex map[string]string
	if err := buildServer.getJSON(ctx, serverURL+"/json/v1/"+dir+"/packages/"+profiles.ArchPackages+"-index.json", &archIndex); err != nil {
		return plan, err
	}
	available := map[string]bool{}
	for name := range targetIndex.Packages {
		available[name] = true
	}
	for name := range archIndex {
		available[name] = true
	}
	defaults := profileDefaults(profiles, profile)
	for name := range defaults {
		available[name] = true
	}
	plan.stock = sortedKeys(defaults)
	planPackages(m, defaults, available, branch.PackageChanges, revisionNumber(m.Revision), revisionNumber(profiles.VersionCode), &plan)

	switch c := compareReleases(version, m.Release); {
	case version == m.Release:
		plan.Warnings = append(plan.Warnings, "This is the release the device already runs; the upgrade would reinstall it.")
	case c < 0:
		plan.Warnings = append(plan.Warnings, "This release is older than the one the device runs. Downgrades can lose settings.")
	}
	if rv.snapshot || rv.branchSn {
		plan.Warnings = append(plan.Warnings, "Snapshots are development builds and can break without notice.")
	}
	if rv.rc > 0 {
		plan.Warnings = append(plan.Warnings, "This is a release candidate, not a final release.")
	}
	if releaseBranch(version) != releaseBranch(m.Release) {
		plan.Warnings = append(plan.Warnings, "This moves to a new release branch. Settings are kept, but some may need review afterwards.")
	}
	if m.PackageManager == "opkg" && plan.PackageManager == "apk" {
		plan.Warnings = append(plan.Warnings, "The package manager changes from opkg to apk. Packages you install yourself need apk builds after the upgrade.")
	}
	if len(plan.Unavailable) > 0 {
		plan.Warnings = append(plan.Warnings, "Some packages you installed are not in the official feeds for this release. They are left out of the image and must be installed again afterwards.")
	}
	if !m.ROMKnown {
		plan.Warnings = append(plan.Warnings, "The device has no firmware image to compare with, so the package list may include packages that came with it.")
	}
	return plan, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// getFirmwarePlan returns the upgrade plan for a device and ?version=. It needs a
// stored inventory and the build server turned on.
func (a *app) getFirmwarePlan(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.ToLower(r.PathValue("id"))
	version := r.URL.Query().Get("version")
	if !validUUID(deviceID) || !releaseVersionPattern.MatchString(version) {
		writeError(w, http.StatusBadRequest, "invalid device ID or release")
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
	plan, err := firmwarePlanFor(ctx, serverURL, inv.Manifest, version)
	if err != nil {
		buildServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"inventory_id": inv.ID, "current": inv.Manifest.Release, "plan": plan})
}
