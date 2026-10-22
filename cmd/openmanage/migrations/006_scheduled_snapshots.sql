-- Scheduled configuration snapshots are opt-in and run from the monitoring loop.
ALTER TABLE monitoring_settings
    ADD COLUMN IF NOT EXISTS snapshots_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS snapshot_interval_seconds INTEGER NOT NULL DEFAULT 3600
        CHECK (snapshot_interval_seconds BETWEEN 900 AND 86400);

-- drift marks a snapshot whose configuration differed from the one before it. It
-- counts towards the dashboard until an administrator acknowledges it.
ALTER TABLE config_snapshots
    ADD COLUMN IF NOT EXISTS drift BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS acknowledged_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS acknowledged_by UUID REFERENCES users(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS config_snapshots_unacknowledged_drift_idx
    ON config_snapshots (device_id) WHERE drift AND acknowledged_at IS NULL;
