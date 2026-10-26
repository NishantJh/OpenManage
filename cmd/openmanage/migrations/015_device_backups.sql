-- Device configuration backups (`sysupgrade -b` archives) for any enrolled OpenWrt
-- device, router or access point. The archive holds every secret on the device, so it
-- is stored only as AES-256-GCM ciphertext bound to the device and backup id (AAD
-- openmanage:firmware-backup:v1:<device_id>:<id>). key_version lets key rotation find
-- and re-encrypt rows. digest is keyed and covers file names and contents, not archive
-- timestamps, so an unchanged device is recognised without decrypting.
-- Old app versions ignore this table. Rolling back the app keeps the rows; dropping the
-- table loses the stored backups.
CREATE TABLE IF NOT EXISTS device_backups (
    id UUID PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_verified_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    captured_by UUID REFERENCES users(id) ON DELETE SET NULL,
    scheduled BOOLEAN NOT NULL DEFAULT FALSE,
    pinned BOOLEAN NOT NULL DEFAULT FALSE,
    digest TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes > 0),
    file_count INTEGER NOT NULL CHECK (file_count >= 0),
    board_name TEXT NOT NULL DEFAULT '',
    target TEXT NOT NULL DEFAULT '',
    release TEXT NOT NULL DEFAULT '',
    hardware_id TEXT NOT NULL DEFAULT '',
    hostkey_fingerprint TEXT NOT NULL DEFAULT '',
    key_version SMALLINT NOT NULL DEFAULT 1,
    ciphertext BYTEA NOT NULL
);

CREATE INDEX IF NOT EXISTS device_backups_device_captured_idx
    ON device_backups (device_id, captured_at DESC, id);

-- Scheduled backups are opt-in and at most daily.
ALTER TABLE monitoring_settings
    ADD COLUMN IF NOT EXISTS backups_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS backup_interval_seconds INTEGER NOT NULL DEFAULT 86400
        CHECK (backup_interval_seconds BETWEEN 86400 AND 2592000);
