package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

var errWiFiOutputTruncated = errors.New("wireless command output exceeded the configured limit")

const (
	wifiScanTimeout = 30 * time.Second
	wifiScanLimit   = 4
	wifiOutputLimit = 12 * 1024
)

type wifiRoom struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type wifiWall struct {
	ID       string  `json:"id"`
	X1       float64 `json:"x1"`
	Y1       float64 `json:"y1"`
	X2       float64 `json:"x2"`
	Y2       float64 `json:"y2"`
	Material string  `json:"material"`
}

type wifiAPPlacement struct {
	DeviceID string  `json:"device_id"`
	Label    string  `json:"label"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
}

type wifiLayout struct {
	WidthM    float64           `json:"width_m"`
	HeightM   float64           `json:"height_m"`
	Rooms     []wifiRoom        `json:"rooms"`
	Walls     []wifiWall        `json:"walls"`
	APs       []wifiAPPlacement `json:"aps"`
	UpdatedAt *time.Time        `json:"updated_at,omitempty"`
}

type wifiInterfaceFailure struct {
	Interface string `json:"interface"`
	Reason    string `json:"reason"`
}

type wifiScanOutcome struct {
	Status               string                 `json:"status"`
	InterfacesDiscovered int                    `json:"interfaces_discovered"`
	InterfacesScanned    int                    `json:"interfaces_scanned"`
	InterfacesFailed     []wifiInterfaceFailure `json:"interfaces_failed"`
	InterfacesOmitted    int                    `json:"interfaces_omitted"`
	Observations         []wifiObservation      `json:"observations"`
	Count                int                    `json:"count"`
	CheckedAt            time.Time              `json:"checked_at"`
}

type wifiObservation struct {
	ID             int64     `json:"id,omitempty"`
	SourceDeviceID string    `json:"source_device_id"`
	SourceName     string    `json:"source_name,omitempty"`
	SourceRadio    string    `json:"source_radio"`
	SourceBSSID    string    `json:"source_bssid,omitempty"`
	ObservedBSSID  string    `json:"observed_bssid"`
	SSID           string    `json:"ssid,omitempty"`
	Channel        int       `json:"channel,omitempty"`
	SignalDBM      int       `json:"signal_dbm"`
	ObservedAt     time.Time `json:"observed_at"`
}

var (
	wifiIfaceLine = regexp.MustCompile("(?m)^\\s*Interface[[:space:]]+([[:alnum:]_.:-]+)[[:space:]]*$")
	wifiIfaceName = regexp.MustCompile("^[[:alnum:]_.:-]{1,32}$")
	wifiBSSIDLine = regexp.MustCompile("(?i)Access Point:[[:space:]]*([0-9a-f:]{17})")
	wifiCellStart = regexp.MustCompile("(?m)^Cell[[:space:]]+[0-9]+[[:space:]]+-[[:space:]]+Address:[[:space:]]*([0-9a-fA-F:]{17})")
	wifiSignal    = regexp.MustCompile("(?i)Signal:[[:space:]]*(-?[0-9]+)[[:space:]]*dBm")
	wifiChannel   = regexp.MustCompile("(?i)Channel:[[:space:]]*([0-9]+)")
	wifiSSID      = regexp.MustCompile("(?m)^\\s*ESSID:[[:space:]]*\\\"?(.*?)\\\"?[[:space:]]*$")
)

func (a *app) getWiFiLayout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var layout wifiLayout
	var updated time.Time
	var raw []byte
	err := a.db.QueryRow(ctx, "SELECT layout,updated_at FROM wifi_layouts WHERE name='Home'").Scan(&raw, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, wifiLayout{WidthM: 12, HeightM: 8, Rooms: []wifiRoom{}, Walls: []wifiWall{}, APs: []wifiAPPlacement{}})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load Wi-Fi layout")
		return
	}
	if err := json.Unmarshal(raw, &layout); err != nil {
		writeError(w, http.StatusInternalServerError, "stored Wi-Fi layout is invalid")
		return
	}
	layout.UpdatedAt = &updated
	writeJSON(w, http.StatusOK, layout)
}

func (a *app) saveWiFiLayout(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var layout wifiLayout
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&layout); err != nil {
		writeError(w, http.StatusBadRequest, "invalid Wi-Fi layout JSON")
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return
	}
	if err := validateWiFiLayout(layout); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	for _, ap := range layout.APs {
		var exists bool
		if err := a.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM devices WHERE id=$1)", ap.DeviceID).Scan(&exists); err != nil {
			writeError(w, http.StatusInternalServerError, "could not validate access point inventory")
			return
		}
		if !exists {
			writeError(w, http.StatusBadRequest, "layout references a device that is not in inventory")
			return
		}
	}
	user := r.Context().Value(userContextKey).(authUser)
	raw, err := json.Marshal(layout)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not encode Wi-Fi layout")
		return
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save Wi-Fi layout")
		return
	}
	defer tx.Rollback(ctx)
	var layoutID string
	err = tx.QueryRow(ctx, `INSERT INTO wifi_layouts(name,layout,updated_by,updated_at)
		VALUES('Home',$1,$2,NOW())
		ON CONFLICT(name) DO UPDATE SET layout=EXCLUDED.layout,updated_by=EXCLUDED.updated_by,updated_at=NOW()
		RETURNING id::text`, string(raw), user.ID).Scan(&layoutID)
	if err == nil {
		err = insertAuditEvent(ctx, tx, user.ID, "wifi.layout.updated", "wifi_layout", layoutID, map[string]any{
			"rooms": len(layout.Rooms), "walls": len(layout.Walls), "access_points": len(layout.APs),
		})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save Wi-Fi layout and audit event")
		return
	}
	now := time.Now().UTC()
	layout.UpdatedAt = &now
	writeJSON(w, http.StatusOK, layout)
}

func validateWiFiLayout(l wifiLayout) error {
	if !finiteWiFiNumber(l.WidthM) || !finiteWiFiNumber(l.HeightM) {
		return errors.New("layout dimensions must be finite numbers")
	}
	if l.WidthM < 2 || l.WidthM > 100 || l.HeightM < 2 || l.HeightM > 100 {
		return errors.New("layout dimensions must be between 2 and 100 metres")
	}
	if len(l.Rooms) > 100 || len(l.Walls) > 300 || len(l.APs) > 50 {
		return errors.New("layout exceeds the supported room, wall, or access-point limit")
	}
	seen := map[string]bool{}
	for _, room := range l.Rooms {
		if strings.TrimSpace(room.ID) == "" || strings.TrimSpace(room.Name) == "" || seen["room:"+room.ID] {
			return errors.New("each room needs a unique ID and a name")
		}
		seen["room:"+room.ID] = true
		if !finiteWiFiNumber(room.X) || !finiteWiFiNumber(room.Y) || !finiteWiFiNumber(room.Width) || !finiteWiFiNumber(room.Height) {
			return errors.New("room dimensions and coordinates must be finite numbers")
		}
		if room.Width <= 0 || room.Height <= 0 || room.X < 0 || room.Y < 0 || room.X+room.Width > l.WidthM || room.Y+room.Height > l.HeightM {
			return errors.New("room boundaries must fit inside the layout")
		}
	}
	for _, wall := range l.Walls {
		if strings.TrimSpace(wall.ID) == "" || seen["wall:"+wall.ID] {
			return errors.New("each wall needs a unique ID")
		}
		seen["wall:"+wall.ID] = true
		switch wall.Material {
		case "brick", "drywall", "glass", "open":
		default:
			return errors.New("wall material must be brick, drywall, glass, or open")
		}
		if !finiteWiFiNumber(wall.X1) || !finiteWiFiNumber(wall.Y1) || !finiteWiFiNumber(wall.X2) || !finiteWiFiNumber(wall.Y2) {
			return errors.New("wall coordinates must be finite numbers")
		}
		if wall.X1 < 0 || wall.X1 > l.WidthM || wall.X2 < 0 || wall.X2 > l.WidthM || wall.Y1 < 0 || wall.Y1 > l.HeightM || wall.Y2 < 0 || wall.Y2 > l.HeightM {
			return errors.New("wall coordinates must fit inside the layout")
		}
	}
	seenDevices := map[string]bool{}
	for _, ap := range l.APs {
		if !validUUID(ap.DeviceID) || seenDevices[ap.DeviceID] {
			return errors.New("each access point must reference a unique inventory device")
		}
		seenDevices[ap.DeviceID] = true
		if !finiteWiFiNumber(ap.X) || !finiteWiFiNumber(ap.Y) {
			return errors.New("access point coordinates must be finite numbers")
		}
		if ap.X < 0 || ap.X > l.WidthM || ap.Y < 0 || ap.Y > l.HeightM {
			return errors.New("access point coordinates must fit inside the layout")
		}
	}
	return nil
}

func finiteWiFiNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (a *app) listWiFiObservations(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	rows, err := a.db.Query(ctx, `SELECT o.id,o.source_device_id::text,d.name,o.source_radio,o.source_bssid,o.observed_bssid,o.ssid,o.channel,o.signal_dbm,o.observed_at
		FROM wifi_radio_observations o JOIN devices d ON d.id=o.source_device_id
		WHERE o.observed_at > NOW() - INTERVAL '30 days'
		ORDER BY o.observed_at DESC LIMIT 500`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not query Wi-Fi observations")
		return
	}
	defer rows.Close()
	out := make([]wifiObservation, 0)
	for rows.Next() {
		var o wifiObservation
		if err := rows.Scan(&o.ID, &o.SourceDeviceID, &o.SourceName, &o.SourceRadio, &o.SourceBSSID, &o.ObservedBSSID, &o.SSID, &o.Channel, &o.SignalDBM, &o.ObservedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read Wi-Fi observations")
			return
		}
		out = append(out, o)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read Wi-Fi observations")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) scanDeviceWiFi(w http.ResponseWriter, r *http.Request) {
	if !validUUID(r.PathValue("id")) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), wifiScanTimeout)
	defer cancel()
	deviceID := r.PathValue("id")
	var name, address string
	err := a.db.QueryRow(ctx, "SELECT name,address FROM devices WHERE id=$1", deviceID).Scan(&name, &address)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load device")
		return
	}
	if !isPrivateDeviceIP(address) {
		writeError(w, http.StatusUnprocessableEntity, "Wi-Fi scans only permit private IP addresses")
		return
	}
	var username, fingerprint string
	var ciphertext []byte
	err = a.db.QueryRow(ctx, "SELECT username,private_key_ciphertext,host_key_fingerprint FROM device_credentials WHERE device_id=$1", deviceID).Scan(&username, &ciphertext, &fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "device must be enrolled before scanning its radios")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load device credentials")
		return
	}
	key, err := encryptionKey()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "Wi-Fi scans are disabled: server encryption key is not configured")
		return
	}
	privateKey, err := decryptSecret(key, ciphertext)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stored SSH credential could not be decrypted")
		return
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stored SSH credential is not a supported private key")
		return
	}
	config := &ssh.ClientConfig{
		User: username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, Timeout: wifiScanTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, hostKey ssh.PublicKey) error {
			if ssh.FingerprintSHA256(hostKey) != fingerprint {
				return errors.New("host key fingerprint mismatch")
			}
			return nil
		},
	}
	dialer := net.Dialer{Timeout: wifiScanTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, "22"))
	if err != nil {
		a.recordWiFiScanFailure(r, deviceID, "connection_failed")
		writeError(w, http.StatusBadGateway, "could not connect to the enrolled device over SSH")
		return
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	deadline := time.Now().Add(wifiScanTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	sshConn, channels, requests, err := ssh.NewClientConn(conn, net.JoinHostPort(address, "22"), config)
	if err != nil {
		a.recordWiFiScanFailure(r, deviceID, "ssh_or_host_key_failed")
		writeError(w, http.StatusBadGateway, "SSH connection or pinned host-key verification failed")
		return
	}
	client := ssh.NewClient(sshConn, channels, requests)
	defer client.Close()

	outcome := wifiScanOutcome{Status: "failed", InterfacesFailed: []wifiInterfaceFailure{}, Observations: []wifiObservation{}, CheckedAt: time.Now().UTC()}
	list, err := runWiFiCommand(client, "iw dev", 4096)
	if ctx.Err() != nil {
		a.finishWiFiScanFailure(w, r, deviceID, http.StatusGatewayTimeout, "Wi-Fi scan timed out or was canceled", "context_done", outcome)
		return
	}
	if err != nil {
		reason := "interface_discovery_failed"
		if errors.Is(err, errWiFiOutputTruncated) {
			reason = "interface_discovery_output_truncated"
		}
		a.finishWiFiScanFailure(w, r, deviceID, http.StatusUnprocessableEntity, "could not safely list wireless interfaces", reason, outcome)
		return
	}
	matches := wifiIfaceLine.FindAllStringSubmatch(list, -1)
	outcome.InterfacesDiscovered = len(matches)
	if len(matches) == 0 {
		a.finishWiFiScanFailure(w, r, deviceID, http.StatusUnprocessableEntity, "no wireless interfaces were found by iw dev", "no_interfaces", outcome)
		return
	}
	if len(matches) > wifiScanLimit {
		outcome.InterfacesOmitted = len(matches) - wifiScanLimit
		matches = matches[:wifiScanLimit]
	}
	for _, match := range matches {
		if ctx.Err() != nil {
			a.finishWiFiScanFailure(w, r, deviceID, http.StatusGatewayTimeout, "Wi-Fi scan timed out or was canceled; no observations were saved", "context_done", outcome)
			return
		}
		iface := match[1]
		if !wifiIfaceName.MatchString(iface) {
			outcome.InterfacesFailed = append(outcome.InterfacesFailed, wifiInterfaceFailure{Interface: iface, Reason: "invalid_interface_name"})
			continue
		}
		info, infoErr := runWiFiCommand(client, "iwinfo "+iface+" info", 4096)
		if ctx.Err() != nil {
			a.finishWiFiScanFailure(w, r, deviceID, http.StatusGatewayTimeout, "Wi-Fi scan timed out or was canceled; no observations were saved", "context_done", outcome)
			return
		}
		if infoErr != nil {
			reason := "interface_info_failed"
			if errors.Is(infoErr, errWiFiOutputTruncated) {
				reason = "interface_info_output_truncated"
			}
			outcome.InterfacesFailed = append(outcome.InterfacesFailed, wifiInterfaceFailure{Interface: iface, Reason: reason})
			continue
		}
		sourceBSSID := ""
		if b := wifiBSSIDLine.FindStringSubmatch(info); len(b) == 2 {
			sourceBSSID = strings.ToUpper(b[1])
		}
		scan, scanErr := runWiFiCommand(client, "iwinfo "+iface+" scan", wifiOutputLimit)
		if ctx.Err() != nil {
			a.finishWiFiScanFailure(w, r, deviceID, http.StatusGatewayTimeout, "Wi-Fi scan timed out or was canceled; no observations were saved", "context_done", outcome)
			return
		}
		parsed, parseErr := parseWiFiScanOutput(deviceID, name, iface, sourceBSSID, scan, scanErr)
		if parseErr != nil {
			reason := "interface_scan_failed"
			if errors.Is(parseErr, errWiFiOutputTruncated) {
				reason = "interface_scan_output_truncated"
			}
			outcome.InterfacesFailed = append(outcome.InterfacesFailed, wifiInterfaceFailure{Interface: iface, Reason: reason})
			continue
		}
		outcome.InterfacesScanned++
		outcome.Observations = append(outcome.Observations, parsed...)
	}
	if ctx.Err() != nil {
		a.finishWiFiScanFailure(w, r, deviceID, http.StatusGatewayTimeout, "Wi-Fi scan timed out or was canceled; no observations were saved", "context_done", outcome)
		return
	}
	finalizeWiFiScanOutcome(&outcome)
	if len(outcome.Observations) == 0 {
		outcome.Status = "failed"
		a.finishWiFiScanFailure(w, r, deviceID, http.StatusUnprocessableEntity, "no neighbour RSSI observations were available from successful interface scans", "no_observations", outcome)
		return
	}
	outcome.Count = len(outcome.Observations)
	outcome.CheckedAt = time.Now().UTC()
	err = persistWiFiScan(ctx, func() error {
		tx, beginErr := a.db.Begin(ctx)
		if beginErr != nil {
			return beginErr
		}
		defer func() {
			rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer rollbackCancel()
			_ = tx.Rollback(rollbackCtx)
		}()
		for i := range outcome.Observations {
			o := &outcome.Observations[i]
			if insertErr := tx.QueryRow(ctx, `INSERT INTO wifi_radio_observations(source_device_id,source_radio,source_bssid,observed_bssid,ssid,channel,signal_dbm)
				VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id,observed_at`,
				o.SourceDeviceID, o.SourceRadio, o.SourceBSSID, o.ObservedBSSID, o.SSID, o.Channel, o.SignalDBM).Scan(&o.ID, &o.ObservedAt); insertErr != nil {
				return insertErr
			}
		}
		if _, deleteErr := tx.Exec(ctx, "DELETE FROM wifi_radio_observations WHERE observed_at < NOW() - INTERVAL '30 days'"); deleteErr != nil {
			return deleteErr
		}
		user := r.Context().Value(userContextKey).(authUser)
		if auditErr := insertAuditEvent(ctx, tx, user.ID, "wifi.radio.scan", "device", deviceID, wifiScanAuditDetails(outcome, "")); auditErr != nil {
			return auditErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		outcome.Status = "failed"
		outcome.Observations = []wifiObservation{}
		outcome.Count = 0
		reason := "persistence_failed"
		status := http.StatusInternalServerError
		message := "scan observations and audit event could not be saved"
		if ctx.Err() != nil {
			reason = "context_done"
			status = http.StatusGatewayTimeout
			message = "Wi-Fi scan timed out or was canceled; no observations were saved"
		}
		a.finishWiFiScanFailure(w, r, deviceID, status, message, reason, outcome)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": deviceID, "device_name": name, "status": outcome.Status,
		"interfaces_discovered": outcome.InterfacesDiscovered, "interfaces_scanned": outcome.InterfacesScanned,
		"interfaces_failed": outcome.InterfacesFailed, "interfaces_omitted": outcome.InterfacesOmitted,
		"observations": outcome.Observations, "count": outcome.Count, "checked_at": outcome.CheckedAt,
	})
}

type wifiOutputBuffer struct {
	builder   strings.Builder
	limit     int
	truncated bool
}

func (b *wifiOutputBuffer) Write(p []byte) (int, error) {
	written := len(p)
	remaining := b.limit - b.builder.Len()
	if remaining < 0 {
		remaining = 0
	}
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	if len(p) > 0 {
		_, _ = b.builder.Write(p)
	}
	return written, nil
}

func (b *wifiOutputBuffer) String() string {
	return b.builder.String()
}

func wifiCommandResult(stdout string, stdoutTruncated, stderrTruncated bool, commandErr error) (string, error) {
	stdout = strings.TrimSpace(stdout)
	if stdoutTruncated || stderrTruncated {
		return stdout, errWiFiOutputTruncated
	}
	if commandErr != nil {
		return stdout, commandErr
	}
	return stdout, nil
}

func runWiFiCommand(client *ssh.Client, command string, limit int) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	stdout := &wifiOutputBuffer{limit: limit}
	stderr := &wifiOutputBuffer{limit: 1024}
	session.Stdout, session.Stderr = stdout, stderr
	runErr := session.Run(command)
	return wifiCommandResult(stdout.String(), stdout.truncated, stderr.truncated, runErr)
}

func finalizeWiFiScanOutcome(outcome *wifiScanOutcome) {
	outcome.Count = len(outcome.Observations)
	if len(outcome.InterfacesFailed) > 0 || outcome.InterfacesOmitted > 0 {
		outcome.Status = "partial"
	} else {
		outcome.Status = "complete"
	}
}

func wifiScanAuditDetails(outcome wifiScanOutcome, reason string) map[string]any {
	details := map[string]any{
		"status":                outcome.Status,
		"interfaces_discovered": outcome.InterfacesDiscovered,
		"interfaces_scanned":    outcome.InterfacesScanned,
		"interfaces_failed":     outcome.InterfacesFailed,
		"interfaces_omitted":    outcome.InterfacesOmitted,
		"observations":          outcome.Count,
	}
	if reason != "" {
		details["reason"] = reason
	}
	return details
}

func persistWiFiScan(ctx context.Context, persist func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return persist()
}

func (a *app) finishWiFiScanFailure(w http.ResponseWriter, r *http.Request, deviceID string, status int, message, reason string, outcome wifiScanOutcome) {
	outcome.Status = "failed"
	outcome.Observations = []wifiObservation{}
	outcome.Count = 0
	outcome.CheckedAt = time.Now().UTC()
	a.recordWiFiScanOutcome(r, deviceID, "wifi.radio.scan.failed", wifiScanAuditDetails(outcome, reason))
	writeJSON(w, status, map[string]any{"error": message, "outcome": outcome})
}

func parseWiFiScanOutput(deviceID, name, iface, sourceBSSID, output string, commandErr error) ([]wifiObservation, error) {
	if commandErr != nil {
		return nil, commandErr
	}
	return parseWiFiScan(deviceID, name, iface, sourceBSSID, output), nil
}

func parseWiFiScan(deviceID, name, iface, sourceBSSID, output string) []wifiObservation {
	matches := wifiCellStart.FindAllStringSubmatchIndex(output, -1)
	out := make([]wifiObservation, 0, len(matches))
	now := time.Now().UTC()
	for i, m := range matches {
		end := len(output)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		block := output[m[0]:end]
		bssid := strings.ToUpper(output[m[2]:m[3]])
		signal := wifiSignal.FindStringSubmatch(block)
		if len(signal) != 2 {
			continue
		}
		dbm, err := strconv.Atoi(signal[1])
		if err != nil || dbm > 0 || dbm < -120 {
			continue
		}
		o := wifiObservation{SourceDeviceID: deviceID, SourceName: name, SourceRadio: iface, SourceBSSID: sourceBSSID, ObservedBSSID: bssid, SignalDBM: dbm, ObservedAt: now}
		if s := wifiSSID.FindStringSubmatch(block); len(s) == 2 {
			o.SSID = strings.TrimSpace(s[1])
		}
		if c := wifiChannel.FindStringSubmatch(block); len(c) == 2 {
			o.Channel, _ = strconv.Atoi(c[1])
		}
		out = append(out, o)
	}
	return out
}

func (a *app) recordWiFiScanFailure(r *http.Request, deviceID, reason string) {
	_, ok := r.Context().Value(userContextKey).(authUser)
	if !ok {
		return
	}
	a.recordWiFiScanOutcome(r, deviceID, "wifi.radio.scan.failed", map[string]any{"reason": reason})
}

func (a *app) recordWiFiScanOutcome(r *http.Request, deviceID, action string, details map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	user, ok := r.Context().Value(userContextKey).(authUser)
	if !ok {
		return
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	if err = insertAuditEvent(ctx, tx, user.ID, action, "device", deviceID, details); err == nil {
		_ = tx.Commit(ctx)
	}
}
