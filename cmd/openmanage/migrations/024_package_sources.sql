-- Package sources (docs/firmware-upgrades.md, "Package sources"): package files an
-- administrator registered for installation on devices, such as a LuCI theme that is not
-- in the official feeds. A source starts pending and is usable only once confirmed after
-- the administrator reviewed its metadata and scripts. The stored copy is what gets
-- installed, so upgrades do not depend on the upstream link. Old app versions ignore this
-- table; dropping it loses only the registered files.
CREATE TABLE IF NOT EXISTS package_sources (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    version TEXT NOT NULL,
    arch TEXT NOT NULL,
    format TEXT NOT NULL CHECK (format IN ('ipk', 'apk')),
    sha256 TEXT NOT NULL,
    size BIGINT NOT NULL,
    file BYTEA NOT NULL,
    info JSONB NOT NULL,
    origin TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'confirmed')),
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_by UUID REFERENCES users(id) ON DELETE SET NULL,
    confirmed_at TIMESTAMPTZ
);

-- One confirmed source per file, and one pending copy at a time.
CREATE UNIQUE INDEX IF NOT EXISTS package_sources_sha256_status_idx ON package_sources (sha256, status);
CREATE INDEX IF NOT EXISTS package_sources_name_idx ON package_sources (name, version);
