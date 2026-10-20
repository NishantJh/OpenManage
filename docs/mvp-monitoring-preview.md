# Persistent monitoring (MVP)

OpenManage now has an opt-in server-side monitoring scheduler. It runs inside the existing Go application process; PostgreSQL stores the settings and per-device check history. No Redis, extra container, or browser timer is required. PostgreSQL remains the source of truth; monitoring settings are a single small row and are cheap to read, so a Redis cache would add invalidation and operational failure modes without a demonstrated performance benefit.

## Why Redis is not included yet

Redis is not currently used as a cache or queue. Monitoring settings are one small PostgreSQL row, device credentials are security-sensitive and should not be copied into a cache, and recent history is durable product data that belongs in PostgreSQL. A cache would introduce invalidation rules and another failure dependency without evidence that these reads are a bottleneck.

The current design instead uses a small bounded worker pool, a modestly tuned PostgreSQL connection pool, indexed history queries, and a PostgreSQL advisory lock for scheduler coordination. Reconsider Redis only after measurements show a concrete need—for example, a distributed job queue for a large fleet or sustained scheduling throughput that cannot be met by the current bounded workers.

## Behaviour

- Monitoring is disabled by default. An administrator enables or disables it from the dashboard; the setting is stored in PostgreSQL and survives browser closure and application-container restarts.
- The configured interval is between 60 seconds and 60 minutes; the dashboard currently uses a five-minute default.
- The scheduler polls settings every 15 seconds and processes at most five due, enrolled devices per batch, with up to three checks running concurrently. Each device check has a 20-second SSH/command deadline. A PostgreSQL advisory lock prevents duplicate batches if multiple app instances overlap. Large fleets will therefore take multiple batches to cycle through all devices.
- Checks use the stored SSH credential and pinned host-key fingerprint and run only the existing server-defined, read-only OpenWrt health commands. No arbitrary command is accepted from the UI.
- Results are stored in PostgreSQL with device, status, sanitized reason, start/end timestamps, and duration. History is retained for 30 days.
- TCP/22 connection failures mark the device offline. Successful SSH with one or more failed diagnostics marks it degraded. Credential decryption, SSH authentication, or pinned host-key failures are recorded as errors and do not falsely mark the device offline.
- Monitoring is independent of an authenticated browser session. Configuration changes are admin-only; authenticated users can view settings/history.

## API

- `GET /api/monitoring/settings` — read the persisted enabled flag and interval.
- `PUT /api/monitoring/settings` — administrator-only; accepts `{"enabled":true,"interval_seconds":300}`. Interval must be 60–3600 seconds and requests require normal session and CSRF protection.
- `GET /api/monitoring/history?limit=50` — latest persisted check outcomes, with a limit of 1–100.

The migration `004_monitoring.sql` creates the settings and history tables and inserts the disabled-by-default settings row. Existing device inventory and credentials are unchanged.

## Resource and safety limits

- One scheduler in the existing Go process; at most five devices are selected per batch and up to three checks run concurrently. PostgreSQL advisory locking prevents two instances from running the same batch.
- The PostgreSQL connection pool is explicitly sized to leave capacity for API requests while monitoring workers run. Credentials and health-check output are not cached in Redis or another external cache.
- The application checks only enrolled devices with stored credentials and only private IP addresses on TCP/22.
- No automatic configuration changes, alerts, or arbitrary shell commands.
- History is pruned after 30 days. Database backups now also preserve monitoring settings and history.
- Monitoring cannot run while the OpenManage application itself is stopped. On restart, persisted settings are loaded and due checks resume.

## Validation

The disposable-stack smoke test verifies the disabled-by-default setting, interval validation, enabling/disabling, history endpoint, and persistence across an application-container restart. CI must pass on the exact PR head. A disposable OpenWrt VM/device is still required to validate real SSH reachability, pinned host-key handling, and health command outcomes before enabling this against any production AP.

## Rollback

Disable monitoring in the UI. Reverting the feature code does not delete monitoring history automatically. Do not drop the new tables unless you intentionally want to remove persisted monitoring settings and check history.
