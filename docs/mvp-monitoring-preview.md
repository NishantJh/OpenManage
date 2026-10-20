# MVP monitoring preview

This branch adds an opt-in browser-session monitoring preview to the dashboard.

## Behaviour

- Monitoring is off by default and must be enabled by an authenticated user.
- The browser schedules a read-only health-check cycle every five minutes while the dashboard remains open.
- Each cycle checks at most five devices that are not marked `unknown`, sequentially. It does not run concurrent SSH checks.
- The existing authenticated, CSRF-protected health-check endpoint and server-defined command allowlist are used.
- `Check now` runs one cycle immediately. The UI reports the cycle summary and local last-run time.
- A failed HTTP 502 health-check response marks that device offline in the current UI session. Other non-success responses are counted as skipped rather than treated as proof that the device is offline.
- The browser stops the timer when the user disables monitoring or leaves the page. Device list refreshes do not reset the five-minute schedule, and an in-progress cycle blocks overlapping cycles.

## Known limitations — not production monitoring

- This is a first-run UI slice, not durable monitoring. There is no server-side scheduler, persisted check history, alert delivery, or monitoring configuration stored in PostgreSQL.
- The monitor only considers the first five eligible devices in the inventory per cycle. Devices marked `unknown` are skipped; a device with missing credentials may be skipped by the endpoint.
- Results for failed checks are not persisted as a monitoring record. The offline status produced by a 502 response is a UI-session state and may be overwritten on refresh.
- Monitoring depends on the authenticated browser session staying active. Closing the tab stops monitoring.
- The health endpoint has its own timeout and audit behaviour. Do not enable this against a large fleet or treat this preview as a substitute for persistent availability monitoring.

## Safe test checklist

1. Start a disposable OpenManage stack using the repository's documented quick start or `bash scripts/smoke-test.sh`.
2. Sign in and add an inventory-only device. Confirm it is shown as `Not enrolled` and is skipped by automatic monitoring.
3. Use a disposable OpenWrt test VM/device to exercise SSH enrollment; independently verify the host-key fingerprint. Never use a live AP for negative credential tests.
4. With the test device enrolled, select **Check now** and confirm the read-only diagnostics modal still works.
5. Enable monitoring and confirm the UI indicates that the first automatic cycle will run after five minutes. Do not wait for a scheduled cycle as the only validation; use **Check now** for the immediate path.
6. Test an unreachable disposable target and verify that it is reported as an error/offline only for a 502 response; verify that authentication/permission failures pause monitoring rather than silently marking devices offline.
7. Disable monitoring and confirm no further automatic cycles occur. Close/reopen the page and verify monitoring is off by default.
8. Run the full PR validation workflow and inspect the result for the exact PR head before considering the branch ready.

## Rollback

Disable monitoring in the UI. To remove the feature from the code, revert this PR. No database migration, extra service, or router configuration change is required.
