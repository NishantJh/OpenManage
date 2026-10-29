-- Firmware upgrade jobs (docs/firmware-upgrades.md, step 3). A job prepares an image
-- (build, hash check, pinned backup, staged on the device, sysupgrade -T) and later
-- flashes it. The image itself is never stored here, only its name, size and hash.
-- At most one open job per device. Old app versions ignore this table; dropping it
-- loses only job history.
CREATE TABLE IF NOT EXISTS firmware_upgrades (
    id BIGSERIAL PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    version TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('preparing', 'ready', 'stopped', 'flashing', 'verifying',
        'done', 'done_with_warnings', 'needs_attention')),
    step TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    plan JSONB,
    image_name TEXT NOT NULL DEFAULT '',
    image_sha256 TEXT NOT NULL DEFAULT '',
    image_size BIGINT NOT NULL DEFAULT 0,
    backup_id UUID REFERENCES device_backups(id) ON DELETE SET NULL,
    backup_pinned_by_job BOOLEAN NOT NULL DEFAULT FALSE,
    boot_id TEXT NOT NULL DEFAULT '',
    inventory_digest TEXT NOT NULL DEFAULT '',
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ready_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS firmware_upgrades_one_open_per_device
    ON firmware_upgrades (device_id)
    WHERE status IN ('preparing', 'ready', 'flashing', 'verifying');

CREATE INDEX IF NOT EXISTS firmware_upgrades_device_created_idx
    ON firmware_upgrades (device_id, created_at DESC, id);
