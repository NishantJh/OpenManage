# OpenManage safety audit

**Audit date:** 2026-10-20  
**Scope:** repository code and deployment configuration at the `main` revision before the LAN-access and API-role hardening PRs. This is a source review, not a penetration test or a validated production certification. Findings marked pending must be rechecked against the merged code.

## Executive assessment

OpenManage has useful foundations: session tokens are random and stored as hashes, passwords use bcrypt, state-changing API requests require a CSRF cookie/header match, SSH host keys are pinned to an independently supplied fingerprint, SSH private keys are encrypted at rest, enrollment is constrained to private IP literals/TCP 22, and the Compose app runs read-only with dropped Linux capabilities and no-new-privileges.

It is still early-development software and should not be treated as a production management plane. LAN availability is a deployment requirement, but direct HTTP must be understood as an explicitly less-secure mode. The highest-priority gaps are server-side role enforcement, best-effort audit writes for several state changes, and lack of login rate limiting. PR #13 also exposed and remediated known Go standard-library and dependency vulnerabilities by upgrading to Go 1.26.x and patched pgx, x/crypto, and x/text versions; the follow-up vulnerability scan passed. Dependency lockfiles and the restore drill are now done (OM-SAF-05 and OM-SAF-06).

## Findings

### OM-SAF-01 — Inconsistent API role enforcement

**Severity: High — remediated in PR #10**

The database defines `admin`, `operator`, and `readonly` roles. The initial audit found that device create/delete and audit listing lacked route-level role checks. PR #10 added server-side checks: only admin/operator can create or delete inventory, only admin can list audit events, and enrollment remains admin-only. Tests cover denied roles before database access. Recheck the post-merge CI and retain regression tests.

