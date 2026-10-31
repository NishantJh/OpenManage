# HTTP API

All routes are under `/api`. Every route except `GET /api/healthz` and `POST /api/auth/login` needs a signed-in session. Login sets an HttpOnly session cookie that expires after 12 hours, and state-changing requests need the CSRF token. Roles are admin, operator and read-only. Routes marked "Admin" are administrator-only; for the rest, each handler checks which roles it allows.

## Health and sessions

- `GET /api/healthz` — application and database health.
- `POST /api/auth/login`, `POST /api/auth/logout`, `GET /api/auth/me` — sign in, sign out, and read the signed-in user. Login attempts are rate-limited.
- `POST /api/auth/password` — changes the caller's own password (JSON `current_password`, `new_password`, 12–72 bytes). Other sessions of that user are signed out; wrong current passwords count towards the login rate limit.
- `GET /api/audit` — the latest 100 audit events. Admin.

## Users

- `GET /api/users`, `POST /api/users` — list users; create one from JSON `username`, `role` (`admin`, `operator`, `readonly`) and `password`. Admin.
- `PATCH /api/users/{id}` — changes `role` and/or `disabled`. Either change signs the user out. The last active administrator cannot be demoted or disabled. Users are disabled rather than deleted so the audit trail keeps its names. Admin.
- `POST /api/users/{id}/password` — sets a user's password and signs them out. Admin.

## Devices and inventory

- `GET /api/devices` — lists up to 500 inventory records. Each record includes `enrollable` (true when the address is a private IP literal that SSH enrollment accepts) and `config_drift` (true while the device has configuration drift no administrator has acknowledged).
- `POST /api/devices` — adds a record with JSON fields `name`, `address`, `model`, `firmware`. The address is inventory metadata until enrollment is completed.
- `GET /api/devices/{id}`, `DELETE /api/devices/{id}` — read or delete one record. Create and delete are recorded in the audit trail.
- `PATCH /api/devices/{id}` — sets the device's role. This only changes how OpenManage names Wi-Fi clients; nothing is written to the device. Admin or operator.

## Enrollment and the Add device wizard

- `POST /api/devices/{id}/enroll` — verifies a pinned SSH host key and public-key login, then stores the management private key encrypted at rest. Request JSON: `username`, `private_key`, and `expected_fingerprint` (the `SHA256:...` fingerprint must be verified independently). Errors include a stable `code`: `encryption_key_missing`, `encryption_key_invalid`, `invalid_request`, `device_not_found`, `address_not_private`, `invalid_private_key`, `device_unreachable`, `host_key_mismatch`, `ssh_auth_failed`, `key_not_verified`, `server_error`. Enrollment accepts private IP literals on TCP/22 only (no DNS names, public or link-local addresses). Re-enrollment replaces the stored credential only after the new key authenticates; it does not change `authorized_keys` on the device.
- `POST /api/device-bootstrap/probe` — reads a device's SSH host-key fingerprint for the wizard.
- `POST /api/device-bootstrap` — takes `name`, `address`, `role`, `username`, `password` and `expected_fingerprint` (or `device_id` to set up an inventory device that has no key yet). It signs in once with the password, installs a generated key, verifies key login and stores the device. The password is never stored, logged or audited. This is the only path that installs a key on a device.
- `POST /api/devices/{id}/radius-secret` — generates or rotates an AP's RADIUS shared secret, shown once. `DELETE` on the same path removes it. Admin.

## Health and monitoring

- `POST /api/devices/{id}/health-check` — runs a fixed set of read-only diagnostics for an enrolled device.
- `GET /api/monitoring/settings` — reads the persistent monitoring settings.
- `PUT /api/monitoring/settings` — enables or disables monitoring and sets an interval from 60 to 3600 seconds. Optional `snapshots_enabled` and `snapshot_interval_seconds` (900 to 86400) turn on scheduled configuration snapshots, independently of health monitoring; omitted fields keep their current values. Admin.
- `GET /api/monitoring/history?limit=50` — recent persisted monitoring outcomes (limit 1–100). Monitoring history is kept for 30 days.

