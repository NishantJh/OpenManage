-- Batch previews reuse config_changes for the device-level applying lock and history.
-- Values are restricted to non-secret curated fields; secret changes require a
-- separate encrypted persistence path before they may be enabled.
ALTER TABLE config_changes
    ADD COLUMN IF NOT EXISTS batch_changes JSONB,
    ADD COLUMN IF NOT EXISTS base_versions JSONB,
    ADD COLUMN IF NOT EXISTS disruption_acknowledged BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN config_changes.batch_changes IS
    'JSON array of curated non-secret UCI changes for a settings batch; null for legacy single-setting changes';
COMMENT ON COLUMN config_changes.base_versions IS
    'Keyed package-version tokens captured from the settings view when a batch was previewed';
