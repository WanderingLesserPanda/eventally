# D33 — Cross-household synced records

Status: **SHIPPED** (decisions signed off 2026-11-17; implemented exactly as
signed — migration 000017, the centralized sync upsert, the named
cross-household surfaces, void cascade, edit lock, verification per the
section below). 2026-11-17 follow-up (same day, signed): the A-side
"synced by [member]" badge — a fourth named cross-household read.
Scope: a member who belongs to several households syncs, one record at a time,
their share of a related record from another household into the current
household, where it counts against the current household's category budgets
(D10). The surface is the **Live view** page plus the **sync** action; the
synced record is an ordinary household transaction on a party endpoint
(ADR-0006).
Every decision below was settled in the interview; do not re-litigate.

Known future rework (user-raised in the interview, out of scope here): the
auto-sync wave — "always sync records from this household" toggle, bulk sync,
parent-category mapping as the default classification, and a review/approve
queue (auto-synced records landing `pending`). Consequence baked into this
design: the sync action is ONE centralized upsert function that both the
manual button and the future auto-sync call; the `source='sync'` marker +
source pointers are the seam; the existing pending/approved workflow is the
queue; the mapping table ships with that wave, not this one.

Background: budgets are household property (`category.budget_per_month`, D10).
A user in two households (D28: global identity) wants the records "involving"
them in the other household to count against their own household's budgets.
This supersedes the 2026-10-23 option analysis (A: household→household share
link, B: user-scoped personal surface, copy variants): the consumer is the
user's OWN household's category budgets, and the mechanism is a per-record
synced copy. The user-tied **Personal budget** concept was explicitly parked
as an Open Item (see glossary) — the MVP budget concept stays household-based.

## §A Live view — syncable records from other households

- Route: `GET /sync`, nav label "From other households". The nav link is
  hidden when the logged-in user holds no other household membership (the
  page renders an empty state if reached directly).
- Content: for each of the user's other households, the user's Related slice
  (the centralized `relatedSliceSQL`), filtered to:
  - `class = 'expense'`,
  - `record_status <> 'void'`,
  - the user holds a `transaction_split` row in it with `amount > 0`.
  The third filter is the **syncability rule**: the view lists only records
  whose share can be synced. Records the user merely recorded or funded a
  settlement leg on are NOT listed here — that involvement stays visible in
  the source household's own /txns, where the full Related slice applies.
- Each row shows: source household name, date, description, the source
  category (reference only), the user's share (their split amount), and sync
  state:
  - **unsynced** → a Sync action (small form: category select — required,
    the current household's expense categories; merchant select — optional).
  - **synced** → a "synced" indicator linking to the local record
    (`/txns/{id}`); a **"source changed"** flag when the source's
    amount/date no longer match the local snapshot (derived at render time,
    not stored).
- The page handler is the wave's only cross-household READ surface: every
  read of another household's tables is centralized here.
- The page grants no household-level visibility into the source household:
  the user sees only their own related records there.

## §B Sync action and synced record

`POST /sync {source_household_id, source_txn_id, category_id (required),
merchant_id (optional)}`

Server-side re-validation: the caller holds a membership in the source
household; the record is still syncable (not voided, share > 0); the caller
holds the current household's create right (the same gate as creating any
record there).

The ONE centralized upsert function (the seam for the parked auto-sync wave)
then:

1. Resolves the source row (the only cross-household read in this handler).
2. Ensures the **source-household party row** exists in the current
   household: `name` = the source household's name, `type = 'organisation'`,
   `app_user_id` NULL — upserted on the party table's
   `(household_id, name)` uniqueness. This is the row the synced record's
   origin points at (one per source household, so "From" distinguishes them
   if the user ever belongs to three).
3. Upserts the synced record keyed by `(household_id, source pointers)` —
   the UNIQUE constraint makes re-sync never create a second row
   (household-level uniqueness: a partner syncing the same source record
   re-classifies the existing row, they don't double-count the budget):
   - **first sync**: `class='expense'`, `total_amount` = the caller's split
     amount in the source (**the share, not the whole** — "only that is
     relevant to me"), `transaction_date` = the source's date,
     `origin_party_id` = the source-household party row, **no other
     endpoint, no split rows**, `category_id` = the chosen one,
     `merchant_id` = optional, `description` = the source's description
     (an editable starting point), `recorded_by_user_id` = the caller,
     `source='sync'`, `sync_source_household_id` + `sync_source_txn_id` set,
     `record_status='approved'` (choosing the category at sync time IS the
     review; the `pending` path is reserved for the parked auto wave).
   - **re-sync** (row exists — the "source changed" case): refresh
     `total_amount` and `transaction_date` from the source ONLY; category,
     merchant, description, status are preserved.
4. History (ADR-0005 path): `created` on first sync, `edited` on re-sync —
   standard write-time access snapshots, actor = the caller.

Consequences of the shape (each verified against the current code):

- Satisfies `chk_txn_class_endpoints` via `origin_party_id` — a party is a
  first-class endpoint (000010); the /txns form already carries party
  selects; imported real data contains party-origin rows that list/detail
  pages already render.
- **No split rows**: the deferred split-sum trigger passes (sum 0);
  settlement allocation is split-keyed (ADR-0004), so a synced record is
  **ineligible for settlement by data** (its money already moved in the
  source household — settling it here would fabricate a ledger flow); the
  person-outstanding and card-balance cards are untouched (they iterate
  splits of credit-card accounts).
- The dashboard's category-actuals query is **unchanged**: it sums visible,
  non-void, current-month expenses by category, and the synced row is an
  ordinary row of the chosen category. The budget card picks it up with zero
  query changes — that is the payoff of the "real transaction" choice.
- Visibility (D27 model): the syncer sees it (Related via
  `recorded_by_user_id`), owner/admin see it; other members in Related mode
  do not (a party-only record exposes no account surface). Matches the
  settled "visible to me and those allowed in my household".
- Editing: a synced record is editable through the normal /txns edit form
  except for money facts and structure — the edit handler rejects changes to
  `total_amount`, `transaction_date`, `class`, or the endpoints of a
  `source='sync'` row (400: "locked to source — un-sync and re-sync to
  refresh"); category, merchant, description, notes stay freely editable by
  anyone who may edit records in this household (that is how a partner
  re-classifies).
- **Un-sync**: the existing delete path (single/bulk delete under this
  household's delete grants; a Deletion-log row as usual). No dedicated
  endpoint.

## §C Void cascade — immediate

- In the centralized void path (single void and bulk void share it): when a
  record is voided, every record in **any** household whose source pointers
  target it is voided immediately. Each cascaded copy gets a standard
  History row (actor = whoever voided the source; timestamp; no reason
  field — the D31 void convention).
- Built as an **app-level cascade, not a database trigger**: ADR-0005
  requires every lifecycle event to carry a History row with actor; a raw
  trigger-void would have no home in either trail.
- The existing void-with-settlements guard (voiding is blocked while
  settlements exist) applies to the source exactly as today; the cascade
  fires only on a successful void.
- **Edits do not cascade.** The distinction that keeps the two cases
  straight: a void is a FACT (the record no longer exists, so the copy must
  stop counting — no judgment needed); an edit is a JUDGMENT about the
  user's share (re-allocation in the source), so the copy keeps its
  snapshot, the Live view flags "source changed", and re-sync refreshes.

## Schema changes (migration `000017_synced_records.sql`)

- `"transaction"`: `ADD COLUMN sync_source_household_id BIGINT`,
  `ADD COLUMN sync_source_txn_id BIGINT` — plain pointers, **no
  cross-household FK** (the isolation convention: no schema link between
  households).
- `CHECK ((source = 'sync') = (sync_source_household_id IS NOT NULL
  AND sync_source_txn_id IS NOT NULL))` — marker and pointers agree in
  both directions.
- The existing `source` CHECK gains `'sync'`.
- `UNIQUE (household_id, sync_source_household_id, sync_source_txn_id)` —
  household-level uniqueness; NULL-safe (Postgres treats NULLs as distinct
  in UNIQUE, so non-sync rows are unaffected).
- `INDEX (sync_source_household_id, sync_source_txn_id)` — the cascade
  lookup.
- No party-table changes: `type 'organisation'` already exists, and names
  are unique per household.

## Routes and surfaces

- `GET /sync` — Live view page (new `sync.html` template; nav item "From
  other households", hidden when the user holds no other membership).
- `POST /sync` — sync / re-sync (fields above; CSRF-checked like every
  POST).
- Un-sync and re-classification ride the existing /txns delete and edit
  routes.
- Unchanged: /txns list/detail, dashboard, settlement, /history,
  /deletion-log — and, in particular, **zero changes on the source
  household's side** (the A-side "synced by [member]" badge is parked).

## Isolation

The wave adds exactly three named, centralized cross-household surfaces:
the `/sync` GET handler (read), the `/sync` POST handler (read), and the
void-path cascade (write). The 2026-11-17 follow-up adds a fourth — the
A-side badge read (`aSyncedBy` in sync.go: the copy rows' `sync_source_*`
pointers + `recorded_by_user_id` only; the target household's name is
never selected, so an A-only member cannot learn where the copy went).
The extended D30-style probe asserts:

- no B-context query touches another household's tables outside those named
  surfaces;
- no source-household-context query touches the current household's tables
  at all (the source side's only new content is the A-side badge itself,
  which names members, never households);
- the visibility negatives hold: a non-syncer member in Related mode cannot
  see the synced row on /txns; a member with no other household gets an
  empty /sync; non-syncable related records never list.

Every other B query (dashboard, /txns, settlement, history, deletion log)
treats a synced row as any other B row — no new leak path is introduced.

## Verification (house style)

- **Go tests** (`internal/web/d33_test.go`, testPool harness, small
  scenario-dense synthetic seeds — never bulk data):
  - Seed: households H1 (owner P) and H2 (owner F); user U a member of
    both; member M in H1, Related mode only. H1 has budgeted categories
    Food & Drink > Coffee (100.00) and Drink (50.00). H2 has its own tree
    Food & Drink > Drink and a credit-card account; expense T1 "coffee
    beverage" 12.00 with split U 6.00 / F 6.00; T2 recorded by U with no
    U split; T3 with U having funded a settlement leg; none voided.
  - Live view: lists T1 only (syncable); T2 and T3 absent.
  - Sync: row created exactly as specified (`source='sync'`, origin party
    = H2's auto-created party row, total 6.00, approved, no splits, no
    account endpoints); duplicate sync is a no-op (uniqueness); re-sync
    after a source amount change refreshes amount/date and preserves the
    category.
  - Budget: the dashboard's Coffee actual includes 6.00 for U and P; M's
    actuals exclude the row (visibility).
  - Edit lock: changing the synced row's amount → 400; changing its
    category → 200 (partner re-classification).
  - Void cascade: voiding T1 in H2 voids the H1 copy immediately, with a
    History row (actor = F); the budget actual drops.
  - Settlement: a synced row never appears as a settlement-allocation
    candidate; the person-outstanding card is unchanged.
  - Un-sync: deletion via the normal path writes a Deletion-log row; the
    pointer frees (re-sync possible).
  - Isolation: the extended-probe rows above.
- **HTTP smoke** (`smoke-d33.sh`, scratch DB, throwaway — dropped by trap):
  nav-link visibility per membership; /sync GET rows + states; POST /sync
  with a category (row appears on /txns for U and P, absent for M);
  dashboard card numbers; duplicate POST no-op; void in H2 → cascade in H1
  + History row; "source changed" flag + re-sync; edit-lock 400; delete →
  Deletion log; /sync empty state for M.
- **Cutover rehearsal** (`rehearse-d33.sh`): `eventally_dev` copied via
  `TEMPLATE`, app boots (auto-migrates v16 → v17), checks: goose version,
  zero synced rows in the real data (the migration is a no-op on existing
  rows), existing dashboard/budget totals unchanged, the existing smoke
  suite green, probe passes. **Live `eventally_dev` migration only after
  the rehearsal passes, an explicit user green light, and a fresh
  template-DB backup** (standing gate).

## Parked (explicitly out of scope for this wave)

- **Personal budget** (a user-tied budget spanning the user's households) —
  user-parked as an Open Item for future re-consideration; the MVP budget
  concept stays household-based.
- **The auto-sync wave** (user-raised in the interview): "always sync
  records from this household" toggle, bulk sync, parent-category mapping as
  the default classification for auto-synced records, and the review/approve
  queue (auto-synced records land `pending` for the user to approve). Seams
  kept by this design: the single centralized sync upsert, the
  `source='sync'` + pointer columns, the existing pending workflow.
- **A-side badge** ("synced by [member]" on the source record) — **done**
  in the 2026-11-17 follow-up (one named cross-household read, `aSyncedBy`,
  + the isolation-probe extension it required; member name only, never the
  target household's name; render-time, so un-syncing a copy removes it).
- **A running balance between households** ("what the other household owes
  me") — the old who-owes-whom idea; a separate future feature.
- Multi-currency (both households are MYR; D13/D18 policy unchanged).
- Pre-existing parked items stay parked (D32 icon picker, D15 dashboard
  layout, statement reconciliation, etc.).
