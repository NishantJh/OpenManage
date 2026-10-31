-- Rollout controller state and bounded per-campaign flash concurrency.
ALTER TABLE firmware_rollouts
    ADD COLUMN IF NOT EXISTS current_batch INTEGER NOT NULL DEFAULT 0 CHECK (current_batch >= 0),
    ADD COLUMN IF NOT EXISTS max_concurrent_flashes SMALLINT NOT NULL DEFAULT 2 CHECK (max_concurrent_flashes BETWEEN 1 AND 2),
    ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS firmware_rollout_devices_batch_status_idx
    ON firmware_rollout_devices (rollout_id, batch_number, status);

CREATE UNIQUE INDEX IF NOT EXISTS firmware_rollout_devices_upgrade_unique_idx
    ON firmware_rollout_devices (upgrade_id) WHERE upgrade_id IS NOT NULL;