Tracking: [PR #10](https://github.com/NishantJh/OpenManage/pull/10) is merged, and the denied-role regression tests stay in the suite. Status 2026-10-23: verified in code; every write and audit route uses `requireAnyRole`.

### OM-SAF-02 — Direct HTTP does not protect credentials or session cookies in transit

**Severity: High when used on an untrusted/shared network; deployment-dependent**

**Status 2026-10-23: partly addressed; still open.** The Compose binding defaults to loopback and LAN access is an explicit choice. The cookie-secure default is still `false`, so HTTPS deployments must set `OPENMANAGE_COOKIE_SECURE=true`.

The default cookie setting is `OPENMANAGE_COOKIE_SECURE=false`, which is necessary for direct HTTP but means the browser can send the session cookie over cleartext HTTP. HttpOnly and SameSite do not encrypt traffic. A LAN-reachable port must therefore be restricted to a trusted network; use HTTPS through a trusted reverse proxy and set `OPENMANAGE_COOKIE_SECURE=true` for routine use. Never port-forward this service to the public internet.

The Compose binding now defaults to `127.0.0.1:8080` (loopback only). LAN access is an explicit `OPENMANAGE_PORT` choice, documented as binding to a specific LAN IP plus firewall restrictions. This is not a substitute for TLS.

### OM-SAF-03 — Audit writes are not atomic with several state changes

**Severity: High for audit integrity — remediated in PR #15; post-merge CI, Security Checks, and CodeQL passed**

SSH enrollment, device create/delete, and login/logout now place the state mutation and mandatory audit insert in the same database transaction. If the audit insert or commit fails, the transaction rolls back and the request returns an error rather than reporting success without a durable audit record. The disposable runtime smoke test now injects audit-insert failures and verifies device creation rolls back and device deletion preserves the row. PR validation and post-merge CI passed.

### OM-SAF-04 — No login rate limiting or lockout control

**Severity: Medium — remediation implemented in PR (post-merge verification pending)**

Login now applies a bounded per-peer-IP failed-attempt limiter with a 10-failure window and a five-minute block, returns HTTP 429 with `Retry-After` when blocked, and clears that peer's failure state after successful authentication. Client identity is derived from the socket peer address; arbitrary forwarded headers are not trusted. Unit tests and the disposable runtime smoke test cover throttling behavior. Recheck these controls after merge; deployments behind a reverse proxy should also configure rate limiting at the proxy.

### OM-SAF-05 — Dependency resolution is not reproducible

**Severity: Medium — remediated in PR #32 (post-merge main CI passed) and the follow-up Dockerfile change**

Status 2026-10-22: generated `go.sum` and `web/package-lock.json` are committed. CI and PR validation install with `npm ci` and verify Go metadata with `go mod tidy -diff`, failing on drift. The Dockerfile installs from the lockfiles with `npm ci` and runs `go mod verify`. The original finding follows.

The repository has no committed Go `go.sum` and no frontend `package-lock.json` at the audited revision. CI and Docker builds use `go mod tidy` / `npm install`, which may resolve changing dependency versions and can mutate dependency metadata during CI. Generate and commit real lockfiles from the project toolchains, switch to `npm ci`, use `go mod download` or equivalent verification in CI, and ensure CI fails if dependency files are stale. Do not fabricate lockfiles.

### OM-SAF-06 — Backup procedure has not been recovery-tested

**Severity: Medium — remediated 2026-10-22**

Status: the drill passed on WSL2 (see the drill record in [backup-recovery.md](backup-recovery.md)). Re-run it on the production host, and after any schema, backup, or key-handling change.

The original finding follows.

The coupled PostgreSQL dump and `OPENMANAGE_ENCRYPTION_KEY` recovery procedure is documented. It has not been proven by a successful isolated restore and credential-decryption check. Run a restore exercise on a disposable stack, verify inventory/audit data and a known enrolled credential, and record the application/database versions and recovery time. Never use the first restore test on production data.

### OM-SAF-07 — HTTP request controls need broader verification

**Severity: Medium / follow-up — remediated; see status below**

Status 2026-10-22: every JSON write endpoint now decodes through one helper that enforces a per-endpoint body cap (login 4 KiB, device create 8 KiB, monitoring 4 KiB, enrollment 32 KiB), rejects unknown fields and trailing data, and applies field-length limits (username, password, device name/address/model/firmware). Every `/api/` request gets a 25-second context deadline, below the 30-second server write timeout, so database and device work cannot outlive the response. Tests cover malformed, unknown-field, trailing, oversized, and over-length bodies on each endpoint, and assert the timeout ordering. The original finding follows.

The enrollment body is limited to 32 KiB and the HTTP server has read-header, read, write, and idle timeouts. Login and inventory writes use a larger 1 MiB body cap. Continue testing oversized and malformed bodies, trailing JSON, cancellation, and timeout behavior across every write endpoint. Consider tighter limits for small JSON APIs and explicit request-context deadlines for database work.

### OM-SAF-08 — Enrollment safety boundaries are good but not yet fully integration-tested

**Severity: Medium** — failure-path tests added in PR #34; real-OpenWrt lab test added

The lab test (`scripts/openwrt-lab-test.sh`) found and fixed two bugs. When dropbear offered both RSA and Ed25519 host keys, Go negotiated RSA, so an Ed25519 pin always failed. A router with a blank root password let SSH "none" auth through, so enrollment "verified" any key; it now fails with `key_not_verified`.

Enrollment restricts targets to private IP literals, TCP/22, independently pinned SHA-256 host keys, and SSH key authentication before persistence. The private key is encrypted with AES-256-GCM and excluded from API responses/audit details. Status 2026-10-23: those tests are in place (PR #34), and the real-OpenWrt lab exercises enrollment and monitoring. The original finding follows: tests were then needed for SSH authentication and fingerprint failures, re-enrollment preserving existing credentials after failure, and database/audit failure rollback. Keep arbitrary remote command execution disabled until the typed operation engine and its policy checks are implemented.

## Positive controls observed

- PostgreSQL is not published on a host port by Compose.
- The app container is non-root, read-only, has all capabilities dropped, uses no-new-privileges, and has a small tmpfs.
- The app and database have explicit memory/CPU limits.
- The SSH enrollment transaction couples credential persistence, device status update, and its audit event.
- Private key ciphertext uses AES-GCM with domain-separation associated data and a fresh nonce.
- CI intentionally avoids running untrusted fork pull-request code on the self-hosted WSL2 runner.

## Required order of work

Status 2026-10-23: items 1 to 6 are done (see the findings above). Item 7 is complete for read-only operations and the first controlled write (hostname); later writes stay behind the same typed-operation and rollback controls.

1. [In progress verification] Enforce API role permissions (merged in PR #10; confirm post-merge CI).
2. Enable trusted-LAN access and document HTTPS/firewall deployment choices without implying that HTTP is encrypted.
3. Make inventory and authentication audit events atomic with their state changes.
4. Add login abuse controls and finish enrollment failure-path tests.
5. Commit generated dependency lockfiles and make CI deterministic.
6. Perform and document a disposable-environment backup/restore exercise.
7. Only then proceed to the shared device-operation engine; keep writes and firmware upgrades gated behind typed operations, explicit authorization, and tested recovery.