## Configuration snapshots and drift

- `POST /api/devices/{id}/config-snapshots` — reads the device's `uci export` and stores it encrypted at rest. Returns `first`, `changed` (drift since the previous snapshot) and `change_count`. An unchanged configuration only refreshes `last_verified_at`. The last 100 snapshots per device are kept. Admin.
- `GET /api/devices/{id}/config-snapshots` — lists snapshot metadata, newest first. Admin.
- `GET /api/devices/{id}/config-snapshots/{snapshot}/changes` — lists UCI paths added, removed or changed since the previous snapshot. Values of secret-looking options (keys, passwords, secrets, tokens) are redacted, and the raw configuration is never returned. Admin.
- `POST /api/devices/{id}/config-snapshots/{snapshot}/acknowledge` — marks drift up to and including this snapshot as reviewed, so the device no longer reports `config_drift`. Admin.

## Per-device settings

- `GET /api/config-schema` — returns the LuCI-like settings menu and curated field metadata used by the device settings view. The `editable` flag is true only for administrators and fields allowed by the current controlled-change policy. Any signed-in role.
- `GET /api/devices/{id}/settings` — reads the live UCI export from an enrolled device over its pinned SSH connection and returns package/section/option data. Values of secret-looking options are omitted and marked `secret`; raw export text is never returned or stored by this endpoint. Each package includes a keyed HMAC `version` token for stale-view detection. Any signed-in role. The request contacts the selected device but makes no configuration changes.

## Controlled changes

- `GET /api/config-settings` — lists the settings OpenManage can change (currently `system.hostname`). Admin.
- `POST /api/devices/{id}/config-changes` — previews a change from JSON `setting` and `value`. Reads the current value and changes nothing. A preview is valid for 10 minutes. Admin.
- `POST /api/devices/{id}/config-changes/{change}/apply` — applies a preview through rpcd with `uci apply` rollback armed, verifies the new value over a fresh SSH connection, then confirms. The result's `status` is `applied`, `rolled_back` (verification failed; the old value is restored) or `failed` (nothing was applied, for example because the value changed since the preview or another apply is pending). Admin.
- `GET /api/devices/{id}/config-changes` — the last 50 changes for a device. Admin.
- `POST /api/devices/{id}/config-change-batches` — previews 1–12 curated non-secret settings across at most four UCI sections. The JSON body contains `base_versions` (the complete package-to-version map from the loaded settings response), `changes` (unique `{setting,value,unlock_existing?}` objects), and `acknowledge_disruption`. Existing SSIDs require `unlock_existing: true` for that field; this is recorded in audit metadata. The server rejects a stale page, unavailable setting, invalid value, secret/management field, or a disruptive change without explicit acknowledgement. Preview is metadata-only on the device and expires after 10 minutes.
- `POST /api/devices/{id}/config-change-batches/{change}/apply` — takes an encrypted baseline configuration snapshot, stages every UCI value in one rpcd session, applies once with the 60-second rollback armed, verifies all changed values over a fresh SSH connection, then confirms. A mismatch or reconnect/confirmation failure triggers rollback; an ambiguous outcome is not automatically retried. Admin.
- `GET /api/devices/{id}/config-change-batches` — latest 50 batch previews and outcomes, including only curated non-secret before/after values. Admin.

Passphrases and other secret fields are not writable through batches because change history and audit storage are not yet encrypted for secret values. Management-access fields and raw UCI edits also remain locked. The batch columns are additive, so older app versions ignore them; downgrading does not delete preview rows, though an older UI will show batch entries without their item details and cannot apply them. Dropping the new columns during a manual schema rollback would delete batch details and previews.

The disposable OpenWrt container lab can verify UCI batching and rpcd rollback behavior, but it has no radio; it does not qualify real Wi-Fi client reconnection or physical AP behavior. Keep disruptive edits explicitly acknowledged and treat representative-device qualification as an operational prerequisite before broad rollout.

