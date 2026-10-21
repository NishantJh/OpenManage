# Agent A Handover — Wi-Fi Layout PR #26

Updated: 2026-10-21T10:10:00Z

Owner / workstream: Agent A — Wi-Fi Layout PR #26 validation only

Message type: REPORT

Status: in-progress — static review findings recorded; implementation fixes not started

PR URL / branch:
- Target PR: https://github.com/NishantJh/OpenManage/pull/26
- PR branch observed during review: `feat/wifi-layout-rssi-foundation`
- This handover branch: `agent-a/pr26-validation-handover`
- Handover commit will be linked in the branch history; no PR was opened for this handover.
- Target PR head SHA observed: `b8942906b8d5e47e5b7723b951c275a894accbce`
- Target PR base SHA observed: `6e6371b785eddf23e22adc3aa924965315df469d`
- Re-verify current head/base before acting; values above are a dated snapshot.

## Verified evidence

- GitHub PR metadata for #26: open, draft, mergeable at inspection time; 30 commits, 10 files, +754/-4.
- PR Validation workflow run: https://github.com/NishantJh/OpenManage/actions/runs/39578019533 — success.
- Security Checks workflow run: https://github.com/NishantJh/OpenManage/actions/runs/39578019978 — success.
- CodeQL workflow run: https://github.com/NishantJh/OpenManage/actions/runs/39578019576 — success.
- PR Validation steps reported successful: Go formatting, frontend build, Go tests with race detector, Compose validation, Docker image build, disposable smoke test.
- Security jobs reported successful: Go vulnerability scan and frontend dependency audit.
- CodeQL Go and JavaScript/TypeScript jobs reported successful.
- Evidence reflects the observed PR head at the time of the review. CI must be checked again against the current head before any readiness decision.
- No local shell commands or tests were run in this validation session. No production APs or router configurations were accessed.

## Changes

- No application source changes by this agent.
- Canonical detailed handover created at `handoffs/pr-26-wifi-layout.md`.
- Earlier detailed note retained at `docs/handover-pr26-validation.md`: https://github.com/NishantJh/OpenManage/blob/agent-a/pr26-validation-handover/docs/handover-pr26-validation.md
- PR #30 establishes this path as Agent A's canonical workstream handover: https://github.com/NishantJh/OpenManage/pull/30 (open and unmerged when checked).
- Handover branch: https://github.com/NishantJh/OpenManage/tree/agent-a/pr26-validation-handover

## Findings and risks

**HIGH — deadline can allow partial observations to be persisted.** In `cmd/openmanage/wifi_coverage.go`, the interface loop breaks on context error but does not clearly reject the expired context before persistence/success. Individual interface scan errors are skipped. Fix deadline checks before parsing/persistence and define explicit partial-result behavior.

**HIGH — output truncation can look like a complete scan.** `runWiFiCommand` uses bounded buffers, but call sites do not visibly reject truncated output. Make truncation observable; fail affected commands and never parse/persist truncated results. Test stdout and stderr size boundaries.

**MEDIUM — layout validation does not explicitly reject non-finite float values.** Add explicit NaN/Inf checks for dimensions and room/wall/AP coordinates and tests confirming rejected saves do not overwrite stored layout.

**MEDIUM — observation field validity and audit coverage.** Parser accepts BSSID plus parseable signal; channel can remain zero if not parsed. Failed scan audit handling is not visibly consistent across later command errors, timeouts, empty results, and persistence failures. Define mandatory/unknown fields and consistently audit sanitized failure outcomes.

**MEDIUM — interfaces and skipped failures can be unclear.** Commands are fixed-form and interface names are regex-checked, but interface list is silently capped at four and individual errors are skipped. Keep fixed command allowlist; test invalid names/failures and clearly communicate the cap/skipped interfaces.

Controls observed in source: session auth; admin/operator authorization; CSRF cookie/header equality for mutations; transactions for layout+audit and observations+success audit; private-device-IP check before SSH (helper not fully verified); pinned SHA-256 SSH host key; request body and object-count limits; database RSSI/channel constraints. These are source observations, not complete proof of end-to-end security.

Additional risks: shared Home layout is last-write-wins under concurrent edits; stored layout reads are unmarshalled without revalidation. UI represents AP-to-neighbour RSSI observations, not a calibrated building-wide coverage heatmap.

Files inspected:
- `cmd/openmanage/wifi_coverage.go`
- `cmd/openmanage/wifi_coverage_test.go`
- `cmd/openmanage/migrations/007_wifi_coverage.sql`
- `cmd/openmanage/main.go`
- `web/src/WifiLayout.tsx`
- `web/src/App.tsx`
- `web/src/styles.css`
- `README.md`, `plan.md`, `AGENTS.md`
- `.github/workflows/pr-validation.yml`, `.github/workflows/security-checks.yml`, `.github/workflows/codeql.yml`

## Blockers / dependencies

No implementation authorization recorded in this handover. The coordinator should check overlap with active PRs before authorizing changes to the Wi-Fi implementation or tests. No coordinator inbox edit made.

## Coordinator decision requested

Please confirm whether Agent A should proceed with a focused fix on a dedicated branch, initially limited to `cmd/openmanage/wifi_coverage.go` and `cmd/openmanage/wifi_coverage_test.go`, addressing timeout/partial-scan handling, truncated output, and finite layout values. Optional UI/documentation/migration edits should be limited to what the confirmed behavior contract requires.

## Next action

Coordinator: reconcile the current PR #26 head/base and latest CI, check active PR file overlap, then authorize or decline the proposed focused fix. Agent A: do not change application code until that scope is confirmed.

## Limitations / unverified assumptions

- This is a static review of source and reported GitHub workflow results, not a local or lab reproduction.
- Private-IP helper behavior, all runtime SSH failure paths, output truncation, timeout rollback behavior, and UI handling of partial scans still require targeted tests.
- No production-device testing is authorized.
