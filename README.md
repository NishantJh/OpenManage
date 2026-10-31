# OpenManage: a self-hosted OpenWrt manager for multiple devices

OpenManage is a lightweight, self-hosted web app for managing several OpenWrt devices from one place: inventory, health, monitoring, configuration history, a few guarded changes, Wi-Fi users, a Wi-Fi floor plan, and firmware preparation. It is one Go binary with PostgreSQL and a React interface, so it is a small alternative to heavier OpenWISP-style deployments.

[![CI](https://github.com/NishantJh/OpenManage/actions/workflows/ci.yml/badge.svg)](https://github.com/NishantJh/OpenManage/actions/workflows/ci.yml)
[![CodeQL](https://github.com/NishantJh/OpenManage/actions/workflows/codeql.yml/badge.svg)](https://github.com/NishantJh/OpenManage/actions/workflows/codeql.yml)

> **Early development.** Do not expose OpenManage to the public internet, and do not rely on it for routine device administration until the remaining [safety audit](docs/security-audit.md) items are done. See [Status](#status).

## Features

- **Adding devices:** one wizard step takes a device's IP address, a host-key fingerprint you verified, and its root password. The password is used once and never stored. OpenManage installs a generated SSH key, checks that it works, and stores the private key encrypted at rest.
- **Health and monitoring:** read-only health checks, plus opt-in background monitoring with 30 days of history.
- **Configuration backups:** encrypted `sysupgrade -b` archives, with download and admin-only restore. Restore needs the administrator's password again and checks that the archive still lets OpenManage sign in.
- **Configuration snapshots and drift:** encrypted `uci export` snapshots, a change review with secrets redacted, and optional scheduled captures.
- **Device settings viewer:** a LuCI-like read-only view of enrolled-device UCI settings, with secret-looking values hidden. Settings cannot yet be edited from this page.
- **Controlled changes:** the system hostname can be changed with a preview, a 60-second rollback, and verification over a fresh SSH connection.
- **Firmware (preparation only):** a read-only inventory of each device's release, packages and free space; a plan for any release the build server offers; and a prepared upgrade that builds the image, checks its hash, takes a pinned backup, and runs `sysupgrade -T`. Nothing is flashed yet.
- **Users and audit:** administrator, operator and read-only roles, and an audit trail of key actions.
- **Wi-Fi floor plan:** rooms and access-point placement on your floor plan, with opt-in client tracking. Connected and recently seen clients appear as dots in the room of their access point, with roaming history.
- **Built-in RADIUS (lab stage):** EAP-TTLS with inner PAP, and plain PAP, for Wi-Fi users. Per-AP shared secrets and an Apple setup profile. Off by default.

## Status

**Available now:** the features listed above.

**Not yet available:**

- Changing Wi-Fi settings such as SSID, passphrase and band. This is next on the roadmap.
- Flashing a prepared firmware image, post-reboot checks, and upgrades that keep installed packages.
- Editing device settings, including Wi-Fi SSID/passphrase/band. The per-device settings page is currently read-only.
- A Software page for installing and removing packages.
- Alert delivery, SSH key rotation, and encryption-key rotation.
- Confirmed RADIUS client coverage. The iPhone test is pending, and Android and Windows are untested. MAC authentication is not built.
- HTTPS as the documented default. Direct HTTP is still supported on a trusted LAN.

The detailed roadmap is in [plan.md](plan.md). Work in review is listed under [open pull requests](https://github.com/NishantJh/OpenManage/pulls).

## OpenManage and OpenWISP

[OpenWISP](https://openwisp.org) is a mature, modular open-source suite for managing OpenWrt networks. It covers far more than OpenManage does today, so choose it if you need that wider scope. OpenManage is for people who want a smaller, single-app tool:

- **Lighter to run:** one Go binary and one PostgreSQL database. The default deployment has no Redis, worker, or other runtime service.
- **No agent on devices:** OpenManage connects over SSH with a pinned host-key fingerprint and runs a fixed set of typed operations.
- **Narrower scope:** no templates, zero-touch provisioning, network topology, VPN/PKI management, or firmware flashing yet. Configuration changes are limited to a few settings so far.

Check each project's documentation for current requirements before you choose.

## Install

OpenManage is designed to run in Docker Compose inside a Proxmox LXC. [Home setup](docs/home-setup.md) walks through that path from start to enrolled devices.

1. Clone the repository and enter its directory.
2. Copy `.env.example` to `.env`.
3. Set `POSTGRES_PASSWORD` and `OPENMANAGE_ADMIN_PASSWORD` to unique random values. The administrator password must be at least 12 characters. The initial administrator is created only when the users table is empty, so later changes to these variables do not reset an existing account.
4. Set `OPENMANAGE_ENCRYPTION_KEY` to a base64-encoded 32-byte random key, and back it up together with the database. Losing it makes stored device credentials unrecoverable. See [Backup and recovery](docs/backup-recovery.md).
5. Set `OPENMANAGE_PORT`. The default, `127.0.0.1:8080`, is reachable only from the server itself or a reverse proxy on it. For LAN access, bind to the server's LAN address, such as `192.168.1.10:8080`.
6. **Choose your transport before logging in from another device.** Preferred: put OpenManage behind a trusted HTTPS reverse proxy such as Caddy, and set `OPENMANAGE_COOKIE_SECURE=true`. Direct HTTP (`http://<server-lan-ip>:8080`) works on a trusted, isolated LAN, but logins and session cookies are not encrypted in transit. Do not use it on guest Wi-Fi, untrusted VLANs, shared networks, or across the internet.
7. Start the stack. To build from this checkout:

   ```sh
   docker compose up -d --build
   docker compose logs -f app
   ```

   To use the prebuilt image instead, set `OPENMANAGE_IMAGE=ghcr.io/nishantjh/openmanage:latest` in `.env`, then run `docker compose pull && docker compose up -d`. CI publishes that image on every push to `main`.

The app runs its SQL migrations at startup. Its data is kept in the `openmanage_db` volume. Read [Backup and recovery](docs/backup-recovery.md) before upgrades or before you enroll devices.

## Documentation

| Document | What it covers |
| --- | --- |
| [Home setup](docs/home-setup.md) | Installing on a Proxmox LXC and enrolling devices |
| [Backup and recovery](docs/backup-recovery.md) | Database and encryption-key backup, and the restore drill |
| [API reference](docs/api.md) | Every HTTP route, role rules and error codes |
| [Wi-Fi floor plan and clients](docs/device-location-map.md) | Floor plan, client tracking and roaming history |
| [Built-in RADIUS](docs/radius-design.md) | Design and threat model for Wi-Fi sign-in |
| [Wi-Fi sign-in setup](docs/radius-client-setup.md) | AP configuration and the device test checklist |
| [Firmware upgrades](docs/firmware-upgrades.md) | Firmware design, the upgrade plan and the device backups |
| [Security audit](docs/security-audit.md) | Open and closed safety findings |
| [Architecture](docs/architecture.md) | How the app is put together |
| [plan.md](plan.md) | The roadmap and what is done |

## Resource profile

Compose sets memory limits of 192 MiB for the app and 256 MiB for PostgreSQL, with one CPU each. Actual use depends on workload and PostgreSQL settings. Redis is intentionally omitted.

## Design principles

- One Go process plus PostgreSQL. No Redis until a measured need justifies it.
- UI assets are embedded in the Go binary.
- Small, reversible pull requests, with additive database changes where possible.
- Typed, server-defined device operations. The browser never sends arbitrary shell commands.
- Configuration changes must be reviewable, auditable and recoverable before they roll out to more than one device.

## Development

Frontend: `cd web`, then `npm ci` and `npm run dev`.

Go: `go test ./...` and `go run ./cmd/openmanage`. The frontend must be built into `cmd/openmanage/web/dist` first; the Docker build does this automatically.

Real-OpenWrt lab: `bash scripts/openwrt-lab-test.sh` needs Docker. It runs enrollment, health checks and monitoring against a disposable OpenWrt container, and RADIUS checks with `scripts/radius-probe`. CI runs the same script weekly, and on demand from the "OpenWrt lab" workflow.

Contributors follow [AGENTS.md](AGENTS.md) for workflow and safety rules, and [CODEQUALITY.md](CODEQUALITY.md) for code quality.

## Security

Keep OpenManage on a trusted LAN, prefer HTTPS, set `OPENMANAGE_COOKIE_SECURE=true` behind a TLS-terminating proxy, and never forward its port from the internet. Read the [safety audit](docs/security-audit.md) before you rely on it for routine administration.
