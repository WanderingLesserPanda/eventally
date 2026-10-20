# Schema conventions — Eventally

The rules and meanings baked into the database. Read this before writing or
modifying a migration or a query. Each rule says **what** and **why**, so a
future change can decide whether it's still right.

## Money

- All amounts are the **household base currency**, stored as
  `NUMERIC(14,2)` in Postgres and **integer cents** (int64) in Go
  (`internal/money`). Arithmetic is exact integer math; no floats ever touch
  an amount.
- `NUMERIC(14,2)` gives 12 digits before the decimal — 99 trillion units —
  plenty for a household, and the 2-decimal type keeps Postgres-side
  aggregation (SUMs) honest.

### Charged currency (foreign-currency transactions)

A transaction charged in another currency keeps three optional columns:

| column | meaning |
|---|---|
| `charged_currency` | ISO code the merchant charged in (e.g. `USD`) |
| `amount_original` | what the merchant charged, in that currency |
| `exchange_rate` | **original units per base unit** = `amount_original ÷ total_amount` |

So the round-trip is `total_amount = amount_original ÷ exchange_rate`
(`txn.go` computes `rate = orig / total` at save time).

> ⚠️ The rework plan's one-line summary said `total = original × rate` — that
> direction was wrong. The stored value is **originals per base** (e.g. a
> 100 USD charge at 316 MYR total stores 0.316, meaning 1 MYR ≈ 0.316 USD),
> so recover the base amount by **dividing**. Keep the column comment and
> this line in sync if the convention ever changes.

`total_amount` is always the base-currency figure that every split, card
balance and reconciliation uses. The original-currency columns are
display-only provenance; nothing aggregates on them.

## Signs

- A **positive** `total_amount` on an expense is money spent (or owed).
- A **positive balance on a credit-card (liability) account** is money
  *owed to the bank* — the card is a liability, so "the card balance" is
  debt, not cash. Asset accounts (savings, cash) use the same positive =
  what-you-have convention.
- Settlements (payments toward the card) and their allocations are positive
  amounts that reduce the outstanding.

## Status & lifecycle

- `record_status`:
  - `pending` = **household review queue** — the app's own "someone recorded
    this, has the household agreed?" state (D9). It is *not* bank
    clearing/settlement; bank-side timing is not modeled.
  - `approved` = agreed by the household; `void` = cancelled (kept for
    history; voided rows are excluded from balances and from recurring
    occurrence uniqueness).
- `party.is_active` / `merchant.is_active` / `category.is_active` — soft
  deletes. Inactive rows stay (foreign keys point at them); the UI hides
  them from *new* selections but old references still resolve to their name.
- A settlement's `payment_date` is the date the payment was made, independent
  of the transactions it allocates to.

## Identity & scoping

- **Every business row belongs to a household** via `household_id`, and the
  household is part of the foreign keys (`(household_id, parent_id)`), so a
  row cannot dangle into another household even if ids collide.
- `party` is the single "who" table: household members get their party row
  seeded automatically by the membership trigger, with `app_user_id` set;
  external people (shop assistants, landlords, friends) have
  `app_user_id = NULL`. `party.type` distinguishes `person`/`organisation`;
  `relationship_type` is free text (e.g. "friend", "landlord").
- Transaction endpoints are the four concrete columns
  `origin_account_id`, `destination_account_id`, `origin_party_id`,
  `destination_party_id` (Gate 3c). A transaction has **at least one**
  endpoint (legacy import rows may have only an origin account). A transfer
  is account→account; an expense/income is account→party or party→account.
  The old polymorphic `*_type`/`*_ref` columns are gone — don't reintroduce
  them.
- `transaction_split.party_id` always points at a party; the split-sum
  constraint (trigger `trg_txn_split_sum`) keeps `SUM(split.amount) =
  transaction.total_amount` per transaction, deferred to commit.

## Card / settlement invariants (Gate 4)

- **Per-split allocation cap.** `SUM(settlement_allocation.amount)` may never
  exceed the owning `transaction_split.amount`. Enforced by the *deferred*
  constraint trigger `trg_settlement_alloc_cap` (runs at commit, not per
  row) so re-allocation within one transaction is legal and two concurrent
  settlements that together overshoot fail atomically at commit with
  `check_violation`. The app maps that SQLSTATE (23514) to a plain-language
  "recorded at the same time" message instead of a raw 500.
