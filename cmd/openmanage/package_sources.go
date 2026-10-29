package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// Package sources (docs/firmware-upgrades.md, "Package sources"): package files an
// administrator registers so they can be installed on devices later, such as a LuCI
// theme that is not in the official feeds. A file comes from an upload or a one-time
// https download. OpenManage reads its metadata, shows it (including install scripts,
// which run as root on a device), and stores its own copy only as a pending source. An
// administrator confirms the source by repeating the SHA-256 they were shown. Nothing is
// installed anywhere by this file.

const (
	packagePendingTTL   = time.Hour
	packageFetchTimeout = 90 * time.Second
	// packageRequestTimeout is the outer deadline for registering a package: the download
	// plus inspecting and storing the file.
	packageRequestTimeout = packageFetchTimeout + 30*time.Second
	packageSourceMaxCount = 100
	packageSourceMaxBytes = 512 << 20
	packageURLMax         = 512
	packageMaxRedirects   = 5
)

type packageSource struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Arch        string          `json:"arch"`
	Format      string          `json:"format"`
	SHA256      string          `json:"sha256"`
	Size        int64           `json:"size"`
	Origin      string          `json:"origin"`
	Status      string          `json:"status"`
	Info        json.RawMessage `json:"info"`
	CreatedAt   time.Time       `json:"created_at"`
	ConfirmedAt *time.Time      `json:"confirmed_at"`
}

const packageSourceColumns = `id, name, version, arch, format, sha256, size, origin, status, info, created_at, confirmed_at`

func scanPackageSource(row pgx.Row, s *packageSource) error {
	return row.Scan(&s.ID, &s.Name, &s.Version, &s.Arch, &s.Format, &s.SHA256, &s.Size, &s.Origin, &s.Status, &s.Info, &s.CreatedAt, &s.ConfirmedAt)
}

// publicAddress is false for addresses a download must never reach: this machine, the
// home network and link-local ranges. The check runs on the address actually dialled, so
// a name that resolves to a private address is refused too.
func publicAddress(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
		switch {
		case ip4[0] == 0, // "this network"; Linux routes 0.x.x.x to the local machine
			ip4[0] == 100 && ip4[1]&0xc0 == 64,                           // carrier-grade NAT, 100.64.0.0/10
			ip4[0] == 198 && ip4[1]&0xfe == 18,                           // benchmarking, 198.18.0.0/15
			ip4[0] >= 240,                                                // reserved and broadcast
			ip4[0] == 192 && ip4[1] == 0 && (ip4[2] == 0 || ip4[2] == 2), // IETF protocol assignments, TEST-NET-1
			ip4[0] == 192 && ip4[1] == 88 && ip4[2] == 99,                // 6to4 relay anycast
			ip4[0] == 198 && ip4[1] == 51 && ip4[2] == 100,               // TEST-NET-2
			ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113:                // TEST-NET-3
			return false
		}
	} else if len(ip) == net.IPv6len && !globalIPv6(ip) {
		return false
	}
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast())
}

// globalIPv6 is true only for global unicast space (2000::/3) outside the tunnelling and
// documentation prefixes, whose embedded addresses this check cannot vouch for.
func globalIPv6(ip net.IP) bool {
	if ip[0]&0xe0 != 0x20 {
		return false
	}
	switch {
	case ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8, // documentation, 2001:db8::/32
		ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0 && ip[3] == 0, // Teredo, 2001::/32
		ip[0] == 0x20 && ip[1] == 0x02:                             // 6to4, 2002::/16
		return false
	}
	return true
}

// packageDialControl is replaced in tests, which download from a local server.
var packageDialControl = func(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("refusing an unreadable address")
	}
	if ip := net.ParseIP(host); ip == nil || !publicAddress(ip) {
		return errors.New("refusing to download from a private or local address")
	}
	return nil
}

func newPackageFetchClient() *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, Control: func(n, a string, c syscall.RawConn) error { return packageDialControl(n, a, c) }}
	return &http.Client{
		Timeout: packageFetchTimeout,
		Transport: &http.Transport{
			Proxy:                 nil, // a proxy would hide the real destination from the address check
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConns:          1,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > packageMaxRedirects {
				return errors.New("too many redirects")
			}
			if !packageRedirectAllowed(req.URL) {
				return errors.New("a redirect away from GitHub was refused")
			}
			return nil
		},
	}
}

var packageFetchClient = newPackageFetchClient()

const githubBase = "https://github.com/"

var githubReleasePath = regexp.MustCompile(`^/[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}/releases/download/[A-Za-z0-9_.+-]{1,200}/[A-Za-z0-9_.+-]{1,200}$`)

