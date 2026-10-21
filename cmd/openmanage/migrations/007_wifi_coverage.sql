CREATE TABLE IF NOT EXISTS wifi_layouts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    layout JSONB NOT NULL,
    updated_by UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS wifi_radio_observations (
    id BIGSERIAL PRIMARY KEY,
    source_device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    source_radio TEXT NOT NULL,
    source_bssid TEXT NOT NULL DEFAULT '',
    observed_bssid TEXT NOT NULL,
    ssid TEXT NOT NULL DEFAULT '',
    channel INTEGER NOT NULL DEFAULT 0 CHECK (channel >= 0 AND channel <= 1000),
    signal_dbm INTEGER NOT NULL CHECK (signal_dbm >= -120 AND signal_dbm <= 0),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS wifi_radio_observations_recent_idx
    ON wifi_radio_observations (observed_at DESC);
CREATE INDEX IF NOT EXISTS wifi_radio_observations_source_idx
    ON wifi_radio_observations (source_device_id, observed_at DESC);
CREATE INDEX IF NOT EXISTS wifi_radio_observations_bssid_idx
    ON wifi_radio_observations (observed_bssid, observed_at DESC);
