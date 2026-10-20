-- +goose Up
-- 000010_party_and_concrete_refs: the party redesign (schema rework,
-- Gate 3c). Three coupled changes, all previously approved:
--
--   1. counterparty -> party: ONE "who/what" table for every counter of a
--      money movement — outsiders AND household members alike. A member
--      always has a party row (seeded by trigger when the membership
--      starts; kept in name-sync with the member's display name).
--   2. split -> transaction_split, with a single party_id FK replacing the
--      polymorphic participant_type/participant_ref pair.
--   3. transaction endpoints: origin_type/origin_ref + destination_type/
--      destination_ref (4 polymorphic columns) become four concrete
--      account/party links. The merchant endpoint dies — the merchant is
--      already in merchant_id.
--
-- All backfills are proven lossless by assertions below before the old
-- columns are dropped (the one-way door).
--
-- Note: goose splits on ';' without dollar-quote awareness, so every
-- plpgsql body sits between StatementBegin/StatementEnd markers.

-- (a) The party table: rename + member link.
ALTER TABLE counterparty RENAME TO party;
ALTER TABLE party RENAME COLUMN user_id TO app_user_id;
DROP INDEX IF EXISTS idx_counterparty_user;
CREATE INDEX idx_party_user ON party (app_user_id);

-- Backfill the member link onto existing party rows (the old user_id).
-- (Already applied in the rename above; the constraint back-ref is rebuilt
-- against app_user by the FK re-creation below.)
ALTER TABLE party DROP CONSTRAINT IF EXISTS counterparty_user_id_fkey;
ALTER TABLE party ADD CONSTRAINT fk_party_user
    FOREIGN KEY (app_user_id) REFERENCES app_user(id) ON DELETE SET NULL;

-- Every membership gets its member party row (members included as parties).
-- ON CONFLICT keeps a pre-existing party row linked to the same member.
INSERT INTO party (household_id, name, type, app_user_id)
SELECT hm.household_id, u.display_name, 'person', u.id
FROM household_member hm
JOIN app_user u ON u.id = hm.user_id
WHERE NOT EXISTS (
    SELECT 1 FROM party p
    WHERE p.household_id = hm.household_id
      AND (p.app_user_id = hm.user_id
           OR (p.app_user_id IS NULL AND p.name = u.display_name)))
ON CONFLICT (household_id, name) DO UPDATE
    SET app_user_id = EXCLUDED.app_user_id
    WHERE party.app_user_id IS NULL;

-- The invariant, enforced by the DB, not the app: starting a membership
-- seeds the party row; a display-name edit syncs to the member's party rows.
-- +goose StatementBegin
CREATE FUNCTION seed_member_party() RETURNS trigger AS $$
BEGIN
    INSERT INTO party (household_id, name, type, app_user_id)
    SELECT NEW.household_id, u.display_name, 'person', NEW.user_id
    FROM app_user u WHERE u.id = NEW.user_id
    ON CONFLICT (household_id, name) DO UPDATE
        SET app_user_id = EXCLUDED.app_user_id
        WHERE party.app_user_id IS NULL;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER trg_party_seed AFTER INSERT ON household_member
    FOR EACH ROW EXECUTE FUNCTION seed_member_party();
-- +goose StatementBegin
CREATE FUNCTION sync_party_name() RETURNS trigger AS $$
BEGIN
    UPDATE party SET name = NEW.display_name WHERE app_user_id = OLD.id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER trg_party_name_sync BEFORE UPDATE OF display_name ON app_user
    FOR EACH ROW EXECUTE FUNCTION sync_party_name();

-- (b) transaction_split: the split table, named for what it is — the N-way
--     division of ONE transaction — and one required link to the party.
ALTER TABLE split RENAME TO transaction_split;
-- The 001 deferred split-sum triggers (trg_split_sum) fire on the table's
-- INSERT/UPDATEs below — and their function bodies still say "split", so
-- they must go now and come back after the backfill. Drop both constraint
-- triggers (their functions die with them and are recreated below).
DROP TRIGGER trg_split_sum ON transaction_split;
DROP TRIGGER trg_txn_split_sum ON "transaction";

-- (no FK yet: rows still carry party_id = 0, and party row 0 does not exist;
-- the constraint is attached after the backfill below.)
ALTER TABLE transaction_split ADD COLUMN party_id BIGINT NOT NULL DEFAULT 0;

-- Backfill: member splits -> their member party row (per household);
-- outsider splits -> the same numeric id (party rows keep their ids).
-- (Correlated subqueries: sp must not be visible inside the FROM clause.)
UPDATE transaction_split sp
SET party_id = (
    SELECT p.id FROM party p
    JOIN "transaction" t ON t.id = sp.transaction_id
    WHERE p.app_user_id = sp.participant_ref
      AND p.household_id = t.household_id
    LIMIT 1)
WHERE sp.participant_type = 'household_user';
UPDATE transaction_split
SET party_id = participant_ref
WHERE participant_type = 'counterparty'
  AND participant_ref <> 0;

-- Guard: the old data never split one share between a member's party row
-- and the same member twice. Merge duplicates (sum amounts, drop the extra)
-- so the unique index below cannot fail on legacy data.
-- +goose StatementBegin
DO $$
DECLARE
    r_txn_id BIGINT;
    r_party  BIGINT;
    r_cnt    BIGINT;
    rec_id   BIGINT;
    keep_id  BIGINT;
BEGIN
    FOR r_txn_id, r_party, r_cnt IN
        SELECT transaction_id, party_id, count(*)
        FROM transaction_split
        WHERE party_id <> 0
        GROUP BY 1, 2 HAVING count(*) > 1
    LOOP
        SELECT min(id) INTO keep_id
        FROM transaction_split
        WHERE transaction_id = r_txn_id AND party_id = r_party;
        FOR rec_id IN
            SELECT id FROM transaction_split
            WHERE transaction_id = r_txn_id
              AND party_id = r_party
              AND id <> keep_id
            ORDER BY id
        LOOP
            UPDATE transaction_split a
            SET amount = a.amount + b.amount
            FROM transaction_split b
            WHERE a.id = keep_id AND b.id = rec_id;
            DELETE FROM transaction_split WHERE id = rec_id;
        END LOOP;
    END LOOP;
END;
$$;
-- +goose StatementEnd
-- Flush the pending deferred split-sum events from the backfill/merge so the
-- DDL below doesn't fail (same pattern as migration 000006).
SET CONSTRAINTS ALL IMMEDIATE;
-- Every row now resolves to a real party; attach the integrity constraints.
ALTER TABLE transaction_split ADD CONSTRAINT fk_split_party
    FOREIGN KEY (party_id) REFERENCES party(id) ON DELETE RESTRICT;
CREATE UNIQUE INDEX uq_split_txn_party
    ON transaction_split (transaction_id, party_id);

-- The 001 split-sum triggers were dropped right after the table rename
-- (their function bodies said "split"); recreate them here, after the
-- backfill, against transaction_split.
DROP FUNCTION IF EXISTS trg_enforce_split_sum_split();
DROP FUNCTION IF EXISTS trg_enforce_split_sum_txn();
DROP FUNCTION IF EXISTS check_split_sum_for(BIGINT);
-- +goose StatementBegin
CREATE FUNCTION check_split_sum_for(txn_id BIGINT) RETURNS void AS $$
DECLARE
    t_total NUMERIC(14,2);
    t_status TEXT;
    s NUMERIC(14,2);
BEGIN
    SELECT total_amount, record_status INTO t_total, t_status
    FROM "transaction" WHERE id = txn_id;
    IF t_status IS NULL OR t_status = 'void' THEN
        RETURN;
    END IF;
    SELECT COALESCE(SUM(amount), 0) INTO s
    FROM transaction_split WHERE transaction_id = txn_id;
    IF s > 0 AND s <> t_total THEN
        RAISE EXCEPTION 'transaction % splits sum to %, total is %',
            txn_id, s, t_total
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION trg_enforce_split_sum_txn() RETURNS trigger AS $$
BEGIN
    PERFORM check_split_sum_for(NEW.id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION trg_enforce_split_sum_split() RETURNS trigger AS $$
DECLARE
    tid BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        tid := OLD.transaction_id;
    ELSE
        tid := NEW.transaction_id;
    END IF;
    PERFORM check_split_sum_for(tid);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER trg_split_sum
    AFTER INSERT OR UPDATE OR DELETE ON transaction_split
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION trg_enforce_split_sum_split();
CREATE CONSTRAINT TRIGGER trg_txn_split_sum
    AFTER INSERT OR UPDATE OF record_status, total_amount ON "transaction"
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION trg_enforce_split_sum_txn();

-- (c) Transaction endpoints: four concrete links.
--
-- Data repair first: the legacy seed rows sometimes carry an endpoint
-- _type with a NULL _ref (pure display residue — the merchant was the
-- "other side" and already lives in merchant_id). Such a type is not a
-- real reference; clear it. The two type CHECKs die with the columns,
-- so they go first.
ALTER TABLE "transaction" DROP CONSTRAINT IF EXISTS transaction_origin_type_check;
ALTER TABLE "transaction" DROP CONSTRAINT IF EXISTS transaction_destination_type_check;
UPDATE "transaction" SET origin_type = ''
 WHERE origin_type = 'merchant' AND origin_ref IS NULL;
UPDATE "transaction" SET destination_type = ''
 WHERE destination_type = 'merchant' AND destination_ref IS NULL;

ALTER TABLE "transaction"
    ADD COLUMN origin_account_id      BIGINT REFERENCES account(id) ON DELETE SET NULL,
    ADD COLUMN destination_account_id BIGINT REFERENCES account(id) ON DELETE SET NULL,
    ADD COLUMN origin_party_id        BIGINT REFERENCES party(id)    ON DELETE SET NULL,
    ADD COLUMN destination_party_id   BIGINT REFERENCES party(id)    ON DELETE SET NULL;

-- Backfill from the polymorphic columns ('merchant' endpoints -> NULL;
-- the merchant is already in merchant_id).
UPDATE "transaction" t SET
    origin_account_id      = CASE WHEN origin_type = 'account'      THEN origin_ref END,
    destination_account_id = CASE WHEN destination_type = 'account'  THEN destination_ref END,
    origin_party_id        = CASE WHEN origin_type = 'counterparty'   THEN origin_ref END,
    destination_party_id   = CASE WHEN destination_type = 'counterparty' THEN destination_ref END;

-- Losslessness assertion before the one-way door (raises on any row whose
-- new columns disagree with the old ones).
-- +goose StatementBegin
DO $$
DECLARE bad INT;
BEGIN
    SELECT count(*) INTO bad FROM "transaction"
    WHERE (origin_type = 'account'      AND origin_account_id IS DISTINCT FROM origin_ref)
       OR (destination_type = 'account' AND destination_account_id IS DISTINCT FROM destination_ref)
       OR (origin_type = 'counterparty' AND origin_party_id IS DISTINCT FROM origin_ref)
       OR (destination_type = 'counterparty' AND destination_party_id IS DISTINCT FROM destination_ref);
    IF bad > 0 THEN
        RAISE EXCEPTION 'endpoint backfill not lossless: % rows disagree', bad;
    END IF;
    SELECT count(*) INTO bad FROM transaction_split s
    WHERE s.party_id = 0;
    IF bad > 0 THEN
        RAISE EXCEPTION 'split party backfill incomplete: % rows unlinked', bad;
    END IF;
END;
$$;
-- +goose StatementEnd

-- Endpoint invariant, now that the links are concrete: every transaction
-- keeps at least one concrete endpoint (an account, a party, or the
-- merchant display attribute). The stricter per-class shape (expense
-- needs an account origin, ...) is enforced by the transaction form,
-- which requires an account origin — legacy seed rows that predate it
-- are preserved, not rewritten.
ALTER TABLE "transaction" ADD CONSTRAINT chk_txn_class_endpoints CHECK (
    origin_account_id IS NOT NULL
 OR destination_account_id IS NOT NULL
 OR origin_party_id IS NOT NULL
 OR destination_party_id IS NOT NULL
 OR merchant_id IS NOT NULL);

-- Composite household-safe FKs (deferred, same pattern as 000006).
ALTER TABLE "transaction" ADD CONSTRAINT fk_txn_origin_account
    FOREIGN KEY (household_id, origin_account_id)
    REFERENCES account (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE "transaction" ADD CONSTRAINT fk_txn_destination_account
    FOREIGN KEY (household_id, destination_account_id)
    REFERENCES account (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE "transaction" ADD CONSTRAINT fk_txn_origin_party
    FOREIGN KEY (household_id, origin_party_id)
    REFERENCES party (household_id, id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE "transaction" ADD CONSTRAINT fk_txn_destination_party
    FOREIGN KEY (household_id, destination_party_id)
    REFERENCES party (household_id, id) DEFERRABLE INITIALLY DEFERRED;

-- One-way door: drop the polymorphic columns (backfill proven lossless).
ALTER TABLE "transaction" DROP COLUMN origin_type, DROP COLUMN origin_ref,
                          DROP COLUMN destination_type, DROP COLUMN destination_ref;
-- The 001 unique constraint still covers the about-to-die participant
-- columns (renamed with the table, so it now lives on transaction_split).
ALTER TABLE transaction_split
    DROP CONSTRAINT split_transaction_id_participant_type_participant_ref_key;
DROP INDEX IF EXISTS split_transaction_id_participant_type_participant_ref_key;
ALTER TABLE transaction_split DROP COLUMN participant_type, DROP COLUMN participant_ref;

-- (d) Indexes for the new hot paths.
CREATE INDEX idx_txn_origin_account      ON "transaction" (origin_account_id);
CREATE INDEX idx_txn_destination_account ON "transaction" (destination_account_id);
CREATE INDEX idx_txn_origin_party        ON "transaction" (origin_party_id);
CREATE INDEX idx_txn_destination_party   ON "transaction" (destination_party_id);
CREATE INDEX idx_split_party ON transaction_split (party_id);

-- +goose Down
-- (Down is provided for completeness; the rework migrations are intended
-- to be applied only to databases at version 9.)
DROP INDEX IF EXISTS idx_split_party;
DROP INDEX IF EXISTS idx_txn_destination_party;
DROP INDEX IF EXISTS idx_txn_origin_party;
DROP INDEX IF EXISTS idx_txn_destination_account;
DROP INDEX IF EXISTS idx_txn_origin_account;
ALTER TABLE transaction_split ADD COLUMN participant_type TEXT;
ALTER TABLE transaction_split ADD COLUMN participant_ref BIGINT;
UPDATE transaction_split s SET
    participant_type = CASE WHEN p.app_user_id IS NOT NULL THEN 'household_user' ELSE 'counterparty' END,
    participant_ref  = COALESCE(p.app_user_id, s.party_id)
FROM party p WHERE p.id = s.party_id;
ALTER TABLE transaction_split DROP CONSTRAINT fk_split_party;
ALTER TABLE transaction_split DROP COLUMN party_id;
DROP TRIGGER IF EXISTS trg_split_sum ON transaction_split;
DROP TRIGGER IF EXISTS trg_txn_split_sum ON "transaction";
DROP FUNCTION IF EXISTS trg_enforce_split_sum_split();
DROP FUNCTION IF EXISTS trg_enforce_split_sum_txn();
DROP FUNCTION IF EXISTS check_split_sum_for(BIGINT);
ALTER TABLE transaction_split ADD CONSTRAINT
    split_transaction_id_participant_type_participant_ref_key
    UNIQUE (transaction_id, participant_type, participant_ref);
-- Recreated columns keep NOT NULL + type CHECKs to match the pre-migration
-- table shape (repaired rows get back a legal value).
ALTER TABLE "transaction" ADD COLUMN origin_type TEXT NOT NULL DEFAULT '';
ALTER TABLE "transaction" ADD COLUMN origin_ref BIGINT;
ALTER TABLE "transaction" ADD COLUMN destination_type TEXT NOT NULL DEFAULT '';
ALTER TABLE "transaction" ADD COLUMN destination_ref BIGINT;
UPDATE "transaction" SET origin_type = 'merchant' WHERE origin_type = '';
UPDATE "transaction" SET destination_type = 'merchant' WHERE destination_type = '';
ALTER TABLE "transaction" ADD CONSTRAINT transaction_origin_type_check
    CHECK (origin_type IN ('account','merchant','counterparty'));
ALTER TABLE "transaction" ADD CONSTRAINT transaction_destination_type_check
    CHECK (destination_type IN ('account','merchant','counterparty'));
ALTER TABLE "transaction" DROP CONSTRAINT fk_txn_destination_party;
ALTER TABLE "transaction" DROP CONSTRAINT fk_txn_origin_party;
ALTER TABLE "transaction" DROP CONSTRAINT fk_txn_destination_account;
ALTER TABLE "transaction" DROP CONSTRAINT fk_txn_origin_account;
ALTER TABLE "transaction" DROP CONSTRAINT chk_txn_class_endpoints;
ALTER TABLE "transaction" DROP COLUMN destination_party_id;
ALTER TABLE "transaction" DROP COLUMN origin_party_id;
ALTER TABLE "transaction" DROP COLUMN destination_account_id;
ALTER TABLE "transaction" DROP COLUMN origin_account_id;
DROP TRIGGER IF EXISTS trg_party_name_sync ON app_user;
DROP FUNCTION IF EXISTS sync_party_name();
DROP TRIGGER IF EXISTS trg_party_seed ON household_member;
DROP FUNCTION IF EXISTS seed_member_party();
ALTER TABLE party DROP CONSTRAINT fk_party_user;
ALTER TABLE party RENAME COLUMN app_user_id TO user_id;
ALTER TABLE party RENAME TO counterparty;
ALTER TABLE transaction_split RENAME TO split;
-- Restore the 001 split-sum constraint triggers (dropped above).
-- +goose StatementBegin
CREATE FUNCTION check_split_sum_for(txn_id BIGINT) RETURNS void AS $$
DECLARE
    t_total NUMERIC(14,2);
    t_status TEXT;
    s NUMERIC(14,2);
BEGIN
    SELECT total_amount, record_status INTO t_total, t_status
    FROM "transaction" WHERE id = txn_id;
    IF t_status IS NULL OR t_status = 'void' THEN
        RETURN;
    END IF;
    SELECT COALESCE(SUM(amount), 0) INTO s
    FROM split WHERE transaction_id = txn_id;
    IF s > 0 AND s <> t_total THEN
        RAISE EXCEPTION 'transaction % splits sum to %, total is %',
            txn_id, s, t_total
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION trg_enforce_split_sum_txn() RETURNS trigger AS $$
BEGIN
    PERFORM check_split_sum_for(NEW.id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION trg_enforce_split_sum_split() RETURNS trigger AS $$
DECLARE
    tid BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        tid := OLD.transaction_id;
    ELSE
        tid := NEW.transaction_id;
    END IF;
    PERFORM check_split_sum_for(tid);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER trg_split_sum
    AFTER INSERT OR UPDATE OR DELETE ON split
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION trg_enforce_split_sum_split();
CREATE CONSTRAINT TRIGGER trg_txn_split_sum
    AFTER INSERT OR UPDATE OF record_status, total_amount ON "transaction"
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION trg_enforce_split_sum_txn();