// packageRedirectAllowed is the rule for where a GitHub release link may redirect: GitHub
// serves release assets from its own hosts.
func packageRedirectAllowed(u *url.URL) bool {
	return u.Scheme == "https" && (u.Host == "github.com" || strings.HasSuffix(u.Host, ".githubusercontent.com"))
}

// parsePackageURL accepts only a link to a GitHub release file, which is where OpenWrt
// themes and add-ons are published; anything else is uploaded. It returns the path under
// github.com that is fetched (the query string and fragment are dropped) and the link as
// remembered and audited.
func parsePackageURL(raw string) (path, origin string, err error) {
	raw = strings.TrimSpace(raw)
	u, perr := url.Parse(raw)
	if perr != nil || len(raw) > packageURLMax || u.Scheme != "https" || u.User != nil || u.Opaque != "" {
		return "", "", errors.New("enter an https link to a package file")
	}
	if u.Host != "github.com" || !githubReleasePath.MatchString(u.Path) {
		return "", "", errors.New("links must point to a GitHub release file (https://github.com/<owner>/<repo>/releases/download/<tag>/<file>); upload other files instead")
	}
	return u.Path, githubBase + strings.TrimPrefix(u.Path, "/"), nil
}

// fetchPackageFile downloads a GitHub release file by its path under github.com.
func fetchPackageFile(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, packageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubBase+strings.TrimPrefix(path, "/"), nil)
	if err != nil {
		return nil, errors.New("enter an https link to a package file")
	}
	req.Header.Set("User-Agent", "OpenManage")
	resp, err := packageFetchClient.Do(req)
	if err != nil {
		return nil, errors.New("could not download the file from that link")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the link answered HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > packageMaxFileBytes {
		return nil, errTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, packageMaxFileBytes+1))
	if err != nil {
		return nil, errors.New("the download was interrupted")
	}
	if len(data) > packageMaxFileBytes {
		return nil, errTooLarge
	}
	return data, nil
}

var errTooLarge = errors.New("the package file is larger than the 32 MiB limit")

func readPackageUpload(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, packageMaxFileBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, errors.New("send the package as a multipart form with a file field")
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the form has no file field")
		}
		if err != nil {
			return nil, errTooLarge
		}
		if part.FormName() != "file" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, packageMaxFileBytes+1))
		if err != nil || len(data) > packageMaxFileBytes {
			return nil, errTooLarge
		}
		return data, nil
	}
}

// createPackageSource registers a package file as a pending source.
func (a *app) createPackageSource(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	var data []byte
	origin := "upload"
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch mediaType {
	case "multipart/form-data":
		var err error
		if data, err = readPackageUpload(w, r); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	case "application/json":
		var in struct {
			URL string `json:"url"`
		}
		if err := decodeJSONBody(w, r, 4<<10, &in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request")
			return
		}
		path, shown, err := parsePackageURL(in.URL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// The server write timeout is shorter than a slow download; extend it for this response only.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(packageRequestTimeout + 5*time.Second))
		if data, err = fetchPackageFile(r.Context(), path); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		origin = shown
	default:
		writeError(w, http.StatusUnsupportedMediaType, "upload a file or send a JSON link")
		return
	}
	info, err := inspectPackage(data)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	infoJSON, _ := json.Marshal(info)

	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not register the package")
		return
	}
	defer tx.Rollback(r.Context())
	// Stale pending copies expire here, so there is no background job to run.
	if _, err = tx.Exec(r.Context(), `DELETE FROM package_sources WHERE status='pending' AND (created_at < NOW() - $1::interval OR sha256=$2)`,
		fmt.Sprintf("%d seconds", int(packagePendingTTL.Seconds())), hash); err != nil {
		writeError(w, http.StatusInternalServerError, "could not register the package")
		return
	}
	var existing int64
	err = tx.QueryRow(r.Context(), `SELECT id FROM package_sources WHERE sha256=$1 AND status='confirmed'`, hash).Scan(&existing)
	if err == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "this exact file is already registered", "code": "already_registered", "id": existing})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "could not register the package")
		return
	}
	// Concurrent registrations would otherwise all see the same totals; serialize the quota check.
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtext('package_sources.quota'))`); err != nil {
		writeError(w, http.StatusInternalServerError, "could not register the package")
		return
	}
	var count int
	var total int64
	if err = tx.QueryRow(r.Context(), `SELECT count(*), COALESCE(sum(size),0) FROM package_sources`).Scan(&count, &total); err != nil {
		writeError(w, http.StatusInternalServerError, "could not register the package")
		return
	}
	if count >= packageSourceMaxCount || total+int64(len(data)) > packageSourceMaxBytes {
		writeError(w, http.StatusConflict, "too many package sources are stored; delete unused ones first")
		return
	}
	var src packageSource
	err = scanPackageSource(tx.QueryRow(r.Context(), `INSERT INTO package_sources (name, version, arch, format, sha256, size, file, info, origin, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+packageSourceColumns,
		info.Name, info.Version, info.Arch, info.Format, hash, len(data), data, infoJSON, origin, user.ID), &src)
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "package_source.create", "package_source", strconv.FormatInt(src.ID, 10),
			map[string]any{"name": info.Name, "version": info.Version, "format": info.Format, "sha256": hash, "origin": origin, "scripts": scriptList(info)})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not register the package")
		return
	}
	writeJSON(w, http.StatusCreated, src)
}

