-- Optional background image for the Wi-Fi floor plan (a scanned or drawn plan). One row; the
-- bytes are only ever served back as PNG or JPEG. Additive: an older app version ignores the
-- table and rolling back only loses the uploaded image, not the rooms or AP positions.
CREATE TABLE IF NOT EXISTS floor_plan_image (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    content_type TEXT NOT NULL CHECK (content_type IN ('image/png', 'image/jpeg')),
    data BYTEA NOT NULL,
    width_px INTEGER NOT NULL CHECK (width_px > 0),
    height_px INTEGER NOT NULL CHECK (height_px > 0),
    updated_by UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
