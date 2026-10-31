# OpenManage implementation plan

This is the roadmap and source of truth for sequencing work. Keep it short and update it when a milestone changes. Prefer small, reversible pull requests over broad rewrites.

## Product direction

Build a lightweight, self-hosted control plane for OpenWrt, inspired by useful OpenWISP concepts but implemented around Go, PostgreSQL, and a React/TypeScript UI. Keep the default deployment small; do not add Redis or another service unless a demonstrated requirement justifies it.

Production target: Docker Compose inside a Proxmox LXC. WSL2 is the test environment (the OpenWrt lab, the restore drill, and the [home setup guide](docs/home-setup.md)), not the production host.

## Priority (agreed 2026-10-22, updated 2026-10-23)

Hardening (Sections 0 and 1) and the operations engine, read-only configuration, and first controlled write (Sections 2 and 3) are largely done. Built-in RADIUS (Section 7) is an optional advanced feature that is already built; no further RADIUS work is planned for now (decided 2026-10-24). The next product work is pushing normal Wi-Fi password settings (Section 9), on top of the hostname write and user management already in place. The device location map (Section 8) continues in parallel. Remaining safety-audit items, alerts, and the deferred items below come after these. PRs that do not map to a roadmap item get a plan entry first, or are parked as drafts or closed.

## Current baseline

- Go HTTP API and React/TypeScript UI, served by one app process; PostgreSQL persists inventory, users, audit events, encrypted credentials, encrypted configuration snapshots, and monitoring history.
- Administrator bootstrap, session authentication, CSRF protection, security headers, and an audit log are implemented.
- Device inventory CRUD and the audit activity UI are implemented.
- User management (admin, operator, and readonly roles), read-only health checks, opt-in monitoring and scheduled snapshots with drift detection, and one controlled write (system hostname with timed rollback) are implemented.
- Built-in RADIUS is implemented at lab stage: opt-in, EAP-TTLS with inner PAP and plain PAP, off unless `OPENMANAGE_RADIUS_LISTEN` is set.
- SSH enrollment API verifies an independently verified SHA-256 host-key fingerprint and SSH public-key authentication before storing the private key encrypted with AES-256-GCM.
- Enrollment is admin-only, limited to private IP literals on TCP/22, and requires OPENMANAGE_ENCRYPTION_KEY.
- Enrollment does not install or rotate router-side authorized keys, run remote commands, or push configuration.
- Trusted main-branch CI, scheduled CodeQL/security scans, dependency-lockfile generation, and opt-in OpenWrt lab/VM tests run on the persistent Ubuntu self-hosted runner labelled `openmanage-ci`. Pull-request validation and PR-triggered scans remain GitHub-hosted: because this is a public repository, PR authors can modify workflow definitions, so a job-level condition is not a sufficient boundary for a persistent self-hosted runner. See [docs/ci-runners.md](docs/ci-runners.md) for service setup and prerequisites.

## Roadmap

### 0. Security and engineering hardening — in progress

