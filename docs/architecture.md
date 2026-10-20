# Architecture and security boundaries

## Runtime shape

Browser -> same-origin HTTP(S), session cookie + CSRF header -> Go HTTP app -> pgx connection pool -> PostgreSQL

The Go process serves the React/TypeScript build and JSON API. PostgreSQL stores device inventory, users/sessions, audit events, and encrypted device credentials. Compose is the initial deployment model. The default host binding is localhost; remote access should use HTTPS and network access controls.

## Main code locations

- cmd/openmanage/main.go: HTTP routes, authentication/session middleware, inventory API, audit API, common helpers.
- cmd/openmanage/enrollment.go: SSH enrollment, host-key pinning, private-key encryption.
- cmd/openmanage/*_test.go: Go tests.
- cmd/openmanage/migrations/: embedded SQL migrations; currently initial inventory, authentication/audit, and device credentials.
- web/: React/TypeScript UI and frontend package manifest.
- compose.yaml and Dockerfile: runtime/deployment.
- .github/workflows/ci.yml: trusted branch-push validation on the self-hosted WSL2 runner.

## Security invariants

- API routes other than health and login require an authenticated session; state-changing requests require CSRF validation.
- Session tokens are random; only token hashes are stored in the database. Cookies are HttpOnly for the session, SameSite Strict, and Secure when OPENMANAGE_COOKIE_SECURE=true.
- Administrator bootstrap credentials come from environment configuration. Use a strong, unique password and do not commit a real .env.
- Enrollment is admin-only. It accepts private IP literals only, uses TCP/22, verifies an independently sourced SHA-256 host-key fingerprint, and proves that the supplied SSH key authenticates before storing it.
- Stored SSH private-key material is encrypted with AES-256-GCM using the 32-byte base64 key in OPENMANAGE_ENCRYPTION_KEY. The encryption key is separate from the database and must be backed up securely. A database backup without the matching key cannot recover credentials.
- The enrollment endpoint does not install or rotate router-side authorized keys and does not run arbitrary commands.
- Audit events should be durable for security-sensitive state changes. Keep the state mutation and audit write in one transaction whenever feasible.
- Do not expose the app to untrusted networks without HTTPS, access controls, and an explicit deployment review.

## Operational cautions

- SQL migrations run at application startup. Prefer additive changes and ensure repeated startup is safe.
- Before changing credential schema or encryption format, define a migration and recovery plan. Never silently replace the encryption key or discard existing ciphertext.
- Keep SSH operations time-bounded and cancellation-aware. Validate device targets and never provide a generic browser-facing command execution endpoint.
- Resource budgets in Compose are intentional; any background polling or concurrency feature must have explicit limits.
