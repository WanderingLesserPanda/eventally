# D30 wave: account archive, bulk record delete, ownership transfer

Decided 2026-10-22 (grill-with-docs session). Three parked features promoted:
**account archive** (deferred from D27), **bulk record delete** (new), and
**ownership transfer** (parked from D28). Each section lists the settled
decisions; hard-to-reverse ones carry an ADR (ADR-0003 for ownership transfer).

## A. Account archive

**Semantics.** Archiving sets `account.is_active = false`. The account keeps
all history (readable, and still referenced by existing transactions), moves
to the existing *"Inactive accounts — history keeps their references"*
section on the `/accounts` list, and is excluded from all new-activity
picklists (capture origin/destination, transaction forms, settlements,
recurring endpoints) — the existing `is_active` picklist filtering already
does this, so no new filtering code is required. The account can be
**restored** at any time. Archiving is a *separate action* from the existing
strict hard-delete (which stays, for the zero-history case).

**Effects of archiving an account:**

- Recurring templates on the account are **paused** (`is_active = false` —
  the existing pause mechanism; they show as paused on the recurring page).
  Due-date generation already skips paused templates, so no generation logic
  changes.
- On **restore**, paused templates **stay paused** — no auto-resume. The
  user re-enables them individually from the recurring page.
- Child accounts (credit-card `parent_id`): archiving the parent does **not**
  cascade by default. The modal notices *"N linked accounts will remain
  active"* and offers an optional checkbox **Also archive child accounts**;
  ticking it cascades the archive to each child (each child's own templates
  pause by the same rule). The checkbox is offered only when the actor holds
  Edit-All on every child (see Permission). **Restore is per-account only** —
  never cascades in either direction.

**Confirmation modals** (both are Confirm / Cancel):

- *Archive*: "No new transactions can be posted to this account · N
  recurring template(s) will be paused · All existing history is kept and
  stays visible · You can restore this account later" + (when children
  exist) "N linked accounts will remain active" + the optional cascade
  checkbox.
- *Restore*: "Transactions can be created again against this account ·
  Previously paused recurring templates will not be automatically resumed —
  re-enable them individually on the Recurring page."

**Permission.** Archive and restore require an **account-level Edit grant
with Edit mode: All** on the account (owner always; admins and members per
their account grants — an Edit grant in *Related* mode is not enough, since
archiving affects every record on the account). It stays at the edit tier,
not the delete tier: reversible and non-destructive. **Cascading to children**
requires the actor to hold Edit-All on *each* child account as well — the
checkbox is only offered when the actor may archive all children; otherwise
the notice lists every child as remaining active and the user archives the
children they can manage individually.

**UI.** The Archive button sits in the action area at the **top of the
account details page** (`/accounts/{id}`); Restore sits in the same spot when
the account is archived. The strict **Delete** stays at the bottom as the
danger zone. No row-level buttons on the list.

**Routes.** `POST /accounts/{id}/archive`, `POST /accounts/{id}/restore`.
Side-cleanup: the existing strict-delete route
`POST /accounts/{id}/deactivate` is renamed `POST /accounts/{id}/delete` so
"deactivate" is no longer a name that means "delete".

**Schema.** None for the archive itself — `is_active` already exists and the
list section already renders.

## B. Recurring templates cascade with their account

Settled shape of the template↔account relationship: **one account may have
many templates; each template is tied to exactly one account** (the current
schema — `account_id` NOT NULL, no uniqueness constraint; no constraint is
added).

- The FK `recurring_template.account_id` becomes **`ON DELETE CASCADE`**:
  deleting an account deletes its templates.
- The strict-delete check stops treating templates as a blocking reason
  (they die with the account). Generated *transactions*, settlements, and
  child cards still block. (Implementation note, 2026-10-22: *voided*
  transactions block too — the transaction→account FKs are SET NULL and the
  class-endpoint invariant demands ≥1 surviving endpoint, so a voided row
  cannot survive its account; removing the old "non-void only" filter turns
  what was a raw 500 into the normal 400 "tied records" refusal. Bulk delete
  can clear voided records first.)

**Migration.** 000015: the FK cascade. (The wave's other schema change, the
`deletion_log` table in section C, lands in the same migration.)

## C. Bulk record delete (and bulk void)

**Shape.** Multi-select on the `/txns` list page → a bulk action bar with two
actions: **Void** and **Delete**.

- Bulk **Void** is checked per-record against the **Edit** grant (void is a
  status transition; status transitions follow the Edit grant — the existing
  rule).
- Bulk **Delete** is checked per-record against the **Delete** grant with
  Related/All mode (a member with delete=Related only deletes rows they are a
  party to).
- **Partial success**, never all-or-nothing: each record is enforced
  individually. The flash reports counts only —
  *"N record(s) successfully deleted"* and, only when there is a failure,
  *"M record(s) failed to delete"* — no per-record names in the flash.

**Deletion log** (deletes only):

- A persistent table `deletion_log` — a real audit trail. Each attempted
  bulk delete records one row per transaction: the transaction's identity
  (name, date, amount — snapshot fields, since a successfully deleted row
  disappears), outcome (`deleted` | `failed`), reason (e.g. "delete grant not
  held", "Related mode — not a party"), actor, timestamp, and an operation id
  grouping the rows of one bulk action.
- Bulk **void** outcomes are **not** logged. Void auditing belongs to the
  parked *transaction history / audit trail* enhancement; the bulk-void flash
  uses the same count-only report.
- The **"Deletion log"** button on the `/txns` page opens the log
  (`GET /deletion-log`): table, newest first, grouped by operation, with a
  reason column. Click-in detail, zero clutter on the list page.
- **Clearing:** owner and admins only. `POST /deletion-log/clear` with its own
  confirmation modal. A clear leaves a **tombstone row** ("Log cleared by X on
  <date>") so the trail records that a clear happened. **No automatic
  expiry** — retention is deliberate, manual only.

**Routes.** `POST /txns/bulk-void`, `POST /txns/bulk-delete`,
`GET /deletion-log`, `POST /deletion-log/clear`.

## D. Ownership transfer

See **ADR-0003** for the decision and trade-offs. Settled shape:

- **Initiator:** the current **owner only**. Admins may not initiate.
- **Recipient:** any **current member** of the household (non-owner). No
  admin prerequisite; outsiders cannot be direct recipients (invite first,
  then transfer).
- **Effect:** `household_member.role` swaps — the old owner becomes a
  **member** with an **admin** household grant (full access, admin flag), the
  recipient becomes the **owner** with the owner grant row. The old owner
  keeps full access but is no longer untouchable: the new owner (and other
  admins) can later edit or revoke those grants, including the admin flag.
- **Password re-entry:** the transfer modal requires the owner's password,
  verified server-side before anything changes; a wrong password re-shows the
  modal with an error.
- **Scope:** only this household. The old owner's memberships and ownership of
  any other households are untouched.

**UI.** Settings → **Household tab**, a new **Ownership** section at the
bottom: recipient picklist (non-owner members) + "Transfer ownership" button.
The confirmation modal lists the consequences ("X will become an admin — full
access, but no longer the owner. Y becomes the new owner and can change X's
access from then on.") and the password field sits at the bottom. When the
household has a single member the section reads *"No one to transfer to —
invite a member first."*

**Route.** `POST /household/transfer-ownership` (authenticated settings area).

## Verification (house style: small scenario-dense seeds, not bulk)

- **Go DB tests** (fresh schema per test, like the existing `*_test.go`
  harnesses):
  - archive state machine: `is_active` flip, templates pause, child-cascade
    with/without the checkbox, restore leaves templates paused;
    permission gate: an Edit grant in Related mode cannot archive, and a
    missing Edit-All on one child suppresses the cascade checkbox;
  - strict delete: cascades templates, still blocks on transactions /
    settlements / children;
  - bulk matrix: void × edit-grant; delete × (Related|All) × party-status,
    asserting the per-record outcome split and the log rows written;
  - deletion log: rows per bulk op, tombstone after clear, clear permission
    denied for non-admins;
  - ownership transfer: role/grant swap, old owner's admin shape, wrong
    password rejected, non-owner initiator rejected.
- **HTTP smoke** on a scratch DB (two-household, multi-member, like
  `smoke-d28.sh`): archive/restore flows with modals, bulk bar with a
  partial-success report, log page + clear, transfer with password re-entry.
- **Cutover rehearsal**: migration 000015 integrity run exactly once on a
  copy of `eventally_dev` (template-DB copy, like Gates 4/5 and D28) — the
  live DB is not touched without the user's explicit green light.
