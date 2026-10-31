-- Staged firmware rollout drafts. Step 6a stores immutable membership and
-- snapshots of already-captured device inventory/package-set intent. Drafts do
-- not contact devices, call the build server, prepare images, or flash firmware.
-- Later rollout stages use these rows to persist plans and execution state.
CREATE TABLE IF NOT EXISTS firmware_rollouts (
    id BIGSERIAL PRIMARY KEY,
    version TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('draft','previewed','awaiting_canary','canary_running',
        'awaiting_batch','batch_running','paused','complete','cancelled','needs_attention')),
    canary_device_id UUID NOT NULL,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    pause_reason TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS firmware_rollouts_created_idx
    ON firmware_rollouts (created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS firmware_rollout_devices (
    rollout_id BIGINT NOT NULL REFERENCES firmware_rollouts(id) ON DELETE CASCADE,
    -- Keep the original ID/name snapshot if the managed device is later deleted.
    device_id UUID REFERENCES devices(id) ON DELETE SET NULL,
    device_id_snapshot UUID NOT NULL,
    device_name TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    current_release TEXT NOT NULL,
    board_name TEXT NOT NULL DEFAULT '',
    target TEXT NOT NULL DEFAULT '',
    inventory_id BIGINT NOT NULL,
    inventory_digest TEXT NOT NULL,
    inventory_manifest JSONB NOT NULL,
    inventory_verified_at TIMESTAMPTZ NOT NULL,
    desired_package_set_revision INTEGER NOT NULL DEFAULT 0 CHECK (desired_package_set_revision >= 0),
    batch_number INTEGER NOT NULL CHECK (batch_number >= 0),
    position INTEGER NOT NULL CHECK (position >= 0),
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','preparing','ready','flashing','verifying',
        'done','done_with_warnings','needs_attention','stopped','skipped')),
    plan JSONB,
    plan_digest TEXT NOT NULL DEFAULT '',
    upgrade_id BIGINT REFERENCES firmware_upgrades(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (rollout_id, device_id_snapshot),
    UNIQUE (rollout_id, batch_number, position)
);

CREATE UNIQUE INDEX IF NOT EXISTS firmware_rollout_one_canary_idx
    ON firmware_rollout_devices (rollout_id) WHERE batch_number = 0;
CREATE INDEX IF NOT EXISTS firmware_rollout_devices_device_idx
    ON firmware_rollout_devices (device_id_snapshot, created_at DESC);
