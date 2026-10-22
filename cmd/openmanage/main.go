package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

//go:embed all:web/dist
var webFS embed.FS

type app struct {
	db           *pgxpool.Pool
	secureCookie bool
}

type contextKey string

const userContextKey contextKey = "openmanage-user"

type authUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type device struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Address    string    `json:"address"`
	Model      string    `json:"model"`
	Firmware   string    `json:"firmware"`
	Status     string    `json:"status"`
	LastSeenAt time.Time `json:"last_seen_at"`
	CreatedAt  time.Time `json:"created_at"`
	// Enrollable reports whether the address meets enrollment rules (a private IP literal).
	Enrollable bool `json:"enrollable"`
}

type auditEvent struct {
	ID           int64           `json:"id"`
	Username     string          `json:"username"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Details      json.RawMessage `json:"details"`
	CreatedAt    time.Time       `json:"created_at"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := env("DATABASE_URL", "postgres://openmanage:openmanage@localhost:5432/openmanage?sslmode=disable")
	var pool *pgxpool.Pool
	var err error
	for i := 1; i <= 30; i++ {
		poolConfig, configErr := pgxpool.ParseConfig(dsn)
		if configErr != nil {
			log.Fatalf("parse database configuration: %v", configErr)
		}
		// Leave capacity for HTTP requests while the monitoring lock and bounded
		// SSH workers are active. This remains modest for the single-container MVP.
		poolConfig.MaxConns = 10
		poolConfig.MinConns = 2
		poolConfig.MaxConnLifetime = 30 * time.Minute
		poolConfig.MaxConnIdleTime = 5 * time.Minute
		poolConfig.HealthCheckPeriod = 30 * time.Second

		pool, err = pgxpool.NewWithConfig(ctx, poolConfig)
		if err == nil {
			err = pool.Ping(ctx)
			if err == nil {
				break
			}
			pool.Close()
		}
		log.Printf("database not ready (%d/30): %v", i, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer pool.Close()

	migrations, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		log.Fatal(err)
	}
	sort.Strings(migrations)
	for _, name := range migrations {
		sql, readErr := migrationFS.ReadFile(name)
		if readErr != nil {
			log.Fatalf("read migration %s: %v", name, readErr)
		}
		if _, err = pool.Exec(ctx, string(sql)); err != nil {
			log.Fatalf("database migration %s: %v", name, err)
		}
	}
	if err = bootstrapAdmin(ctx, pool); err != nil {
		log.Fatalf("admin bootstrap: %v", err)
	}

	a := &app{db: pool, secureCookie: strings.EqualFold(env("OPENMANAGE_COOKIE_SECURE", "false"), "true")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthz", a.health)
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("GET /api/auth/me", a.me)
	mux.HandleFunc("POST /api/auth/logout", a.logout)
	mux.HandleFunc("GET /api/audit", a.listAudit)
	mux.HandleFunc("GET /api/devices", a.listDevices)
	mux.HandleFunc("POST /api/devices", a.createDevice)
	mux.HandleFunc("GET /api/devices/{id}", a.getDevice)
	mux.HandleFunc("DELETE /api/devices/{id}", a.deleteDevice)
	mux.HandleFunc("POST /api/devices/{id}/enroll", a.enrollDevice)
	mux.HandleFunc("POST /api/devices/{id}/health-check", a.runDeviceHealthCheck)
	mux.HandleFunc("POST /api/devices/{id}/config-snapshots", a.captureConfigSnapshot)
	mux.HandleFunc("GET /api/devices/{id}/config-snapshots", a.listConfigSnapshots)
	mux.HandleFunc("GET /api/devices/{id}/config-snapshots/{snapshot}/changes", a.configSnapshotChanges)
	mux.HandleFunc("GET /api/monitoring/settings", a.getMonitoringSettings)
	mux.HandleFunc("PUT /api/monitoring/settings", a.updateMonitoringSettings)
	mux.HandleFunc("GET /api/monitoring/history", a.listMonitoringHistory)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		a.monitorLoop(ctx)
	}()

	handler := securityHeaders(frontendFallback(apiDeadline(a.authMiddleware(mux))))
	srv := &http.Server{
		Addr:              env("LISTEN_ADDR", ":8080"),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Printf("OpenManage listening on %s", srv.Addr)
		if e := srv.ListenAndServe(); e != nil && !errors.Is(e, http.ErrServerClosed) {
			log.Fatalf("HTTP server: %v", e)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		log.Printf("shutdown: %v", err)
	}
	monitorWait, cancelMonitor := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelMonitor()
	select {
	case <-monitorDone:
	case <-monitorWait.Done():
		log.Printf("monitoring scheduler did not stop before timeout")
	}
}

func bootstrapAdmin(ctx context.Context, db *pgxpool.Pool) error {
	var count int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	username := strings.TrimSpace(os.Getenv("OPENMANAGE_ADMIN_USERNAME"))
	password := os.Getenv("OPENMANAGE_ADMIN_PASSWORD")
	if username == "" || len(password) < 12 || strings.Contains(strings.ToLower(password), "replace-with") {
		return errors.New("no users exist: set OPENMANAGE_ADMIN_USERNAME and OPENMANAGE_ADMIN_PASSWORD (at least 12 characters) for first startup")
	}
	if len(password) > 72 {
		return errors.New("OPENMANAGE_ADMIN_PASSWORD must be no more than 72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, "INSERT INTO users(username,password_hash,role) VALUES($1,$2,'admin') ON CONFLICT (username) DO NOTHING", username, string(hash))
	if err != nil {
		return err
	}
	log.Printf("created initial OpenManage administrator %q", username)
	return nil
}

func (a *app) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/healthz" || r.URL.Path == "/api/auth/login" {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie("om_session")
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		hash := tokenHash(cookie.Value)
		var user authUser
		err = a.db.QueryRow(r.Context(), `
			SELECT u.id::text,u.username,u.role
			FROM sessions s JOIN users u ON u.id=s.user_id
			WHERE s.token_hash=$1 AND s.expires_at > NOW()`, hash).Scan(&user.ID, &user.Username, &user.Role)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "session expired or invalid")
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			csrfCookie, cookieErr := r.Cookie("om_csrf")
			csrfHeader := r.Header.Get("X-CSRF-Token")
			if cookieErr != nil || csrfCookie.Value == "" || csrfHeader == "" ||
				subtle.ConstantTimeCompare([]byte(csrfCookie.Value), []byte(csrfHeader)) != 1 {
				writeError(w, http.StatusForbidden, "CSRF validation failed")
				return
			}
		}
		ctx := context.WithValue(r.Context(), userContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAnyRole enforces authorization at the API boundary; UI visibility is not a security control.
func requireAnyRole(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	user, ok := r.Context().Value(userContextKey).(authUser)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return false
	}
	for _, role := range allowed {
		if user.Role == role {
			return true
		}
	}
	writeError(w, http.StatusForbidden, "insufficient role for this action")
	return false
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSONBody(w, r, loginBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if len(in.Username) > maxUsernameLength || len(in.Password) > maxPasswordLength {
		writeError(w, http.StatusBadRequest, "username or password is too long")
		return
	}
	clientIP := loginClientIP(r.RemoteAddr)
	if blocked, retryAfter := loginAttempts.isBlocked(clientIP, time.Now()); blocked {
		writeLoginRateLimited(w, retryAfter)
		return
	}
	var user authUser
	var passwordHash string
	err := a.db.QueryRow(r.Context(), "SELECT id::text,username,role,password_hash FROM users WHERE username=$1", in.Username).
		Scan(&user.ID, &user.Username, &user.Role, &passwordHash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(in.Password)) != nil {
		loginAttempts.recordFailure(clientIP, time.Now())
		time.Sleep(300 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	loginAttempts.clear(clientIP)

	sessionToken, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	csrfToken, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), "DELETE FROM sessions WHERE expires_at <= NOW()"); err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,NOW()+INTERVAL '12 hours')", tokenHash(sessionToken), user.ID)
	}
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "auth.login", "session", user.ID, map[string]string{})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session and audit event")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "om_session", Value: sessionToken, Path: "/", HttpOnly: true, Secure: a.secureCookie, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	http.SetCookie(w, &http.Cookie{Name: "om_csrf", Value: csrfToken, Path: "/", HttpOnly: false, Secure: a.secureCookie, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "csrf_token": csrfToken})
}

