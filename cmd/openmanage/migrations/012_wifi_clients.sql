-- Wi-Fi client tracking for the device location map (docs/device-location-map.md).
-- Opt-in and off by default. Additive; an older app version ignores these tables, and
-- rolling back loses only the tracked client state and roam history.
ALTER TABLE monitoring_settings
    ADD COLUMN IF NOT EXISTS client_tracking_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS client_poll_interval_seconds INTEGER NOT NULL DEFAULT 300
        CHECK (client_poll_interval_seconds BETWEEN 60 AND 600);

-- One row per client. client_key is a keyed hash of the MAC, so the table reveals
-- nothing without OPENMANAGE_ENCRYPTION_KEY; the MAC itself is stored encrypted.
CREATE TABLE IF NOT EXISTS wifi_clients (
    client_key BYTEA PRIMARY KEY,
    mac_ciphertext BYTEA NOT NULL,
    mac_hint TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    hidden BOOLEAN NOT NULL DEFAULT FALSE,
    first_seen TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Where the client is now, or was last: the AP it is (or was) associated with.
CREATE TABLE IF NOT EXISTS wifi_client_state (
    client_key BYTEA PRIMARY KEY REFERENCES wifi_clients(client_key) ON DELETE CASCADE,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    interface TEXT NOT NULL,
    signal_dbm INTEGER,
    connected BOOLEAN NOT NULL,
    connected_since TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Association history for roaming tests: connect, roam and disconnect events.
CREATE TABLE IF NOT EXISTS wifi_client_events (
    id BIGSERIAL PRIMARY KEY,
    client_key BYTEA NOT NULL REFERENCES wifi_clients(client_key) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('connect','roam','disconnect')),
    from_device_id UUID REFERENCES devices(id) ON DELETE SET NULL,
    to_device_id UUID REFERENCES devices(id) ON DELETE SET NULL,
    signal_dbm INTEGER,
    at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS wifi_client_events_client_idx ON wifi_client_events (client_key, at DESC);
CREATE INDEX IF NOT EXISTS wifi_client_events_at_idx ON wifi_client_events (at);
