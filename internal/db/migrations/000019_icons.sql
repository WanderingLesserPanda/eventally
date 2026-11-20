-- 000019: D32 — category and account icons (ADR-0009,
-- docs/notes/d32-icon-picker.md).
--
-- The stored value is a glyph key from the code-managed registry
-- (internal/web/icons.go, Line Awesome 2.1.0): the glyph's class name,
-- e.g. 'la-coffee'. NULL means "unpicked — render the default".
--
-- No CHECK constraint: keys are validated app-side against the registry,
-- so expanding the set stays a pure file edit (the point of the
-- code-managed registry — a CHECK would force DDL per expansion).
-- No backfill: existing rows stay NULL and render with defaults on
-- day one.

-- +goose Up

ALTER TABLE category ADD COLUMN icon TEXT;
ALTER TABLE account  ADD COLUMN icon TEXT;

-- +goose Down

-- Down restores the pre-000019 shape; restoring data is the backup's job
-- (house convention).
ALTER TABLE category DROP COLUMN icon;
ALTER TABLE account  DROP COLUMN icon;
