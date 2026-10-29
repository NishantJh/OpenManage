-- Package installs (docs/firmware-upgrades.md, "Installing a package source"): one row per
-- request to install a confirmed package source on a device, with the checks that ran.
-- The source is kept by id for reference and by name, version and hash for the record, so
-- deleting a source later does not erase what was installed. Old app versions ignore this
-- table; dropping it loses only the install history.
CREATE TABLE IF NOT EXISTS package_installs (
    id BIGSERIAL PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    source_id BIGINT REFERENCES package_sources(id) ON DELETE SET NULL,
    name TEXT NOT NULL,
    version TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'done', 'failed')),
    step TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    checks JSONB NOT NULL DEFAULT '[]',
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ
);

-- At most one install runs on a device at a time.
CREATE UNIQUE INDEX IF NOT EXISTS package_installs_one_running_per_device
    ON package_installs (device_id) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS package_installs_device_idx ON package_installs (device_id, created_at DESC);
