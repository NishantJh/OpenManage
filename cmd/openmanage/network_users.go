package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

// Network users are Wi-Fi identities checked by the RADIUS listener. They are
// managed like OpenManage users (disable, not delete) but cannot sign in here.

type networkUser struct {
	ID         string     `json:"id"`
	Username   string     `json:"username"`
	CreatedAt  time.Time  `json:"created_at"`
	Disabled   bool       `json:"disabled"`
	DisabledAt *time.Time `json:"disabled_at"`
}

const networkUserColumns = `id::text, username, created_at, disabled_at IS NOT NULL, disabled_at`

func scanNetworkUser(row pgx.Row, u *networkUser) error {
	return row.Scan(&u.ID, &u.Username, &u.CreatedAt, &u.Disabled, &u.DisabledAt)
}

func (a *app) listNetworkUsers(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT `+networkUserColumns+` FROM network_users ORDER BY username`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load network users")
		return
	}
	defer rows.Close()
	out := []networkUser{}
	for rows.Next() {
		var u networkUser
		if err := scanNetworkUser(rows, &u); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read network users")
			return
		}
		out = append(out, u)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read network users")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) createNetworkUser(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSONBody(w, r, userBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid network user")
		return
	}
	if !validUsername.MatchString(in.Username) {
		writeError(w, http.StatusBadRequest, "username must be 1-64 letters, digits, dots, dashes or underscores, starting with a letter or digit")
		return
	}
	if msg := checkNewPassword(in.Password); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create network user")
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create network user")
		return
	}
	defer tx.Rollback(r.Context())
	var u networkUser
	err = scanNetworkUser(tx.QueryRow(r.Context(), `INSERT INTO network_users(username,password_hash,created_by)
		VALUES($1,$2,NULLIF($3,'')::uuid) RETURNING `+networkUserColumns, in.Username, string(hash), actor.ID), &u)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, "a network user with that username already exists")
		return
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, actor.ID, "network_user.create", "network_user", u.ID,
			map[string]string{"username": u.Username})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create network user")
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (a *app) updateNetworkUser(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid network user ID")
		return
	}
	var in struct {
		Disabled *bool `json:"disabled"`
	}
	if err := decodeJSONBody(w, r, userBodyLimit, &in); err != nil || in.Disabled == nil {
		writeError(w, http.StatusBadRequest, "request must set disabled")
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update network user")
		return
	}
	defer tx.Rollback(r.Context())
	var before, after networkUser
	err = scanNetworkUser(tx.QueryRow(r.Context(), `SELECT `+networkUserColumns+` FROM network_users WHERE id=$1 FOR UPDATE`, id), &before)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "network user not found")
		return
	}
	if err == nil {
		err = scanNetworkUser(tx.QueryRow(r.Context(), `UPDATE network_users SET
			disabled_at=CASE WHEN $2 THEN COALESCE(disabled_at, NOW()) ELSE NULL END
			WHERE id=$1 RETURNING `+networkUserColumns, id, *in.Disabled), &after)
	}
	if err == nil && before.Disabled != after.Disabled {
		err = insertAuditEvent(r.Context(), tx, actor.ID, "network_user.update", "network_user", id,
			map[string]any{"username": after.Username, "disabled_before": before.Disabled, "disabled": after.Disabled})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update network user")
		return
	}
	if !after.Disabled {
		a.radius.clearUserFailures(after.Username)
	}
	writeJSON(w, http.StatusOK, after)
}

func (a *app) resetNetworkUserPassword(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid network user ID")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := decodeJSONBody(w, r, userBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid password request")
		return
	}
	if msg := checkNewPassword(in.Password); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not reset password")
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not reset password")
		return
	}
	defer tx.Rollback(r.Context())
	var username string
	err = tx.QueryRow(r.Context(), `UPDATE network_users SET password_hash=$2, password_changed_at=NOW()
		WHERE id=$1 RETURNING username`, id, string(hash)).Scan(&username)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "network user not found")
		return
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, actor.ID, "network_user.password_reset", "network_user", id,
			map[string]string{"username": username})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not reset password")
		return
	}
	a.radius.clearUserFailures(username)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type radiusClientInfo struct {
	DeviceID   string    `json:"device_id"`
	DeviceName string    `json:"device_name"`
	Address    string    `json:"address"`
	CreatedAt  time.Time `json:"created_at"`
	RotatedAt  time.Time `json:"rotated_at"`
}

// getRADIUSStatus reports whether the listener is on and which APs have a secret.
// Secrets themselves are never returned here.
func (a *app) getRADIUSStatus(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT d.id::text, d.name, d.address, rc.created_at, rc.rotated_at
		FROM radius_clients rc JOIN devices d ON d.id=rc.device_id ORDER BY d.name, d.id`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load RADIUS clients")
		return
	}
	defer rows.Close()
	clients := []radiusClientInfo{}
	for rows.Next() {
		var c radiusClientInfo
		if err := rows.Scan(&c.DeviceID, &c.DeviceName, &c.Address, &c.CreatedAt, &c.RotatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read RADIUS clients")
			return
		}
		clients = append(clients, c)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read RADIUS clients")
		return
	}
	certificate, _, err := loadRADIUSCertInfo(r.Context(), a.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the RADIUS certificate")
		return
	}
	out := map[string]any{"enabled": a.radius != nil, "listen_addr": "", "dropped_packets": int64(0), "clients": clients,
		"certificate": certificate}
	if a.radius != nil {
		out["listen_addr"] = a.radius.addr
		out["dropped_packets"] = a.radius.dropped.Load()
	}
	writeJSON(w, http.StatusOK, out)
}

// rotateRADIUSSecret creates or replaces a device's shared secret and returns it
// once, so it can be entered on the AP. Until the AP side is pushed through the
// change workflow, rotating breaks authentication until the AP is updated.
func (a *app) rotateRADIUSSecret(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	key, err := encryptionKey()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create RADIUS secret")
		return
	}
	defer tx.Rollback(r.Context())
	var address string
	err = tx.QueryRow(r.Context(), `SELECT address FROM devices WHERE id=$1 FOR UPDATE`, id).Scan(&address)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create RADIUS secret")
		return
	}
	// Requests are matched to devices by source address, so it must be an IP literal.
	if !isPrivateDeviceIP(address) {
		writeError(w, http.StatusBadRequest, "device address must be a private IP address")
		return
	}
	secret, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create RADIUS secret")
		return
	}
	sealed, err := sealSecret(key, []byte(secret), radiusSecretAAD(id))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create RADIUS secret")
		return
	}
	var created bool
	err = tx.QueryRow(r.Context(), `INSERT INTO radius_clients(device_id, secret_ciphertext, created_by)
		VALUES($1,$2,NULLIF($3,'')::uuid)
		ON CONFLICT (device_id) DO UPDATE SET secret_ciphertext=EXCLUDED.secret_ciphertext, rotated_at=NOW()
		RETURNING xmax=0`, id, sealed, actor.ID).Scan(&created)
	action := "radius.secret_rotate"
	if created {
		action = "radius.secret_create"
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, actor.ID, action, "device", id, map[string]string{})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create RADIUS secret")
		return
	}
	a.radius.invalidateClients()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret})
}

func (a *app) deleteRADIUSSecret(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not remove RADIUS secret")
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), `DELETE FROM radius_clients WHERE device_id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "device has no RADIUS secret")
		return
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, actor.ID, "radius.secret_delete", "device", id, map[string]string{})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not remove RADIUS secret")
		return
	}
	a.radius.invalidateClients()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
