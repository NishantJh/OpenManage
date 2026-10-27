-- The OpenManage LAN address that APs send RADIUS requests to, entered once and used to fill
-- in the AP setup commands for every device. One row. Additive: an older app version ignores
-- the table and rolling back only loses the saved address.
CREATE TABLE IF NOT EXISTS radius_settings (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    server_address TEXT NOT NULL,
    updated_by UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
