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

### 0. Security and engineering hardening — next

- Make credential persistence, device status update, and the enrollment audit event atomic in one database transaction.
- Add handler/integration-style tests for enrollment success, bad host fingerprint, failed SSH authentication, re-enrollment preserving old credentials on failure, and persistence/audit failures.
- Run gofmt consistently and make CI check formatting rather than silently modifying source.
- Improve malformed device-ID handling and verify request-size/timeout behavior.
- Improve dependency reproducibility where practical; prefer npm ci when a valid lockfile is present.
- Document backup/restore for PostgreSQL and OPENMANAGE_ENCRYPTION_KEY; test the recovery path.
- Keep this phase narrowly scoped and do not add remote command execution.

Exit criteria: tests cover critical enrollment failure paths; credential changes cannot commit without their audit event; CI passes on the PR head and after merge.

### 1. Enrollment UX and device lifecycle

- Add an admin-only enrollment form to the UI with clear instructions for obtaining and verifying a router host-key fingerprint.
- Never store private-key material in browser local storage, logs, audit details, analytics, or API responses. Clear key input state after submission and on navigation.
- Show enrollment status and last-seen data honestly; distinguish inventory metadata from a verified connection.
- Add a safe credential status/re-enrollment workflow without exposing stored secrets.
- Provide clear errors for missing encryption-key configuration, invalid key formats, unreachable devices, and fingerprint mismatch.

Exit criteria: the user can enroll a device through the UI, see a safe outcome, and cannot retrieve the stored private key.

### 2. Read-only SSH connectivity and health

- Add bounded, time-limited SSH checks using stored credentials and pinned host keys.
- Start with a small allowlist of read-only OpenWrt commands; parse structured output where possible and handle firmware/version differences.
- Record result, timestamp, and sanitized error details; do not log secrets or full command output by default.
- Define polling cadence and concurrency limits before implementing a worker. Avoid Redis until scale or queue requirements prove it necessary.
- Audit privileged actions and enforce per-device timeouts and cancellation.

Exit criteria: device reachability and basic system information are reliable, resource-bounded, and visible in the UI without arbitrary command execution.

### 3. Versioned configuration templates

- Model templates and revisions in PostgreSQL with explicit validation and immutable revision history.
- Render a preview and diff before applying; validate target compatibility and require explicit operator confirmation.
- Roll out to one device or a small canary group first, then expand in batches.
- Store a recoverable last-known-good configuration and document rollback limits before enabling writes.
- Audit who proposed, approved, applied, or rolled back each change.

Exit criteria: changes are reviewable, staged, auditable, and have a tested recovery procedure.

### 4. Guarded operations and key lifecycle

- Design router-side key installation, rotation, and revocation as explicit workflows with preflight checks and recovery access.
- Add write-capable SSH operations only after read-only operations, command allowlisting, audit coverage, and rollback behavior are established.
- Never expose a generic arbitrary-command API to the browser.
- Use least privilege on routers where feasible and make destructive actions require deliberate confirmation.

Exit criteria: credential rotation and write operations have tested failure recovery and do not silently lock out management access.

### 5. Monitoring, rollout, and later integrations

- Add bounded reachability monitoring, alerts, and firmware inventory first.
- Treat firmware upgrades as a separate high-risk workflow with compatibility checks, staged rollout, health verification, and rollback caveats.
- Consider topology, IPAM, PKI/VPN, RADIUS, multi-user roles, and larger fleet features only when a concrete use case warrants them.

## Delivery rules

1. Work from a feature branch and open a PR for every logical change; never commit directly to main.
2. Keep PRs small and reversible. Database changes should be additive where possible, with documented compatibility and rollback/data-loss implications.
3. Validate frontend build, go test ./..., Docker image build, and relevant security tests in CI. Report the actual run URL and result; do not infer success from a previous commit.
4. The self-hosted runner is trusted infrastructure. Do not add pull_request execution for untrusted fork code or run unreviewed external code on it.
5. After each major merge, audit the merged diff and current project for security, correctness, migrations/rollback, dependencies, Compose hardening, tests/CI, and roadmap consistency.
6. Update this file when scope, sequencing, or milestone status materially changes.
