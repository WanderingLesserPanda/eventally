# D34 — Auto-sync and detach

Settled in the 2026-11-18 grill-with-docs session (four rounds + final
confirmations). Status: **DESIGNED — awaiting sign-off**.

Builds on shipped D33 (`6330c49..f3c6f00`): the centralized `syncUpsert`,
`source='sync'` + plain source pointers, the `pending` workflow, the
one-way void cascade, the A-side "synced by [member]" badge. No D33 decision
is re-opened; this wave fills the seams D33 deliberately left.

## §A Auto-sync — per-member "always sync" toggle

- **Per member (Q2, settled).** The toggle is a setting of a member of the
  *target* household B: "while I'm in B, always sync my records from A". It
  covers the member's **Related records** in A only (D33's settled scope:
  recorded by them, split party, or payer) — never records they are not
  involved in.
- **Actor semantics.** The toggle owner is the sync's actor: the copy's
  `recorded_by_user_id` = toggle owner; amount = their split share (D33);
  the A-side "synced by [member]" badge (shipped `f3c6f00`) shows them.
  Nothing is invented for the actor.
- **Conflict.** Two members of B both have the toggle on for A: the copy is
  unique per (B, source record) — the first upsert creates it, the later
  upsert re-syncs it. The trigger processes members in `user.id` order, so
  last-writer is deterministic. This is the existing re-sync semantics, not
  new machinery.
- **Trigger (Q1, settled — ADR-0008).** Write-time, best-effort, after the
  source record's own commit: after a create/edit (`saveTxn` path) or a void
  (existing cascade) of a record in A, one centralized trigger function
  runs the upsert into every target household that has a toggle-on member in
  the record's Related slice. A trigger failure never fails the member's own
  save. A missed copy shows in the Live view with a **"syncing…"** chip
  (a state in motion, per the user — not a static "not yet synced") with the
  manual Sync button on the same row as the fallback.
- **Catch-up (Q18, settled).** Switching the toggle **ON** immediately runs
  the same upsert over the currently-syncable backlog (the member's Related
  slice in A). The backlog is part of turning the setting on. This **retires
  the parked "bulk sync" item** — no separate bulk-sync button is built.
- **Arrival (Q3, settled).** Auto-synced copies arrive as **`pending`** —
  the path D33 reserved for this wave. `syncUpsert` gains a status
  parameter: `'approved'` (manual sync, unchanged) vs `'pending'` (auto).
  The category is pre-filled from the mapping (§B) when a rule matches,
  otherwise the copy carries **no category** — the review pop-up forces one.
  Every auto-synced record requires manual approval; human intervention is
  the point.
- **Ongoing source changes.** Source edit → the upsert's re-sync branch:
  money facts (amount/date) refresh, classification and status kept (a
  pending copy stays pending until B approves; an approved one stays
  approved). Source void → the existing one-way cascade voids *linked*
  copies (now filtered by `is_synced = true`, §D). A synced copy never
  becomes a source itself (it has no split rows, so no member holds a
  share in it — the syncability rule excludes it): **no transitive sync**.

## §B Category mapping and the mandatory-category rule

- **Mapping table (Q3/Q12/Q13, settled).** `category_mapping`: one rule per
  (target household, source household, source category) → target category.
  A member maintains *their own* rules, created from B's context.
  No cross-household FK: `source_household_id` and `source_category_id`
  are plain BIGINTs validated app-side (isolation convention); the
  `target_category_id` FK is household-local.
- **Management surface (Q13).** Settings, a **dedicated section** (its own
  section on the /settings page), full create / edit / delete, rules
  grouped by source household. Source categories for the create form come
  from the named source-category-list read (§"Routes and surfaces").
- **Save-as-mapping (Q12, corrected in Q16).** The single-record approval
  pop-up offers an optional "remember this mapping" (source category →
  picked target category). It is **not** offered in the bulk modal
  (user: prevent UI bloat and mix-up).
- **Universal category rule (Q9/Q15, settled).** A record without a
  category can never be *approved*:
  - Review pop-up (single record): approving **any** pending row missing a
    category — any source, manual/template/import/sync — forces a category
    pick first. Same picker as the record form, **any category level**
    (Q10: no sub-category requirement).
  - Bulk-approve modal (§C): lists every selected row; missing ones are
    blank and required.
  - **Creation forms**: the manual record form requires a category for
    every class (expense/income/transfer), and the recurring-template form
    requires a category at template creation.
  - **Legacy rows** (existing records with no category): **no backfill** —
    we do not invent categories for real data. They render with an
    explicit "no category" marker and self-heal: the edit form now requires
    one, so the next edit assigns it.
- **Park (Q10, user-requested).** The category/sub-category MANDATORY
  *policy* (forcing a leaf level, etc.) is parked — the user has a future
  idea. Anticipated rework, kept cheap by design: the "category is required"
  validation lives in **one shared helper** used by the manual form, the
  recurring-template form, and the review pop-up, so a future
  sub-category-mandatory rule changes one place. Do not build the policy
  in this wave.

## §C Review queue

