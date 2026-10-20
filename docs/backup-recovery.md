# Backup and recovery

OpenManage's PostgreSQL database and `OPENMANAGE_ENCRYPTION_KEY` are a coupled recovery set. The database contains SSH private keys encrypted with this key. A database backup without the matching key cannot recover those credentials; a key without the database does not recover inventory or audit history.

## Protect the encryption key

- Generate a 32-byte key with `openssl rand -base64 32` before enrolling devices.
- Store the exact value of `OPENMANAGE_ENCRYPTION_KEY` in an encrypted password manager or secrets vault, and keep a separately protected recovery copy.
- Do not commit `.env`, print the key into logs, attach it to issues, or store it beside an unencrypted database dump.
- Do not replace the key casually. Existing encrypted credentials require the original key unless a deliberate, tested credential re-encryption/rotation workflow is implemented.

If the key is lost, OpenManage cannot decrypt the SSH credentials stored in the database. Re-enrolling devices with new credentials may be required.

## Create a database backup

Run from the directory containing `compose.yaml`. Ensure the destination is on a protected disk with sufficient free space. This example creates a PostgreSQL custom-format dump and restricts local file permissions:

```sh
umask 077
mkdir -p backups
docker compose stop app
docker compose exec -T db pg_dump -U openmanage -d openmanage --format=custom > "backups/openmanage-$(date +%F-%H%M%S).dump"
docker compose start app
```

If the dump command fails, do not treat the resulting file as a valid backup. Check the command's exit status and logs, and rerun it. Store backup files encrypted and off the host; a backup on the same disk is not disaster recovery.

Keep a record of which application version the dump came from, and preserve the matching encryption key in a separate secure location. Test backups periodically by restoring to an isolated environment.

## Restore a database backup

**This replaces data in the target OpenManage database.** Confirm the target is the intended instance and preserve a current backup before proceeding.

1. Stop the app so it cannot write while recovery is in progress:

   ```sh
   docker compose stop app
   ```

2. Restore the custom-format dump into the existing database. `--clean --if-exists` removes existing objects represented in the archive before recreating them:

   ```sh
   docker compose exec -T db pg_restore --clean --if-exists --no-owner --no-privileges -U openmanage -d openmanage < backups/your-backup.dump
   ```

3. Ensure the original matching `OPENMANAGE_ENCRYPTION_KEY` is present in the app's environment. If the key was stored in `.env`, restore it from the secure copy; do not generate a replacement key for an existing database.
4. Start the app and inspect startup logs and `GET /api/healthz`:

   ```sh
   docker compose start app
   docker compose logs --tail=100 app
   ```

5. Log in and verify inventory and audit history. In a controlled test, verify that an enrolled device's stored credential can still authenticate. Never test recovery against production devices until the restored instance is confirmed healthy.

If the database restore fails, keep the app stopped until the cause is understood. Do not run a partial or unverified restore against the only copy of the data.

## Recovery limitations

- This procedure covers the PostgreSQL logical backup and the encryption key, not router configuration backups or the router-side SSH authorized-key state.
- A dump does not back up Compose configuration, the application image, reverse-proxy/TLS configuration, or the key stored outside PostgreSQL. Preserve deployment configuration separately, excluding secrets from source control.
- Validate the process against the PostgreSQL version used by the deployment and test recovery on an isolated stack before relying on it for disaster recovery.
- OpenManage is early-development software. Do not treat this procedure as a substitute for tested, off-host, encrypted backups and a documented recovery exercise.
