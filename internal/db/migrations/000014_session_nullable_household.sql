-- 000014: session.household_id nullable (D28).
--
-- A user who has no current household (fresh account that hasn't joined
-- anything yet, or a default household that was deleted) still gets a
-- session: the row carries household_id = NULL and the dashboard shows
-- the "create a household / join with code" recovery. The switch and
-- join flows PATCH the session row instead of recreating it.

-- +goose Up

ALTER TABLE session ALTER COLUMN household_id DROP NOT NULL;

-- +goose Down

ALTER TABLE session ALTER COLUMN household_id SET NOT NULL;
