# OpenManage safety audit

**Audit date:** 2026-10-20  
**Scope:** repository code and deployment configuration at the `main` revision before the LAN-access and API-role hardening PRs. This is a source review, not a penetration test or a validated production certification. Findings marked pending must be rechecked against the merged code.

## Executive assessment

OpenManage has useful foundations: session tokens are random and stored as hashes, passwords use bcrypt, state-changing API requests require a CSRF cookie/header match, SSH host keys are pinned to an independently supplied fingerprint, SSH private keys are encrypted at rest, enrollment is constrained to private IP literals/TCP 22, and the Compose app runs read-only with dropped Linux capabilities and no-new-privileges.

It is still early-development software and should not be treated as a production management plane. LAN availability is a deployment requirement, but direct HTTP must be understood as an explicitly less-secure mode. The highest-priority gaps are server-side role enforcement, best-effort audit writes for several state changes, and lack of login rate limiting. Dependency lockfiles and actual restore testing are also outstanding.

## Findings

### OM-SAF-01 — Inconsistent API role enforcement

**Severity: High — remediated in PR #10; post-merge CI verification pending**

The database defines `admin`, `operator`, and `readonly` roles. The initial audit found that device create/delete and audit listing lacked route-level role checks. PR #10 added server-side checks: only admin/operator can create or delete inventory, only admin can list audit events, and enrollment remains admin-only. Tests cover denied roles before database access. Recheck the post-merge CI and retain regression tests.

Tracking: [PR #10](https://github.com/NishantJh/OpenManage/pull/10) is merged; post-merge CI is being verified.

### OM-SAF-02 — Direct HTTP does not protect credentials or session cookies in transit

**Severity: High when used on an untrusted/shared network; deployment-dependent**

The default cookie setting is `OPENMANAGE_COOKIE_SECURE=false`, which is necessary for direct HTTP but means the browser can send the session cookie over cleartext HTTP. HttpOnly and SameSite do not encrypt traffic. A LAN-reachable port must therefore be restricted to a trusted network; use HTTPS through a trusted reverse proxy and set `OPENMANAGE_COOKIE_SECURE=true` for routine use. Never port-forward this service to the public internet.

The LAN-access PR changes the Compose binding default to make LAN use possible and documents binding to a specific LAN IP plus firewall restrictions. This is not a substitute for TLS.

### OM-SAF-03 — Audit writes are not atomic with several state changes

**Severity: High for audit integrity — remediation pending**

SSH enrollment already writes credentials, device status, and its audit event in one transaction. However, device create/delete and login/logout audit writes are best-effort and occur separately from the state change. A database error can therefore leave a successful action without a durable corresponding audit record. Move each relevant state mutation and audit insert into one transaction; define which events are mandatory and fail the operation if the audit write fails.

### OM-SAF-04 — No login rate limiting or lockout control

**Severity: Medium**

Login has a fixed delay after invalid credentials, but no per-account or per-source rate limit. The delay alone does not prevent distributed or sustained password guessing and can tie up server resources. Add bounded rate limiting with safe proxy/IP handling and tests; avoid trusting arbitrary forwarded headers.

### OM-SAF-05 — Dependency resolution is not reproducible

**Severity: Medium**

The repository has no committed Go `go.sum` and no frontend `package-lock.json` at the audited revision. CI and Docker builds use `go mod tidy` / `npm install`, which may resolve changing dependency versions and can mutate dependency metadata during CI. Generate and commit real lockfiles from the project toolchains, switch to `npm ci`, use `go mod download` or equivalent verification in CI, and ensure CI fails if dependency files are stale. Do not fabricate lockfiles.

### OM-SAF-06 — Backup procedure has not been recovery-tested

**Severity: Medium**

The coupled PostgreSQL dump and `OPENMANAGE_ENCRYPTION_KEY` recovery procedure is documented. It has not been proven by a successful isolated restore and credential-decryption check. Run a restore exercise on a disposable stack, verify inventory/audit data and a known enrolled credential, and record the application/database versions and recovery time. Never use the first restore test on production data.

### OM-SAF-07 — HTTP request controls need broader verification

**Severity: Medium / follow-up**

The enrollment body is limited to 32 KiB and the HTTP server has read-header, read, write, and idle timeouts. Login and inventory writes use a larger 1 MiB body cap. Continue testing oversized and malformed bodies, trailing JSON, cancellation, and timeout behavior across every write endpoint. Consider tighter limits for small JSON APIs and explicit request-context deadlines for database work.

### OM-SAF-08 — Enrollment safety boundaries are good but not yet fully integration-tested

**Severity: Medium**

Enrollment restricts targets to private IP literals, TCP/22, independently pinned SHA-256 host keys, and SSH key authentication before persistence. The private key is encrypted with AES-256-GCM and excluded from API responses/audit details. Tests still need to cover SSH authentication/fingerprint failures, re-enrollment preserving existing credentials after failure, and database/audit failure rollback. Keep arbitrary remote command execution disabled until the typed operation engine and its policy checks are implemented.

## Positive controls observed

- PostgreSQL is not published on a host port by Compose.
- The app container is non-root, read-only, has all capabilities dropped, uses no-new-privileges, and has a small tmpfs.
- The app and database have explicit memory/CPU limits.
- The SSH enrollment transaction couples credential persistence, device status update, and its audit event.
- Private key ciphertext uses AES-GCM with domain-separation associated data and a fresh nonce.
- CI intentionally avoids running untrusted fork pull-request code on the self-hosted WSL2 runner.

## Required order of work

1. [In progress verification] Enforce API role permissions (merged in PR #10; confirm post-merge CI).
2. Enable trusted-LAN access and document HTTPS/firewall deployment choices without implying that HTTP is encrypted.
3. Make inventory and authentication audit events atomic with their state changes.
4. Add login abuse controls and finish enrollment failure-path tests.
5. Commit generated dependency lockfiles and make CI deterministic.
6. Perform and document a disposable-environment backup/restore exercise.
7. Only then proceed to the shared device-operation engine; keep writes and firmware upgrades gated behind typed operations, explicit authorization, and tested recovery.
