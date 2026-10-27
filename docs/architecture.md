# Architecture and security boundaries

## Runtime shape

Browser -> same-origin HTTP(S), session cookie + CSRF header -> Go HTTP app -> policy/validation -> bounded device-operation engine -> OpenWrt device

The Go app serves the React/TypeScript build and JSON API. PostgreSQL stores device inventory, users/sessions, audit events, and encrypted device credentials. Compose is the initial deployment model, and the planned production host is Docker inside a Proxmox LXC. When `OPENMANAGE_RADIUS_LISTEN` is set, the same process also answers RADIUS on UDP for Wi-Fi sign-in. The app can be bound to a specific host LAN IP or all interfaces; prefer a specific LAN IP and firewall restrictions. Direct HTTP is supported only for a trusted, isolated LAN and does not encrypt passwords or session cookies in transit. Prefer HTTPS through a trusted reverse proxy and set OPENMANAGE_COOKIE_SECURE=true. Never port-forward the app to the public internet. Keep the single app + PostgreSQL deployment as the baseline; add a queue or Redis only when measured operational requirements justify it.

## Main code locations

- cmd/openmanage/main.go: HTTP routes, authentication/session middleware, inventory API, audit API, common helpers.
- cmd/openmanage/enrollment.go: SSH enrollment, host-key pinning, private-key encryption.
- cmd/openmanage/operations.go: typed device-operation engine (timeouts, output limits, audit policy) used by health checks and monitoring.
- cmd/openmanage/config_snapshots.go, config_changes.go, uci.go: encrypted `uci export` snapshots, drift diff, and the hostname write with rollback.
- cmd/openmanage/users.go: user management and password/session handling. monitoring.go and scheduled_snapshots.go: background work.
- cmd/openmanage/radius.go, eap.go, radius_cert.go, radius_profile.go, network_users.go: built-in RADIUS listener, EAP-TTLS, certificate, Apple profile, and Wi-Fi users.
- cmd/openmanage/*_test.go: Go tests.
- cmd/openmanage/migrations/: embedded SQL migrations 001 to 010, covering inventory, authentication and audit, device credentials, monitoring, configuration snapshots and their schedule, user management, configuration changes, and RADIUS.
- web/: React/TypeScript UI and frontend package manifest.
- compose.yaml and Dockerfile: runtime/deployment.
- .github/workflows/ci.yml: trusted branch-push validation on the self-hosted WSL2 runner.

## Device-operation model

The system should use one reusable operation engine for read-only checks and future controlled writes. The engine is an execution and policy boundary, not a general-purpose shell proxy.

Each operation should be a server-defined type with a stable name, read/write classification, validated parameters, target compatibility checks, privilege requirements, timeout/output limits, cancellation behavior, and audit policy. The browser may request a named operation and provide schema-validated parameters; it must never supply an arbitrary shell command or script. Build commands from validated operation definitions on the server.

Start with read-only health and diagnostics. This first implementation should establish pinned-host-key verification, credential retrieval/decryption, bounded SSH sessions, concurrency control, cancellation, output sanitization, and audit lifecycle. Future operation types can reuse these safeguards while adding stricter preflight and confirmation requirements for writes.

### Configuration operations

Configuration changes should use structured, validated inputs and UCI-aware operations where practical. The workflow should expose a preview/diff, validate compatibility, require deliberate operator confirmation, apply in a bounded session, verify both connectivity and intended state, and record the outcome. For changes that can break access, establish a timed rollback before applying and test it on supported device classes. A rollback mechanism must not be described as guaranteed unless it has been verified for that operation and device.

### Firmware operations

Firmware upgrades are a distinct high-risk operation type with stricter checks than routine maintenance. Validate exact hardware/target compatibility, supported upgrade path, image provenance and integrity, storage, and configuration backup. Require explicit confirmation, staged canaries, concurrency limits, progress tracking, reboot/reconnect handling, and post-upgrade health verification. SSH cannot recover every failed flash; a device may require failsafe mode, physical reset, or serial access. Never imply that a universal rollback is available.

### Audit and failure semantics

Audit security-sensitive operations with initiator, device, operation type, lifecycle status, and sanitized outcome. Keep state changes and their audit writes in one transaction whenever feasible. Never store private keys, credentials, secret-bearing arguments, or unbounded command output in audit records. Distinguish operation failure from verification failure and from an unreachable device after a potentially disruptive change.

## Security invariants

- API routes other than health and login require an authenticated session; state-changing requests require CSRF validation. Every route that changes data or exposes audit information must enforce its role policy server-side; UI visibility is not authorization.
- Direct HTTP leaves login credentials and session cookies observable to anyone able to capture traffic on the path. Use HTTPS for normal operation and restrict any temporary direct-HTTP mode to a trusted isolated LAN.
- Session tokens are random; only token hashes are stored in the database. Cookies are HttpOnly for the session, SameSite Strict, and Secure when OPENMANAGE_COOKIE_SECURE=true.
- Administrator bootstrap credentials come from environment configuration. Use a strong, unique password and do not commit a real .env.
- Enrollment is admin-only. It accepts private IP literals only, uses TCP/22, verifies an independently sourced SHA-256 host-key fingerprint, and proves that the supplied SSH key authenticates before storing it.
- Stored SSH private-key material is encrypted with AES-256-GCM using the 32-byte base64 key in OPENMANAGE_ENCRYPTION_KEY. The encryption key is separate from the database and must be backed up securely. A database backup without the matching key cannot recover credentials.
- The enrollment endpoint does not install or rotate device-side authorized keys and does not run arbitrary commands. The Add device wizard (`/api/device-bootstrap`) is the exception: it installs one generated key, using the root password once, and never stores that password.
- The RADIUS listener is off unless configured. It accepts requests only from the inventory address of an enrolled AP that carries a valid Message-Authenticator, and it never logs passwords or secrets.
- The operation engine must revalidate device targets and credentials, enforce host-key pinning and resource bounds, and authorize every operation server-side. A successful SSH connection alone does not authorize a requested action.
- Audit events should be durable for security-sensitive state changes. Keep the state mutation and audit write in one transaction whenever feasible.
- Do not expose the app to untrusted networks without HTTPS, access controls, and an explicit deployment review.

## Operational cautions

- SQL migrations run at application startup. Prefer additive changes and ensure repeated startup is safe.
- Before changing credential schema or encryption format, define a migration and recovery plan. Never silently replace the encryption key or discard existing ciphertext.
- Keep SSH operations time-bounded and cancellation-aware. Bound command output and concurrency, and ensure cancellations release sessions and connections.
- Resource budgets in Compose are intentional; any background polling or concurrency feature must have explicit limits.
