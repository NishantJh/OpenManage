-- Flashing and verifying a prepared firmware upgrade (docs/firmware-upgrades.md). Additive:
-- flash_sent_at is set just before the flash command is sent, so a restarted server knows
-- the device may have flashed; result holds the post-reboot checks; acknowledged_at is
-- when a person reviewed the outcome, which is when the pinned backup is released.
-- Old app versions ignore the new columns.
ALTER TABLE firmware_upgrades
    ADD COLUMN IF NOT EXISTS flash_sent_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS result JSONB,
    ADD COLUMN IF NOT EXISTS acknowledged_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS acknowledged_by UUID REFERENCES users(id) ON DELETE SET NULL;
