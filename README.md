# OpenManage: a self-hosted OpenWrt manager for multiple devices

OpenManage is a lightweight, self-hosted web app for managing several OpenWrt routers and access points from one place: inventory, health, configuration history, safe changes, Wi-Fi users, and a Wi-Fi floor plan. It is a single Go binary with PostgreSQL, so it is a small alternative to heavier OpenWISP-style deployments.

[![CI](https://github.com/NishantJh/OpenManage/actions/workflows/ci.yml/badge.svg)](https://github.com/NishantJh/OpenManage/actions/workflows/ci.yml)
[![CodeQL](https://github.com/NishantJh/OpenManage/actions/workflows/codeql.yml/badge.svg)](https://github.com/NishantJh/OpenManage/actions/workflows/codeql.yml)

OpenManage uses a Go API, PostgreSQL, and a React/TypeScript UI served by the same Go process. Docker Compose is the initial deployment target.

## Features

- **Adding devices:** one wizard step takes a device's IP address, a host-key fingerprint you verified, and its root password (used once, never stored). OpenManage installs a generated key, checks it works, and stores the management key encrypted at rest. A RADIUS tick box on every add path (wizard, **Advanced / manual** form, and the key icon on a device without a key) also sets the device up for Wi-Fi sign-in, and the screen with the new secret lists the commands that turn on RADIUS on the AP's existing Wi-Fi networks. Persistent monitoring stays a separate, global setting.
- **Health and monitoring:** read-only health checks, plus opt-in persistent monitoring with 30 days of history.
- **Configuration snapshots and drift:** encrypted `uci export` snapshots, change review with secrets redacted, and optional scheduled captures.
- **Controlled changes:** the system hostname can be changed with a preview, a timed rollback, and verification over a fresh SSH connection.
- **Users and audit:** admin, operator, and read-only roles, with an audit trail of key actions.
- **Built-in RADIUS (lab stage):** EAP-TTLS and PAP for Wi-Fi users, per-AP shared secrets, and an Apple setup profile. Off by default.
- **Wi-Fi floor plan:** rooms and AP placement, with opt-in client tracking and roaming history. Client dots on the map are in progress.

> **Early development:** broader configuration changes, router-side SSH key rotation, production RADIUS, and firmware upgrades are not yet available. Do not expose OpenManage to the public internet. See the status summary below.

## OpenManage and OpenWISP

[OpenWISP](https://openwisp.org) is a mature, modular open-source suite for managing OpenWrt networks. It covers far more than OpenManage does today, and it is the better choice if you need its wider feature set. OpenManage is aimed at people who want a smaller, single-app tool:

- **Lighter to run:** one Go binary and one PostgreSQL database. There is no Redis, worker, or other runtime service in the default deployment.
- **No software installed on routers:** OpenManage connects over SSH with a pinned host-key fingerprint and runs a fixed set of typed, read-only checks or reviewed changes.
- **Narrower scope:** OpenManage does not yet offer templates, zero-touch provisioning, network topology, VPN/PKI management, or firmware upgrades. Its configuration changes are limited to a few settings so far.

Check each project's documentation for current requirements before choosing between them.

## Project status at a glance

Last updated: 2026-10-23. This section is a quick summary of what works now and what comes next. See [plan.md](plan.md) for the detailed roadmap and [open pull requests](https://github.com/NishantJh/OpenManage/pulls) for work in review.

### Implemented

- Go HTTP API, React/TypeScript frontend, PostgreSQL persistence, and Docker Compose deployment.
- Administrator bootstrap, session authentication, role checks, CSRF protection, and security headers.
- User management API: create users, change roles, disable, and reset or change passwords, with sessions revoked on each change.
- Device inventory: add, view, and delete devices.
- Audit activity API and UI.
- SSH enrollment: independently verified host-key fingerprint plus SSH public-key authentication; private keys are encrypted at rest and never returned by the API.
- Enrollment constrained to private IP addresses and TCP/22.
- Read-only health checks, opt-in scheduled configuration snapshots with drift flags, and opt-in, server-side persistent monitoring with bounded concurrency and 30-day history, run through a typed operation engine. Device model and firmware are filled in from what the router reports.
- Atomic audit logging for key state changes and bounded login rate limiting.
- One controlled configuration write (system hostname) with a 60-second rollback unless verified over a fresh SSH connection.
- Opt-in built-in RADIUS at lab stage: EAP-TTLS with inner PAP and plain PAP, Wi-Fi (network) user and per-AP secret management, and an Apple setup profile.
- Committed Go and npm lockfiles; CI installs with `npm ci` and fails if dependency metadata drifts.

### Next

The active work is built-in RADIUS, then the remaining hardening. In order:

1. **Built-in RADIUS:** the iPhone client test, MAC authentication, and pushing AP RADIUS settings through the configuration workflow. Android and Windows clients are untested. See [the RADIUS design](docs/radius-design.md).
2. **Device location map:** the floor plan and client tracking backend with 30-day roaming history are merged (client tracking is opt-in). Still to do: the client dots on the floor plan and a lab test against real hostapd. See [the design](docs/device-location-map.md).
3. **Configuration changes:** further settings on the same engine (for example Wi-Fi SSID or channel), each with a lab rollback test.
4. **Hardening:** the remaining [safety audit](docs/security-audit.md) items, including HTTPS as the documented default, and a re-run of the restore drill on the production host.
5. **Later:** alert delivery, SSH key lifecycle and encryption-key rotation (with device-bound key ciphertext), then firmware upgrades with canary rollout and honest recovery limits.

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
4. Set OPENMANAGE_PORT in .env. The default, `127.0.0.1:8080`, is reachable only from the server itself (or a reverse proxy running on it). For LAN access, bind to the server's specific LAN address, such as `192.168.1.10:8080`. Use `0.0.0.0:8080` only if you intend to listen on all IPv4 interfaces and have restricted access with the host/router firewall.
5. **Choose your transport before logging in from another device.** Preferred: put OpenManage behind a trusted HTTPS reverse proxy (for example, Caddy) and set `OPENMANAGE_COOKIE_SECURE=true`. Ensure the browser trusts the proxy certificate. Direct HTTP at `http://<server-lan-ip>:8080` is supported for a trusted, isolated LAN, but login credentials and session cookies are not encrypted in transit; do not use it on guest Wi-Fi, untrusted VLANs, shared networks, or across the internet.
6. Start the stack:

   ```sh
   docker compose up -d --build
   docker compose logs -f app
   ```

7. Open `https://<your-openmanage-hostname>` through the HTTPS proxy, or `http://<server-lan-ip>:8080` only if you deliberately chose direct HTTP on a trusted LAN.

For the full path from install to enrolled devices on a Proxmox LXC, see [Home setup](docs/home-setup.md).

The app runs its SQL migration at startup. PostgreSQL data is persisted in the `openmanage_db` volume. Back up the database and securely preserve `OPENMANAGE_ENCRYPTION_KEY` together; see [Backup and recovery](docs/backup-recovery.md) before upgrades or enrolling devices.

## API

- `GET /api/healthz` — application and database health.
- `GET /api/devices` — lists up to 500 inventory records. Each record includes `enrollable`, which is true when the address is a private IP literal that SSH enrollment accepts, and `config_drift`, which is true while the device has configuration drift no administrator has acknowledged.
- `POST /api/devices` — adds a record with JSON fields `name`, `address`, `model`, `firmware`.
- `GET /api/devices/{id}` — fetches one record.
- `DELETE /api/devices/{id}` — deletes one record.
- `POST /api/device-bootstrap/probe` reads a device's SSH host-key fingerprint for the Add device wizard. `POST /api/device-bootstrap` takes `name`, `address`, `role`, `username`, `password` and `expected_fingerprint` (or `device_id` to set up an inventory device that has no key yet), signs in once with the password, installs a generated key, verifies key login and stores the device. The password is never stored, logged or audited. When the RADIUS box is ticked, the wizard then creates the device's secret with `POST /api/devices/{id}/radius-secret`.
- `POST /api/devices/{id}/enroll` — verifies a pinned SSH host key and public-key login, then stores the management private key encrypted at rest. Request JSON: `username`, `private_key`, and `expected_fingerprint` (the `SHA256:...` fingerprint must be verified independently). Errors include a stable `code` (`encryption_key_missing`, `encryption_key_invalid`, `invalid_request`, `device_not_found`, `address_not_private`, `invalid_private_key`, `device_unreachable`, `host_key_mismatch`, `ssh_auth_failed`, `key_not_verified`, `server_error`).
- `POST /api/devices/{id}/health-check` — runs a fixed set of read-only diagnostics for an enrolled device.
- `POST /api/devices/{id}/config-snapshots` — administrator-only; reads the device's `uci export` and stores it encrypted at rest. Returns `first`, `changed` (drift since the previous snapshot) and `change_count`. An unchanged configuration only refreshes `last_verified_at`. The last 100 snapshots per device are kept.
- `GET /api/devices/{id}/config-snapshots` — administrator-only; lists snapshot metadata, newest first.
- `GET /api/devices/{id}/config-snapshots/{snapshot}/changes` — administrator-only; lists UCI paths added, removed or changed since the previous snapshot. Values of secret-looking options (keys, passwords, secrets, tokens) are redacted, and the raw configuration is never returned.
- `POST /api/devices/{id}/config-snapshots/{snapshot}/acknowledge` — administrator-only; marks drift up to and including this snapshot as reviewed, so the device no longer reports `config_drift`.
- `GET /api/auth/me` returns the signed-in user; `POST /api/auth/logout` ends the session.
- `POST /api/auth/password` — changes the caller's own password (JSON `current_password`, `new_password`, 12–72 bytes). Other sessions of that user are signed out; wrong current passwords count towards the login rate limit.
- `GET /api/users` — administrator-only; lists users with role and disabled state.
- `POST /api/users` — administrator-only; creates a user from JSON `username`, `role` (`admin`, `operator`, `readonly`) and `password`.
- `PATCH /api/users/{id}` — administrator-only; changes `role` and/or `disabled`. Either change signs the user out. The last active administrator cannot be demoted or disabled. Users are disabled rather than deleted so the audit trail keeps its names.
- `POST /api/users/{id}/password` — administrator-only; sets a user's password and signs them out.
- `GET /api/network-users`, `POST /api/network-users`, `PATCH /api/network-users/{id}`, `POST /api/network-users/{id}/password` — administrator-only; manage Wi-Fi (network) users, which are separate from OpenManage logins.
- `GET /api/radius` — administrator-only; RADIUS listener and certificate status. `GET /api/radius/ca.pem` downloads the RADIUS CA, and `GET /api/radius/apple-profile` generates the Apple Wi-Fi setup profile (both administrator-only).
- `POST /api/devices/{id}/radius-secret` generates or rotates an AP's RADIUS shared secret, shown once; `DELETE /api/devices/{id}/radius-secret` removes it. Both are administrator-only.
- `GET /api/config-settings` — administrator-only; lists the settings OpenManage can change (currently `system.hostname`).
- `POST /api/devices/{id}/config-changes` — administrator-only; previews a change from JSON `setting` and `value`. Reads the device's current value and changes nothing. A preview is valid for 10 minutes.
- `POST /api/devices/{id}/config-changes/{change}/apply` — administrator-only; applies a preview through rpcd with `uci apply` rollback armed, verifies the new value over a fresh SSH connection, then confirms. The result's `status` is `applied`, `rolled_back` (verification failed; the old value is restored) or `failed` (nothing was applied, for example because the value changed since the preview or another apply is pending).
- `GET /api/devices/{id}/config-changes` — administrator-only; the last 50 changes for a device.
- `GET /api/monitoring/settings` — reads persistent server-side monitoring settings.
- `PUT /api/monitoring/settings` — administrator-only; enables/disables monitoring and sets an interval from 60 to 3600 seconds. Optional `snapshots_enabled` and `snapshot_interval_seconds` (900 to 86400) turn on scheduled configuration snapshots, independently of health monitoring; omitted fields keep their current values.
- `GET /api/monitoring/history?limit=50` — returns recent persisted monitoring outcomes (limit 1–100).

All API routes except `/api/healthz` and `/api/auth/login` require an authenticated session. Login uses an HttpOnly session cookie, CSRF protection, and a 12-hour expiry. Device create/delete actions are recorded in the audit trail. `GET /api/audit` returns the latest 100 events and is intended for administrators. The address field is inventory metadata until enrollment is explicitly completed. SSH enrollment verifies a host key fingerprint supplied from a trusted source and authenticates with a private key before storing it encrypted at rest. Enrollment accepts private IP literals only (no DNS names, public addresses, or link-local addresses) and uses TCP/22. Configure `OPENMANAGE_ENCRYPTION_KEY` using a securely backed-up base64-encoded 32-byte random key before enrollment; losing it makes stored SSH credentials unrecoverable. The private key is never returned by the API. The Add device wizard is the one path that installs a key on the device: it signs in once with the root password, adds the generated public key to the device's authorized keys, and discards the password. Re-enrollment replaces the stored credential only after the new key successfully authenticates; it does not install or rotate `authorized_keys` on the router itself.

## Built-in RADIUS (lab stage)

OpenManage can answer RADIUS requests (EAP-TTLS with inner PAP, and plain PAP) from enrolled APs so that Wi-Fi users ("network users", separate from OpenManage logins) can be checked against it. It is off unless `OPENMANAGE_RADIUS_LISTEN` is set (for example `:1812`) together with `OPENMANAGE_ENCRYPTION_KEY`, and Compose then needs a `ports` entry that binds UDP 1812 to the LAN address only. Administrators manage Wi-Fi users and give each AP a shared secret, either with the RADIUS tick box when adding it or in the dashboard's "Wi-Fi sign-in" section; the secret is shown once, with the AP commands filled in, and stored encrypted. Requests are accepted only from that device's inventory address and must carry a valid Message-Authenticator. Failures are rate-limited per user and per AP and audited without passwords. When RADIUS is first enabled, OpenManage generates a private CA and a server certificate for `radius.openmanage.home.arpa` (keys encrypted like SSH keys); the dashboard shows the CA fingerprint and offers the CA for download. EAP-TTLS uses TLS 1.2 with that certificate; set clients to inner method PAP, trust the downloaded CA and expect that server name. It is tested with `eapol_test` in the lab. The iPhone test is pending; Android and Windows are untested. See [the RADIUS design](docs/radius-design.md). For iPhone, iPad and Mac, the dashboard generates a setup profile; see [Wi-Fi sign-in setup](docs/radius-client-setup.md) for the AP configuration and the device test checklist. Never forward UDP 1812 from the internet.

## Resource profile

Compose starts with memory limits of 192 MiB for the app and 256 MiB for PostgreSQL. Actual consumption depends on workload and PostgreSQL settings. Redis is intentionally omitted.

## Development

Frontend: `cd web`, then `npm ci` and `npm run dev`.

For Go: `go test ./...` and `go run ./cmd/openmanage`. The embedded frontend must be built into `cmd/openmanage/web/dist` first; the Docker build does this automatically.

Real-OpenWrt lab: `bash scripts/openwrt-lab-test.sh` needs Docker. It runs enrollment, health checks and monitoring (including outage, revoked key and host-key change) against a disposable OpenWrt 24.10 container, and RADIUS checks with `scripts/radius-probe` (so it also needs Go). CI runs the same script weekly, and on demand from the "OpenWrt lab" workflow.

## Security and recovery

Do not treat this early build as a production management plane. Review [the safety audit](docs/security-audit.md) and complete the remaining findings before relying on it for routine router administration. Keep it on a trusted LAN, prefer HTTPS, set `OPENMANAGE_COOKIE_SECURE=true` behind a TLS-terminating proxy, and do not forward its port from the internet. See [Backup and recovery](docs/backup-recovery.md) for the coupled database and encryption-key recovery procedure.
