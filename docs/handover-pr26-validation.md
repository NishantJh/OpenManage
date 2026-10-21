# Coordinator Handover — Wi-Fi Layout PR #26 Validation

Date: 2026-10-21
Agent: Wi-Fi Layout PR #26 validation (Agent A)
PR: https://github.com/NishantJh/OpenManage/pull/26

## Status

**Recommendation: keep PR #26 in draft pending focused fixes and targeted regression tests. Do not merge yet.**

This is a static review of the PR head and the reported GitHub CI runs. No repository source files were changed as part of the validation review, no local tests were run in this agent session, and no production APs or router configurations were accessed.

## Repository state verified

- PR state: open, draft, reported mergeable by GitHub metadata.
- Base branch: `main`
- Base SHA at inspection: `6e6371b785eddf23e22adc3aa924965315df469d`
- Head branch: `feat/wifi-layout-rssi-foundation`
- Head SHA at inspection: `b8942906b8d5e47e5b7723b951c275a894accbce`
- PR reports 30 commits, 10 changed files, 754 additions and 4 deletions.

Re-check the PR head and CI before acting on this handover; these identifiers describe the state observed on 2026-10-21 and may have changed.

## CI results at the inspected head

All three workflow runs were completed with conclusion `success`:

| Workflow | Run ID | Result |
| --- | ---: | --- |
| PR Validation | 39578019533 | success |
| Security Checks | 39578019978 | success |
| CodeQL | 39578019576 | success |

Verified completed PR Validation steps: Go formatting, frontend build, Go package tests with race detector, Compose configuration validation, Docker image build, and disposable end-to-end smoke test.

Verified completed Security Checks jobs: Go vulnerability scan and frontend dependency audit.

Verified completed CodeQL jobs: Go and JavaScript/TypeScript analysis.

These workflow results are evidence of the configured CI passing for the observed head; they do not prove the Wi-Fi scan failure modes below are safe. The combined commit-status endpoint returned no legacy statuses; the workflow runs and job records above are the evidence used.

## Files inspected

- `cmd/openmanage/wifi_coverage.go` — layout API, validation, scan flow, SSH setup, command runner, parser, audit failure helper.
- `cmd/openmanage/wifi_coverage_test.go` — parser and layout validation unit tests.
- `cmd/openmanage/migrations/007_wifi_coverage.sql` — layout/observation schema and database constraints.
- `cmd/openmanage/main.go` — route registration and authentication/CSRF middleware context.
- `web/src/WifiLayout.tsx` — layout persistence and manual sequential scan UI.
- `web/src/App.tsx` — route/UI integration in the PR diff.
- `web/src/styles.css` — Wi-Fi layout styling in the PR diff.
- `README.md`, `plan.md`, `AGENTS.md` — documented API behavior, roadmap and contributor guidance.
- `.github/workflows/pr-validation.yml`, `.github/workflows/security-checks.yml`, `.github/workflows/codeql.yml` — CI definitions.

PR changed-file list: `AGENTS.md`, `README.md`, `cmd/openmanage/main.go`, `cmd/openmanage/migrations/007_wifi_coverage.sql`, `cmd/openmanage/wifi_coverage.go`, `cmd/openmanage/wifi_coverage_test.go`, `plan.md`, `web/src/App.tsx`, `web/src/WifiLayout.tsx`, `web/src/styles.css`.

## Findings

### High — scan deadline can allow partial observations to be persisted

In `cmd/openmanage/wifi_coverage.go`, the interface loop breaks when `ctx.Err()` is non-nil, but the code does not immediately reject an expired context before proceeding to the persistence path. Observations gathered earlier may therefore be saved after the overall deadline expires. Individual interface scan errors are also skipped, so the API may return success based on only a subset of interfaces.

Minimum fix:
- Check and return on context deadline/cancellation before parsing/persistence and before declaring success.
- Define the partial-success contract explicitly. If partial success is allowed for a failed interface, report each skipped interface and distinguish partial from complete scans; do not represent a timeout as a successful complete scan.
- Ensure failed scans do not alter stored observation state. Preserve transaction atomicity for inserts, retention pruning and audit events.

### High — truncated command output is not visibly distinguishable from complete output

`runWiFiCommand` directs stdout and stderr into bounded buffers. The inspected call sites do not check an explicit truncation result before parsing stdout. Truncated scan output could be interpreted as valid but incomplete measurements.

Minimum fix:
- Make the bounded buffer expose whether a write exceeded its limit.
- Fail the affected command and do not parse or persist truncated output.
- Confirm both stdout and stderr memory use remains bounded and test the limit boundary.

### Medium — layout validation does not explicitly reject non-finite floats

`validateWiFiLayout` applies numeric range comparisons but does not explicitly reject NaN or infinities for dimensions and coordinates.