- **Surface (Q4, settled).** The existing `/review` page + `/review/bulk` —
  no new page. Auto-synced pending rows appear there. Party-only
  visibility is unchanged (D33): the toggle owner sees their own copies;
  which matches whose budget they feed.
- **Actions.** **Approve** (category editable) or **void**. Void is a safe
  "reject": the cascade is one-way (verified in `voidTransaction`), so
  voiding a pending copy can never touch the A-side original, and it frees
  the pointer so the same source can be re-synced fresh later. There is no
  "reject and keep pending" state.
- **Bulk workflow (Q16/Q17, settled, revised 2026-11-18).** Checkbox
  selection → **Approve** → the modal appears when at least 1 selected
  record is a currently-synced record (`is_synced = true`), and it lists
  **only those sync records**: mapped/pre-filled categories editable,
  missing ones blank and required, no save-as-mapping here. The modal is
  the sync-review step — it does not apply to the household's own
  records: non-sync selected records are approved straight through with
  their existing categories. A non-sync record missing a category (only
  possible for legacy rows, since creation now requires one) cannot ride
  the fast path — it is **skipped and reported** (the existing
  `/review/bulk` pattern) and must be approved individually through the
  single-row pop-up. **Apply** → a summary of exactly what will commit
  (the sync rows with their final categories, plus the straight-through
  rows) → **Confirm** → one commit. All-or-nothing: cancelling at any
  stage approves nothing; skipped rows are reported, never silent.
- **Endpoint shape.** `/review/bulk` and the single-row
  `/txns/{id}/status` gain an optional per-row category update (a row
  without a category is a 400 with the pop-up cue until one is supplied).
- **Budget gate (Q8, settled).** The dashboard/budget query excludes rows
  with `is_synced = true AND record_status = 'pending'`: an import counts
  in B's budget only once a human approves it. Manual pending rows still
  count — they are the household's own in-progress facts.

## §D Detach and re-link

- **Model (Q5, settled — ADR-0007).** Detach is a **state flag**, not a
  source change. New columns on `"transaction"`:
  `is_synced BOOLEAN NOT NULL DEFAULT false` (backfilled `true` for every
  current `source='sync'` row) and `detached_at TIMESTAMPTZ NULL`.
  `source` stays the pure birth marker (`'sync'` forever on a synced row);
  the source pointers stay; the existing bidirectional CHECK and the
  per-household UNIQUE are untouched. Every "is this row currently
  synced?" key point re-keys from `source='sync'` to `is_synced`:
  the edit lock, the "synced ✓" badge on /txns (list + detail),
  `aSyncedBy`, the void cascade, and the §C budget-gate predicate.
