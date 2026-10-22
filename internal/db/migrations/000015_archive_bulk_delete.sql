-- 000015: D30 wave — recurring templates cascade with their account, and the
-- persistent deletion_log audit trail for bulk transaction deletes.
--
-- 1. Template cascade. Today an account that owns recurring templates can
--    never be deleted: the composite household-integrity FK
--    (fk_recurring_account, (household_id, account_id) -> account, ON DELETE
--    SET NULL) NULLs the NOT NULL account_id column and the delete aborts
--    with a not-null violation. Replace both template->account FKs with a
--    single ON DELETE CASCADE column FK: one account may have many
--    templates (1-to-many, unchanged), and deleting the account deletes its
--    templates (design doc §B).
--
-- 2. deletion_log (design doc §C). Bulk transaction DELETES are audited:
--    one row per attempted record. Snapshot fields (name/date/amount) because
--    the transaction row disappears on success. outcome 'cleared' marks the
--    tombstone left when the owner or an admin clears the log. Bulk voids
--    are NOT logged (that belongs to the parked transaction-history
--    audit enhancement).

-- +goose Up

ALTER TABLE recurring_template DROP CONSTRAINT recurring_template_account_id_fkey;
ALTER TABLE recurring_template DROP CONSTRAINT fk_recurring_account;
ALTER TABLE recurring_template ADD CONSTRAINT fk_recurring_account
    FOREIGN KEY (account_id) REFERENCES account(id) ON DELETE CASCADE;

CREATE SEQUENCE deletion_operation_seq;

CREATE TABLE deletion_log (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id  BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    operation_id  BIGINT NOT NULL,
    txn_id        BIGINT,
    txn_name      TEXT NOT NULL DEFAULT '',
    txn_date      DATE,
    amount        NUMERIC(14,2),
    outcome       TEXT NOT NULL CHECK (outcome IN ('deleted','failed','cleared')),
    reason        TEXT NOT NULL DEFAULT '',
    actor_id      BIGINT REFERENCES app_user(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_deletion_log_household ON deletion_log (household_id, created_at DESC);

-- +goose Down

DROP TABLE deletion_log;
DROP SEQUENCE deletion_operation_seq;
ALTER TABLE recurring_template DROP CONSTRAINT fk_recurring_account;
ALTER TABLE recurring_template ADD CONSTRAINT fk_recurring_account
    FOREIGN KEY (household_id, account_id) REFERENCES account(household_id, id)
    ON DELETE SET NULL;
ALTER TABLE recurring_template ADD CONSTRAINT recurring_template_account_id_fkey
    FOREIGN KEY (account_id) REFERENCES account(id);
