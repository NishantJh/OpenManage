# Instructions for coding agents and contributors

Read plan.md before planning work. Use this file as the repository-specific operating guide.

## Workflow

- Never commit directly to main. Create a descriptive feature branch from the latest main, make a focused change, run available checks, and open a PR.
- Before editing, inspect the relevant implementation, tests, migrations, Compose configuration, and current CI workflow. Prefer the smallest change that solves the task.
- Batch related edits before pushing when practical: branch-push CI cancels superseded runs.
- After a PR merge, verify the merge commit and its own CI run, then audit major changes. Do not claim a check passed unless its run for the relevant commit actually passed.
- Do not merge unrelated work together. Include summary, rationale, security considerations, test results, operational impact, and rollback/data-loss notes in PR descriptions.

## Project constraints

- Backend: Go HTTP server, PostgreSQL via pgx, SQL migrations embedded under cmd/openmanage/migrations/.
- Frontend: React + TypeScript under web/; built assets are copied into cmd/openmanage/web/dist/ by CI and embedded in the Go app.
- Deployment: compose.yaml, Dockerfile, .env.example. Keep the single-app + PostgreSQL baseline lightweight.
- Do not add Redis, a separate worker, or another runtime service without a specific requirement and documented trade-off.
- Use additive, restart-safe migrations where possible. Explain how old and new app versions interact with each migration and whether rollback loses data.
- Keep API errors sanitized. Never log credentials, session tokens, private keys, encryption keys, CSRF tokens, or sensitive command output.
- SSH enrollment requires an independently verified host-key fingerprint and OPENMANAGE_ENCRYPTION_KEY. Never return stored private keys. Do not add arbitrary remote-command execution; follow the phased controls in plan.md.
- Never put secrets in Git, fixtures, test output, PR text, or audit details. Use generated test keys and fake credentials.
- Treat inventory addresses as untrusted input. Bound network operations with timeouts and cancellation, and document the network targets a feature can reach.
- Keep browser-side secrets out of local storage. Avoid persisting private-key material beyond the enrollment interaction.

## CI and security

- CI is defined in .github/workflows/ci.yml and runs on trusted branch pushes using the self-hosted WSL2 runner. It currently builds the frontend, prepares embedded assets, resolves Go dependencies, runs Go tests, and builds the Docker image.
- The repository is public. Do not enable a pull_request workflow that executes code from untrusted forks on the self-hosted runner. Prefer branch-push validation for repository writers; review any CI trigger or permission change carefully.
- Check the latest workflow run for the exact PR head before describing a PR as ready. Also check post-merge CI for important changes.
- Keep workflow permissions minimal and pin third-party actions to full commit SHAs when practical.

## Useful checks

Run from repository root unless noted:

Frontend: cd web && npm ci && npm run build
Backend: go test ./...
Formatting: gofmt -w cmd/openmanage/*.go; gofmt -l cmd/openmanage/*.go
Container: docker build -t openmanage:local .

CI prepares embedded frontend assets before running Go tests and building the Docker image. Do not interpret a standalone local Docker build failure as an application defect until the required generated assets are prepared.

## Documentation discipline

- Use plan.md as the roadmap and update it when milestones change.
- Keep architecture and operational documentation concise and close to the code it describes.
- Do not duplicate large source excerpts in documentation; explain interfaces, invariants, commands, and failure modes.

## Multi-chat coordination

- The project coordinator is the integration point for cross-chat planning, task allocation, dependency ordering, risk tracking, and consolidated status. A chat's claim is a report, not repository evidence; verify consequential state against GitHub, diffs, and CI.
- Before starting work, read `plan.md`, inspect open PRs and active branches relevant to the task, and check for overlapping file ownership. Do not duplicate an active task without an explicit handoff.
- Every task must have one accountable owner, a narrow scope, acceptance criteria, dependencies, and a concrete deliverable. Report blockers early.
- Each working chat must provide a handover with: task/status; branch and PR; files changed; implementation summary; tests and exact results; dependencies/conflicts; decisions needed; next action; and whether `plan.md` / `AGENTS.md` changed.
- Treat repository state and merged commits as authoritative. Distinguish implemented, reviewed, merged, CI-verified, and runtime-validated; these are separate states.
- The coordinator maintains the consolidated task ledger in `plan.md` and this operating policy in `AGENTS.md`. Update both when priorities, ownership, dependencies, or coordination rules materially change. Do not mark work complete based only on a chat's assertion.
- Never merge PRs, close/discard PRs, rewrite shared branches, modify production configuration, or make destructive changes without the user's explicit approval. Preserve existing ownership boundaries and never overwrite another chat's in-progress work.
- Do not claim direct control of other ChatGPT chats. Assign work through explicit handover messages and reconcile their returned status reports.
- When tasks overlap, pause the conflicting edit, compare branch diffs and intended outcomes, then recommend one canonical implementation. Close or supersede duplicate PRs only after confirming their unique changes are preserved and obtaining required approval.
- Keep test evidence reproducible. Record the exact command, target commit/ref, result, and relevant CI run URL. A passing check on an earlier commit is not evidence for the current head.

