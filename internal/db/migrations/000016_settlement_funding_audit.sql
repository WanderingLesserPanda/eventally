-- 000016: D31 wave — multi-payer settlement funding (ADR-0004) and the
-- audit trail (ADR-0005): the new txn_history table, plus the access
-- snapshot columns that give both audit pages their per-row visibility.
--
-- 1. settlement_funding (ADR-0004). A settlement's funding is a set of
--    LEGS — one payer + optional source account + method + amount each,
--    summing to the settlement amount (the app enforces the sum in one
--    transaction; the column stays as the display total). The single
--    payer column — and the settlement-level method, which moves to the
--    leg (mixed funding is the point) — is DROPPED after backfilling
--    exactly one leg per existing settlement: payer from the old column;
--    source from the linked D6 transfer's ORIGIN account (D6 transfers
--    run source -> card), else NULL (out-of-band, e.g. cash); method and
--    amount from the settlement. The leg's user_id FK (RESTRICT) carries
--    the old payer delete-guard; the household-safe composite FK keeps
--    legs inside their settlement's household (schema convention).
--
-- 2. txn_history (ADR-0005). The lifecycle trail: transaction
--    created/edited/voided, settlement created/deleted. Deletions stay
--    in the D30 deletion_log — each event has exactly one home. Every
--    row carries a WRITE-TIME access snapshot (the record's account
--    surface + the people in its Related slice at event time) so the
--    per-row visibility rule can be evaluated after the record changes
--    or disappears.
--
-- 3. deletion_log gains the same snapshot columns. Pre-000016 rows keep
--    their empty snapshots and therefore keep today's all-members
--    visibility (no regression).

-- +goose Up

-- Household-safe composite key for the settlement FK below (matches the
-- uq_*_hh convention of the concrete references).
ALTER TABLE settlement ADD CONSTRAINT uq_settlement_hh
    UNIQUE (household_id, id);

CREATE TABLE settlement_funding (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id      BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    settlement_id     BIGINT NOT NULL,
    user_id           BIGINT NOT NULL REFERENCES app_user(id) ON DELETE RESTRICT,
    source_account_id BIGINT REFERENCES account(id) ON DELETE SET NULL,
    method            TEXT NOT NULL DEFAULT 'transfer',
    amount            NUMERIC(14,2) NOT NULL CHECK (amount > 0),
    CONSTRAINT fk_settlement_funding_settlement
        FOREIGN KEY (household_id, settlement_id)
        REFERENCES settlement (household_id, id) ON DELETE CASCADE
);
CREATE INDEX idx_settlement_funding_settlement ON settlement_funding (settlement_id);
CREATE INDEX idx_settlement_funding_user ON settlement_funding (user_id);

-- Backfill: exactly one leg per existing settlement.
INSERT INTO settlement_funding
    (household_id, settlement_id, user_id, source_account_id, method, amount)
SELECT s.household_id, s.id, s.paid_by_user_id,
       (SELECT t.origin_account_id FROM "transaction" t
         WHERE t.id = s.ledger_transaction_id),
       s.method, s.amount
FROM settlement s;

ALTER TABLE settlement DROP COLUMN paid_by_user_id;
ALTER TABLE settlement DROP COLUMN method;
-- (The old payer index dies with the column; no explicit DROP INDEX.)

CREATE TABLE txn_history (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id     BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    entity_type      TEXT NOT NULL
                     CHECK (entity_type IN ('transaction','settlement')),
    entity_id        BIGINT NOT NULL,
    action           TEXT NOT NULL
                     CHECK (action IN ('created','edited','voided','deleted')),
    summary          TEXT NOT NULL DEFAULT '',
    -- Display snapshot (the record's name/date/amount at event time, so a
    -- later rename or the record's own deletion can't blank the trail).
    name             TEXT NOT NULL DEFAULT '',
    event_date       DATE,
    amount           NUMERIC(14,2),
    -- Write-time access snapshot (ADR-0005): the record's account surface
    -- and the people in its Related slice at event time.
    account_ids      BIGINT[] NOT NULL DEFAULT '{}',
    related_user_ids BIGINT[] NOT NULL DEFAULT '{}',
    actor_id         BIGINT REFERENCES app_user(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_txn_history_household ON txn_history (household_id, created_at DESC);
CREATE INDEX idx_txn_history_entity ON txn_history (entity_type, entity_id);

-- +goose Down
-- The Down section restores the pre-000016 SHAPES only; a full data
-- restore belongs to a backup (per the house convention, these
-- migrations apply forward to live databases).
ALTER TABLE deletion_log DROP COLUMN related_user_ids;
ALTER TABLE deletion_log DROP COLUMN account_ids;
DROP TABLE txn_history;
ALTER TABLE settlement ADD COLUMN method TEXT NOT NULL DEFAULT 'transfer';
ALTER TABLE settlement ADD COLUMN paid_by_user_id BIGINT NOT NULL DEFAULT 0
    REFERENCES app_user(id) ON DELETE RESTRICT;
CREATE INDEX idx_payment_paid_by ON settlement (paid_by_user_id);
DROP TABLE settlement_funding;
ALTER TABLE settlement DROP CONSTRAINT uq_settlement_hh;
