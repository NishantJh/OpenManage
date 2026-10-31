-- Campaign execution controls. Preparing a device only stages an image;
-- every flash remains an individual, typed-name-confirmed action.
ALTER TABLE firmware_rollouts
    ADD COLUMN IF NOT EXISTS current_batch INTEGER NOT NULL DEFAULT 0 CHECK (current_batch >= 0);
ALTER TABLE firmware_rollouts
    ADD COLUMN IF NOT EXISTS max_concurrent_flashes INTEGER NOT NULL DEFAULT 2
        CHECK (max_concurrent_flashes BETWEEN 1 AND 2);
ALTER TABLE firmware_rollouts
    ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ;
ALTER TABLE firmware_rollouts
    ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

CREATE UNIQUE INDEX IF NOT EXISTS firmware_rollout_upgrade_once_idx
    ON firmware_rollout_devices (upgrade_id) WHERE upgrade_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS firmware_rollout_active_idx
    ON firmware_rollouts (status,current_batch) WHERE status IN
        ('awaiting_canary','canary_running','awaiting_batch','batch_running','paused');