Minimum fix:
- Reject non-finite dimensions and every room, wall and AP coordinate before persistence.
- Add validation tests and verify rejected saves leave the previous stored layout unchanged.

### Medium — incomplete measurement fields and failure audit coverage

The parser accepts an observation when it finds a BSSID and a parseable signal. Channel parsing is optional and can leave channel at zero. Failure audit calls are visible for connection and SSH setup failures, but not consistently for every later command, timeout, empty-result or persistence failure path.

Minimum fix:
- Decide and document which fields are mandatory versus explicitly unknown.
- Do not store malformed/incomplete measurements as complete readings.
- Audit failed scan outcomes consistently without recording secrets or raw sensitive command output.

### Medium — interface/command behavior needs explicit regression coverage

The visible command forms are fixed (`iw dev`, `iwinfo <interface> info`, `iwinfo <interface> scan`) and interface names are checked with a restrictive regex. No command injection was demonstrated in static inspection. However, the code silently truncates the interface list to four and skips individual command failures.

Minimum fix:
- Keep the fixed command set; never accept a remote command from the API.
- Test interface-name rejection and command failures.
- Report the four-interface limit and skipped interfaces clearly so results are not misleading.

## Controls observed in source

- Session authentication is applied by API middleware.
- Layout writes and scans require `admin` or `operator`.
- Mutating API requests are checked for CSRF cookie/header equality by middleware.
- Layout update plus audit event and observation inserts plus success audit are grouped in transactions.
- Scan code calls `isPrivateDeviceIP(address)` before SSH. The helper's complete implementation was not verified during this review, so private-IP behavior still needs a targeted test.
- SSH host-key callback compares the presented SHA-256 fingerprint to the stored fingerprint.
- Commands are fixed-form and interface count is capped at four.
- Observation schema constrains signal to -120..0 dBm and channel to 0..1000.
- Layout request bodies are capped at 64 KiB, unknown JSON fields are rejected, extra JSON values are rejected, and room/wall/AP counts are capped.

These are source-level observations, not a claim that all controls have passed end-to-end security tests.

## Other correctness considerations

- The shared `Home` layout uses last-write-wins updates. Concurrent operators can overwrite each other's changes. Consider optimistic concurrency via a version or update timestamp, or document this as a known limitation if concurrency is outside this PR's acceptance criteria.
- Layout reads unmarshal stored JSON without re-running current layout validation. Consider safe handling for stored records that violate current invariants.
- The UI describes the observations as measured AP-to-neighbour RSSI at the AP location, not a calibrated building-wide coverage heatmap. Keep that distinction in the API and documentation.

## Proposed minimum scope and file overlap

No source changes have been made by this agent. Proposed focused implementation scope:

1. `cmd/openmanage/wifi_coverage.go` — deadline and output-truncation handling, measurement validation, failure audit consistency, explicit partial-scan semantics, finite coordinate validation.
2. `cmd/openmanage/wifi_coverage_test.go` — regression tests for timeout, partial results, output limits, invalid interfaces, malformed measurements, and non-finite coordinates.
3. `web/src/WifiLayout.tsx` — only if needed to present explicit partial/failure outcomes from the API.
4. `cmd/openmanage/migrations/007_wifi_coverage.sql` — only if a database constraint is needed to enforce the selected measurement contract.
5. Documentation files only if the behavior/acceptance criteria change.

Avoid unrelated changes to `main.go`, styling, dependency lockfiles, FreeRADIUS, or other roadmap items unless a test demonstrates a specific defect. The PR's changed files above should be checked against other active PRs before edits. Preserve PR #26 ownership; use a dedicated validation branch for any implementation work, and do not merge.

## Commands run / evidence

No shell commands or local test commands were run in this validation session.

GitHub connector operations used to inspect repository state and evidence:
- PR metadata, changed-file list and PR diff for #26.
- Workflow runs and job-step results for the three run IDs above.
- Source fetches for the Wi-Fi implementation, tests, migration, route/middleware context, UI and CI definitions.
- PR comment lookup returned no comments at the time checked.

## Recommended next actions for coordinator

1. Re-check current PR head/base and latest CI because the values above are time-specific.
2. Check active PR file overlap before implementation.
3. Approve a focused fix on a dedicated branch, initially limited to `wifi_coverage.go` and `wifi_coverage_test.go`.
4. Test with a disposable mock SSH endpoint/router emulator and disposable database; never use production APs.
5. Verify wrong host key, private-IP rejection, authorization/CSRF denial, timeout, command nonzero exit, output truncation, invalid interface names, malformed scan output, and transactional rollback on failures.
6. Run targeted tests, then the full configured CI on the exact candidate SHA.
7. Keep PR #26 draft until the above findings are resolved and evidence is recorded. Do not merge without coordinator approval.