- **Unapplied credit.** A settlement may be paid for *more* than its
  allocations (the payment form validates `Σ allocations ≤ amount`, not
  equality). The unallocated remainder is **unapplied credit**: it stays on
  the card (shown as the "Unapplied credit" stat and a `credit …` tag on the
  settlement) and is not a liability. Over-allocation of a single item's
  outstanding is rejected earlier, at form validation ("exceeds its
  outstanding"); the commit trigger is the backstop for races.
- **Base-currency lock.** `household.base_currency` may be changed only while
  the household has zero transactions (trigger
  `trg_household_currency`, migration 000011). Once any transaction exists,
  the base currency is the unit of account for all history, and changing it
  would silently reprice every amount — so the DB refuses it.

## Triggers & deferred checks (migration hygiene)

- Invariant checks that span rows (split sums, allocation caps) are
  **DEFERRABLE INITIALLY DEFERRED constraint triggers** — the check runs at
  commit against the final state. This is what lets the app and the import
  script do multi-row writes in one transaction without mid-write failures.
  Keep them deferred when adding siblings.
- Because of the deferred triggers, any script (import, test, migration data
  step) that writes `transaction` + `transaction_split` +
  `settlement_allocation` should do so **inside one transaction**: the
  checks then validate at COMMIT. If an intermediate state would be illegal
  (e.g. a split sum that only matches after the last row), that is exactly
  what deferral is for — do not add per-row immediate checks to "help". An
  aborted transaction rolls everything back, which is the intended safety.
- Multi-statement plpgsql bodies in goose migrations need
  `-- +goose StatementBegin` / `-- +goose StatementEnd`, otherwise goose
  splits the function body into separate statements and Postgres sees
  broken SQL.
- Migrations are `go:embed`ded into the binary — bump the version, rebuild.

## Misc conventions

- `external_source_id` = the id of a row in an external import source
  (spreadsheet export). Unique per household *where not null* — it is the
  idempotency key for re-running an import, not a business concept.
- `recurring_template` still carries the pre-Gate-3c polymorphic columns
  (`origin_type`/`origin_ref`/`dest_type`/`dest_ref`): due-generation maps
  them onto the concrete endpoint columns at INSERT time. If the template
  schema is ever reworked, keep the mapping in `recurring.go`, not in SQL.
- Session rows store a `csrf_token`; every POST compares it. Session ids are
  64-hex random values (see the auth notes); the token *value* in the cookie
  is the session id itself.

## Future requirements (considered, deliberately deferred)

One line each — what it is, why it's deferred. None of these are bugs; the
schema currently supports them, or doesn't need them yet.

- **Multi-currency balances** — per-account `currency_code` + live
  conversion on the dashboard; deferred because every amount today is base
  currency by construction and per-account conversion is a product
  decision, not a schema one.
- **Audit/history table + idempotency keys** — who changed what and when
  beyond `created_at`/`updated_at`; deferred until the app outlives its
  first year of data.
- **Multi-payer funding rows** — a settlement funded by several accounts;
  one payer per settlement covers the household.
- **Double-entry `ledger_entry`** — full bookkeeping with debit/credit
  pairs; the card family already models the entry the app needs.
- **Recurring start/end dates** — templates currently generate while active;
  explicit `starts_at`/`ends_at` when the UI asks for it.
- **Period-based budget table** — budgets keyed to (household, period,
  category); today's budget page aggregates by month in code.
- **`reconciliation.as_of_date`** — pin a reconciliation to a statement
  cutoff; today the month filter is UI-side.
- **Multi-receipt attachment table** — one `receipt_path` per transaction;
  many attachments when receipts need editing.
- **Household timezone / per-user prefs** — today one app-wide timezone.
- **Session-token hashing** — store a hash of the session id; deferred
  because the table is only reachable with an authenticated cookie.
- **Invitation + permission-enforcement UI, household picker at login,
  ownership transfer** — the *schema* (roles, `can_edit`, `record_scope`)
  landed in Gate 1; enforcement (blocking a read-only member's writes,
  filtering a scoped member's accounts) is deferred with the UI. Known gap,
  accepted: today all members act as full admins.
- **Renaming `transaction`** — decided **no** (it is a SQL keyword
  workaround already quoted everywhere; renaming buys nothing).