- [x] Make credential persistence, device status update, and the enrollment audit event atomic in one database transaction.
- [x] Add handler/integration-style tests for enrollment success, bad host fingerprint, failed SSH authentication, re-enrollment preserving old credentials on failure, and persistence/audit failures (merged in PR #34).
- [x] Make inventory create/delete and login/logout audit writes atomic with their corresponding state changes; mandatory audit failure must fail the action (merged in PR #15; post-merge CI, security checks, and CodeQL passed).
- [x] Add bounded login rate limiting with safe peer-address parsing, no trust in forwarded headers, and Retry-After responses (merged; post-merge verification tracked as OM-SAF-04 in docs/security-audit.md).
- [ ] Complete the safety-audit findings in docs/security-audit.md before treating this as a routine LAN management plane. OM-SAF-01, 03, 04, 05, 06, 07 and 08 are remediated; OM-SAF-02 (direct HTTP and cookie-secure defaults) is partly addressed and stays open until HTTPS is the documented default.
- [x] Format Go sources and make CI reject unformatted Go files.
- [x] Reject malformed device IDs before database queries.
- [x] Add an enrollment request body-size cap and test malformed/oversized enrollment payloads.
- [x] Verify request-size and timeout behavior across all API handlers (per-endpoint body caps, single-object decoding, field-length limits, and a per-request API deadline below the server write timeout).
- [x] Improve dependency reproducibility: generated `go.sum` and `web/package-lock.json` were committed in PR #32 (generator run [39588749075](https://github.com/NishantJh/OpenManage/actions/runs/39588749075)); post-merge main CI passed ([run 39816060882](https://github.com/NishantJh/OpenManage/actions/runs/39816060882)). CI and PR validation use `npm ci` and `go mod tidy -diff`, and the Dockerfile builds from the lockfiles with `npm ci` and `go mod verify`.
- [x] Document the PostgreSQL and OPENMANAGE_ENCRYPTION_KEY backup/restore procedure; recovery testing remains an operator task.
- [x] Run and record a disposable-stack restore exercise, including verification that an enrolled credential decrypts with the restored key (passed 2026-10-22 on WSL2; drill record in docs/backup-recovery.md). Re-run it on the production LXC host before go-live.
- [x] Default the Compose port binding to `127.0.0.1:8080`; LAN access becomes an explicit `OPENMANAGE_PORT` choice.

Exit criteria: tests cover critical enrollment failure paths; credential changes cannot commit without their audit event; CI passes on the PR head and after merge.

### 1. Enrollment UX and device lifecycle — done

- [x] Add an admin-only enrollment form with independent host-key fingerprint verification guidance.
- [x] Keep private-key material out of local storage, logs, audit details, analytics, and API responses; clear form state after submission and on close.
- [x] Refresh device status and last-seen data after successful SSH authentication.
- [x] Allow admins to re-enroll from the inventory without exposing stored secrets.
- [x] Verify UI error handling for missing encryption-key configuration, invalid key formats, unreachable devices, and fingerprint mismatch. Return stable error codes from the API so the UI can show specific guidance.
- [x] Validate inventory addresses at creation so devices that cannot be enrolled (hostnames, public or link-local IPs) are flagged up front.

Exit criteria: the user can enroll a device through the UI, see a safe outcome, and cannot retrieve the stored private key.

### 2. Shared device-operations engine and health — done

Read-only health checks and monitoring are shipped and run through the typed operation engine in `operations.go`.

- [x] Read-only health checks with bounded output and timeouts, reused by monitoring.
- [x] Extract a typed operation engine (name, read/write class, role, timeout, output limit, audit policy) and move health checks and monitoring onto it with no behaviour change. Document that read-only operation audits may be best-effort, while write operations must audit transactionally. (`cmd/openmanage/operations.go`; write-class operations are refused until Section 3 adds transactional audit and rollback.)
- [x] Populate device model and firmware automatically from `ubus call system board` results (the basis for firmware inventory). Health checks and monitoring overwrite them with what the device reports; fields it does not report are left as entered.
- [x] Split the frontend into components with a shared fetch/CSRF helper, with no behaviour change, before configuration UI is added. (`web/src/api.ts`, `web/src/components/`.)

Design notes:

- Reuse stored credentials and pinned host keys; enforce per-device timeouts, cancellation, concurrency limits, output-size limits, and safe error handling.
- Represent work as typed, named operations with explicit policies: allowed operation, read/write classification, validated parameters, privilege requirements, timeout, and audit requirements.
- Keep command construction and validation server-side. Do not expose a generic shell/command field to the browser or accept arbitrary commands from API clients.
- Start with a small allowlist of read-only OpenWrt health commands; parse structured output where possible and handle firmware/version differences.
- Record operation identity, initiator, target, start/end status, and sanitized result metadata in the audit trail. Do not log credentials, secret-bearing arguments, or full command output by default.
- Keep execution in the existing app initially. Add a queue or Redis only if measured fleet size or reliability needs justify the added service.

Exit criteria: health checks are reliable, resource-bounded, auditable, and use the same policy/execution boundary intended for later controlled writes.

### 3. Versioned configuration workflows — in progress

Start read-only: capture configuration snapshots (`uci export`), encrypted at rest because they can contain Wi-Fi keys and other secrets, then detect drift between snapshots. Only then add writes, starting with one narrow setting on a single device. Prefer OpenWrt's rpcd `uci apply` with `rollback` and `confirm` for timed rollback.

- [x] Read-only snapshots: `uci export` captured on demand by administrators, AES-256-GCM encrypted and bound to the device, keyed-HMAC digest for drift, last 100 kept per device.
- [x] Drift: UCI-aware diff between consecutive snapshots, with secret-looking option values redacted.
- [x] Snapshot and drift UI: per-device capture, history, and change view with secrets hidden.
- [x] Scheduled snapshots alongside monitoring (opt-in, 15 min to 24 h), so drift is noticed without a manual capture; drift stays flagged on the device until an administrator acknowledges it.
- [x] Dashboard "Configuration drift" count, a drift tag per device, snapshot schedule settings, and "Mark changes reviewed" in the UI.
- [x] First controlled write (API): the system hostname on one device. Preview stores the current value; an administrator applies it through an rpcd session with `uci apply` rollback armed (60 s); a fresh SSH connection must read the new value back before `uci confirm`, otherwise OpenManage rolls back. Stale previews and concurrent applies (including one pending from LuCI) are refused. A baseline snapshot is taken first and the follow-up snapshot is not counted as drift. Lab-tested for apply, confirm surviving the window, stale preview, pending apply, and forced rollback.
- [x] Hostname change UI: preview, apply with rollback, and per-device change history in the configuration dialog.
- [ ] Next settings on the same engine (for example Wi-Fi SSID/channel), each with a lab rollback test.

- Model templates and revisions in PostgreSQL with explicit validation and immutable revision history.
- Render a preview and diff before applying; validate target compatibility and require explicit operator confirmation.
- Apply structured, validated configuration changes (prefer UCI-aware operations) rather than arbitrary shell text.
- Use a staged workflow: preflight, preview, approval/confirmation, apply, verify connectivity and intended state, then mark success.
- Where feasible, arrange a timed rollback before applying network-affecting changes; verify rollback behavior on representative devices before relying on it.
- Roll out to one device or a small canary group first, then expand in batches.
- Audit who proposed, approved, applied, verified, or rolled back each change.

Exit criteria: changes are reviewable, staged, auditable, and have a tested recovery procedure with clearly documented limits.

### 4. Guarded maintenance operations and SSH key lifecycle

- Add only named, server-defined maintenance operations with validated parameters (for example, collecting diagnostics or restarting a specific service); never accept arbitrary command strings.
- Design router-side key installation, rotation, and revocation as explicit workflows with preflight checks, audit records, and recovery access.
- Require explicit authorization, confirmation for disruptive actions, bounded execution, and post-operation verification.
- Preserve least privilege on routers where feasible and ensure failures do not silently remove the only management path.
- Plan rotation of `OPENMANAGE_ENCRYPTION_KEY` (re-encrypt stored credentials without downtime or data loss).
- Add minimal user management (create operator/readonly users, change password) when a second user needs access, and before the first configuration write so writes can be authorised by role.
  - [x] API: administrators create users, change roles, disable/re-enable and reset passwords; everyone can change their own password. Role changes, disabling and password changes revoke sessions; the last active administrator cannot be demoted or disabled.
  - [x] UI: a Users section for administrators (add, role, disable, reset password) and "Password" in the top bar for everyone.

Exit criteria: maintenance and credential lifecycle operations have tested failure recovery and cannot silently lock out management access.

### 5. Firmware upgrades and fleet rollout

Treat firmware upgrades as a separate high-risk operation type, not as a routine shell command.

- Check exact device model/target, current version, supported upgrade path, image format, available storage, and required configuration backup before offering an upgrade.
- Verify image provenance and integrity using trusted metadata/checksums; reject unknown or incompatible images.
- Require explicit per-device confirmation, a staged canary rollout, concurrency limits, progress/status reporting, and post-reboot health verification.
- Document that SSH cannot guarantee recovery from a failed flash. A failed upgrade may require device failsafe, physical reset, or serial access; do not claim automatic rollback unless that hardware/upgrade path actually supports it.
- Do not begin fleet-wide firmware writes until single-device upgrade and recovery scenarios have been tested on supported hardware.

Exit criteria: supported upgrade paths are validated, staged, observable, and have honest recovery guidance for failures outside the control plane's reach.

### 6. Monitoring and later integrations

- [x] Add opt-in server-side monitoring in the existing Go process, persisted settings and per-device check history in PostgreSQL, bounded three-worker batches, PostgreSQL advisory-lock coordination, and 30-day history retention.
- [x] Validate real SSH checks against a disposable OpenWrt VM/device and exercise outage, bad credential, pinned-host-key mismatch, and restart recovery scenarios. (`scripts/openwrt-lab-test.sh`, OpenWrt 24.10.4 rootfs container.)
- [x] Add a disposable OpenWrt VM/container smoke job (opt-in) so enrollment and monitoring are exercised against real OpenWrt in CI. (`.github/workflows/openwrt-lab.yml`: manual dispatch and weekly, GitHub-hosted.)
- [ ] Add alert delivery after persistent monitoring is validated: one outbound webhook (ntfy/Gotify-compatible), no new service.
- Consider topology, IPAM, PKI/VPN, Wi-Fi planning, and larger-fleet features only when a concrete use case warrants them.

### 7. Built-in RADIUS for WPA-Enterprise

Status: optional advanced feature, built; no further RADIUS work for now. RADIUS is built into the Go application rather than run as a separate FreeRADIUS container. It comes after user management and the first controlled configuration write, because it needs user identities and a safe way to push RADIUS settings to APs. EAP (PEAP/TTLS) is security-sensitive protocol code with no mature Go server library, so treat it as high-risk.

- [x] Design doc: scope, EAP methods, server certificate handling, per-AP shared-secret storage, threat model. ([docs/radius-design.md](docs/radius-design.md); EAP-TTLS/PAP first, `layeh.com/radius` for packet encoding.)
- [ ] Opt-in UDP 1812 listener in the app with PAP and MAC authentication for a lab, per-AP secrets encrypted at rest, rate limiting, and audit. (PAP, secrets, rate limits, audit and the dashboard section done; MAC authentication remains.)
- [ ] EAP-TTLS with a server certificate, tested against real phone and laptop clients. (EAP-TTLS/PAP done and tested with `eapol_test`; iPhone test pending; Android and Windows untested. PEAP-MSCHAPv2 only if a needed client cannot use TTLS.)
- [ ] Push AP RADIUS configuration through the Section 3 configuration workflow. Parked with the rest of RADIUS; Section 9 comes first.

Exit criteria: a client can join a managed AP with per-user credentials, failures are audited without logging secrets, and the listener is off unless enabled.

### 8. Device location map — in progress

Show each client device as a dot on a persistent Wi-Fi floor plan. Design: [device-location-map.md](docs/device-location-map.md) (approved 2026-10-23). Placement is room-of-connected-AP only; finer position (multi-AP fingerprinting) is a later goal.

- [x] PR 1 (#68): rebase and reduce #26 to floor plan and AP placement only (DB-persisted, audited, on current main). Close #25 and #26 once it merges.
- [x] PR 2: client tracking backend and roaming history. One typed read operation `wifi_clients` (hostapd `get_clients` for each AP), opt-in `client_tracking_enabled` (off by default), 5 min default poll adjustable 60-600 s, state rows hard-capped at 24 h, `wifi_client_events` history (connect, roam, disconnect with RSSI) kept 30 days, hashed MAC identity with encrypted MAC, label/hide/clear API. Migration 012. Not yet exercised against a real hostapd.
- [ ] Lab test with real hostapd `get_clients` (a 60 s poll may be needed for roam tests); configurable roam-history retention.
- [x] PR 3 (UI): green/red dots on the floor plan (room of the connected AP), legend, labels, hide, "Clear client data", client tracking toggle with 1/5/10 min poll, and a roaming history list. Verified against mocked API in a browser; not yet against a real AP.
- [ ] Privacy: keyed-hash identity, masked MACs, reveal audited, never logged. Feature off by default.
- [ ] Optional later: manual room pin and fingerprinting calibration.

### 9. Wi-Fi password settings — next

Push normal WPA2/WPA3 password Wi-Fi settings (SSID, passphrase, band, radio on/off) to APs through the Section 3 controlled-change workflow. Use the same rules as the hostname write: preview and diff, `uci apply` with a 60 s rollback, read-back over a fresh connection, then confirm. Passphrases are secrets: encrypted at rest, shown once, and never logged or left unredacted in snapshots.

- [ ] Preview and apply SSID, passphrase, and band on one AP, with rollback, lab-tested like the hostname write.
- [ ] Same flow in the configuration dialog, with per-device change history.

Exit criteria: a changed SSID or passphrase applies safely on one AP, rolls back when the read-back fails, and never logs the passphrase.

### 10. Optional later: IoT/guest VLAN separation

Put chosen devices on their own VLAN with its own firewall zone, selected by SSID or by a per-device passphrase. On the AP, hostapd can map a passphrase to a VLAN ID (`wpa_psk_file` with `vlanid`), so RADIUS is not needed. RADIUS dynamic VLAN is the alternative. Dependencies: the VLANs and firewall zones live on the main router, which is not enrolled yet, and the path from AP to router must carry tagged VLANs. Firewall rules stay on the router, so separation only works once that path is set up.

Trusted hosts such as Home Assistant still need to reach IoT devices. Policy: main to IoT is allowed. IoT to main is allowed only for established traffic, plus narrow pinholes: IoT devices may open connections to the HA address only (for example MQTT 1883, ESPHome API, and webhooks). mDNS does not cross VLANs by default, so it needs a reflector (avahi or umdns), or HA with an interface on the IoT VLAN.

Safety defaults: the IoT VLAN is opt-in and off by default. Cast and print devices stay on the main network unless the user moves them. A scoped reflector (avahi or umdns, limited to the chosen service types and the IoT and main interfaces) is offered only to users who move such devices, with a pre-flight warning listing what may break, such as phone casting and printer discovery. A second HA interface on the IoT VLAN helps HA only; it does not let phones on the main network see IoT devices.

### Later: per-device firmware build and upgrade — in progress

Started 2026-10-29. Goal: build and flash a firmware image per device that keeps all installed packages (including third-party ones such as `luci-theme-fluent`) and reapplies configuration, like ASU. Design, feasibility verdict and open questions: [docs/firmware-upgrades.md](docs/firmware-upgrades.md). It refines Section 5 and inherits its safety rules.

- [x] Phase 0: device config backup (#92, #94, #98, #99). A typed `firmware_backup` operation (`sysupgrade -b` archive of `/etc/config` and the keep-list files), stored encrypted with the existing key bound to device and backup id, size-capped, never logged, never extracted on the server. Download and restore are admin-only and audited; restore takes only an OpenManage-captured archive to the same device and board, with lockout checks. Last 5 kept per device, pre-flash backup pinned. Usable as a standalone "back up this router" button and optional schedule, and required before any firmware flash. Agreed by Nishant 2026-10-26; security rules from the review agreed 2026-10-26 (see the firmware design doc).
- [ ] Phase 1: package and feed inventory (read-only `firmware_inventory`: release, board, package manager, hand-installed packages, removed defaults, custom feeds with credentials stripped, keep list, extroot, free space, shown on the Firmware page; done), a read-only upgrade plan for any release the build server offers (opt-in build server; done), preparing an upgrade (build, hash check, pinned backup, staged image, `sysupgrade -T`; done), flashing it with post-reboot checks (done, tested in the container lab and in a real OpenWrt x86-64 VM; hardware test next), plan-only package diff (read-only), and a package source registry (stored release file or upload, installed allow-untrusted after admin review of its control data; the registry and installing a confirmed source on a device are done; reinstalling after a flash is reported (never automatic) and installed on request) for installing external packages such as themes, with source-unknown packages flagged, and a versioned desired package set per device (group inheritance deferred) that is diffed against the installed packages. Step 5a is being implemented in PR #132 as metadata-only immutable revisions; package changes remain person-started and are never automatic.
- [ ] Phase 2: single-device upgrade through the ASU API, third-party packages reinstalled afterward, encrypted config backup, post-reboot verification and drift review.
- [ ] Phase 3: staged rollout (canary, concurrency limit, stop on failure).
- [ ] Phase 4 (optional): self-hosted ASU mode. A build-server URL setting plus a single setup script for a separate Linux machine (outside the OpenManage stack), with optional extra feeds. Public ASU stays the default. Decided by Nishant 2026-10-23.

### Later: per-device settings editor (after firmware)

Not scheduled; starts after the firmware work is done and tested. Agreed in principle by Nishant 2026-10-29. Goal: view and change each device's settings from OpenManage, with a LuCI-like menu (System, Network, Wireless, DHCP/DNS, Firewall, Software) so users find things where they expect them. LuCI's own pages are not reused; OpenManage builds its own pages.

- [ ] Read-only view of all UCI settings per device, grouped in the LuCI-like menu, secrets redacted.
- [ ] Widen the Section 3 controlled-change workflow from one setting to a batch: pre-change backup, diff, `uci apply` with 60 s rollback, read-back over a fresh connection, confirm. Refuse if the device config changed since the page loaded.
- [ ] Hand-built pages for common sections, Wireless first (built on the Section 9 Wi-Fi write, which stays scheduled as next and is not deferred), then System, Network interfaces, DHCP/DNS, Firewall.
- [ ] Admin-only raw UCI editor for everything else, including third-party apps, through the same workflow.

Safety rules: changes that can drop clients (wireless, network, firewall, management access) need an explicit opt-in tickbox; existing SSIDs and passphrases stay locked unless the user unlocks that field; no change ever forces clients to rejoin without opt-in.

### Later: Software page (package install and remove)

Builds on the firmware package work (inventory, package source registry, desired package sets); not a separate package system. Search the package list, install, remove, and refresh lists, for apk and opkg, through typed operations.

- [ ] Software page with search, install, remove, refresh lists; installs and removes update the device's desired package set so they survive a firmware upgrade.
- [ ] Guardrails: core packages (dropbear, wpad/hostapd, firewall, kernel modules, OpenManage dependencies) blocked or behind a typed confirmation; free-space check before install; kernel modules must match the running kernel; note that removing a package that shipped in the firmware image frees no space.

## Deferred and optional items

- SSH key ciphertext is bound to a fixed domain label (`sshKeyAAD`), not to the device. Binding it to the device is deferred to the encryption-key rotation work in Section 4, because existing ciphertext must be re-encrypted in the same change.
- PostgreSQL 18 is optional and not scheduled; Compose currently uses `postgres:17-alpine`.
- RADIUS client coverage: the iPhone test is pending; Android and Windows are untested. MAC authentication remains open in Section 7.

## Delivery rules

1. Work from a feature branch and open a PR for every logical change; never commit directly to main.
2. Keep PRs small and reversible. Database changes should be additive where possible, with documented compatibility and rollback/data-loss implications.
3. Validate frontend build, go test ./..., Docker image build, and relevant security tests in CI. Report the actual run URL and result; do not infer success from a previous commit.
4. The self-hosted runner is trusted infrastructure. Do not add pull_request execution for untrusted fork code; gate the CI job to refs/heads/main so workflow_dispatch cannot execute a non-main ref on that runner.
5. After each major merge, audit the merged diff and current project for security, correctness, migrations/rollback, dependencies, Compose hardening, tests/CI, and roadmap consistency.
6. Update this file when scope, sequencing, or milestone status materially changes.
