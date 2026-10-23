-- Wi-Fi floor plan: one document with rooms and the position of each AP. Additive;
-- an older app version ignores the table. Rolling back only loses the drawn layout.
CREATE TABLE IF NOT EXISTS floor_plan (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    layout JSONB NOT NULL DEFAULT '{"rooms":[],"placements":[]}',
    updated_by UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO floor_plan (id) VALUES (TRUE) ON CONFLICT DO NOTHING;
