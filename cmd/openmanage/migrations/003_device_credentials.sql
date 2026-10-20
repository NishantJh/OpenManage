CREATE TABLE IF NOT EXISTS device_credentials (
    device_id UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    username TEXT NOT NULL,
    private_key_ciphertext BYTEA NOT NULL,
    host_key_fingerprint TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
