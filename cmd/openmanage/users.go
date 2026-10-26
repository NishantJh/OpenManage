package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

const (
	userBodyLimit = 4 * 1024
	// bcrypt ignores bytes past 72, so longer passwords would only look stronger.
	minNewPasswordLength = 12
	maxNewPasswordLength = 72
)

// validUsername keeps names readable in the audit log and free of lookalike tricks.
var validUsername = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var validRoles = map[string]bool{"admin": true, "operator": true, "readonly": true}

type userRecord struct {
	ID         string     `json:"id"`
	Username   string     `json:"username"`
	Role       string     `json:"role"`
	CreatedAt  time.Time  `json:"created_at"`
	Disabled   bool       `json:"disabled"`
	DisabledAt *time.Time `json:"disabled_at"`
}

const userColumns = `id::text, username, role, created_at, disabled_at IS NOT NULL, disabled_at`

func scanUser(row pgx.Row, u *userRecord) error {
	return row.Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt, &u.Disabled, &u.DisabledAt)
}

func checkNewPassword(password string) string {
	switch {
	case len(password) < minNewPasswordLength:
		return "password must be at least 12 characters"
	case len(password) > maxNewPasswordLength:
		return "password must be no more than 72 bytes"
	}
	return ""
}

func (a *app) listUsers(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	rows, err := a.db.Query(r.Context(), `SELECT `+userColumns+` FROM users ORDER BY created_at, username`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load users")
		return
	}
	defer rows.Close()
	out := []userRecord{}
	for rows.Next() {
		var u userRecord
		if err := scanUser(rows, &u); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read users")
			return
		}
		out = append(out, u)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read users")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) createUser(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	var in struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if err := decodeJSONBody(w, r, userBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid user")
		return
	}
	if !validUsername.MatchString(in.Username) {
		writeError(w, http.StatusBadRequest, "username must be 1-64 letters, digits, dots, dashes or underscores, starting with a letter or digit")
		return
	}
	if !validRoles[in.Role] {
		writeError(w, http.StatusBadRequest, "role must be admin, operator or readonly")
		return
	}
	if msg := checkNewPassword(in.Password); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create user")
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create user")
		return
	}
	defer tx.Rollback(r.Context())
	var u userRecord
	err = scanUser(tx.QueryRow(r.Context(), `INSERT INTO users(username,password_hash,role) VALUES($1,$2,$3)
		RETURNING `+userColumns, in.Username, string(hash), in.Role), &u)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, "a user with that username already exists")
		return
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, actor.ID, "user.create", "user", u.ID,
			map[string]string{"username": u.Username, "role": u.Role})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create user")
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

