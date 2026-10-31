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
- Desired package sets are per-device, admin-edited through the Firmware page, immutable audited revisions. Staged firmware rollout must follow plan.md Section 5 and docs/firmware-upgrades.md: require recorded physical-hardware upgrade/recovery qualification, immutable campaign preview, one canary, fully successful canary verification before batches, explicit admin confirmation for every device, bounded concurrency, and automatic pause on any warning/failure/ambiguous state. Reuse the existing single-device firmware job; never issue a second flash after flash_sent_at or automatically retry. QEMU/container success is not hardware qualification. Step 5d exposes the current set, official additions, stock-default removals, confirmed custom-source tracking (pinned or latest), and the latest 50 revisions; restore creates a new revision. UI edits are intent-only and never contact devices or install/remove packages. Step 5a stores intent only; Step 5b snapshots the selected revision into each firmware plan, adds only target-feed-available official packages, removes only target-release stock defaults, and resolves custom sources to exact source ids/versions for post-flash reporting. Step 5c adds an administrator-started typed removal job: exact device and package-name confirmation, explicit apply-now acknowledgement, user-installed-only scope, protected-package denylist, fixed apk/opkg commands, audit and result verification. Plan creation and post-flash checks never install or remove packages. Keep package names validated, package-source references typed, and never expose a generic shell or package-manager command.
- SSH enrollment requires an independently verified host-key fingerprint and OPENMANAGE_ENCRYPTION_KEY. Never return stored private keys. Do not add arbitrary remote-command execution; follow the phased controls in plan.md.
- Never put secrets in Git, fixtures, test output, PR text, or audit details. Use generated test keys and fake credentials.
- Treat inventory addresses as untrusted input. Bound network operations with timeouts and cancellation, and document the network targets a feature can reach.
- Keep browser-side secrets out of local storage. Avoid persisting private-key material beyond the enrollment interaction.

## CI and security

- CI is defined in .github/workflows/ci.yml and runs on trusted pushes to main using the persistent Ubuntu self-hosted runner labelled `self-hosted, Linux, X64, openmanage-ci`. It checks gofmt, builds the frontend with `npm ci`, verifies Go module metadata with `go mod tidy -diff`, prepares embedded assets, runs Go tests, builds the Docker image, and runs the full smoke test including browser navigation.
- pr-validation.yml runs on pull requests to main on GitHub-hosted runners only. It runs gofmt, vet, staticcheck, `go test -race`, `docker compose config`, the Docker build, and the smoke test, with a disposable PostgreSQL. Keep all PR workflow execution GitHub-hosted: because this is a public repository, PR authors can modify workflow definitions, so an in-workflow condition is not a safe boundary for the persistent runner. Check the run for the exact PR head before describing a PR as ready.
- CodeQL and security-checks run PR scans on GitHub-hosted runners and trusted main/scheduled scans on the persistent Ubuntu runner. The dependency-lockfile generator and opt-in OpenWrt lab/VM workflows also use that runner; their publication-only jobs remain GitHub-hosted. publish-image.yml publishes the GHCR image on main pushes. See docs/ci-runners.md for persistent-service setup and host prerequisites.
- Dependency lockfile generation is isolated in `.github/workflows/dependency-lockfiles.yml`: only trusted `main` pushes trigger it; the generator has `contents: read` and no persisted checkout credentials; only the separate publish job has `contents: write`, and it must not install dependencies or run project scripts. Both jobs must use the exact triggering `github.sha`, and the artifact must contain only `go.mod`, `go.sum`, and `web/package-lock.json`.
- The repository is public. Do not run pull-request workflow definitions or PR code on the persistent self-hosted runner; PR authors can modify those workflow definitions. Keep PR validation and PR-triggered security scans on GitHub-hosted runners. Self-hosted workflows must execute trusted main-branch code only, and every `workflow_dispatch` job targeting the runner must gate `github.ref == 'refs/heads/main'`. Never use `pull_request_target` to execute PR code.
- Keep self-hosted runner accounts unprivileged. Do not grant blanket passwordless sudo or access to general-purpose network/system commands. The QEMU VM test may use only the root-owned fixed-purpose helper `scripts/openwrt-vm-net-helper.sh`, installed at `/usr/local/sbin/openmanage-vm-net` with exact-command sudoers entries for `setup` and `cleanup`; see `docs/ci-runners.md`. Do not make `/dev/kvm` world-writable.
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
Restore drill: scripts/restore-drill.sh. Lab UI checks: scripts/ui-add-ap-test.mjs and scripts/ui-backups-test.mjs, run by scripts/openwrt-lab-test.sh against a disposable OpenWrt container and stack with throwaway credentials. No real device or secrets are needed.

CI prepares embedded frontend assets before running Go tests and building the Docker image. Do not interpret a standalone local Docker build failure as an application defect until the required generated assets are prepared.

## Documentation discipline

- Use plan.md as the roadmap and update it when milestones change.
- Keep architecture and operational documentation concise and close to the code it describes.
- Do not duplicate large source excerpts in documentation; explain interfaces, invariants, commands, and failure modes.
