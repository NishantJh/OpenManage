-- Disabled users keep their row so audit history stays attributed; they cannot log in
-- and their sessions are revoked when they are disabled.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS password_changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions (user_id);
