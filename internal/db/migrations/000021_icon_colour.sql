-- 000021: D36 — icon colours for categories and accounts (ADR-0013,
-- docs/notes/d36-icon-colour-palette.md).
--
-- The stored value is a free #rrggbb in its normalised form: a 6-hex string
-- WITHOUT the "#", e.g. 'b84810'. NULL means "no tint — the glyph inherits
-- the surrounding text colour".
--
-- No CHECK constraint: the hex is validated app-side (normaliseColour +
-- colourValid in internal/web/icons.go), so a curated preset and a hand-typed
-- hex are stored identically and the value space never changes the schema
-- (ADR-0013, extending ADR-0009's no-registry philosophy).
-- No backfill: existing rows stay NULL and render with the inherited text
-- colour on day one (zero visual drift).

-- +goose Up

ALTER TABLE category ADD COLUMN colour TEXT;
ALTER TABLE account  ADD COLUMN colour TEXT;

-- +goose Down

-- Down restores the pre-000021 shape; restoring data is the backup's job
-- (house convention).
ALTER TABLE category DROP COLUMN colour;
ALTER TABLE account  DROP COLUMN colour;
