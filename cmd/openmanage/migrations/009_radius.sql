-- Built-in RADIUS (docs/radius-design.md). Network users are Wi-Fi identities, kept
-- apart from the users table so a Wi-Fi account can never sign in to OpenManage.
CREATE TABLE IF NOT EXISTS network_users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    disabled_at TIMESTAMPTZ,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    password_changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One RADIUS client per device. The shared secret is AES-256-GCM encrypted with the
-- device ID as associated data, so a row copied to another device does not decrypt.
CREATE TABLE IF NOT EXISTS radius_clients (
    device_id UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    secret_ciphertext BYTEA NOT NULL,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rotated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
