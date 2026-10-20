# OpenManage

A lightweight, self-hosted control plane for OpenWrt devices. OpenManage uses a Go API, PostgreSQL, and a React/TypeScript UI served by the same Go process. Docker Compose is the initial deployment target.

> **Early development:** authentication and an audit trail are now implemented, but SSH enrollment, device connectivity, configuration push, firmware management, and rollback are not yet implemented. Do not expose the service to the internet or an untrusted network.

## Design principles

- One Go process plus PostgreSQL; no Redis until a real queue justifies it.
- UI assets embedded in the Go binary for simple deployment.
- Safe device enrollment and configuration rollback before enabling changes to routers.

## Quick start

1. Clone the repository and enter its directory.
2. Copy .env.example to .env.
3. Set POSTGRES_PASSWORD and OPENMANAGE_ADMIN_PASSWORD to unique random passwords. The administrator password must be at least 12 characters. The initial administrator is created only when the users table is empty; later changes to these environment variables do not reset an existing account.
4. Keep OPENMANAGE_PORT at 127.0.0.1:8080 for local-only access, or explicitly bind it to a trusted LAN address.
5. Start the stack:

   ```sh
   docker compose up -d --build
   docker compose logs -f app
   ```

6. Open http://127.0.0.1:8080.

The app runs its SQL migration at startup. PostgreSQL data is persisted in the openmanage_db volume. Back up that volume before upgrades.

## API

- GET /api/healthz — application and database health.
- GET /api/devices — lists up to 500 inventory records.
- POST /api/devices — adds a record with JSON fields name, address, model, firmware.
- GET /api/devices/{id} — fetches one record.
- DELETE /api/devices/{id} — deletes one record.

All API routes except /api/healthz and /api/auth/login require an authenticated session. Login uses an HttpOnly session cookie, CSRF protection, and a 12-hour expiry. Device create/delete actions are recorded in the audit trail. GET /api/audit returns the latest 100 events. The address field is still metadata; OpenManage does not connect to devices yet. Set OPENMANAGE_COOKIE_SECURE=true when serving through HTTPS.

## Resource profile

Compose starts with memory limits of 192 MiB for the app and 256 MiB for PostgreSQL. Actual consumption depends on workload and PostgreSQL settings. Redis is intentionally omitted.

## Roadmap

1. Foundation: inventory, health checks, Docker build and CI.
2. Authentication, roles, sessions and audit trail.
3. Secure OpenWrt enrollment with key rotation and revocation.
4. Versioned configuration templates, staged rollout and rollback.
5. Reachability monitoring, telemetry, alerts and firmware lifecycle.
6. Optional topology, IPAM, PKI/VPN and RADIUS integrations.

## Development

Frontend: cd web, npm install, npm run dev.
For Go: go test ./... and go run ./cmd/openmanage. The embedded frontend must be built into cmd/openmanage/web/dist first; the Docker build does this automatically.

## Security

Do not use this initial build as a production management plane. Secure enrollment and guarded configuration deployment must be completed before using OpenManage to change router state. Use HTTPS for remote access and set OPENMANAGE_COOKIE_SECURE=true behind a TLS-terminating proxy.
