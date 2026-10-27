package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newWifiTestApp gives a PostgreSQL-backed app with tracking on and three APs: AP1 and AP2
// have SSH credentials, AP3 was added without a key.
func newWifiTestApp(t *testing.T) (*app, map[string]string) {
	t.Helper()
	db := newEnrollmentTestDB(t)
	t.Setenv("OPENMANAGE_ENCRYPTION_KEY", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
	ctx := context.Background()
	ids := map[string]string{}
	for i, name := range []string{"AP1", "AP2", "AP3"} {
		var id string
		if err := db.QueryRow(ctx, `INSERT INTO devices(name,address) VALUES($1,$2) RETURNING id::text`,
			name, fmt.Sprintf("192.168.1.%d", 2+i)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
		if name != "AP3" {
			if _, err := db.Exec(ctx, `INSERT INTO device_credentials(device_id,username,private_key_ciphertext,host_key_fingerprint)
				VALUES($1,'root','\x00','SHA256:test')`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(ctx, `UPDATE monitoring_settings SET client_tracking_enabled=TRUE, client_poll_interval_seconds=300`); err != nil {
		t.Fatal(err)
	}
	return &app{db: db}, ids
}

func wifiClientsByMAC(t *testing.T, a *app) map[string]wifiClientView {
	t.Helper()
	rec := httptest.NewRecorder()
	a.listWifiClients(rec, httptest.NewRequest(http.MethodGet, "/api/wifi-clients", nil))
	var got []wifiClientView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	out := map[string]wifiClientView{}
	for _, v := range got {
		out[v.MACHint] = v
	}
	return out
}

// phone returns a client whose masked MAC (first three octets) is unique per n.
func phone(n int) polledClient {
	return polledClient{MAC: fmt.Sprintf("%02x:bb:cc:00:00:01", n), Interface: "phy0-ap0"}
}

// Reproduces the field report: devices that moved to an AP OpenManage does not read stay red,
// "last on AP1", although they are connected.
func TestWifiClientsRoamToUnreadAP(t *testing.T) {
	a, ids := newWifiTestApp(t)
	ctx := context.Background()
	if err := a.applyClientPoll(ctx, ids["AP1"], []polledClient{phone(1), phone(2), phone(3)}); err != nil {
		t.Fatal(err)
	}
	if err := a.applyClientPoll(ctx, ids["AP2"], []polledClient{phone(4)}); err != nil {
		t.Fatal(err)
	}
	// Phone 2 walks to AP3, which has no SSH key, so only AP1's next poll says anything.
	if err := a.applyClientPoll(ctx, ids["AP1"], []polledClient{phone(1), phone(3)}); err != nil {
		t.Fatal(err)
	}
	got := wifiClientsByMAC(t, a)
	p2 := got[maskMAC(phone(2).MAC)]
	if p2.Connected || p2.DeviceID != ids["AP1"] {
		t.Fatalf("phone 2 should be red, last on AP1: %+v", p2)
	}
	if p1 := got[maskMAC(phone(1).MAC)]; !p1.Connected {
		t.Fatalf("phone 1 is still on AP1 and should be green: %+v", p1)
	}
}

// The status list names the AP that is never read, so the red devices above are explained.
func TestWifiPollStatusNamesAPWithoutKey(t *testing.T) {
	a, ids := newWifiTestApp(t)
	ctx := context.Background()
	a.clientPolls.setStatus(ids["AP1"], true, "", 2)
	a.clientPolls.setStatus(ids["AP2"], false, "could not connect over SSH", 0)
	if _, err := a.db.Exec(ctx, `INSERT INTO devices(name,address,role) VALUES('gateway','192.168.1.1','router')`); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.wifiPollStatus(rec, httptest.NewRequest(http.MethodGet, "/api/wifi-clients/status", nil))
	var got []pollStatus
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	byID := map[string]pollStatus{}
	for _, s := range got {
		byID[s.DeviceID] = s
	}
	if len(got) != 3 || !byID[ids["AP1"]].OK || byID[ids["AP2"]].OK {
		t.Fatalf("unexpected statuses: %+v", got)
	}
	if s, ok := byID[ids["AP3"]]; !ok || s.OK || s.Error != noKeyPollError {
		t.Fatalf("AP3 without a key must be reported, got %+v", got)
	}
}
