-- Physical-hardware qualification evidence for staged firmware rollouts.
-- Records are immutable; only current, exact hardware/release matches can satisfy
-- the campaign gate. VM/container records remain visible but never qualify.
CREATE TABLE IF NOT EXISTS firmware_hardware_qualifications (
    id BIGSERIAL PRIMARY KEY,
    board_name TEXT NOT NULL CHECK (length(trim(board_name)) BETWEEN 1 AND 128),
    model TEXT NOT NULL CHECK (length(trim(model)) BETWEEN 1 AND 128),
    source_release TEXT NOT NULL,
    target_release TEXT NOT NULL,
    tested_on DATE NOT NULL,
    physical_hardware BOOLEAN NOT NULL DEFAULT FALSE,
    upgrade_checks TEXT[] NOT NULL DEFAULT '{}'::text[],
    upgrade_outcome TEXT NOT NULL CHECK (upgrade_outcome IN ('passed','failed','not_run')),
    recovery_method TEXT NOT NULL CHECK (recovery_method IN ('failsafe','factory_reset','serial_console','tftp','dual_image','not_exercised')),
    recovery_outcome TEXT NOT NULL CHECK (recovery_outcome IN ('passed','failed','not_run')),
    evidence_reference TEXT NOT NULL DEFAULT '',
    notes TEXT NOT NULL DEFAULT '',
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (source_release <> target_release)
);

CREATE INDEX IF NOT EXISTS firmware_hardware_qualification_match_idx
    ON firmware_hardware_qualifications
       (board_name, model, source_release, target_release, tested_on DESC, id DESC);

CREATE TABLE IF NOT EXISTS firmware_rollout_qualifications (
    rollout_id BIGINT NOT NULL REFERENCES firmware_rollouts(id) ON DELETE CASCADE,
    qualification_id BIGINT NOT NULL REFERENCES firmware_hardware_qualifications(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (rollout_id, qualification_id)
);
