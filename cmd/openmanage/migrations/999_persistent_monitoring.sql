CREATE TABLE IF NOT EXISTS monitoring_settings (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id = TRUE),
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    interval_seconds INTEGER NOT NULL DEFAULT 300 CHECK (interval_seconds BETWEEN 60 AND 3600),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO monitoring_settings (id, enabled, interval_seconds)
VALUES (TRUE, FALSE, 300)
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS monitoring_checks (
    id BIGSERIAL PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('online', 'offline', 'degraded', 'error')),
    reason TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    duration_ms BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS monitoring_checks_device_finished_idx
    ON monitoring_checks (device_id, finished_at DESC);
CREATE INDEX IF NOT EXISTS monitoring_checks_finished_idx
    ON monitoring_checks (finished_at DESC);
