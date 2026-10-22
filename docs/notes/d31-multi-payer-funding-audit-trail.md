# D31 — Multi-payer funding + audit trail

Status: **DESIGNED — awaiting sign-off** (grill-with-docs session 2026-10-22).
Scope: two parked enhancements plus one absorbed item — multi-payer funding of
settlements (item 3), bulk-void auditing (item 4) and the general
transaction-history audit trail (item 5), the latter two delivered as the
**History** trail with deletions staying in the D30 **Deletion log**.
Every decision below was settled in the interview; do not re-litigate.

Known future reworks (user-stated, out of scope here): a large permissions
rework and an audit-UI redesign. Consequence baked into this design: all
settlement permission logic stays centralized in `perm.go` surface helpers,
and audit write/read paths stay cleanly separated, so both reworks touch one
place each.

## §A Multi-payer funding

A settlement is a card-debt paydown. Today it has exactly one payer
(`paid_by_user_id`) and at most one optional D6 ledger transfer from one
source account. D31 generalizes the *funding side* only: one settlement may
carry **several funding legs**; each leg is a payer + optional source
account + method + amount, and the settlement's amount is the sum of its
legs. A single *purchase* paid from two origin accounts stays out of scope
(parked — different wave; it would change the transaction model).

- **A1 Schema (migration 000016, per ADR-0004):**
  - New table `settlement_funding`: `id`, `household_id` (→ household
    CASCADE), `settlement_id` (→ settlement CASCADE), `user_id` (→ app_user
    RESTRICT — the payer FK guard moves here), `source_account_id` nullable
    (→ account SET NULL, consistent with `settlement.account_id`), `method`
    (TEXT, the payMethods list), `amount` NUMERIC(14,2) CHECK > 0.
    Indexes: `(settlement_id)`, `(user_id)`. No uniqueness constraint —
    leg identity is row order; the app merges duplicates at the form level.
  - **Backfill** exactly one leg per existing settlement: payer = old
    `paid_by_user_id`; source = the linked D6 transfer's
    `origin_account_id` when `ledger_transaction_id` is set, else NULL;
    method = old `settlement.method`; amount = `settlement.amount`.
  - **Drop `settlement.paid_by_user_id` and `settlement.method`** (method
    moves to the leg — mixed funding is the point of the feature); drop the
    old payer index. `settlement.amount`, `payment_date`, `notes`,
    `ledger_transaction_id` stay (one event = one date).
  - Invariant `settlement.amount = Σ legs`: app-enforced in one DB
    transaction (the pay flow writes settlement + legs + transfers
    atomically) — no partial state possible.
- **A2 Ledger transfers (per leg):** each leg **with** a source account
  writes its own D6 transfer (that leg's amount, the payment date,
  description `Card payment: <name> (leg N of M)`); a leg without a source
  is out-of-band (cash etc.) and writes nothing. `cardRecordedBalance`
  changes accordingly: it subtracts the sum of **out-of-band legs** (no
  source account) instead of whole ledger-less settlements.
- **A3 Permissions — the v1 matrix (full image: vault Inbox
  `2026-10-22_mpf-permission-matrix.png`):**
  - **Create**: Create grant on **every** account in the surface — the card
    family (all family accounts) **and the source account of every leg
    actually used** (a leg's source the actor can't touch → 403 naming the
    account). Owner/admin: always. `canCreateSettlement` is unchanged in
    shape — the pay handler simply builds the surface from all legs.
  - **Read**: unchanged surface — the settlement's own card account + the
    card family behind its allocated splits; All-view short-circuits;
    Related-view requires the slice: payer of **any leg** (Δ — today: the
    single payer) or a split party in an allocated split.
  - **Delete**: unchanged — a Delete grant on the card family (any family
    account, existing ANY semantics), then the mode: All passes, Related
    requires the slice (payer of any leg / split party).
  - **Related slice** (`relatedSliceSQL`): the "settlement payer" clause
    becomes "payer of any leg" (EXISTS over `settlement_funding`).
  - **Leg source accounts** are part of the *create* surface only — never
    of the read/delete surface (the existing D6 decision, per leg).
  - All five existing payer query sites re-point to legs: `relatedSliceSQL`,
    `canSeeSettlement`, `cardPayments` (display), reconcile payment list +
    balance, txn-detail payment display.
- **A4 Pay form UX (agreed; user will check production behaviour first):**
  `/cards/{id}/pay` gains a **Funding** section with dynamic leg rows
  (add/remove). Each row: payer picklist, source-account picklist (optional
  — "out-of-band" when empty), method picklist, amount. Defaults reproduce
  today's single-payer flow exactly: one pre-filled leg (actor, full
  amount, ledger write ON with source required). Validation: every leg
  amount > 0; legs sum to the payment amount; per leg, ledger write ⇔
  source selected. **Legs are locked at recording** — a settlement stays
  create-or-delete (no edit route), as today.
- **A5 Settlement delete**: allocations and legs cascade with the
  settlement; each leg's D6 transfer remains as a separate transaction
  record, exactly as today's single transfer does.

## §B History — the new audit trail

- **B1 Scope v1:** lifecycle events of the two money-record types —
  transaction **created / edited / voided** (single and bulk voids alike)
  and settlement **created / deleted**. Deletions of transactions are NOT
  in History — the Deletion log (§C) owns them; each event has exactly one
  home. Household-administration events (grants, membership, ownership,
  account lifecycle, templates, receipts) stay parked.
