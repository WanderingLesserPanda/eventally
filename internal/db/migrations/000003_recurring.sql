-- +goose Up
-- 000003_recurring: align the P1 scaffold's recurring_template (D11) with
-- how P7 uses it. The scaffold declared it with placeholder shapes
-- (JSONB split_spec default '[]', nullable account, last_generated) that
-- no code ever wrote to; P7 stores split_spec as a ';'-separated TEXT list
-- of participant keys ("user:N;cp:N"), requires an account, and records
-- generation per month in last_generated_date.
--
-- Every database reaching this migration has an EMPTY table (P7 is the
-- first code to write to it), so the reshaping is data-safe.

ALTER TABLE recurring_template ALTER COLUMN split_spec DROP DEFAULT;
ALTER TABLE recurring_template ALTER COLUMN split_spec TYPE TEXT;
ALTER TABLE recurring_template ALTER COLUMN split_spec SET DEFAULT '';
ALTER TABLE recurring_template ALTER COLUMN account_id SET NOT NULL;
ALTER TABLE recurring_template ADD CONSTRAINT recurring_template_amount_check CHECK (amount > 0);
ALTER TABLE recurring_template RENAME COLUMN last_generated TO last_generated_date;

-- +goose Down
ALTER TABLE recurring_template RENAME COLUMN last_generated_date TO last_generated;
ALTER TABLE recurring_template ALTER COLUMN amount DROP CONSTRAINT recurring_template_amount_check;
ALTER TABLE recurring_template ALTER COLUMN account_id DROP NOT NULL;
ALTER TABLE recurring_template ALTER COLUMN split_spec DROP DEFAULT;
ALTER TABLE recurring_template ALTER COLUMN split_spec TYPE JSONB USING '[]'::jsonb;
ALTER TABLE recurring_template ALTER COLUMN split_spec SET DEFAULT '[]'::jsonb;
