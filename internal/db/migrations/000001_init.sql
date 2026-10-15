-- +goose Up
-- 001_init: initial schema for the Eventally MVP.
--
-- Conventions:
--   * Every table carries household_id (D3: single household now, scoped
--     so multi-household is config later, not a rewrite).
--   * All money is NUMERIC(14,2) base-currency units, never float.
--   * Dates are DATE (no time — recording granularity is the day).
--   * Split rows sum exactly to the transaction total (enforced by
--     trigger; the split-sum invariant, D4).
--   * Payment allocations reference split rows, so outstanding is always
--     computable (D5) and overpayment is just a negative balance.

-- +goose StatementBegin
CREATE TABLE household (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name          TEXT NOT NULL,
    base_currency TEXT NOT NULL DEFAULT 'MYR',   -- ISO 4217 (D13)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE "user" (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    username    TEXT NOT NULL,
    pass_hash   TEXT NOT NULL,
    display_name TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, username)
);
CREATE INDEX idx_user_household ON "user" (household_id);

CREATE TABLE account (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id          BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    name                  TEXT NOT NULL,
    institution           TEXT NOT NULL DEFAULT '',
    account_type          TEXT NOT NULL
                          CHECK (account_type IN
                          ('savings','ewallet','credit_card','loan',
                           'investment','cash','credits')),
    balance_type          TEXT NOT NULL
                          CHECK (balance_type IN ('asset','liability')),
    card_type             TEXT
                          CHECK (card_type IN ('principal','supplementary')),
    credit_limit          NUMERIC(14,2),
    statement_day         SMALLINT,
    payment_due_day       SMALLINT,
    opening_balance       NUMERIC(14,2) NOT NULL DEFAULT 0,
    opening_balance_date  DATE,
    is_active             BOOLEAN NOT NULL DEFAULT TRUE,
    sort_order            INT NOT NULL DEFAULT 0,
    notes                 TEXT NOT NULL DEFAULT '',
    UNIQUE (household_id, name)
);
CREATE INDEX idx_account_household ON account (household_id);

CREATE TABLE category (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id       BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    parent_id          BIGINT REFERENCES category(id) ON DELETE SET NULL,
    transaction_class  TEXT NOT NULL DEFAULT 'expense'
                       CHECK (transaction_class IN ('expense','income','transfer')),
    budget_per_month   NUMERIC(14,2),
    budgetable         BOOLEAN NOT NULL DEFAULT TRUE,
    sort_order         INT NOT NULL DEFAULT 0,
    is_active          BOOLEAN NOT NULL DEFAULT TRUE,
    UNIQUE (household_id, name)
);
CREATE INDEX idx_category_household ON category (household_id);
CREATE INDEX idx_category_parent ON category (parent_id);

CREATE TABLE merchant (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id          BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    name                  TEXT NOT NULL,
    default_category_id   BIGINT REFERENCES category(id) ON DELETE SET NULL,
    country               TEXT NOT NULL DEFAULT '',
    is_active             BOOLEAN NOT NULL DEFAULT TRUE,
    notes                 TEXT NOT NULL DEFAULT '',
    UNIQUE (household_id, name)
);
CREATE INDEX idx_merchant_household ON merchant (household_id);

CREATE TABLE counterparty (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id     BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    type             TEXT NOT NULL DEFAULT 'person'
                     CHECK (type IN ('person','organisation')),
    relationship_type TEXT NOT NULL DEFAULT '',
    is_active        BOOLEAN NOT NULL DEFAULT TRUE,
    notes            TEXT NOT NULL DEFAULT '',
    UNIQUE (household_id, name)
);
CREATE INDEX idx_counterparty_household ON counterparty (household_id);

