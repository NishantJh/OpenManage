# OpenManage

A lightweight, self-hosted control plane for OpenWrt devices. OpenManage uses a Go API, PostgreSQL, and a React/TypeScript UI served by the same Go process. Docker Compose is the initial deployment target.

> **Early development:** authentication, audit logging, and a first SSH enrollment API are implemented. Remote command execution, configuration push, router-side key installation/rotation, firmware management, and rollback are not implemented. Do not expose the service to the internet or an untrusted network.

## Design principles

- One Go process plus PostgreSQL; no Redis until a real queue justifies it.
- UI assets embedded in the Go binary for simple deployment.
- Safe device enrollment and configuration rollback before enabling changes to routers.

## Quick start

1. Clone the repository and enter its directory.
2. Copy .env.example to .env.
3. Set POSTGRES_PASSWORD and OPENMANAGE_ADMIN_PASSWORD to unique random passwords. The administrator password must be at least 12 characters. The initial administrator is created only when the users table is empty; later changes to these environment variables do not reset an existing account.
4. Set OPENMANAGE_PORT in .env. For a LAN-only deployment, prefer binding to the server's specific LAN address, such as `192.168.1.10:8080`. Use `0.0.0.0:8080` only if you intend to listen on all IPv4 interfaces and have restricted access with the host/router firewall.
5. **Choose your transport before logging in from another device.** Preferred: put OpenManage behind a trusted HTTPS reverse proxy (for example, Caddy) and set `OPENMANAGE_COOKIE_SECURE=true`. Ensure the browser trusts the proxy certificate. Direct HTTP at `http://<server-lan-ip>:8080` is supported for a trusted, isolated LAN, but login credentials and session cookies are not encrypted in transit; do not use it on guest Wi-Fi, untrusted VLANs, shared networks, or across the internet.
6. Start the stack:

   ```sh
   docker compose up -d --build
   docker compose logs -f app
   ```

7. Open `https://<your-openmanage-hostname>` through the HTTPS proxy, or `http://<server-lan-ip>:8080` only if you deliberately chose direct HTTP on a trusted LAN.

The app runs its SQL migration at startup. PostgreSQL data is persisted in the openmanage_db volume. Back up the database and securely preserve OPENMANAGE_ENCRYPTION_KEY together; see [Backup and recovery](docs/backup-recovery.md) before upgrades or enrolling devices.

## API

- GET /api/healthz — application and database health.
- GET /api/devices — lists up to 500 inventory records.
- POST /api/devices — adds a record with JSON fields name, address, model, firmware.
- GET /api/devices/{id} — fetches one record.
- DELETE /api/devices/{id} — deletes one record.
- POST /api/devices/{id}/enroll — verifies a pinned SSH host key and public-key login, then stores the management private key encrypted at rest. Request JSON: `username`, `private_key`, and `expected_fingerprint` (the `SHA256:...` fingerprint must be verified independently).

All API routes except /api/healthz and /api/auth/login require an authenticated session. Login uses an HttpOnly session cookie, CSRF protection, and a 12-hour expiry. Device create/delete actions are recorded in the audit trail. GET /api/audit returns the latest 100 events and is intended for administrators. The address field is inventory metadata until enrollment is explicitly completed. SSH enrollment verifies a host key fingerprint supplied from a trusted source and authenticates with a private key before storing it encrypted at rest. Enrollment accepts private IP literals only (no DNS names, public addresses, or link-local addresses) and uses TCP/22. Configure OPENMANAGE_ENCRYPTION_KEY using a securely backed-up base64-encoded 32-byte random key before enrollment; losing it makes stored SSH credentials unrecoverable. The private key is never returned by the API. Re-enrollment replaces the stored credential only after the new key successfully authenticates; it does not install or rotate authorized_keys on the router itself.

## Resource profile

Compose starts with memory limits of 192 MiB for the app and 256 MiB for PostgreSQL. Actual consumption depends on workload and PostgreSQL settings. Redis is intentionally omitted.

## Roadmap

See [plan.md](plan.md) for the current implementation plan, security hardening gates, and controlled device-operation sequence.

## Development

Frontend: `cd web`, `npm install`, `npm run dev`.
For Go: `go test ./...` and `go run ./cmd/openmanage`. The embedded frontend must be built into cmd/openmanage/web/dist first; the Docker build does this automatically.

## Security

Do not use this initial build as a production management plane. Review [the security audit](docs/security-audit.md) and close its high-priority findings before using OpenManage to administer routers. Keep it on a trusted LAN, prefer HTTPS, set OPENMANAGE_COOKIE_SECURE=true behind a TLS-terminating proxy, and do not forward its port from the internet.
