-- Controlled configuration changes. A change is previewed first (the device's current
-- value is read and stored), then applied by an administrator with an rpcd timed
-- rollback, verified over a fresh SSH connection, and only then confirmed.
CREATE TABLE IF NOT EXISTS config_changes (
    id BIGSERIAL PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    setting TEXT NOT NULL,
    old_value TEXT NOT NULL,
    new_value TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('previewed', 'applying', 'applied', 'rolled_back', 'failed', 'expired')),
    detail TEXT NOT NULL DEFAULT '',
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    applied_by UUID REFERENCES users(id) ON DELETE SET NULL,
    finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS config_changes_device_created_idx
    ON config_changes (device_id, created_at DESC, id DESC);
-- At most one change per device may be applying at a time.
CREATE UNIQUE INDEX IF NOT EXISTS config_changes_one_applying_idx
    ON config_changes (device_id) WHERE status = 'applying';
