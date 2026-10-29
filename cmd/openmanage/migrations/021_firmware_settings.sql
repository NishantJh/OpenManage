-- Firmware build server settings, one row. The build server (ASU) is contacted only
-- when an administrator turns it on. Additive: an older app version ignores the table,
-- and rolling back loses only these two settings.
CREATE TABLE IF NOT EXISTS firmware_settings (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    build_server_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    build_server_url TEXT NOT NULL DEFAULT 'https://sysupgrade.openwrt.org',
    updated_by UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
