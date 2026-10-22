-- Read-only configuration snapshots (`uci export`). The text can hold Wi-Fi keys and
-- other secrets, so it is stored only as AES-256-GCM ciphertext bound to the device.
-- digest is a keyed HMAC of the plaintext, so drift is detected without decrypting.
CREATE TABLE IF NOT EXISTS config_snapshots (
    id BIGSERIAL PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_verified_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    captured_by UUID REFERENCES users(id) ON DELETE SET NULL,
    digest TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    option_count INTEGER NOT NULL CHECK (option_count >= 0),
    ciphertext BYTEA NOT NULL
);

CREATE INDEX IF NOT EXISTS config_snapshots_device_captured_idx
    ON config_snapshots (device_id, captured_at DESC, id DESC);
