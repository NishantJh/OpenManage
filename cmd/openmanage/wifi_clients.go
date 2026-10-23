package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Wi-Fi client tracking for the device location map (docs/device-location-map.md).
// Opt-in, run from the monitoring loop under its advisory lock. Each enrolled AP is
// asked, over the typed operation below, which clients hostapd has associated. A
// client's place is the AP it is associated with; nothing here measures position.

const (
	clientPollMin         = 60
	clientPollMax         = 600
	clientVisibleFor      = 24 * time.Hour      // dots disappear after this
	clientEventRetention  = 30 * 24 * time.Hour // roaming history
	clientLabelLimit      = 60
	clientBodyLimit       = 4 * 1024
	clientMaxInterfaces   = 16
	clientMaxPerInterface = 256
)

// wifiClientsOperation reads the hostapd association tables and the DHCP lease file
// (for device names). The command is fixed: it prints the first leases, then lists the
// hostapd ubus objects and asks each for its clients, nothing else.
var wifiClientsOperation = operation{
	Name: "wifi_clients", Label: "Wi-Fi clients", Class: operationRead, Roles: readOnlyRoles,
	Timeout: healthCheckTimeout, OutputLimit: 64 * 1024, Audit: auditBestEffort,
	command: `echo "##leases"; head -n 512 /tmp/dhcp.leases 2>/dev/null; for n in $(ubus list 'hostapd.*'); do echo "@@ $n"; ubus call "$n" get_clients; done`,
}

var hostapdObjectPattern = regexp.MustCompile(`^hostapd\.[A-Za-z0-9_.-]{1,32}$`)

type polledClient struct {
	MAC       string
	Interface string
	Signal    *int
	Name      string // DHCP hostname, empty if the router has none
}

var leaseNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,59}$`)

// parseDHCPLeases reads dnsmasq lease lines ("expiry mac ip hostname clientid") from the
// text before the first hostapd section. Hostnames come from the clients themselves, so
// anything that is not a plain hostname is dropped.
func parseDHCPLeases(preamble string) map[string]string {
	names := map[string]string{}
	_, body, ok := strings.Cut(preamble, "##leases\n")
	if !ok {
		return names
	}
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !leaseNamePattern.MatchString(f[3]) {
			continue
		}
		if mac, ok := normalizeMAC(f[1]); ok {
			names[mac] = f[3]
		}
	}
	return names
}

// parseWifiClients turns the operation's output into associated clients. Malformed
// sections are skipped; the device is untrusted, so names and counts are bounded.
func parseWifiClients(output string) []polledClient {
	var out []polledClient
	seen := map[string]bool{}
	interfaces := 0
	parts := strings.Split("\n"+output, "\n@@ ")
	leases := parseDHCPLeases(parts[0])
	for _, section := range parts[1:] {
		name, body, _ := strings.Cut(section, "\n")
		name = strings.TrimSpace(name)
		if !hostapdObjectPattern.MatchString(name) || interfaces >= clientMaxInterfaces {
			continue
		}
		interfaces++
		var parsed struct {
			Clients map[string]struct {
				Assoc  *bool `json:"assoc"`
				Signal *int  `json:"signal"`
			} `json:"clients"`
		}
		if json.Unmarshal([]byte(body), &parsed) != nil {
			continue
		}
		n := 0
		for rawMAC, c := range parsed.Clients {
			if n++; n > clientMaxPerInterface {
				break
			}
			mac, ok := normalizeMAC(rawMAC)
			if !ok || (c.Assoc != nil && !*c.Assoc) || seen[mac] {
				continue
			}
			seen[mac] = true
			sig := c.Signal
			if sig != nil && (*sig > 0 || *sig < -127) {
				sig = nil
			}
			out = append(out, polledClient{MAC: mac, Interface: strings.TrimPrefix(name, "hostapd."), Signal: sig, Name: leases[mac]})
		}
	}
	return out
}

func normalizeMAC(raw string) (string, bool) {
	hw, err := net.ParseMAC(strings.TrimSpace(raw))
	if err != nil || len(hw) != 6 {
		return "", false
	}
	return hw.String(), true
}

// maskMAC keeps the first three octets so the UI can tell devices apart without
// showing a full hardware address.
func maskMAC(mac string) string { return mac[:8] + ":••:••:••" }

func clientKey(key []byte, mac string) []byte {
	sub := hmac.New(sha256.New, key)
	sub.Write([]byte("openmanage:wifi-client-key:v1"))
	m := hmac.New(sha256.New, sub.Sum(nil))
	m.Write([]byte(mac))
	return m.Sum(nil)
}

func clientAAD(k []byte) string { return "openmanage:wifi-client:v1:" + hex.EncodeToString(k) }

type clientState struct {
	DeviceID  string
	Connected bool
}

// classifyAssociation says what event, if any, a client seen on deviceID produces
// given where it was last recorded.
func classifyAssociation(prev *clientState, deviceID string) string {
	switch {
	case prev == nil || !prev.Connected:
		return "connect"
	case prev.DeviceID != deviceID:
		return "roam"
	}
	return ""
}

// pollTracker remembers when each AP was last polled. In memory: after a restart
// every AP is simply polled once.
type pollTracker struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (t *pollTracker) due(id string, now time.Time, every time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return now.Sub(t.last[id]) >= every
}

func (t *pollTracker) mark(id string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	t.last[id] = at
}

func (a *app) runClientBatch(ctx context.Context, lockConn *pgxpool.Conn, settings monitoringSettings) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ids, err := queryDeviceIDs(queryCtx, lockConn, `SELECT d.id::text FROM devices d
		JOIN device_credentials dc ON dc.device_id=d.id ORDER BY d.created_at LIMIT 50`)
	cancel()
	if err != nil {
		log.Printf("client tracking schedule query failed")
		return
	}
	every := time.Duration(settings.ClientPollIntervalSeconds) * time.Second
	now := time.Now()
	var due []string
	for _, id := range ids {
		if a.clientPolls.due(id, now, every) {
			due = append(due, id)
		}
	}
	runBounded(ctx, due, a.pollWifiClients)
	a.pruneWifiClients(ctx)
}

func (a *app) pollWifiClients(parent context.Context, deviceID string) {
	ctx, cancel := context.WithTimeout(parent, healthCheckTimeout+3*time.Second)
	defer cancel()
	a.clientPolls.mark(deviceID, time.Now()) // failures wait a full interval too
	target, err := a.loadDeviceTarget(ctx, deviceID)
	if err != nil {
		return
	}
	session, err := openDeviceSession(ctx, target, healthCheckTimeout)
	if err != nil {
		return
	}
	result := session.run(wifiClientsOperation)
	session.Close()
	if result.Status != "ok" {
		return // unknown, not "disconnected": leave the recorded state alone
	}
	if err := a.applyClientPoll(ctx, deviceID, parseWifiClients(result.Output)); err != nil {
		log.Printf("client tracking could not store results for device %s", deviceID)
	}
}

func (a *app) applyClientPoll(ctx context.Context, deviceID string, clients []polledClient) error {
	key, err := encryptionKey()
	if err != nil {
		return err
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	listed := make([][]byte, 0, len(clients))
	for _, c := range clients {
		k := clientKey(key, c.MAC)
		listed = append(listed, k)
		sealed, err := sealSecret(key, []byte(c.MAC), clientAAD(k))
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO wifi_clients(client_key,mac_ciphertext,mac_hint,dhcp_name) VALUES($1,$2,$3,$4)
			ON CONFLICT (client_key) DO UPDATE SET last_seen=NOW(),
				dhcp_name=CASE WHEN $4 <> '' THEN $4 ELSE wifi_clients.dhcp_name END`, k, sealed, maskMAC(c.MAC), c.Name); err != nil {
			return err
		}
		var prev clientState
		var prevPtr *clientState
		err = tx.QueryRow(ctx, `SELECT device_id::text, connected FROM wifi_client_state WHERE client_key=$1 FOR UPDATE`, k).
			Scan(&prev.DeviceID, &prev.Connected)
		switch {
		case err == nil:
			prevPtr = &prev
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		kind := classifyAssociation(prevPtr, deviceID)
		if _, err = tx.Exec(ctx, `INSERT INTO wifi_client_state(client_key,device_id,interface,signal_dbm,connected)
			VALUES($1,$2,$3,$4,TRUE)
			ON CONFLICT (client_key) DO UPDATE SET device_id=$2, interface=$3, signal_dbm=$4, connected=TRUE, last_seen=NOW(),
				connected_since=CASE WHEN $5 = '' THEN wifi_client_state.connected_since ELSE NOW() END`,
			k, deviceID, c.Interface, c.Signal, kind); err != nil {
			return err
		}
		if kind != "" {
			var from *string
			if prevPtr != nil && kind == "roam" {
				from = &prev.DeviceID
			}
			if _, err = tx.Exec(ctx, `INSERT INTO wifi_client_events(client_key,kind,from_device_id,to_device_id,signal_dbm)
				VALUES($1,$2,$3,$4,$5)`, k, kind, from, deviceID, c.Signal); err != nil {
				return err
			}
		}
	}
	// Clients last seen on this AP and not listed now have left it.
	rows, err := tx.Query(ctx, `UPDATE wifi_client_state SET connected=FALSE
		WHERE device_id=$1 AND connected AND NOT (client_key = ANY($2::bytea[])) RETURNING client_key, signal_dbm`, deviceID, listed)
	if err != nil {
		return err
	}
	type gone struct {
		key []byte
		sig *int
	}
	var left []gone
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.key, &g.sig); err != nil {
			rows.Close()
			return err
		}
		left = append(left, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range left {
		if _, err = tx.Exec(ctx, `INSERT INTO wifi_client_events(client_key,kind,from_device_id,signal_dbm)
			VALUES($1,'disconnect',$2,$3)`, g.key, deviceID, g.sig); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// pruneWifiClients enforces the 24 hour rule for the map and the 30 day history.
// Labelled clients keep their name but lose their location.
func (a *app) pruneWifiClients(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	_, _ = a.db.Exec(ctx, `DELETE FROM wifi_client_state WHERE last_seen < NOW() - INTERVAL '24 hours'`)
	_, _ = a.db.Exec(ctx, `DELETE FROM wifi_client_events WHERE at < NOW() - INTERVAL '30 days'`)
	_, _ = a.db.Exec(ctx, `DELETE FROM wifi_clients c WHERE c.label='' AND c.last_seen < NOW() - INTERVAL '30 days'
		AND NOT EXISTS (SELECT 1 FROM wifi_client_state s WHERE s.client_key=c.client_key)`)
}

type wifiClientView struct {
	ID             string     `json:"id"`
	Label          string     `json:"label"`
	Name           string     `json:"name"` // hostname the router's DHCP server knows, if any
	MACHint        string     `json:"mac_hint"`
	Hidden         bool       `json:"hidden"`
	DeviceID       string     `json:"device_id"`
	Interface      string     `json:"interface"`
	SignalDBM      *int       `json:"signal_dbm"`
	Connected      bool       `json:"connected"`
	ConnectedSince *time.Time `json:"connected_since,omitempty"`
	LastSeen       time.Time  `json:"last_seen"`
}

// listWifiClients returns clients seen in the last 24 hours. A client counts as
// connected only if its AP was polled recently enough to trust the record.
func (a *app) listWifiClients(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), `SELECT encode(c.client_key,'hex'), c.label, c.dhcp_name, c.mac_hint, c.hidden,
			s.device_id::text, s.interface, s.signal_dbm,
			s.connected AND s.last_seen > NOW() - (2 * m.client_poll_interval_seconds * INTERVAL '1 second'),
			s.connected_since, s.last_seen
		FROM wifi_client_state s
		JOIN wifi_clients c ON c.client_key=s.client_key
		CROSS JOIN monitoring_settings m
		WHERE s.last_seen > NOW() - ($1 * INTERVAL '1 second')
		ORDER BY s.last_seen DESC LIMIT 500`, int(clientVisibleFor.Seconds()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load Wi-Fi clients")
		return
	}
	defer rows.Close()
	out := []wifiClientView{}
	for rows.Next() {
		var v wifiClientView
		if err := rows.Scan(&v.ID, &v.Label, &v.Name, &v.MACHint, &v.Hidden, &v.DeviceID, &v.Interface, &v.SignalDBM,
			&v.Connected, &v.ConnectedSince, &v.LastSeen); err != nil {
			writeError(w, http.StatusInternalServerError, "could not load Wi-Fi clients")
			return
		}
		out = append(out, v)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not load Wi-Fi clients")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var clientIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (a *app) updateWifiClient(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	id := r.PathValue("id")
	if !clientIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid client ID")
		return
	}
	var in struct {
		Label  *string `json:"label"`
		Hidden *bool   `json:"hidden"`
	}
	if err := decodeJSONBody(w, r, clientBodyLimit, &in); err != nil || (in.Label == nil && in.Hidden == nil) {
		writeError(w, http.StatusBadRequest, "request must set label or hidden")
		return
	}
	if in.Label != nil {
		trimmed := strings.TrimSpace(*in.Label)
		if len(trimmed) > clientLabelLimit {
			writeError(w, http.StatusBadRequest, "label must be at most 60 characters")
			return
		}
		in.Label = &trimmed
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update Wi-Fi client")
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), `UPDATE wifi_clients SET label=COALESCE($2,label), hidden=COALESCE($3,hidden)
		WHERE client_key=decode($1,'hex')`, id, in.Label, in.Hidden)
	if err == nil && tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "client not found")
		return
	}
	if err == nil {
		// The audit record names the client by its hashed ID prefix, never by MAC or label.
		err = insertAuditEvent(r.Context(), tx, actor.ID, "wifi_client.update", "wifi_client", id[:12],
			map[string]any{"label_changed": in.Label != nil, "hidden": in.Hidden})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update Wi-Fi client")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clearWifiClients deletes every tracked client, its location and its history.
func (a *app) clearWifiClients(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not clear Wi-Fi client data")
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), `DELETE FROM wifi_clients`)
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, actor.ID, "wifi_client.clear", "wifi_client", "",
			map[string]any{"clients": tag.RowsAffected()})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not clear Wi-Fi client data")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type wifiClientEvent struct {
	ID         int64     `json:"id"`
	ClientID   string    `json:"client_id"`
	Label      string    `json:"label"`
	MACHint    string    `json:"mac_hint"`
	Kind       string    `json:"kind"`
	FromDevice *string   `json:"from_device_id"`
	ToDevice   *string   `json:"to_device_id"`
	SignalDBM  *int      `json:"signal_dbm"`
	At         time.Time `json:"at"`
}