func (a *app) me(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(userContextKey).(authUser)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	cookie, err := r.Cookie("om_csrf")
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "session requires login")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "csrf_token": cookie.Value})
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	cookie, _ := r.Cookie("om_session")
	user, ok := r.Context().Value(userContextKey).(authUser)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not end session")
		return
	}
	defer tx.Rollback(r.Context())
	if cookie != nil && cookie.Value != "" {
		if _, err = tx.Exec(r.Context(), "DELETE FROM sessions WHERE token_hash=$1", tokenHash(cookie.Value)); err != nil {
			writeError(w, http.StatusInternalServerError, "could not end session")
			return
		}
	}
	if err = insertAuditEvent(r.Context(), tx, user.ID, "auth.logout", "session", user.ID, map[string]string{}); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not end session and record audit event")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "om_session", Value: "", Path: "/", HttpOnly: true, Secure: a.secureCookie, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "om_csrf", Value: "", Path: "/", HttpOnly: false, Secure: a.secureCookie, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	if err := a.db.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "database": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "ok"})
}

func (a *app) listDevices(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), `SELECT id::text,name,address,model,firmware,status,last_seen_at,created_at FROM devices ORDER BY created_at DESC LIMIT 500`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not query devices")
		return
	}
	defer rows.Close()
	out := make([]device, 0)
	for rows.Next() {
		var d device
		if err := rows.Scan(&d.ID, &d.Name, &d.Address, &d.Model, &d.Firmware, &d.Status, &d.LastSeenAt, &d.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read devices")
			return
		}
		d.Enrollable = isPrivateDeviceIP(d.Address)
		out = append(out, d)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read devices")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) createDevice(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	var in struct {
		Name     string `json:"name"`
		Address  string `json:"address"`
		Model    string `json:"model"`
		Firmware string `json:"firmware"`
	}
	if err := decodeJSONBody(w, r, deviceBodyLimit, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Address = strings.TrimSpace(in.Address)
	in.Model = strings.TrimSpace(in.Model)
	in.Firmware = strings.TrimSpace(in.Firmware)
	if in.Name == "" || in.Address == "" {
		writeError(w, http.StatusBadRequest, "name and address are required")
		return
	}
	if len(in.Name) > maxDeviceFieldLength || len(in.Address) > maxDeviceAddressLength || len(in.Model) > maxDeviceFieldLength || len(in.Firmware) > maxDeviceFieldLength {
		writeError(w, http.StatusBadRequest, "device fields are too long")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create device")
		return
	}
	defer tx.Rollback(r.Context())
	var d device
	err = tx.QueryRow(r.Context(), `INSERT INTO devices(name,address,model,firmware,status) VALUES($1,$2,$3,$4,'unknown') RETURNING id::text,name,address,model,firmware,status,last_seen_at,created_at`, in.Name, in.Address, in.Model, in.Firmware).
		Scan(&d.ID, &d.Name, &d.Address, &d.Model, &d.Firmware, &d.Status, &d.LastSeenAt, &d.CreatedAt)
	if err == nil {
		err = insertAuditEvent(r.Context(), tx, user.ID, "device.create", "device", d.ID, map[string]string{"name": d.Name, "address": d.Address})
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create device and record audit event")
		return
	}
	d.Enrollable = isPrivateDeviceIP(d.Address)
	writeJSON(w, http.StatusCreated, d)
}