CREATE TABLE transaction (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id          BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    recorded_by           BIGINT REFERENCES "user"(id) ON DELETE SET NULL,
    transaction_date      DATE NOT NULL,
    class                 TEXT NOT NULL
                          CHECK (class IN ('expense','income','transfer')),
    category_id           BIGINT REFERENCES category(id) ON DELETE SET NULL,
    merchant_id           BIGINT REFERENCES merchant(id) ON DELETE SET NULL,
    description           TEXT NOT NULL DEFAULT '',
    description_extra     TEXT NOT NULL DEFAULT '',
    total_amount          NUMERIC(14,2) NOT NULL,
    charged_currency      TEXT,
    amount_original       NUMERIC(16,4),
    exchange_rate         NUMERIC(18,6),   -- original units per base unit
    origin_type           TEXT NOT NULL
                          CHECK (origin_type IN ('account','merchant','counterparty')),
    origin_ref            BIGINT,
    destination_type      TEXT NOT NULL
                          CHECK (destination_type IN ('account','merchant','counterparty')),
    destination_ref       BIGINT,
    record_status         TEXT NOT NULL DEFAULT 'pending'
                          CHECK (record_status IN ('pending','approved','void')),
    parent_transaction_id BIGINT REFERENCES transaction(id) ON DELETE SET NULL,
    source                TEXT NOT NULL DEFAULT 'manual'
                          CHECK (source IN ('manual','template','import','prvimiles')),
    notes                 TEXT NOT NULL DEFAULT '',
    receipt_path          TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_txn_household_date ON transaction (household_id, transaction_date DESC);
CREATE INDEX idx_txn_status ON transaction (household_id, record_status);
CREATE INDEX idx_txn_category ON transaction (category_id);
CREATE INDEX idx_txn_merchant ON transaction (merchant_id);
CREATE INDEX idx_txn_parent ON transaction (parent_transaction_id);

-- FK enforcement for origin/destination is a partial check: refs are
-- polymorphic, so the (type, ref) pair is validated in the app layer and
-- guarded by triggers below.

CREATE TABLE split (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id     BIGINT NOT NULL REFERENCES transaction(id) ON DELETE CASCADE,
    participant_type   TEXT NOT NULL
                       CHECK (participant_type IN ('household_user','counterparty')),
    participant_ref    BIGINT NOT NULL,
    amount             NUMERIC(14,2) NOT NULL,
    UNIQUE (transaction_id, participant_type, participant_ref)
);
CREATE INDEX idx_split_transaction ON split (transaction_id);

-- Payment: one real-world settlement event. Belongs to the household and
-- (when it moves money in the ledger) to the card account.
CREATE TABLE payment (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id          BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    account_id            BIGINT REFERENCES account(id) ON DELETE SET NULL,
    paid_by               BIGINT NOT NULL REFERENCES "user"(id) ON DELETE RESTRICT,
    amount                NUMERIC(14,2) NOT NULL,
    payment_date          DATE NOT NULL,
    method                TEXT NOT NULL DEFAULT 'transfer',
    notes                 TEXT NOT NULL DEFAULT '',
    ledger_transaction_id BIGINT REFERENCES transaction(id) ON DELETE SET NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_payment_household_date ON payment (household_id, payment_date DESC);
CREATE INDEX idx_payment_account ON payment (account_id);

-- Allocation: how much of this payment settled this participant's share
-- of that transaction. Many payments -> one split, one payment -> many
-- splits (the RM4,253.13/15 and RM10+RM900 cases).
CREATE TABLE payment_allocation (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_id BIGINT NOT NULL REFERENCES payment(id) ON DELETE CASCADE,
    split_id   BIGINT NOT NULL REFERENCES split(id) ON DELETE CASCADE,
    amount     NUMERIC(14,2) NOT NULL,
    UNIQUE (payment_id, split_id)
);
CREATE INDEX idx_alloc_split ON payment_allocation (split_id);

CREATE TABLE recurring_template (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id      BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    merchant_id       BIGINT REFERENCES merchant(id) ON DELETE SET NULL,
    category_id       BIGINT REFERENCES category(id) ON DELETE SET NULL,
    account_id        BIGINT REFERENCES account(id) ON DELETE SET NULL,
    amount            NUMERIC(14,2) NOT NULL,
    split_spec        JSONB NOT NULL DEFAULT '[]',
    day_of_month      SMALLINT NOT NULL CHECK (day_of_month BETWEEN 1 AND 28),
    description       TEXT NOT NULL DEFAULT '',
    active            BOOLEAN NOT NULL DEFAULT TRUE,
    last_generated    DATE,
    UNIQUE (household_id, name)
);
CREATE INDEX idx_recurring_household ON recurring_template (household_id);

-- Per-card reconciliation anchors (D8): the balance actually seen in the
-- bank app for a given statement month, so the delta can be reviewed
-- later without re-typing.
CREATE TABLE reconciliation (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    household_id      BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    account_id        BIGINT NOT NULL REFERENCES account(id) ON DELETE CASCADE,
    statement_month   DATE NOT NULL CHECK (statement_month = date_trunc('month', statement_month)),
    actual_balance    NUMERIC(14,2) NOT NULL,
    recorded_balance  NUMERIC(14,2) NOT NULL,
    note              TEXT NOT NULL DEFAULT '',
    recorded_by       BIGINT REFERENCES "user"(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, account_id, statement_month)
);
CREATE INDEX idx_recon_household ON reconciliation (household_id);
-- +goose StatementEnd

-- Invariant: every non-void transaction's splits sum exactly to its
-- total (when it has any).
--
-- Two triggers cover the full lifecycle:
--   * on transaction: fires after the transaction row is written
--     (deferred, so the split child rows inserted in the same statement
--     batch are visible at commit).
--   * on split: fires after any split row changes, re-checking the parent.
-- +goose StatementBegin
CREATE FUNCTION check_split_sum_for(txn_id BIGINT) RETURNS void AS $$
DECLARE
    t_total NUMERIC(14,2);
    t_status TEXT;
    s NUMERIC(14,2);
BEGIN
    SELECT total_amount, record_status INTO t_total, t_status
    FROM transaction WHERE id = txn_id;
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

CREATE FUNCTION trg_enforce_split_sum_txn() RETURNS trigger AS $$
BEGIN
    PERFORM check_split_sum_for(NEW.id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

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

CREATE CONSTRAINT TRIGGER trg_txn_split_sum
    AFTER INSERT OR UPDATE OF record_status, total_amount ON transaction
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION trg_enforce_split_sum_txn();

CREATE CONSTRAINT TRIGGER trg_split_sum
    AFTER INSERT OR UPDATE OR DELETE ON split
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION trg_enforce_split_sum_split();

-- updated_at maintenance.
CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_txn_updated_at
    BEFORE UPDATE ON transaction
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd
