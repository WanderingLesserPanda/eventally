-- +goose Up
-- 000009_rename_money_terms: settle on the reviewer-recommended money
-- vocabulary (schema rework, Gate 3b). Pure renames — no data, no behavior.
--
--   payment             -> settlement          (paying down card debt)
--   payment_allocation  -> settlement_allocation (how a settlement maps to a share)
--   paid_by             -> paid_by_user_id
--   recorded_by         -> recorded_by_user_id   (transaction AND reconciliation)
--   note (reconciliation) -> notes              (unify with transaction/settlement notes)
--   active (recurring)   -> is_active           (unify with category/merchant/account)
--   budgetable (category) -> is_budgetable
--
-- Column renames auto-rename any index built on the column (e.g. the
-- idx_payment_paid_by from 000006), so no separate index DDL is needed.

ALTER TABLE payment            RENAME TO settlement;
ALTER TABLE settlement         RENAME COLUMN paid_by  TO paid_by_user_id;
ALTER TABLE payment_allocation RENAME TO settlement_allocation;

ALTER TABLE "transaction" RENAME COLUMN recorded_by TO recorded_by_user_id;
ALTER TABLE reconciliation RENAME COLUMN recorded_by TO recorded_by_user_id;
ALTER TABLE reconciliation RENAME COLUMN note      TO notes;

ALTER TABLE recurring_template RENAME COLUMN active    TO is_active;
ALTER TABLE category          RENAME COLUMN budgetable TO is_budgetable;

-- +goose Down
ALTER TABLE category          RENAME COLUMN is_budgetable TO budgetable;
ALTER TABLE recurring_template RENAME COLUMN is_active   TO active;
ALTER TABLE reconciliation RENAME COLUMN notes      TO note;
ALTER TABLE reconciliation RENAME COLUMN recorded_by_user_id TO recorded_by;
ALTER TABLE "transaction" RENAME COLUMN recorded_by_user_id TO recorded_by;
ALTER TABLE settlement_allocation RENAME TO payment_allocation;
ALTER TABLE settlement RENAME COLUMN paid_by_user_id TO paid_by;
ALTER TABLE settlement RENAME TO payment;