- **Detach** = one UPDATE (`is_synced := false`, `detached_at := now()`).
  The record becomes **fully ordinary** (Q7, settled): amount, date,
  class, endpoints, category — everything — freely editable (the edit lock
  keys off the flag). A History row is written ("detached from source …",
  and the log line may name the source household — it is B's own log). The
  A-side badge drops off (`aSyncedBy` filters `is_synced = true`); the
  un-sync trace convention is unchanged (detach is a History event, not a
  deletion-log entry — nothing is deleted).
- **Re-link** = the upsert's existing "found the row" branch detects
  `is_synced = false`: flip the flag back, clear `detached_at`, reset
  amount/date from the source, keep classification/status/merchant/notes.
  The re-link click carries the consequences modal (amount and date reset
  to the source's values; the edit lock returns). **A second record is
  never created** — the UNIQUE constraint makes the 2026-11-17 "two records
  from one source" consequence structurally impossible.
- **Cascade filter.** The void cascade gains `AND is_synced = true`:
  voiding the source no longer touches a *detached* copy (it is B's own
  record now). Linked copies still cascade-void as shipped.
- **Surface (Q6, settled).** "Detach from source" action on the B-side
  copy's **detail page**, gated by the **Edit grant in the target
  household** (a modification, not a deletion — no delete-grant required),
  with the D30-style consequences modal. Re-link uses the existing manual
  sync path (`POST /sync`). The existing delete path remains "un-sync";
  no dedicated un-sync endpoint.

## Schema changes (migration `000018_auto_sync_detach.sql`)

- `"transaction"`: + `is_synced BOOLEAN NOT NULL DEFAULT false`,
  + `detached_at TIMESTAMPTZ NULL`; backfill
  `UPDATE "transaction" SET is_synced = true WHERE source = 'sync';`.
- New table `auto_sync` (row existence = toggle ON):
  `member_user_id BIGINT NOT NULL` (plain — app-validated membership),
  `source_household_id BIGINT NOT NULL`, `target_household_id BIGINT NOT
  NULL REFERENCES household(id) ON DELETE CASCADE`,
  `UNIQUE (member_user_id, source_household_id, target_household_id)`,
  indexes for the trigger (`source_household_id`) and the settings page
  (`member_user_id, target_household_id`).
- New table `category_mapping`: `id` identity,
  `household_id BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE`
  (the target household that owns the rule),
  `source_household_id BIGINT NOT NULL` (plain — isolation),
  `source_category_id BIGINT NOT NULL` (plain — app-validated),
  `target_category_id BIGINT NOT NULL REFERENCES category(id) ON DELETE
  CASCADE`, `UNIQUE (household_id, source_household_id, source_category_id)`.
- **Unchanged:** the bidirectional pointers CHECK, `uq_txn_sync_source`,
  `transaction_source_check`, the cascade-lookup index. The migration is
  purely additive.

## Routes and surfaces

New routes (all authed, all CSRF-checked):

- `POST /sync/auto` — the toggle: `{source_household_id, on}`; upserts or
  deletes the caller's `auto_sync` row; `on` also runs the catch-up sweep.
- `POST /settings/mapping` — create a mapping rule.
- `POST /settings/mapping/{id}` — update a rule's target category
  (the source pair is immutable: edit = delete + create).
- `POST /settings/mapping/{id}/delete` — delete a rule.
- `POST /txns/{id}/detach` — detach (Edit-gated, consequence modal).

Extended routes: `POST /review/bulk` and `POST /txns/{id}/status` (optional
per-row category update); `POST /sync` (manual — gains the re-link branch
inside `syncUpsert`); `/settings` renders the mapping section; `/review`
renders the checkbox selection + the two-stage modal; `/sync` Live view
renders the "syncing…" chip and the household-detail modal.

Named cross-household surfaces — D33's four, unchanged, plus three new
(the isolation probe is extended to all seven):

1. `/sync` GET (Live view)
2. `/sync` POST → `syncUpsert` (now also the auto trigger's and re-link's
   only write path)
3. the void-path cascade (gains `AND is_synced = true`)
4. `aSyncedBy` badge read (gains `AND is_synced = true`; reads only
   pointers + `recorded_by_user_id`, never a household name)
5. **the auto-sync trigger**: reads the caller's `auto_sync` rows (which
   live in the target household's context) from the source household's
   write path and evaluates the Related slice — then writes only through
   surface 2
6. **the source-category list**: Settings' mapping form lists the source
   household's category names (create/edit)
7. **the household detail**: the Live view's household modal shows the
   source household's name and members (the contextual home of the toggle)

## Verification (house style)

- **Go tests** — `internal/web/d34_test.go`, table-driven, seeded Postgres,
  HTTP assertions (house style: `d33_test.go`, `d30_test.go`):
  - toggle round-trip via `POST /sync/auto` (Live view modal) and the
    settings section — same row, same state;
  - write-time trigger: a source create in A produces a **pending** copy in
    B with the mapped category (or none) — no page load in between;
  - catch-up: toggle-ON syncs the existing syncable backlog;
  - two-member conflict: deterministic last-writer, one row;
  - single-record approval pop-up: missing category → rejected until
    supplied; save-as-mapping writes the rule;
  - bulk modal: surfaces only when ≥1 selected row has `is_synced =
    true`, and lists only those rows (pre-filled editable, missing
    required); non-sync rows commit straight through; a legacy non-sync
    row missing a category is skipped + reported; all-or-nothing commit;
  - budget gate: a pending synced copy does not move the dashboard total,
    the same row counts after approval; a manual pending row counts;
  - detach: edit lock releases (amount/date/class/endpoints editable),
    History row, badge gone, void cascade skips the detached row;
  - re-link: money facts reset, classification kept, badge back, still
    exactly one row;
  - no transitive sync: editing a copy triggers nothing.
- **Smoke** — `smoke-d34.sh`, HTTP on a scratch DB, small scenario-dense
  synthetic seeds (2 households, 3 members, a handful of records and
  categories — never bulk data): the full journey — record in A → pending
  copy in B without any page load → map a category → approve through the
  modal → budget moves → detach → edit freely → re-link.
- **Rehearsal** — `rehearse-d34.sh`: the cutover rehearsal, run exactly once
  on a **copy** of the live DB — applies `000018`, verifies the backfill
  (`is_synced = true` count equals the `source='sync'` count), boots the
  app, runs the smoke against the copy.
- **Isolation probe** — `TestD34IsolationProbe` (extends D33's):
  cross-household reads occur only on the seven named surfaces; the
  A-side badge absent after detach, present after re-link; "syncing…" chip
  only on auto-sync targets whose copy is absent; no household-name leak
  beyond surface 7's allowed direction (a B member sees A's members; an
  A-only member still learns nothing about B).
- Definition of done: `go build ./...`, `go vet ./...`, `go test ./...`
  green; `smoke-d34.sh` green on scratch; `rehearse-d34.sh` green on the
  copy. Live-DB migration only after the rehearsal plus explicit green
  light plus a fresh template-DB backup (standing gate).

## Parked (explicitly out of scope for this wave)

- **Category/sub-category mandatory policy** (Q10 — user has a future
  idea): any level is acceptable for now; the requirement validation is
  centralized in one helper so the policy change later touches one place.
  Do not build the policy.
- **"Bulk sync" button**: retired by the toggle-ON catch-up (Q18) — do not
  build a separate bulk-sync surface.
- Everything previously parked and untouched: personal budget,
  cross-household running balance, multi-currency, the D31/D30 parked sets,
  D32, D15.
