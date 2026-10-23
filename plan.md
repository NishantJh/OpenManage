# OpenManage implementation plan

This is the roadmap and source of truth for sequencing work. Keep it short and update it when a milestone changes. Prefer small, reversible pull requests over broad rewrites.

## Product direction

Build a lightweight, self-hosted control plane for OpenWrt, inspired by useful OpenWISP concepts but implemented around Go, PostgreSQL, and a React/TypeScript UI. Keep the default deployment small; do not add Redis or another service unless a demonstrated requirement justifies it.

Production target: Docker Compose inside a Proxmox LXC. WSL2 is the test environment (the OpenWrt lab, the restore drill, and the [home setup guide](docs/home-setup.md)), not the production host.

## Priority (agreed 2026-10-22, updated 2026-10-23)

Hardening (Sections 0 and 1) and the operations engine, read-only configuration, and first controlled write (Sections 2 and 3) are largely done. Built-in RADIUS (Section 7) is the active product work, with user management and the hostname write already in place. Remaining safety-audit items, alerts, and the deferred items below come after it. PRs that do not map to a roadmap item get a plan entry first, or are parked as drafts or closed.

## Current baseline

- Go HTTP API and React/TypeScript UI, served by one app process; PostgreSQL persists inventory, users, audit events, encrypted credentials, encrypted configuration snapshots, and monitoring history.
- Administrator bootstrap, session authentication, CSRF protection, security headers, and an audit log are implemented.
- Device inventory CRUD and the audit activity UI are implemented.
- User management (admin, operator, and readonly roles), read-only health checks, opt-in monitoring and scheduled snapshots with drift detection, and one controlled write (system hostname with timed rollback) are implemented.
- Built-in RADIUS is implemented at lab stage: opt-in, EAP-TTLS with inner PAP and plain PAP, off unless `OPENMANAGE_RADIUS_LISTEN` is set.
- SSH enrollment API verifies an independently verified SHA-256 host-key fingerprint and SSH public-key authentication before storing the private key encrypted with AES-256-GCM.
- Enrollment is admin-only, limited to private IP literals on TCP/22, and requires OPENMANAGE_ENCRYPTION_KEY.
- Enrollment does not install or rotate router-side authorized keys, run remote commands, or push configuration.
- CI on trusted branch pushes runs frontend build, Go tests, and Docker image build on the WSL2 self-hosted runner. Pull requests are validated on GitHub-hosted runners only; the repository is public, so untrusted fork code must never run on the self-hosted runner.

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

RADIUS is built into the Go application rather than run as a separate FreeRADIUS container. It comes after user management and the first controlled configuration write, because it needs user identities and a safe way to push RADIUS settings to APs. EAP (PEAP/TTLS) is security-sensitive protocol code with no mature Go server library, so treat it as high-risk.

- [x] Design doc: scope, EAP methods, server certificate handling, per-AP shared-secret storage, threat model. ([docs/radius-design.md](docs/radius-design.md); EAP-TTLS/PAP first, `layeh.com/radius` for packet encoding.)
- [ ] Opt-in UDP 1812 listener in the app with PAP and MAC authentication for a lab, per-AP secrets encrypted at rest, rate limiting, and audit. (PAP, secrets, rate limits, audit and the dashboard section done; MAC authentication remains.)
- [ ] EAP-TTLS with a server certificate, tested against real phone and laptop clients. (EAP-TTLS/PAP done and tested with `eapol_test`; iPhone test pending; Android and Windows untested. PEAP-MSCHAPv2 only if a needed client cannot use TTLS.)
- [ ] Push AP RADIUS configuration through the Section 3 configuration workflow.

Exit criteria: a client can join a managed AP with per-user credentials, failures are audited without logging secrets, and the listener is off unless enabled.

### 8. Device location map — active

Show each client device as a dot on a persistent Wi-Fi floor plan. Design: [device-location-map.md](docs/device-location-map.md) (approved 2026-10-23). Placement is room-of-connected-AP only; finer position (multi-AP fingerprinting) is a later goal.

- [x] PR 1 (#68): rebase and reduce #26 to floor plan and AP placement only (DB-persisted, audited, on current main). Close #25 and #26 once it merges.
- [ ] PR 2: client tracking backend. `wifi_ap_list` and `wifi_clients` as typed operations, opt-in `client_tracking_enabled` (off by default), 5 min default poll, adjustable from 60 s up to 10 min if 5 min is too noisy, 24 h hard cap on current-state rows, and a migration.
- [ ] PR 3: roaming history in `wifi_client_roams` (AP changes with RSSI, 30 days by default, minimum 24 h, configurable). Lab test with `get_clients`; a 60 s poll may be needed for roam tests.
- [ ] PR 4: UI with green/red dots, legend, labels, hide and "Clear client data".
- [ ] Privacy: keyed-hash identity, masked MACs, reveal audited, never logged. Feature off by default.
- [ ] Optional later: manual room pin and fingerprinting calibration.

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
