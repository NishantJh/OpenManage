# OpenManage implementation plan

This is the roadmap and source of truth for sequencing work. Keep it short and update it when a milestone changes. Prefer small, reversible pull requests over broad rewrites.

## Product direction

Build a lightweight, self-hosted control plane for OpenWrt, inspired by useful OpenWISP concepts but implemented around Go, PostgreSQL, and a React/TypeScript UI. Keep the default deployment small; do not add Redis or another service unless a demonstrated requirement justifies it.

## Current baseline

- Go HTTP API and React/TypeScript UI, served by one app process; PostgreSQL persists inventory and security data.
- Administrator bootstrap, session authentication, CSRF protection, security headers, and an audit log are implemented.
- Device inventory CRUD and the audit activity UI are implemented.
- SSH enrollment API verifies an independently verified SHA-256 host-key fingerprint and SSH public-key authentication before storing the private key encrypted with AES-256-GCM.
- Enrollment is admin-only, limited to private IP literals on TCP/22, and requires OPENMANAGE_ENCRYPTION_KEY.
- Enrollment does not install or rotate router-side authorized keys, run remote commands, or push configuration.
- CI on trusted branch pushes runs frontend build, Go tests, and Docker image build on the WSL2 self-hosted runner. There is intentionally no pull_request trigger because the repository is public and untrusted fork code must not run on that runner.

## Roadmap

### 0. Security and engineering hardening — in progress

- [x] Make credential persistence, device status update, and the enrollment audit event atomic in one database transaction.
- [ ] Add handler/integration-style tests for enrollment success, bad host fingerprint, failed SSH authentication, re-enrollment preserving old credentials on failure, and persistence/audit failures.
- [x] Make inventory create/delete and login/logout audit writes atomic with their corresponding state changes; mandatory audit failure must fail the action (merged in PR #15; post-merge CI, security checks, and CodeQL passed).
- [x] Add bounded login rate limiting with safe peer-address parsing, no trust in forwarded headers, and Retry-After responses (PR in progress; verify after merge).
- [ ] Complete the safety-audit findings in docs/security-audit.md before treating this as a routine LAN management plane.
- [x] Format Go sources and make CI reject unformatted Go files.
- [x] Reject malformed device IDs before database queries.
- [x] Add an enrollment request body-size cap and test malformed/oversized enrollment payloads.
- [ ] Verify request-size and timeout behavior across all API handlers.
- [ ] Improve dependency reproducibility: generate and commit go.sum and package-lock.json from the real toolchains, use npm ci, and make CI verify rather than mutate dependency metadata.
- [x] Document the PostgreSQL and OPENMANAGE_ENCRYPTION_KEY backup/restore procedure; recovery testing remains an operator task.
- [ ] Run and record a disposable-stack restore exercise, including verification that an enrolled credential decrypts with the restored key.

Exit criteria: tests cover critical enrollment failure paths; credential changes cannot commit without their audit event; CI passes on the PR head and after merge.

### 1. Enrollment UX and device lifecycle — in progress

- [x] Add an admin-only enrollment form with independent host-key fingerprint verification guidance.
- [x] Keep private-key material out of local storage, logs, audit details, analytics, and API responses; clear form state after submission and on close.
- [x] Refresh device status and last-seen data after successful SSH authentication.
- [x] Allow admins to re-enroll from the inventory without exposing stored secrets.
- [ ] Verify UI error handling for missing encryption-key configuration, invalid key formats, unreachable devices, and fingerprint mismatch.

Exit criteria: the user can enroll a device through the UI, see a safe outcome, and cannot retrieve the stored private key.

### 2. Shared device-operations engine and health — next

Design one reusable, bounded SSH operation engine that can support both read-only and future write-capable operations. Begin by implementing only read-only health and diagnostics; the shared engine must not imply that arbitrary commands are allowed.

- Reuse stored credentials and pinned host keys; enforce per-device timeouts, cancellation, concurrency limits, output-size limits, and safe error handling.
- Represent work as typed, named operations with explicit policies: allowed operation, read/write classification, validated parameters, privilege requirements, timeout, and audit requirements.
- Keep command construction and validation server-side. Do not expose a generic shell/command field to the browser or accept arbitrary commands from API clients.
- Start with a small allowlist of read-only OpenWrt health commands; parse structured output where possible and handle firmware/version differences.
- Record operation identity, initiator, target, start/end status, and sanitized result metadata in the audit trail. Do not log credentials, secret-bearing arguments, or full command output by default.
- Keep execution in the existing app initially. Add a queue or Redis only if measured fleet size or reliability needs justify the added service.

Exit criteria: health checks are reliable, resource-bounded, auditable, and use the same policy/execution boundary intended for later controlled writes.

### 3. Versioned configuration workflows

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

- Add bounded reachability monitoring, alerts, and firmware inventory.
- Consider topology, IPAM, PKI/VPN, RADIUS, multi-user roles, and larger-fleet features only when a concrete use case warrants them.

## Delivery rules

1. Work from a feature branch and open a PR for every logical change; never commit directly to main.
2. Keep PRs small and reversible. Database changes should be additive where possible, with documented compatibility and rollback/data-loss implications.
3. Validate frontend build, go test ./..., Docker image build, and relevant security tests in CI. Report the actual run URL and result; do not infer success from a previous commit.
4. The self-hosted runner is trusted infrastructure. Do not add pull_request execution for untrusted fork code or run unreviewed external code on it.
5. After each major merge, audit the merged diff and current project for security, correctness, migrations/rollback, dependencies, Compose hardening, tests/CI, and roadmap consistency.
6. Update this file when scope, sequencing, or milestone status materially changes.
