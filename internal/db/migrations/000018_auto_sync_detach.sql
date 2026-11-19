-- 000018: D34 wave — auto-sync + detach (ADR-0007, ADR-0008).
--
-- Purely additive (spec): the D33 pointer shape is untouched (the
-- bidirectional CHECK, uq_txn_sync_source, transaction_source_check and
-- the cascade-lookup index all stay).
--
-- 1. "transaction" gains the sync-STATE flag (ADR-0007). source stays the
--    pure birth marker ('sync' forever on a synced row); is_synced answers
--    "is this row currently synced?" for every key point (edit lock,
--    badges, void cascade, budget gate). Backfill: every current
--    source='sync' row is currently synced.
--
-- 2. auto_sync — the per-member "always sync" toggle (spec §A). Row
--    existence = toggle ON. member_user_id is plain (app-validated
--    membership — the isolation convention: no cross-household FK); the
--    target household FK is household-safe.
--
-- 3. category_mapping — per-target-household rules (spec §B). The source
--    pair is plain (no cross-household FKs — isolation convention); the
--    target category FK is household-local.

-- +goose Up

ALTER TABLE "transaction"
    ADD COLUMN is_synced BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN detached_at TIMESTAMPTZ NULL;

-- Backfill: the existing synced copies are currently synced.
UPDATE "transaction" SET is_synced = true WHERE source = 'sync';

CREATE TABLE auto_sync (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    member_user_id      BIGINT NOT NULL,
    source_household_id BIGINT NOT NULL,
    target_household_id BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    UNIQUE (member_user_id, source_household_id, target_household_id)
);

-- The write-time trigger (ADR-0008) looks up toggle-on members of a source
-- household's records.
CREATE INDEX idx_auto_sync_source ON auto_sync (source_household_id);
-- The settings section + the Live-view toggle state list by member.
CREATE INDEX idx_auto_sync_member ON auto_sync (member_user_id, target_household_id);

-- The detach action (spec §D) writes a History row on the B-side copy; the
-- action enum (000016) must admit 'detached'. Additive: no existing row is
-- invalidated.
ALTER TABLE txn_history DROP CONSTRAINT txn_history_action_check;
ALTER TABLE txn_history ADD CONSTRAINT txn_history_action_check
    CHECK (action IN ('created','edited','voided','deleted','cleared','detached'));

CREATE TABLE category_mapping (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id       BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    source_household_id BIGINT NOT NULL,
    source_category_id  BIGINT NOT NULL,
    target_category_id  BIGINT NOT NULL REFERENCES category(id) ON DELETE CASCADE,
    UNIQUE (household_id, source_household_id, source_category_id)
);

-- +goose Down

-- Down restores the pre-000018 shape; restoring data is the backup's job
-- (house convention).
DROP TABLE category_mapping;
DROP TABLE auto_sync;
-- Restore the pre-000018 action enum.
ALTER TABLE txn_history DROP CONSTRAINT txn_history_action_check;
ALTER TABLE txn_history ADD CONSTRAINT txn_history_action_check
    CHECK (action IN ('created','edited','voided','deleted','cleared'));
ALTER TABLE "transaction" DROP COLUMN is_synced, DROP COLUMN detached_at;