// lockActiveAdmins serialises changes that could remove the last active
// administrator, so two concurrent requests cannot each remove "the other" one.
func lockActiveAdmins(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM users WHERE role='admin' AND disabled_at IS NULL FOR UPDATE`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// updateUser changes a user's role or disables/re-enables them. Either change
// revokes the user's sessions, so a demotion takes effect immediately.
func (a *app) updateUser(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid user ID")
		return
	}
	var in struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
	}
	if err := decodeJSONBody(w, r, userBodyLimit, &in); err != nil || (in.Role == nil && in.Disabled == nil) {
		writeError(w, http.StatusBadRequest, "request must set role or disabled")
		return
	}
	if in.Role != nil && !validRoles[*in.Role] {
		writeError(w, http.StatusBadRequest, "role must be admin, operator or readonly")
		return
	}
	actor := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update user")
		return
	}
	defer tx.Rollback(r.Context())
	activeAdmins, err := lockActiveAdmins(r.Context(), tx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update user")
		return
	}
	var before userRecord
	err = scanUser(tx.QueryRow(r.Context(), `SELECT `+userColumns+` FROM users WHERE id=$1 FOR UPDATE`, id), &before)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update user")
		return
	}
	role, disabled := before.Role, before.Disabled
	if in.Role != nil {
		role = *in.Role
	}
	if in.Disabled != nil {
		disabled = *in.Disabled
	}
	wasActiveAdmin := before.Role == "admin" && !before.Disabled
	if wasActiveAdmin && (role != "admin" || disabled) && activeAdmins <= 1 {
		writeError(w, http.StatusConflict, "at least one active administrator must remain")
		return
	}
	var after userRecord
	err = scanUser(tx.QueryRow(r.Context(), `UPDATE users SET role=$2,
		disabled_at=CASE WHEN $3 THEN COALESCE(disabled_at, NOW()) ELSE NULL END
		WHERE id=$1 RETURNING `+userColumns, id, role, disabled), &after)
	if err == nil && (role != before.Role || disabled != before.Disabled) {
		if _, err = tx.Exec(r.Context(), `DELETE FROM sessions WHERE user_id=$1`, id); err == nil {
			err = insertAuditEvent(r.Context(), tx, actor.ID, "user.update", "user", id, map[string]any{
				"username": after.Username, "role_before": before.Role, "role": after.Role,
				"disabled_before": before.Disabled, "disabled": after.Disabled,
			})
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update user")
		return
	}
	writeJSON(w, http.StatusOK, after)
}

// resetUserPassword lets an administrator set another user's password. It revokes
// that user's sessions.
func (a *app) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) {
		writeError(w, http.StatusBadRequest, "invalid user ID")
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
	actor := r.Context().Value(userContextKey).(authUser)
	if err := a.setPassword(r.Context(), actor.ID, id, in.Password, "", "user.password_reset"); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not reset password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// changeOwnPassword requires the current password. It keeps the caller's current
// session and revokes their others.
func (a *app) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(userContextKey).(authUser)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeJSONBody(w, r, userBodyLimit, &in); err != nil || len(in.CurrentPassword) > maxPasswordLength {
		writeError(w, http.StatusBadRequest, "invalid password request")
		return
	}
	if msg := checkNewPassword(in.NewPassword); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if !a.verifyUserPassword(w, r, user.ID, in.CurrentPassword, "current password is incorrect") {
		return
	}
	keep := ""
	if cookie, err := r.Cookie("om_session"); err == nil {
		keep = tokenHash(cookie.Value)
	}
	if err := a.setPassword(r.Context(), user.ID, user.ID, in.NewPassword, keep, "user.password_change"); err != nil {
		writeError(w, http.StatusInternalServerError, "could not change password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// setPassword stores a new password hash and revokes the user's sessions except the
// one whose hash is keepSession.
func (a *app) setPassword(ctx context.Context, actorID, userID, password, keepSession, action string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var username string
	err = tx.QueryRow(ctx, `UPDATE users SET password_hash=$2, password_changed_at=NOW() WHERE id=$1 RETURNING username`,
		userID, string(hash)).Scan(&username)
	if err == nil {
		_, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2`, userID, keepSession)
	}
	if err == nil {
		err = insertAuditEvent(ctx, tx, actorID, action, "user", userID, map[string]string{"username": username})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	return err
}

// verifyUserPassword re-checks a signed-in user's password before a sensitive action
// (a password change, a backup download). Wrong passwords count towards the login
// limit, so these endpoints cannot be used to guess a password from a stolen session.
// It writes the error response itself and returns false when the check fails.
func (a *app) verifyUserPassword(w http.ResponseWriter, r *http.Request, userID, password, wrongMessage string) bool {
	clientIP := loginClientIP(r.RemoteAddr)
	if blocked, retryAfter := loginAttempts.isBlocked(clientIP, time.Now()); blocked {
		writeLoginRateLimited(w, retryAfter)
		return false
	}
	var hash string
	if err := a.db.QueryRow(r.Context(), `SELECT password_hash FROM users WHERE id=$1`, userID).Scan(&hash); err != nil {
		writeError(w, http.StatusInternalServerError, "could not verify password")
		return false
	}
	if len(password) > maxPasswordLength || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		loginAttempts.recordFailure(clientIP, time.Now())
		writeError(w, http.StatusForbidden, wrongMessage)
		return false
	}
	return true
}
