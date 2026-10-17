-- +goose Up
-- 004_card_family: P8 import support.
--
-- D19: one-level parent-child on accounts so the PrviMiles family
-- (parent "UOB PRVI Miles Elite Card" + Principal / Supplementary
-- physical cards) can roll up at the parent in the card views. The
-- one-level restriction is enforced in the app layer (account CRUD),
-- not in the DB, so the import can be fixed without a migration.
-- D18: a counterparty may optionally be linked to a user (household
-- members are users AND counterparties; external parties are not).

ALTER TABLE account
    ADD COLUMN parent_id BIGINT REFERENCES account(id) ON DELETE SET NULL;
CREATE INDEX idx_account_parent ON account (parent_id);

ALTER TABLE counterparty
    ADD COLUMN user_id BIGINT REFERENCES "user"(id) ON DELETE SET NULL;
CREATE INDEX idx_counterparty_user ON counterparty (user_id);

-- +goose Down

DROP INDEX IF EXISTS idx_counterparty_user;
ALTER TABLE counterparty DROP COLUMN IF EXISTS user_id;
DROP INDEX IF EXISTS idx_account_parent;
ALTER TABLE account DROP COLUMN IF EXISTS parent_id;