## Device backups and restore

Backups are `sysupgrade -b` archives, encrypted at rest and bound to the device. The last 5 are kept per device, and the backup taken before a firmware flash is pinned.

- `POST /api/devices/{id}/backups` — captures a backup.
- `GET /api/devices/{id}/backups`, `DELETE /api/devices/{id}/backups/{backup}` — list backups and delete one.
- `POST /api/devices/{id}/backups/{backup}/download` — returns the decrypted archive to an administrator who has just re-entered their password. It is a POST so the password is never in a URL. Admin.
- `POST /api/devices/{id}/backups/{backup}/restore` — restores an OpenManage-captured archive to the same device and board. It needs the administrator's password and the device name typed as confirmation. Lockout checks run first, and the device reboots in the background. Admin.
- `GET /api/devices/{id}/restores` — restore history.

## Firmware

- `POST /api/devices/{id}/firmware-inventory` — reads the device's release, packages, custom feeds (credentials stripped), sysupgrade keep list and free space. Nothing is written to the device.
- `GET /api/devices/{id}/firmware-inventory` — the newest stored inventory, or `null`. Any signed-in role.
- `GET /api/devices/{id}/firmware-plan?version=` — the upgrade plan for one release. Needs a stored inventory and the build server turned on. Admin or operator.
- `GET /api/firmware/releases` — every release the build server offers, newest first. Admin or operator.
- `GET /api/firmware/settings` — the build server setting. Any signed-in role. `PUT /api/firmware/settings` changes it (the public build server is the default; a self-hosted one is optional). Admin.
- `POST /api/devices/{id}/firmware-upgrades` — starts preparing an upgrade: it builds the image, checks its hash, takes and pins a backup, stages the image in `/tmp` on the device and runs `sysupgrade -T`. It returns at once and the job runs on. Nothing is flashed yet.
- `GET /api/devices/{id}/firmware-upgrades` — the newest upgrade jobs for a device. Admin or operator.
- `POST /api/devices/{id}/firmware-upgrades/{job}/cancel` — stops a job that is preparing or ready. Nothing has been flashed at either point.
- `POST /api/package-sources/{id}/check-update` — asks GitHub whether a confirmed, GitHub-release package source has a newer release and returns `up_to_date` or `newer` with the link. Downloads and registers nothing. Admin.

## Wi-Fi users and RADIUS

Wi-Fi (network) users are separate from OpenManage logins. See [Built-in RADIUS](radius-design.md).

- `GET /api/network-users`, `POST /api/network-users` — list and create Wi-Fi users. Admin. `PATCH /api/network-users/{id}` and `POST /api/network-users/{id}/password` change one.
- `GET /api/radius` — listener and certificate status. Admin.
- `PUT /api/radius/settings` — saves the OpenManage LAN address APs send RADIUS requests to, so AP setup commands are filled in for every device. Admin.
- `GET /api/radius/ca.pem` — downloads the RADIUS CA. `GET /api/radius/apple-profile` — generates the Apple Wi-Fi setup profile. Both admin.

## Floor plan and Wi-Fi clients

- `GET /api/floor-plan`, `PUT /api/floor-plan` — read or save the floor-plan layout (rooms and AP placement). `PUT` is admin or operator.
- `GET /api/floor-plan/image`, `PUT /api/floor-plan/image`, `DELETE /api/floor-plan/image` — the background picture. `PUT` is admin or operator.
- `GET /api/wifi-clients` — connected and recently seen clients, by room of the AP they are on. Tracking is opt-in.
- `GET /api/wifi-clients/status` — each AP's latest poll outcome, plus any AP that is never polled because it has no SSH key. Held in memory, so it is empty until the first poll.
- `PATCH /api/wifi-clients/{id}` — sets a label or hides a client. Admin or operator.
- `DELETE /api/wifi-clients` — deletes every tracked client, its last location and its history. Admin.
- `GET /api/wifi-client-events` — association history (connect, roam, disconnect with RSSI), newest first.
