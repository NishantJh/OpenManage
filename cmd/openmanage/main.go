package main

import (
 "context"
 "embed"
 "encoding/json"
 "errors"
 "io/fs"
 "log"
 "net/http"
 "os"
 "os/signal"
 "strings"
 "syscall"
 "time"

 "github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

//go:embed all:web/dist
var webFS embed.FS

type app struct { db *pgxpool.Pool }
type device struct {
 ID string `json:"id"`
 Name string `json:"name"`
 Address string `json:"address"`
 Model string `json:"model"`
 Firmware string `json:"firmware"`
 Status string `json:"status"`
 LastSeenAt time.Time `json:"last_seen_at"`
 CreatedAt time.Time `json:"created_at"`
}

func main() {
 ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
 defer stop()
 dsn := env("DATABASE_URL", "postgres://openmanage:openmanage@localhost:5432/openmanage?sslmode=disable")
 var pool *pgxpool.Pool
 var err error
 for i := 1; i <= 30; i++ {
  pool, err = pgxpool.New(ctx, dsn)
  if err == nil { err = pool.Ping(ctx); if err == nil { break }; pool.Close() }
  log.Printf("database not ready (%d/30): %v", i, err)
  select { case <-ctx.Done(): return; case <-time.After(2*time.Second): }
 }
 if err != nil { log.Fatalf("connect to database: %v", err) }
 defer pool.Close()
 migration, err := migrationFS.ReadFile("migrations/001_initial.sql")
 if err != nil { log.Fatal(err) }
 if _, err = pool.Exec(ctx, string(migration)); err != nil { log.Fatalf("database migration: %v", err) }
 a := &app{db:pool}
 mux := http.NewServeMux()
 mux.HandleFunc("GET /api/healthz", a.health)
 mux.HandleFunc("GET /api/devices", a.listDevices)
 mux.HandleFunc("POST /api/devices", a.createDevice)
 mux.HandleFunc("GET /api/devices/{id}", a.getDevice)
 mux.HandleFunc("DELETE /api/devices/{id}", a.deleteDevice)
 mux.Handle("/", frontendHandler())
 srv := &http.Server{Addr:env("LISTEN_ADDR", ":8080"), Handler:securityHeaders(mux), ReadHeaderTimeout:5*time.Second, ReadTimeout:15*time.Second, WriteTimeout:30*time.Second, IdleTimeout:60*time.Second}
 go func(){ log.Printf("OpenManage listening on %s", srv.Addr); if e:=srv.ListenAndServe(); e!=nil && !errors.Is(e,http.ErrServerClosed) { log.Fatalf("HTTP server: %v",e) } }()
 <-ctx.Done()
 shutdown, cancel := context.WithTimeout(context.Background(),10*time.Second); defer cancel()
 if err:=srv.Shutdown(shutdown); err!=nil { log.Printf("shutdown: %v",err) }
}

func (a *app) health(w http.ResponseWriter,r *http.Request) { if err:=a.db.Ping(r.Context()); err!=nil { writeJSON(w,503,map[string]string{"status":"degraded","database":"unavailable"}); return }; writeJSON(w,200,map[string]string{"status":"ok","database":"ok"}) }
func (a *app) listDevices(w http.ResponseWriter,r *http.Request) {
 rows,err:=a.db.Query(r.Context(),`SELECT id::text,name,address,model,firmware,status,last_seen_at,created_at FROM devices ORDER BY created_at DESC LIMIT 500`); if err!=nil { writeError(w,500,"could not query devices"); return }; defer rows.Close()
 out:=make([]device,0); for rows.Next(){ var d device; if err:=rows.Scan(&d.ID,&d.Name,&d.Address,&d.Model,&d.Firmware,&d.Status,&d.LastSeenAt,&d.CreatedAt); err!=nil { writeError(w,500,"could not read devices"); return }; out=append(out,d) }; if rows.Err()!=nil { writeError(w,500,"could not read devices"); return }; writeJSON(w,200,out)
}
func (a *app) createDevice(w http.ResponseWriter,r *http.Request) {
 r.Body=http.MaxBytesReader(w,r.Body,1<<20); var in struct { Name string `json:"name"`; Address string `json:"address"`; Model string `json:"model"`; Firmware string `json:"firmware"` }
 dec:=json.NewDecoder(r.Body); dec.DisallowUnknownFields(); if err:=dec.Decode(&in); err!=nil { writeError(w,400,"invalid JSON body"); return }
 in.Name=strings.TrimSpace(in.Name); in.Address=strings.TrimSpace(in.Address); in.Model=strings.TrimSpace(in.Model); in.Firmware=strings.TrimSpace(in.Firmware)
 if in.Name=="" || in.Address=="" { writeError(w,400,"name and address are required"); return }
 var d device; err:=a.db.QueryRow(r.Context(),`INSERT INTO devices(name,address,model,firmware,status) VALUES($1,$2,$3,$4,'unknown') RETURNING id::text,name,address,model,firmware,status,last_seen_at,created_at`,in.Name,in.Address,in.Model,in.Firmware).Scan(&d.ID,&d.Name,&d.Address,&d.Model,&d.Firmware,&d.Status,&d.LastSeenAt,&d.CreatedAt)
 if err!=nil { writeError(w,500,"could not create device"); return }; writeJSON(w,201,d)
}
func (a *app) getDevice(w http.ResponseWriter,r *http.Request) { var d device; err:=a.db.QueryRow(r.Context(),`SELECT id::text,name,address,model,firmware,status,last_seen_at,created_at FROM devices WHERE id=$1`,r.PathValue("id")).Scan(&d.ID,&d.Name,&d.Address,&d.Model,&d.Firmware,&d.Status,&d.LastSeenAt,&d.CreatedAt); if err!=nil { writeError(w,404,"device not found"); return }; writeJSON(w,200,d) }
func (a *app) deleteDevice(w http.ResponseWriter,r *http.Request) { tag,err:=a.db.Exec(r.Context(),`DELETE FROM devices WHERE id=$1`,r.PathValue("id")); if err!=nil { writeError(w,500,"could not delete device"); return }; if tag.RowsAffected()==0 { writeError(w,404,"device not found"); return }; w.WriteHeader(http.StatusNoContent) }
func frontendHandler() http.Handler { sub,err:=fs.Sub(webFS,"web/dist"); if err!=nil { return http.NotFoundHandler() }; return http.FileServer(http.FS(sub)) }
func securityHeaders(next http.Handler) http.Handler { return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){ w.Header().Set("X-Content-Type-Options","nosniff"); w.Header().Set("X-Frame-Options","DENY"); w.Header().Set("Referrer-Policy","strict-origin-when-cross-origin"); if strings.HasPrefix(r.URL.Path,"/api/") { w.Header().Set("Cache-Control","no-store") }; next.ServeHTTP(w,r) }) }
func writeJSON(w http.ResponseWriter,status int,v any){ w.Header().Set("Content-Type","application/json; charset=utf-8"); w.WriteHeader(status); _=json.NewEncoder(w).Encode(v) }
func writeError(w http.ResponseWriter,status int,message string){ writeJSON(w,status,map[string]string{"error":message}) }
func env(k, fallback string) string { if v:=os.Getenv(k); v!="" { return v }; return fallback }
