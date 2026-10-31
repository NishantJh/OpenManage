-- Explicit, audited package removals. Package-manager output is never stored because
-- package removal scripts may run as root and could print sensitive information.
CREATE TABLE IF NOT EXISTS package_removals (
    id BIGSERIAL PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'done', 'failed')),
    step TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    checks JSONB NOT NULL DEFAULT '[]',
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS package_removals_one_running_per_device
    ON package_removals (device_id) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS package_removals_device_idx ON package_removals (device_id, created_at DESC);
