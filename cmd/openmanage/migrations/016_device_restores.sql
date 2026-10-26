-- Restores of a stored device backup. A restore uploads the archive, runs
-- `sysupgrade -r`, reboots the device and verifies the management path afterwards, so
-- it outlives the API request that started it. Only one restore may run per device.
-- backup_id is not a foreign key: the record of what was restored outlives the backup.
-- The row never holds archive content. Old app versions ignore this table; dropping it
-- loses restore history only.
CREATE TABLE IF NOT EXISTS device_restores (
    id BIGSERIAL PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    backup_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('running', 'done', 'failed', 'interrupted')),
    step TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    release_mismatch_accepted BOOLEAN NOT NULL DEFAULT FALSE,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS device_restores_device_created_idx
    ON device_restores (device_id, created_at DESC, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS device_restores_one_running_idx
    ON device_restores (device_id) WHERE status = 'running';
