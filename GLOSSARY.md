# Eventally

A personal household-finance ledger: members of a household share accounts,
transactions, splits and settlements, with access controlled per account.

## Language

### The household

**Household**:
The unit of shared finance. Members belong to a household; all money lives in
its accounts.
_Avoid_: family, group

**Owner**:
The household's founder. Holds implicit full access to everything and is
untouchable by other members' administration.
_Avoid_: root, admin (when meaning the owner specifically)

**Admin**:
A member granted household-level management: can manage other members'
grants and delete any account, but can never administer the owner.
_Avoid_: moderator, manager

**Member**:
A household participant. May hold household-level and account-level grants;
by default holds read access only.

**Membership**:
A user belonging to a household. A user may hold several memberships —
one username is one person across the whole app.
_Avoid_: account, profile

**Default household**:
The household a user lands in at login; settable in Settings, and set
automatically on joining a household or founding a new one.
_Avoid_: home household, primary household

**Invite code**:
A one-time, expiring, single-use credential issued by the owner or an admin
that grants a user membership in a household.
_Avoid_: token, invite link

### Access

**Household-level grant**:
Per-member rights over the household itself: read, create, edit, delete
accounts, and the admin flag. A member's edit and delete are scoped to
accounts they created (`created` mode); an admin's (and the owner's) reach any
account (`all` mode).

**Account-level grant**:
Per-member rights over one account: read, create, edit, and a view mode.
All access to a transaction flows from grants on that transaction's accounts.

**View mode**:
Which transactions a read grant exposes on an account: *Related* (the
member's slice) or *All records* (everything on the account).

**Edit mode** / **Delete mode**:
Mode tiers on a grant, bounding which records or accounts a permission
reaches. On an account-level grant each is `Related | All` (edit = All forces
view = All; delete = All forces both; edit requires read, delete requires
read + edit). On a household-level grant each is `created | all` (member's
reach is limited to accounts they created; owner/admin are `all`), with the
same forcing: delete = `all` forces edit = `all`. On recurring templates,
delete is author-scoped instead: only the creator may delete; split parties
may see and (with edit) edit.

**Related**:
The transactions in a member's slice: ones they recorded, in which they are a
split party, or that they funded a leg of a settlement (any leg).
_Avoid_: own (too ambiguous — "their own" is exactly what "Related" clarifies)

**Grant**:
A permission row as held by a member — at household level or account level.
Only the owner and admins can create, change or remove grants of non-owner
members.
_Avoid_: permission (when meaning the row held by a user; the individual
booleans — read/create/edit/admin — are the permission *kinds*)

**Ownership transfer**:
The owner-only act of handing the household's ownership to an existing
member. The old owner becomes an admin (full access, no longer untouchable);
the transfer requires the owner's password. See ADR-0003.
_Avoid_: handover (fine in speech, but the UI and docs say transfer)

### Money records & audit

**Settlement**:
Paying down card debt: money moving from one or more funding legs onto a
credit card, allocated to the people's shares (splits) of the expenses.
_Avoid_: payment (the old table name), transfer (a leg's D6 ledger transfer
is a separate record)

**Funding leg**:
One component of a settlement's funding: a payer, an optional source
account, a method, and an amount; a settlement's amount is the sum of its
legs. Legs are fixed at recording — a settlement is created or deleted,
never edited.
_Avoid_: payer (the person alone), source (the account alone)

**History**:
The household audit trail of money-record lifecycles: transactions
created/edited/voided, settlements created/deleted. Shown per record on the
transaction details page and household-wide on /history. Per-row visibility:
a member sees only rows they could have seen at the time; the owner or an
admin sees all. Clearable by the owner or an admin, which leaves a
tombstone; no automatic expiry. Deletions are NOT in History — they live in
the Deletion log.
_Avoid_: audit log (the D30-era name for this same idea), activity feed

### Budget

**Budget**:
A household's monthly allocation for a category (on the category; a parent
collects its subtree). Display-only: the dashboard shows current-month
actual-vs-budget per top-level category. A budget is household property — it
lives on categories, and categories belong to a household.
_Avoid_: spending limit, cap, personal budget

**Personal budget**:
A budget tied to a user rather than a household: the user's own monthly limit
measured against the user's share of spend across every household they belong
to. Parked as an Open Item (not implemented) — household budgets are the
budget concept for now.
_Avoid_: user budget (ambiguous with a household budget the user edits)

### Cross-household

**Source household**:
The household a synced record came from. A synced record's origin is a party
row representing the source household (named after it), so the row carries
its provenance.
_Avoid_: origin household (ambiguous with a transaction's origin endpoint),
the other household

**Live view**:
The page listing a logged-in member's syncable records from the other
households they belong to — expense records in which they hold a split
share — with sync state and the sync action.
_Avoid_: foreign view, cross-household list, mirror (suggests a syncing
copy)

**Synced record**:
A record in the current household created by syncing one of a member's
related records from a source household. Its money facts (amount = the
syncing member's share, date) are locked to the source at sync time; its
category and merchant are the current household's own and freely editable.
It counts in the current household's category budgets and cannot be funded
by a settlement.
_Avoid_: mirror, import (the P8 data migration — a different thing,
`source='import'`), foreign record

**Auto-sync**:
A per-member setting in a target household: the member's related records from a
source household are copied over automatically as they occur, without any
per-record action. Unlike a manual sync, an auto-synced copy arrives as
*pending* and awaits the member's review before it is approved.
_Avoid_: mirror, import (the P8 one-time data migration), auto-import

**Category mapping**:
A per-household rule mapping one of a source household's categories to one of
the current household's own, used as the default classification for
auto-synced records. A rule is created explicitly or as a saved convenience
during review; when no rule matches, the record arrives without a category and
one is chosen at review time.
_Avoid_: merchant mapping (merchants carry no budget value, only a
description), catch-all (no default bucket is used)

**Detached record**:
A synced record the target household kept but un-linked from its source:
the `is_synced` flag is off and `detached_at` is set, while `source` and
the source pointers are unchanged. It becomes a fully ordinary, editable
record; the source no longer controls it (no void cascade, no money-fact
refresh); the A-side "synced by" badge disappears. It can be re-linked.
_Avoid_: un-synced (that is deleting the copy), orphaned, unlinked

**Re-link**:
Re-syncing a source record whose copy in this household is detached: the
upsert re-attaches the same row (the flag flips back on, `detached_at`
clears), resetting amount and date to the source's values and keeping the
local classification and status. It never creates a second copy.
_Avoid_: re-sync (the general refresh of a linked copy), resync

### Account lifecycle

**Archived account**:
A soft-inactivated account (`is_active = false`): history stays intact and
readable, new activity is blocked (transactions, settlements, recurring
endpoints), its recurring templates are paused, and it can be restored at any
time. Restoring does not resume paused templates or un-archive cascaded
children.
_Avoid_: closed, deleted (deleting is the strict zero-history hard delete — a
different action)

**Deletion log**:
The persistent audit trail of bulk transaction *deletes*: one row per
attempted record (what it was, outcome, reason, who, when). Clearable by the
owner or an admin, which leaves a tombstone row. Voiding is not logged here —
void events live in History (D31).
_Avoid_: history (History records created/edited/voided events — deletions
live here)
