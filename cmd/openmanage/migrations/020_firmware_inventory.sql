-- Read-only firmware and package inventories (docs/firmware-upgrades.md, Phase 1). The
-- manifest is validated device output with feed credentials removed; it holds no
-- secrets. digest is a plain SHA-256 of the manifest without free space and memory, so
-- an unchanged device updates its newest row instead of storing a copy. The app keeps the newest 20 per device.
-- Old app versions ignore this table. Rolling back the app keeps the rows; dropping the
-- table loses only inventory history, which the next capture rebuilds.
CREATE TABLE IF NOT EXISTS firmware_inventories (
    id BIGSERIAL PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_verified_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    captured_by UUID REFERENCES users(id) ON DELETE SET NULL,
    digest TEXT NOT NULL,
    manifest JSONB NOT NULL
);

CREATE INDEX IF NOT EXISTS firmware_inventories_device_captured_idx
    ON firmware_inventories (device_id, captured_at DESC, id);
