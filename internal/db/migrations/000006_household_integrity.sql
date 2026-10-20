-- +goose Up
-- 000006_household_integrity: composite household-safe FKs + multi-household
-- membership schema (schema rework, Gate 1).
--
-- Rationale (external schema review, items 1, 8, 10):
--   * Separate FKs to household(id) and account(id) do NOT prevent a row in
--     household A from referencing an account of household B. The composite
--     UNIQUE (household_id, id) + FK (household_id, ref) closes that gap.
--   * Users are now GLOBAL (a user may belong to 0/1/many households);
--     membership lives in household_member. Accounts always belong to
--     exactly one household (unchanged).
--   * split / payment_allocation inherit ownership transitively today;
--     giving them their own household_id makes household-safe references
--     enforceable (Gate 4's allocation cap uses it).
--
-- Deliberate deviations from the plan draft (documented in
-- docs/notes/schema-conventions.md):
--   * user.household_id is KEPT as a legacy primary-household pointer
--     (login scoping, email uniqueness). Dropping it would churn every
--     "WHERE household_id = $1" user query for no safety gain.
--   * recorded_by / paid_by stay plain FKs to user(id): a composite
--     (household_id, user_id) FK to household_member would forbid a member
--     recording in a second household — a cross-household reference is
--     legitimate under multi-household membership.
--   * Parent-cycle guard deferred: the app enforces one-level card
--     nesting already and category parents are not UI-exposed yet.

-- (a) Membership: user_id x household_id. A user with no rows belongs to
--     no household and can create nothing (every business table requires
--     household_id). role/can_edit/record_scope are the owner-controlled
--     permission model; ENFORCEMENT lands with the invitation UI (future),
--     today every member is a full admin.
CREATE TABLE household_member (
    user_id       BIGINT NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    household_id  BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    role          TEXT NOT NULL DEFAULT 'member'
                  CHECK (role IN ('owner','member')),
    can_edit      BOOLEAN NOT NULL DEFAULT TRUE,   -- FALSE = read-only
    record_scope  TEXT NOT NULL DEFAULT 'all'
                  CHECK (record_scope IN ('all','accounts','own')),
    PRIMARY KEY (user_id, household_id)
);
CREATE INDEX idx_member_household ON household_member (household_id);
-- Backfill: every existing user joins their household; the earliest user id
-- per household becomes its owner (first-created = founder).
INSERT INTO household_member (user_id, household_id, role)
SELECT u.id, u.household_id,
       CASE WHEN row_number() OVER (PARTITION BY u.household_id ORDER BY u.id) = 1
            THEN 'owner' ELSE 'member' END
FROM "user" u;
-- Deferred constraint triggers fire at COMMIT, but their PENDING events
-- block the next DDL ("cannot ALTER TABLE because it has pending trigger
-- events"); flush them before the section (c) ALTER TABLEs.
SET CONSTRAINTS ALL IMMEDIATE;
-- Exactly one owner per household.
CREATE UNIQUE INDEX uq_member_owner ON household_member (household_id)
    WHERE role = 'owner';

-- Per-account visibility (used when record_scope = 'accounts'). Created in
-- section (d) because its composite FK needs uq_account_hh first.

-- No orphaned households: the last membership of a household cannot be
-- deleted — retire the household itself instead. The trigger is created at
-- the END of the migration (section (g)): creating it here would leave
-- pending DEFERRED events from the membership backfill above, which makes
-- the ALTER TABLE statements below fail ("pending trigger events").
-- A household that is itself deleted in the same transaction may lose its
-- members (the rows are gone anyway) — only a SURVIVING orphan is an error.

-- (c) Household-ownership columns on the two transitively-owned tables.
ALTER TABLE split ADD COLUMN household_id BIGINT NOT NULL DEFAULT 0;
UPDATE split s SET household_id = t.household_id
    FROM "transaction" t WHERE t.id = s.transaction_id;
ALTER TABLE split ADD CONSTRAINT fk_split_household
    FOREIGN KEY (household_id) REFERENCES household(id) ON DELETE CASCADE;
ALTER TABLE payment_allocation ADD COLUMN household_id BIGINT NOT NULL DEFAULT 0;
UPDATE payment_allocation pa SET household_id = p.household_id
    FROM payment p WHERE p.id = pa.payment_id;
ALTER TABLE payment_allocation ADD CONSTRAINT fk_alloc_household
    FOREIGN KEY (household_id) REFERENCES household(id) ON DELETE CASCADE;

-- (d) Composite household-safe UNIQUE + FKs for the CONCRETE refs.
-- (Polymorphic origin/destination/participant refs become concrete links
-- in 000010.)
ALTER TABLE account      ADD CONSTRAINT uq_account_hh      UNIQUE (household_id, id);
ALTER TABLE category     ADD CONSTRAINT uq_category_hh     UNIQUE (household_id, id);
ALTER TABLE merchant     ADD CONSTRAINT uq_merchant_hh     UNIQUE (household_id, id);
ALTER TABLE counterparty ADD CONSTRAINT uq_counterparty_hh UNIQUE (household_id, id);
ALTER TABLE "transaction" ADD CONSTRAINT uq_txn_hh         UNIQUE (household_id, id);

-- Deferred: INSERT order inside one transaction is then free, and every
-- ref is guaranteed to live in the row's own household.
ALTER TABLE "transaction" ADD CONSTRAINT fk_txn_category
    FOREIGN KEY (household_id, category_id)
    REFERENCES category (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE "transaction" ADD CONSTRAINT fk_txn_merchant
    FOREIGN KEY (household_id, merchant_id)
    REFERENCES merchant (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE "transaction" ADD CONSTRAINT fk_txn_parent
    FOREIGN KEY (household_id, parent_transaction_id)
    REFERENCES "transaction" (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE account ADD CONSTRAINT fk_account_parent
    FOREIGN KEY (household_id, parent_id)
    REFERENCES account (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE merchant ADD CONSTRAINT fk_merchant_default_cat
    FOREIGN KEY (household_id, default_category_id)
    REFERENCES category (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE payment ADD CONSTRAINT fk_payment_account
    FOREIGN KEY (household_id, account_id)
    REFERENCES account (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE reconciliation ADD CONSTRAINT fk_recon_account
    FOREIGN KEY (household_id, account_id)
    REFERENCES account (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE recurring_template ADD CONSTRAINT fk_recurring_account
    FOREIGN KEY (household_id, account_id)
    REFERENCES account (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE recurring_template ADD CONSTRAINT fk_recurring_merchant
    FOREIGN KEY (household_id, merchant_id)
    REFERENCES merchant (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE recurring_template ADD CONSTRAINT fk_recurring_category
    FOREIGN KEY (household_id, category_id)
    REFERENCES category (household_id, id) DEFERRABLE INITIALLY DEFERRED;

-- Per-account visibility (used when record_scope = 'accounts'); lives here
-- because its composite FK requires uq_account_hh.
CREATE TABLE member_account_scope (
    user_id      BIGINT NOT NULL,
    household_id BIGINT NOT NULL,
    account_id   BIGINT NOT NULL,
    PRIMARY KEY (user_id, household_id, account_id),
    FOREIGN KEY (user_id, household_id) REFERENCES household_member
        (user_id, household_id) ON DELETE CASCADE,
    FOREIGN KEY (household_id, account_id) REFERENCES account (household_id, id)
        ON DELETE CASCADE
);

-- (e) Missing FK indexes (item 10).
CREATE INDEX idx_merchant_default_cat ON merchant (default_category_id);
CREATE INDEX idx_payment_paid_by      ON payment (paid_by);
CREATE INDEX idx_recon_account        ON reconciliation (account_id);
CREATE INDEX idx_recon_recorded_by    ON reconciliation (recorded_by);
CREATE INDEX idx_session_user         ON session (user_id);
CREATE INDEX idx_txn_recorded_by      ON "transaction" (recorded_by);

-- (f) Allocation positivity (item 4, half one): a zero/negative allocation
--     row is always wrong; the per-split CAP is a deferred trigger in 000011.
ALTER TABLE payment_allocation ADD CONSTRAINT chk_alloc_positive CHECK (amount > 0);

-- (g) No-orphan guard, created LAST so the earlier ALTER TABLEs are not
--     blocked by its pending deferred events.
-- +goose StatementBegin
CREATE FUNCTION no_orphan_household() RETURNS trigger AS $$
BEGIN
    -- Deferred, so at COMMIT time: if the household itself was deleted in
    -- the same transaction it is invisible here and the guard stands down
    -- (retiring a household legitimately drops its memberships).
    IF NOT EXISTS (SELECT 1 FROM household_member
                   WHERE household_id = OLD.household_id)
       AND EXISTS (SELECT 1 FROM household WHERE id = OLD.household_id) THEN
        RAISE EXCEPTION 'household % would lose its last member — delete the household instead',
            OLD.household_id;
    END IF;
    RETURN OLD;
END; $$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER trg_no_orphan_household
    AFTER DELETE ON household_member
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION no_orphan_household();

-- +goose Down
-- (Down is provided for completeness; the rework migrations are intended
-- to be applied only to databases at version 5.)
ALTER TABLE payment_allocation DROP CONSTRAINT chk_alloc_positive;
DROP INDEX IF EXISTS idx_txn_recorded_by;
DROP INDEX IF EXISTS idx_session_user;
DROP INDEX IF EXISTS idx_recon_recorded_by;
DROP INDEX IF EXISTS idx_recon_account;
DROP INDEX IF EXISTS idx_payment_paid_by;
DROP INDEX IF EXISTS idx_merchant_default_cat;
ALTER TABLE recurring_template DROP CONSTRAINT fk_recurring_category;
ALTER TABLE recurring_template DROP CONSTRAINT fk_recurring_merchant;
ALTER TABLE recurring_template DROP CONSTRAINT fk_recurring_account;
ALTER TABLE reconciliation DROP CONSTRAINT fk_recon_account;
ALTER TABLE payment DROP CONSTRAINT fk_payment_account;
ALTER TABLE merchant DROP CONSTRAINT fk_merchant_default_cat;
ALTER TABLE account DROP CONSTRAINT fk_account_parent;
ALTER TABLE "transaction" DROP CONSTRAINT fk_txn_parent;
ALTER TABLE "transaction" DROP CONSTRAINT fk_txn_merchant;
ALTER TABLE "transaction" DROP CONSTRAINT fk_txn_category;
ALTER TABLE "transaction" DROP CONSTRAINT uq_txn_hh;
ALTER TABLE counterparty DROP CONSTRAINT uq_counterparty_hh;
ALTER TABLE merchant DROP CONSTRAINT uq_merchant_hh;
ALTER TABLE category DROP CONSTRAINT uq_category_hh;
ALTER TABLE account DROP CONSTRAINT uq_account_hh;
ALTER TABLE payment_allocation DROP CONSTRAINT fk_alloc_household;
ALTER TABLE payment_allocation DROP COLUMN IF EXISTS household_id;
ALTER TABLE split DROP CONSTRAINT fk_split_household;
ALTER TABLE split DROP COLUMN IF EXISTS household_id;
DROP TRIGGER IF EXISTS trg_no_orphan_household ON household_member;
DROP FUNCTION IF EXISTS no_orphan_household();
DROP TABLE IF EXISTS member_account_scope;
DROP TABLE IF EXISTS household_member;