func scriptList(info packageInfo) []string {
	names := make([]string, 0, len(info.Scripts))
	for _, s := range info.Scripts {
		names = append(names, s.Name)
	}
	return names
}

type confirmPackageRequest struct {
	SHA256 string `json:"sha256"`
}

// confirmPackageSource makes a reviewed pending source usable. The administrator repeats
// the SHA-256 shown on the review screen, so the file they confirm is the file they saw.
func (a *app) confirmPackageSource(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid package source")
		return
	}
	var in confirmPackageRequest
	if err := decodeJSONBody(w, r, 1<<10, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not confirm the package")
		return
	}
	defer tx.Rollback(r.Context())
	var src packageSource
	var expired bool
	err = tx.QueryRow(r.Context(), `SELECT `+packageSourceColumns+`, created_at < NOW() - $2::interval FROM package_sources WHERE id=$1 FOR UPDATE`,
		id, fmt.Sprintf("%d seconds", int(packagePendingTTL.Seconds()))).
		Scan(&src.ID, &src.Name, &src.Version, &src.Arch, &src.Format, &src.SHA256, &src.Size, &src.Origin, &src.Status, &src.Info, &src.CreatedAt, &src.ConfirmedAt, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "package source not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not confirm the package")
		return
	}
	switch {
	case src.Status != "pending":
		writeError(w, http.StatusConflict, "this package source is already confirmed")
		return
	case expired:
		writeError(w, http.StatusConflict, "this review expired; register the package again")
		return
	case !strings.EqualFold(strings.TrimSpace(in.SHA256), src.SHA256):
		writeError(w, http.StatusBadRequest, "the SHA-256 does not match the file under review")
		return
	}
	var existing int64
	if err = tx.QueryRow(r.Context(), `SELECT id FROM package_sources WHERE sha256=$1 AND status='confirmed'`, src.SHA256).Scan(&existing); err == nil {
		writeError(w, http.StatusConflict, "this exact file is already registered")
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "could not confirm the package")
		return
	}
	err = scanPackageSource(tx.QueryRow(r.Context(), `UPDATE package_sources SET status='confirmed', confirmed_by=$2, confirmed_at=NOW()
		WHERE id=$1 RETURNING `+packageSourceColumns, id, user.ID), &src)
	if err == nil {
		var info packageInfo
		_ = json.Unmarshal(src.Info, &info)
		err = insertAuditEvent(r.Context(), tx, user.ID, "package_source.confirm", "package_source", strconv.FormatInt(id, 10),
			map[string]any{"name": src.Name, "version": src.Version, "format": src.Format, "sha256": src.SHA256, "origin": src.Origin, "scripts": scriptList(info)})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not confirm the package")
		return
	}
	writeJSON(w, http.StatusOK, src)
}

// listPackageSources never returns file contents. Operators see confirmed sources;
// administrators also see the ones awaiting review.
func (a *app) listPackageSources(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	rows, err := a.db.Query(r.Context(), `SELECT `+packageSourceColumns+` FROM package_sources
		WHERE status='confirmed' OR ($1 AND created_at >= NOW() - $2::interval) ORDER BY name, version DESC, id DESC`,
		user.Role == "admin", fmt.Sprintf("%d seconds", int(packagePendingTTL.Seconds())))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load package sources")
		return
	}
	defer rows.Close()
	out := []packageSource{}
	for rows.Next() {
		var s packageSource
		if err := scanPackageSource(rows, &s); err != nil {
			writeError(w, http.StatusInternalServerError, "could not load package sources")
			return
		}
		out = append(out, s)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not load package sources")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) deletePackageSource(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid package source")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete the package source")
		return
	}
	defer tx.Rollback(r.Context())
	var name, version, hash, status string
	err = tx.QueryRow(r.Context(), `DELETE FROM package_sources WHERE id=$1 RETURNING name, version, sha256, status`, id).Scan(&name, &version, &hash, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "package source not found")
		return
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "package_source.delete", "package_source", strconv.FormatInt(id, 10),
			map[string]any{"name": name, "version": version, "sha256": hash, "status": status})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete the package source")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// isPackageSourceCreate matches POST /api/package-sources, which may download a file and so
// outlasts apiRequestTimeout.
func isPackageSourceCreate(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/api/package-sources"
}