func (a *app) getDevice(w http.ResponseWriter, r *http.Request) {
	if !validUUID(r.PathValue("id")) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	var d device
	err := a.db.QueryRow(r.Context(), `SELECT id::text,name,address,model,firmware,status,last_seen_at,created_at FROM devices WHERE id=$1`, r.PathValue("id")).
		Scan(&d.ID, &d.Name, &d.Address, &d.Model, &d.Firmware, &d.Status, &d.LastSeenAt, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) || err != nil {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	d.Enrollable = isPrivateDeviceIP(d.Address)
	writeJSON(w, http.StatusOK, d)
}

func (a *app) deleteDevice(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin", "operator") {
		return
	}
	if !validUUID(r.PathValue("id")) {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}
	user := r.Context().Value(userContextKey).(authUser)
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete device")
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), "DELETE FROM devices WHERE id=$1", r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete device")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err = insertAuditEvent(r.Context(), tx, user.ID, "device.delete", "device", r.PathValue("id"), map[string]string{"id": r.PathValue("id")}); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete device and record audit event")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) listAudit(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	rows, err := a.db.Query(r.Context(), `
		SELECT e.id, COALESCE(u.username,'system'),e.action,e.resource_type,e.resource_id::text,e.details,e.created_at
		FROM audit_events e LEFT JOIN users u ON u.id=e.user_id
		ORDER BY e.created_at DESC LIMIT 100`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not query audit events")
		return
	}
	defer rows.Close()
	out := make([]auditEvent, 0)
	for rows.Next() {
		var e auditEvent
		if err := rows.Scan(&e.ID, &e.Username, &e.Action, &e.ResourceType, &e.ResourceID, &e.Details, &e.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read audit events")
			return
		}
		out = append(out, e)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "could not read audit events")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func insertAuditEvent(ctx context.Context, tx pgx.Tx, userID, action, resourceType, resourceID string, details any) error {
	data, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		"INSERT INTO audit_events(user_id,action,resource_type,resource_id,details) VALUES($1,$2,$3,$4,$5)",
		userID, action, resourceType, resourceID, string(data))
	return err
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return false
			}
			continue
		}
		ch := value[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

func randomToken(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func frontendFallback(next http.Handler) http.Handler {
	assets := frontendHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/" {
			// Serve real embedded assets when present; fall back to index.html for SPA routes.
			sub, err := fs.Sub(webFS, "web/dist")
			if err == nil {
				if _, statErr := fs.Stat(sub, strings.TrimPrefix(r.URL.Path, "/")); statErr == nil {
					assets.ServeHTTP(w, r)
					return
				}
			}
		}
		index, err := webFS.ReadFile("web/dist/index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}

func frontendHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// Request-size limits. Small JSON APIs get tight caps; enrollment carries a private key.
const (
	loginBodyLimit         = 4 * 1024
	deviceBodyLimit        = 8 * 1024
	enrollmentBodyLimit    = 32 * 1024
	monitoringBodyLimit    = 4 * 1024
	maxUsernameLength      = 128
	maxPasswordLength      = 1024
	maxDeviceFieldLength   = 128
	maxDeviceAddressLength = 255

	// apiRequestTimeout bounds database and device work per API request. It must stay
	// below the server WriteTimeout so handlers can still write their error response.
	apiRequestTimeout  = 25 * time.Second
	serverWriteTimeout = 30 * time.Second
)

var errTrailingJSON = errors.New("request must contain a single JSON object")

// decodeJSONBody reads exactly one JSON object of at most limit bytes into dst,
// rejecting unknown fields and trailing data.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, limit int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errTrailingJSON
	}
	return nil
}

// apiDeadline gives every API request a context deadline so database queries and
// device operations cannot outlive the HTTP write timeout.
func apiDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), apiRequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeErrorCode adds a stable, machine-readable code so the UI can show specific guidance.
func writeErrorCode(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
