# OpenManage

A lightweight, self-hosted control plane for OpenWrt devices. OpenManage uses a Go API, PostgreSQL, and a React/TypeScript UI served by the same Go process. Docker Compose is the initial deployment target.

> **Early development:** inventory, authentication, audit UI/API, SSH enrollment, and read-only health checks are implemented. An opt-in FreeRADIUS lab is under review in [PR #21](https://github.com/NishantJh/OpenManage/pull/21); it is a PAP smoke-test setup, not production WPA-Enterprise support. Configuration changes, router-side SSH key installation/rotation, and firmware upgrades are not yet available. Do not expose OpenManage to the public internet.

## Project status at a glance

Last updated: 2026-10-20. This section is the quick reminder of what works now and what comes next. See [plan.md](plan.md) for the detailed engineering roadmap and [open pull requests](https://github.com/NishantJh/OpenManage/pulls) for work awaiting review or CI.

### Implemented

- Go HTTP API, React/TypeScript frontend, PostgreSQL persistence, and Docker Compose deployment.
- Administrator bootstrap, session authentication, role checks, CSRF protection, and security headers.
- Device inventory: add, view, and delete devices.
- Audit activity API and UI.
- SSH enrollment: independently verified host-key fingerprint plus SSH public-key authentication; private keys are encrypted at rest and never returned by the API.
- Enrollment constrained to private IP addresses and TCP/22.
- Atomic audit logging for key state changes and bounded login rate limiting (check the roadmap/audit notes for remaining verification work).
- CI workflows for build/tests and security checks. PR validation must pass on the latest commit before a change is merged.

### In progress

- **FreeRADIUS lab — [PR #21](https://github.com/NishantJh/OpenManage/pull/21).** Adds an opt-in, loopback-bound FreeRADIUS container and a disposable PAP authentication smoke test. This is a lab-only first slice; AP management UI, production secrets, EAP/PEAP certificates, and database-backed identity integration are not included.
- **Dependency lockfile workflow — [PR #18](https://github.com/NishantJh/OpenManage/pull/18).** Adds trusted generation of Go/npm dependency metadata. Reproducible dependency installation and CI verification still need to be completed.

### Planned next

1. **Structured configuration workflows:** inspect configuration, define validated changes, show a preview/diff, and require explicit confirmation before applying. Prefer UCI-aware operations over shell text.
2. **Apply and verify safely:** preflight checks, bounded execution, post-change connectivity/state verification, audit events, and a tested timed rollback where feasible.
3. **Device lifecycle and fleet visibility:** clearer device details, firmware inventory, operation history/status, and controlled multi-device workflows.
4. **SSH key lifecycle and named maintenance operations:** explicit install/rotate/revoke workflows and narrowly defined operations with recovery access. No generic command runner.
5. **Firmware upgrades:** only after single-device compatibility checks, image integrity validation, backup, canary rollout, reboot/health verification, and documented recovery limits.
6. **Monitoring and RADIUS:** extend manual health checks to scheduled reachability/health monitoring; validate the RADIUS lab before adding trusted AP management and production WPA-Enterprise support.
7. **Later integrations:** broader topology/IPAM/PKI/VPN integrations only when justified.

### Deferred, not forgotten

The current priority is to make the application work end to end. Broader security hardening can follow, but existing authentication, CSRF protection, SSH host-key verification, encrypted credential storage, role checks, and audit controls remain part of the baseline. Before relying on OpenManage as a routine router-management plane, finish the remaining items in [the safety audit](docs/security-audit.md), including enrollment failure-path coverage, deterministic dependency installation, and a disposable backup/restore exercise.

## Design principles

- One Go process plus PostgreSQL; no Redis until a measured requirement justifies it.
- UI assets embedded in the Go binary for simple deployment.
- Small, reversible pull requests; additive database changes where possible.
- Typed, server-defined device operations. Never expose arbitrary shell commands to the browser.
- Configuration changes must be reviewable, auditable, and recoverable before fleet-wide rollout.

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

The app runs its SQL migration at startup. PostgreSQL data is persisted in the `openmanage_db` volume. Back up the database and securely preserve `OPENMANAGE_ENCRYPTION_KEY` together; see [Backup and recovery](docs/backup-recovery.md) before upgrades or enrolling devices.

## API

- `GET /api/healthz` — application and database health.
- `GET /api/devices` — lists up to 500 inventory records.
- `POST /api/devices` — adds a record with JSON fields `name`, `address`, `model`, `firmware`.
- `GET /api/devices/{id}` — fetches one record.
- `DELETE /api/devices/{id}` — deletes one record.
- `POST /api/devices/{id}/enroll` — verifies a pinned SSH host key and public-key login, then stores the management private key encrypted at rest. Request JSON: `username`, `private_key`, and `expected_fingerprint` (the `SHA256:...` fingerprint must be verified independently).
- `POST /api/devices/{id}/health-check` — runs a fixed set of read-only diagnostics for an enrolled device.

All API routes except `/api/healthz` and `/api/auth/login` require an authenticated session. Login uses an HttpOnly session cookie, CSRF protection, and a 12-hour expiry. Device create/delete actions are recorded in the audit trail. `GET /api/audit` returns the latest 100 events and is intended for administrators. The address field is inventory metadata until enrollment is explicitly completed. SSH enrollment verifies a host key fingerprint supplied from a trusted source and authenticates with a private key before storing it encrypted at rest. Enrollment accepts private IP literals only (no DNS names, public addresses, or link-local addresses) and uses TCP/22. Configure `OPENMANAGE_ENCRYPTION_KEY` using a securely backed-up base64-encoded 32-byte random key before enrollment; losing it makes stored SSH credentials unrecoverable. The private key is never returned by the API. Re-enrollment replaces the stored credential only after the new key successfully authenticates; it does not install or rotate `authorized_keys` on the router itself.

## Resource profile

Compose starts with memory limits of 192 MiB for the app and 256 MiB for PostgreSQL. Actual consumption depends on workload and PostgreSQL settings. Redis is intentionally omitted.

## Development

Frontend: `cd web`, then `npm install` and `npm run dev`.

For Go: `go test ./...` and `go run ./cmd/openmanage`. The embedded frontend must be built into `cmd/openmanage/web/dist` first; the Docker build does this automatically.

## Security and recovery

Do not treat this early build as a production management plane. Review [the safety audit](docs/security-audit.md) and complete the remaining findings before relying on it for routine router administration. Keep it on a trusted LAN, prefer HTTPS, set `OPENMANAGE_COOKIE_SECURE=true` behind a TLS-terminating proxy, and do not forward its port from the internet. See [Backup and recovery](docs/backup-recovery.md) for the coupled database and encryption-key recovery procedure.
