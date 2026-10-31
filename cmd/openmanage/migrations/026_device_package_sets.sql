-- Desired package sets are immutable, per-device revisions. This migration only stores
-- intent; it does not run package operations or change any device.
CREATE TABLE IF NOT EXISTS device_package_sets (
    id BIGSERIAL PRIMARY KEY,
    device_id UUID NOT NULL UNIQUE REFERENCES devices(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS device_package_set_revisions (
    id BIGSERIAL PRIMARY KEY,
    package_set_id BIGINT NOT NULL REFERENCES device_package_sets(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL CHECK (revision > 0),
    official_packages TEXT[] NOT NULL DEFAULT '{}',
    removed_defaults TEXT[] NOT NULL DEFAULT '{}',
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    based_on_revision INTEGER,
    UNIQUE (package_set_id, revision)
);

ALTER TABLE device_package_sets
    ADD COLUMN IF NOT EXISTS current_revision_id BIGINT
    REFERENCES device_package_set_revisions(id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS device_package_set_revision_sources (
    revision_id BIGINT NOT NULL REFERENCES device_package_set_revisions(id) ON DELETE CASCADE,
    package_name TEXT NOT NULL,
    tracking_mode TEXT NOT NULL CHECK (tracking_mode IN ('pinned', 'latest')),
    source_id BIGINT REFERENCES package_sources(id) ON DELETE RESTRICT,
    source_version TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (revision_id, package_name),
    CHECK (
        (tracking_mode = 'pinned' AND source_id IS NOT NULL AND source_version <> '') OR
        (tracking_mode = 'latest' AND source_id IS NULL AND source_version = '')
    )
);

CREATE INDEX IF NOT EXISTS device_package_set_revisions_history_idx
    ON device_package_set_revisions (package_set_id, revision DESC);
CREATE INDEX IF NOT EXISTS device_package_set_revision_sources_source_idx
    ON device_package_set_revision_sources (source_id) WHERE source_id IS NOT NULL;
