-- 000020: D35 — budget envelope model (ADR-0012, wave doc §D / §H).
--
-- A subcategory's monthly budget has a mode:
--   standalone     (default): its own allocation; it adds on to the parent's
--                            number.
--   part_of_parent (a "slice"): carved out of the parent's envelope; it does
--                             NOT increase the parent's total.
-- The mode is meaningful only on a subcategory (parent_id IS NOT NULL); a
-- top-level category is always its own envelope.
--
-- No CHECK constraint: the two values are validated app-side (standalone |
-- part_of_parent), matching the D32 app-side-validation convention.
-- No backfill: existing rows default to 'standalone', which is exactly today's
-- behaviour (a sub's budget already adds on to the parent).
--
-- The effective total is computed at display time in the budget roll-up path
-- and is never written back into the parent's budget_per_month, which keeps
-- the envelope the user typed.

-- +goose Up

ALTER TABLE category ADD COLUMN budget_mode TEXT NOT NULL DEFAULT 'standalone';

-- +goose Down

-- Dropping the column discards each sub's mode; the roll-up falls back to the
-- pre-000020 rule (every sub standalone, so its budget adds on). Restoring
-- data is the backup's job (house convention).
ALTER TABLE category DROP COLUMN budget_mode;
