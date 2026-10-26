-- How many unpinned backups to keep per device. Pinned backups are never counted or
-- pruned. A lower value takes effect the next time a backup is stored.
ALTER TABLE monitoring_settings
    ADD COLUMN IF NOT EXISTS backup_retention INTEGER NOT NULL DEFAULT 5
        CHECK (backup_retention BETWEEN 1 AND 20);