// listWifiClientEvents is the association history, newest first, for roaming tests.
func (a *app) listWifiClientEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	client := r.URL.Query().Get("client")
	if client != "" && !clientIDPattern.MatchString(client) {
		writeError(w, http.StatusBadRequest, "invalid client ID")
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT e.id, encode(e.client_key,'hex'), COALESCE(NULLIF(c.label,''),c.dhcp_name), c.mac_hint, e.kind,
			e.from_device_id::text, e.to_device_id::text, e.signal_dbm, e.at
		FROM wifi_client_events e JOIN wifi_clients c ON c.client_key=e.client_key
		WHERE ($1 = '' OR e.client_key = decode($1,'hex'))
		ORDER BY e.at DESC, e.id DESC LIMIT $2`, client, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load Wi-Fi client history")
		return
	}
	defer rows.Close()
	out := []wifiClientEvent{}
	for rows.Next() {
		var e wifiClientEvent
		if err := rows.Scan(&e.ID, &e.ClientID, &e.Label, &e.MACHint, &e.Kind, &e.FromDevice, &e.ToDevice, &e.SignalDBM, &e.At); err != nil {
			writeError(w, http.StatusInternalServerError, "could not load Wi-Fi client history")
			return
		}
		out = append(out, e)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not load Wi-Fi client history")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