- **B2 Schema (000016):** `txn_history`: `id`, `household_id` (CASCADE),
  `entity_type` CHECK `transaction|settlement`, `entity_id`, `action`
  (`created|edited|voided|deleted`), `summary` (rendered text — e.g.
  `amount 100.00 → 120.00 · status pending → approved`, built by comparing
  the form's before/after values; no JSON), snapshot columns for stable
  display (`name`, `event_date`, `amount`), **access-snapshot columns for
  per-row visibility** (`account_ids BIGINT[]` — the record's surface
  accounts at event time; `related_user_ids BIGINT[]` — the people in the
  record's Related slice at event time), `actor_id` (→ app_user SET NULL),
  `created_at`. Indexes: `(household_id, created_at DESC)`,
  `(entity_type, entity_id)`. Settlement `created` summaries list the legs
  ("600 from <source> by <payer> + 400 from <source> by <payer>").
- **B3 Per-row visibility (ADR-0005):** a member sees a row only if, at the
  event time, they could have seen the record: owner/admin see every row;
  otherwise the viewer needs a read grant on **some** account in
  `account_ids` **and** (view mode All on it **or** membership of
  `related_user_ids`). The snapshot — not live grants — is the source of
  truth, so the answer is stable after the record changes or disappears.
- **B4 UI:**
  - Per-record: collapsible **History** section on the transaction details
    page (`/txns/{id}`), that record's events newest first (no extra filter
    needed — reaching the page already implies visibility).
  - Household: new page **GET /history** — all History rows, newest first,
    columns when/who/entity/action/summary; "History" button next to the
    Deletion-log button on /txns.
  - **POST /history/clear** — owner/admin only, confirmation modal, removes
    every row except tombstones, inserts one tombstone
    ("History cleared by X on <date>"); **no automatic expiry** (D30 house
    pattern).
- **B5 Write points:** every create/edit/void handler and the settlement
  pay flow write their `txn_history` row **in the same DB transaction** as
  the action (D30 house pattern for the deletion log).

## §C Deletion log — extended (kept separate, ADR-0005)

- **C1** The D30 `deletion_log` table **stays** and continues to own bulk
  transaction deletes (one row per attempted record, deleted/failed). It is
  extended, not re-created.
- **C2 Per-row visibility** — 000016 adds the same access-snapshot columns
  (`account_ids`, `related_user_ids`) to `deletion_log`; the shared
  visibility rule from B3 applies at render time. **Legacy rows** (written
  before 000016, empty snapshots) keep today's all-members visibility — no
  regression.
- **C3 Action deep-link:** `GET /deletion-log?op=<operation_id>` renders
  only that bulk action's rows (within the viewer's visibility). The
  post-bulk-delete flash links there ("…see the Deletion log for details"
  → `?op=N`), so a user lands exactly on the rows of *their* action.
  `operation_id` already exists (D30); this only adds the link + filter.
- **C4 Clear** unchanged: owner/admin, tombstone, no auto-expiry.

## Verification (house style)

- **Go tests** (`internal/web/d31_test.go`, testPool harness, small
  scenario-dense synthetic seeds — never bulk data):
  - legs: single-leg settlement equals today's row; two-leg settlement
    (sum invariant, per-leg D6 transfers, recorded balance subtracts only
    out-of-band legs); backfill equivalence (existing settlement → one leg,
    source derived from its transfer).
  - the permission matrix Δ rows (V3 view via leg-2 payer, D2 delete via
    leg-2 payer in Related mode, R3 related slice) plus the key negatives
    (C2 create 403 on an unowned leg source, V2 view with only a source
    grant).
  - History: rows written on create/edit/void with correct snapshots;
    visibility (a member sees their own record's rows, not another
    member's; owner/admin see all); clear leaves the tombstone.
  - Deletion log: `?op` filter returns one action's rows; new rows carry
    snapshots; legacy rows stay visible to all.
- **HTTP smoke** (`smoke-d31.sh`, scratch DB, throwaway — dropped by trap):
  pay-form GET defaults to one leg; POST two legs (transfers appear,
  outstanding reduces); validation failures (leg > 0, sum mismatch, ledger
  without source); /history page rows + per-record section;
  /deletion-log?op= rows; 403 paths; clear tombstone.
- **Cutover rehearsal** (`rehearse-d31.sh`): `eventally_dev` copied via
  `TEMPLATE`, app boots (auto-migrates v15 → v16), checks: goose version,
  `leg count == settlement count` (1:1 backfill), FK shape, settlement
  counts unchanged, page walk. **Live `eventally_dev` migration only after
  the rehearsal passes, an explicit user green light, and a fresh
  template-DB backup** (standing gate).

## Parked (explicitly out of scope for this wave)

- Purchase-level multi-funding (a purchase paid from two origin accounts).
- Void-reason field / modal (voids are logged with actor + timestamp only —
  user's idea, parked for a later wave).
- Audit of household-administration events (grants, membership, ownership
  transfer, account archive/restore/delete, template pause/resume, receipt
  upload/delete, reconciliation marks).
- The expected **permissions rework** and **audit-UI redesign** — both
  anticipated by the user; this design only ensures they stay cheap
  (centralized permission helpers, separated audit paths).
- Pre-existing parked items remain parked: email/SMTP invite relay, SSO.
