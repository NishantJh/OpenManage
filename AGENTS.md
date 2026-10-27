# Instructions for coding agents and contributors

Read plan.md before planning work. Read [CODEQUALITY.md](CODEQUALITY.md) before making any change: it is the general code quality standard for every agent and contributor, including its Section 18 completion report. This file is the repository-specific operating guide and the Section 20 project-specific enforcement for that standard. Where the two overlap, the rules in this file win. The checks CODEQUALITY.md Sections 6 and 17 ask for are listed under "CI and security" and "Useful checks" below; run those rather than inventing others.

## Workflow

- Never commit directly to main. Create a descriptive feature branch from the latest main, make a focused change, run available checks, and open a PR.
- Before editing, inspect the relevant implementation, tests, migrations, Compose configuration, and current CI workflow. Prefer the smallest change that solves the task.
- Batch related edits before pushing when practical: branch-push CI cancels superseded runs.
- After a PR merge, verify the merge commit and its own CI run, then audit major changes. Do not claim a check passed unless its run for the relevant commit actually passed.
- Do not merge unrelated work together. Include summary, rationale, security considerations, test results, operational impact, and rollback/data-loss notes in PR descriptions.

## Project constraints

- Backend: Go HTTP server, PostgreSQL via pgx, SQL migrations embedded under cmd/openmanage/migrations/.
- Frontend: React + TypeScript under web/; built assets are copied into cmd/openmanage/web/dist/ by CI and embedded in the Go app.
- Deployment: compose.yaml, Dockerfile, .env.example. The production target is Docker Compose inside a Proxmox LXC; WSL2 is the test environment. Every push to main publishes the prebuilt image ghcr.io/nishantjh/openmanage (publish-image.yml); the server pulls it via OPENMANAGE_IMAGE and never builds. Keep the single-app + PostgreSQL baseline lightweight.
- Do not add Redis, a separate worker, or another runtime service without a specific requirement and documented trade-off.
- Use additive, restart-safe migrations where possible. Explain how old and new app versions interact with each migration and whether rollback loses data.
- Keep API errors sanitized. Never log credentials, session tokens, private keys, encryption keys, RADIUS shared secrets, CSRF tokens, device backup contents, or sensitive command output.
- Device backups are stored encrypted in PostgreSQL (device_backups). Restoring one requires the OpenManage administrator's password to be entered again; keep that check on any new restore path.
- SSH enrollment requires an independently verified host-key fingerprint and OPENMANAGE_ENCRYPTION_KEY. Never return stored private keys. Do not add arbitrary remote-command execution; follow the phased controls in plan.md.
- Never put secrets in Git, fixtures, test output, PR text, or audit details. Use generated test keys and fake credentials.
- Treat inventory addresses as untrusted input. Bound network operations with timeouts and cancellation, and document the network targets a feature can reach.
- Keep browser-side secrets out of local storage. Avoid persisting private-key material beyond the enrollment interaction.

## CI and security

- CI is defined in .github/workflows/ci.yml and runs on trusted branch pushes to main using the self-hosted WSL2 runner. It checks gofmt, builds the frontend with `npm ci`, verifies Go module metadata with `go mod tidy -diff`, prepares embedded assets, runs Go tests, builds the Docker image, and runs scripts/smoke-test.sh.
- pr-validation.yml runs on pull requests to main on GitHub-hosted runners only (never the self-hosted runner). It runs gofmt, vet, staticcheck, `go test -race`, `docker compose config`, the Docker build, and the smoke test, with a disposable PostgreSQL. Check its run for the exact PR head before describing a PR as ready.
- Other workflows: codeql.yml and security-checks.yml (Go and frontend dependency scans) run on PRs and main; publish-image.yml publishes the GHCR image on main pushes; openwrt-lab.yml is manual and GitHub-hosted only (it uses the OpenWrt lab scripts).
- Dependency lockfile generation is isolated in `.github/workflows/dependency-lockfiles.yml`: only trusted `main` pushes trigger it; the generator has `contents: read` and no persisted checkout credentials; only the separate publish job has `contents: write`, and it must not install dependencies or run project scripts. Both jobs must use the exact triggering `github.sha`, and the artifact must contain only `go.mod`, `go.sum`, and `web/package-lock.json`.
- The repository is public. Do not enable a pull_request workflow that executes code from untrusted forks on the self-hosted runner. Prefer branch-push validation for repository writers; review any CI trigger or permission change carefully.
- Check the latest workflow run for the exact PR head before describing a PR as ready. Also check post-merge CI for important changes.
- Keep workflow permissions minimal and pin third-party actions to full commit SHAs when practical.

## Useful checks

Run from repository root unless noted:

Frontend: cd web && npm ci && npm run build
Backend: go test ./...; go vet ./...; staticcheck ./... (go install honnef.co/go/tools/cmd/staticcheck@v0.8.2)
PostgreSQL-backed tests run only when OPENMANAGE_TEST_DATABASE_URL is set (PR validation sets it).
Formatting: gofmt -w cmd/openmanage/*.go; gofmt -l cmd/openmanage/*.go
Container: docker build -t openmanage:local .
Browser navigation check: scripts/ui-nav-test.mjs (run by scripts/smoke-test.sh; needs Node and Chromium)
Restore drill: scripts/restore-drill.sh. Lab-only UI checks, run by scripts/openwrt-lab-test.sh: scripts/ui-add-ap-test.mjs and scripts/ui-backups-test.mjs (they need a real device and credentials passed through the environment, never committed).

CI prepares embedded frontend assets before running Go tests and building the Docker image. Do not interpret a standalone local Docker build failure as an application defect until the required generated assets are prepared.

## Documentation discipline

- Use plan.md as the roadmap and update it when milestones change.
- Keep architecture and operational documentation concise and close to the code it describes.
- Do not duplicate large source excerpts in documentation; explain interfaces, invariants, commands, and failure modes.
