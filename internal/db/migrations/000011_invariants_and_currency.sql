-- +goose Up
-- Gate 4 (feedback #4 + #5, option (a) approved):
--  1. Allocation cap per transaction_split, enforced at COMMIT (deferred).
--     Two concurrent settlements that together exceed a share both INSERT
--     fine; the second COMMIT fails with check_violation and rolls back.
--  2. Base-currency lock: a household's base currency can no longer change
--     once it has any transactions (the currency of every amount in the
--     household is implied, so a change would silently reprice history).
--
-- The cap is a DEFERRED CONSTRAINT TRIGGER (like the split-sum triggers in
-- 000001): the check runs at commit against the final allocation set, so
-- within-transaction re-allocation (delete + re-insert) is legal as long as
-- the committed state is sound.

-- +goose StatementBegin
CREATE FUNCTION check_allocation_cap_for(split_id BIGINT) RETURNS void AS $$
DECLARE
    s_amt NUMERIC(14,2);
    al    NUMERIC(14,2);
BEGIN
    SELECT s.amount,
           COALESCE((SELECT SUM(a.amount) FROM settlement_allocation a
                     WHERE a.split_id = s.id), 0)
    INTO s_amt, al
    FROM transaction_split s WHERE s.id = split_id;
    IF al > s_amt THEN
        RAISE EXCEPTION 'split %: allocations % exceed share %',
            split_id, al, s_amt USING ERRCODE = 'check_violation';
    END IF;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- The trigger body fires on every allocation row change and re-checks the
-- whole split at commit.
-- +goose StatementBegin
CREATE FUNCTION trg_alloc_cap() RETURNS trigger AS $$
BEGIN
    PERFORM check_allocation_cap_for(COALESCE(NEW.split_id, OLD.split_id));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER trg_settlement_alloc_cap
    AFTER INSERT OR UPDATE OR DELETE ON settlement_allocation
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION trg_alloc_cap();

-- Base-currency lock once activity exists.
-- +goose StatementBegin
CREATE FUNCTION no_currency_change_with_activity() RETURNS trigger AS $$
BEGIN
    IF NEW.base_currency IS DISTINCT FROM OLD.base_currency
       AND EXISTS (SELECT 1 FROM "transaction" t WHERE t.household_id = OLD.id) THEN
        RAISE EXCEPTION 'base currency cannot change once transactions exist';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_household_currency
    BEFORE UPDATE OF base_currency ON household
    FOR EACH ROW
    EXECUTE FUNCTION no_currency_change_with_activity();

-- +goose Down
DROP TRIGGER IF EXISTS trg_household_currency ON household;
DROP FUNCTION IF EXISTS no_currency_change_with_activity();
DROP TRIGGER IF EXISTS trg_settlement_alloc_cap ON settlement_allocation;
DROP FUNCTION IF EXISTS trg_alloc_cap();
DROP FUNCTION IF EXISTS check_allocation_cap_for(BIGINT);
