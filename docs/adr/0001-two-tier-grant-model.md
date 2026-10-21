# Two-tier grant model with Related/All mode tiers for household permissions

Decided 2026-10-21 (grill-with-docs session). We replace Gate 1's
`household_member.can_edit` / `record_scope` columns and the
`member_account_scope` table with two grant tiers: a **household-level** grant
per member (read/create/edit/delete over accounts + admin flag, edit and
delete scoped by a `created | all` mode) and an **account-level** grant per
(member, account) (read/create/edit/delete, each with a `Related | All` mode
tier; forcing rules: edit = All requires view = All, delete = All requires
both, edit requires read, delete requires read + edit). Access to every
record flows from the grants on that record's accounts — a transaction's
accounts (Gate 3c guarantees ≥1), a settlement's own account plus every
allocated split's transaction account, a recurring move's endpoint accounts.
"Related" is the member's slice: records they recorded, in which they are a
split party, or that they paid as settlement payer.

## Why

- The Gate-1 trichotomy (`all | accounts | own`) conflated *who edits* with
  *what is visible* in one column, and `member_account_scope` was only
  meaningful under one of the three values — contradictory states were legal
  (an `own` member with a populated scope list).
- Per-account grants are what the UI actually is (checkboxes per account),
  make contradictory states unrepresentable, and match the enforcement point:
  the household-scoped read queries Gate 3c just rewrote.
- Owner and admins form the management tier: the owner has implicit full
  access and is untouchable; an admin can manage every non-owner member's
  grants (including other admins'); members are read-only by default.
- Deletion is strict: an account is deletable only with zero tied records
  (no archive, no cascade — archive and bulk record delete are parked future
  work in `docs/notes/schema-conventions.md`).

## Considered options

- **Keep Gate 1's mode column + side table** — rejected: the trichotomy is a
  false scale; two independent questions (edit rights, visibility) in one
  column; extension (e.g. per-account edit scope) would mean adding values
  forever.
- **Row-per-permission ("permission string") table** — right shape for
  open-ended RBAC; overkill for a fixed ~6 booleans. Boolean-per-permission
  columns on a grant row keep enforcement to one simple JOIN and readable
  SQL; adding a 7th permission is a one-off migration, not an ongoing
  pattern.
- **PostgreSQL RLS** — rejected (see the deferred list): the app runs as a
  single DB role that owns the tables, and table owners bypass RLS, so it
  would give false comfort. Household isolation is structural: composite
  `(household_id, …)` foreign keys.
- **Author-scoped edit/delete** (members may only touch records *they*
  created) — rejected: it breaks the owner's workflow of members editing
  shared household records; the mode tiers give the owner explicit control
  instead.
- **Two independent view/edit modes without forcing** — rejected: an
  edit-All / view-Related state is incoherent (you can edit records you
  can't see); the forcing rules make it unrepresentable.

## Consequences

- Migration drops `can_edit`, `record_scope`, `member_account_scope` and adds
  the two grant tables (dev data only — clobber, no backfill).
- Enforcement loads both tiers into request context; every read query gains
  a grant filter, every write path checks the grant set of the record's
  accounts. **Q7 (settled 2026-10-21, "anchor rule" — pending live-UI
  confirmation):** a settlement's permission surface is its own account +
  the card family behind its allocated splits; the D6 source asset account
  counts only for create-with-transfer. Read/delete of a settlement never
  requires a grant on the D6 source — the linked transfer is a separate
  record with its own checks.
- Recurring templates follow the single-`account_id` rule: read/create/
  edit checks run against that one account. **Delete is author-scoped**
  (settled 2026-10-21, R5): a member may delete a template only if they
  created it; a split party may see and (with an edit grant) edit a
  template they appear in, but not delete it. This is a deliberate
  exception to the account-level delete = Related mode.
- Card-family pages show only the family accounts the member is granted;
  hidden entirely if none.
- UI wording for the Related mode: "only records involving them" — a
  participation link alone never grants access; the account grant is the
  only key.
