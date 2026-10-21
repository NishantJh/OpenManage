# PR #21 — FreeRADIUS Lab Validation Handover

**Reviewed:** 2026-10-21  
**PR:** https://github.com/NishantJh/OpenManage/pull/21  
**Scope:** Optional FreeRADIUS lab only. No merge performed.

## Revision inspected

- PR state at review: open, draft.
- Head branch: `feat/mvp-radius-lab`
- Head SHA: `242798348e9b867c9d5b14fc557e24e1eb6e607f`
- Base branch: `main`
- Base SHA: `efa48f16fb5f4673b7c330010ade69f43425d5d5`
- Files changed by the PR: 7.
- Review submissions and inline review threads: none found during this inspection.

Recheck the current PR head and checks before acting; these are the exact revisions observed during this review, not a guarantee of the current live state.

## CI evidence observed

The following GitHub Actions runs associated with the inspected head were completed successfully:

- [PR Validation, run 39464867506](https://github.com/NishantJh/OpenManage/actions/runs/39464867506): all listed steps completed successfully. Included formatting, frontend build, embedded assets preparation, `go mod tidy`, `go test -race ./...`, `docker compose config --quiet`, Docker image build, disposable application smoke test, and optional RADIUS test.
- [Security Checks, run 39464867415](https://github.com/NishantJh/OpenManage/actions/runs/39464867415): Go vulnerability scan and frontend dependency audit both succeeded.
- [CodeQL, run 39464867665](https://github.com/NishantJh/OpenManage/actions/runs/39464867665): Go and JavaScript/TypeScript analysis succeeded.

The RADIUS test command in CI was:

```bash
bash scripts/test-radius.sh
```

The recorded job log shows the image being pulled, the `openmanage-radius-lab` network and RADIUS container being created and started, followed by:

```
RADIUS smoke test passed: lab identity received Access-Accept.
```

This is observed CI evidence. This agent did not execute a local Docker test, and the log does not independently prove the network/container were absent after the EXIT cleanup ran.

## Isolation and security findings

- The default `compose.yaml` contains only `app` and `db`; it does not start RADIUS. The PR does not modify that default Compose file.
- The optional `compose.radius.yaml` publishes UDP authentication/accounting ports to loopback by default: `127.0.0.1:1812` and `127.0.0.1:1813`.
- The optional lab uses a dedicated Docker network named `openmanage-radius-lab`, with the server at `172.30.99.2` and disposable test client at `172.30.99.3`.
- `testuser` / `testpass` and shared secret `testing123` are documented as lab-only and must not be used as production credentials.
- The README accurately limits the demonstrated behavior to PAP. A PAP Access-Accept does **not** validate EAP, PEAP, certificate lifecycle, or production WPA-Enterprise readiness.
- CI output observed for the positive test logs the Access-Accept result and does not print the sample password/shared secret. Failure diagnostics print `radtest` output and FreeRADIUS logs; keep these diagnostic paths free of secrets if the lab configuration evolves.

Loopback publishing protects the host-published ports by default, but does not make the container inherently unreachable to other containers attached to its Docker network. Keep the lab network isolated.

## Minimum recommended hardening

1. **Strengthen the smoke-test assertion.** In `scripts/test-radius.sh`, `radtest` is run under `|| true`, and success is determined by a substring search for `Access-Accept`. Check the actual client result and match the expected response robustly. Add a negative test that confirms a wrong password receives Access-Reject.
2. **Remove the fixed readiness sleep.** Replace `sleep 2` with a bounded readiness/retry loop so slow startup is not confused with a service failure.
3. **Improve cleanup verification.** The script has an EXIT trap that runs Compose down and removes the lab network, with cleanup errors ignored. Consider asserting that the disposable container and network are gone after the test, and ensure cleanup remains reliable on failures.
4. **Image reproducibility.** The FreeRADIUS image uses the explicit `3.2.10-alpine` tag. Pinning its digest would further improve reproducibility; no image failure was observed in this CI run.

These are test-hardening recommendations, not reported CI failures. The existing positive PAP smoke test did pass in the recorded run.

## Coordination notes

The PR changes these files:

- `.github/workflows/pr-validation.yml`
- `compose.radius.yaml`
- `plan.md`
- `radius/README.md`
- `radius/authorize`
- `radius/clients.conf`
- `scripts/test-radius.sh`

`plan.md` and `.github/workflows/pr-validation.yml` are shared integration points. Coordinate with the project coordinator before editing either. Keep any remediation narrowly scoped to PR #21 and avoid dependency lockfiles, Wi-Fi Layout PR #26, and unrelated application features.

## Recommended next action

Have the PR owner/coordinator review the four hardening items, make only the agreed lab-scoped changes, and rerun `bash scripts/test-radius.sh` plus the full PR Validation workflow in CI. Reconfirm loopback defaults and default-stack isolation after any Compose edits. Do not merge until the coordinator makes that decision.

No files were changed by the validation review itself other than adding this handover document. No production APs were configured, and no RADIUS service was exposed to the LAN or Internet during the review.
