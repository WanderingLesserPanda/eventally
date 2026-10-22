# Audit: two trails, per-row visibility, write-time access snapshots

Decided 2026-10-22 (grill-with-docs session, D31 wave). D31 ships a new
`txn_history` table (transaction created/edited/voided; settlement
created/deleted) alongside the D30 `deletion_log`, which stays separate and
keeps owning bulk transaction deletes. **Both** trails filter rows by
per-row visibility, evaluated from **access snapshots written into each row
at event time** (`account_ids`, `related_user_ids`).

## Why

- The user requires audit-grade privacy: **a regular member must not see
  entries outside their account grants.** This rejects the D30 pattern for
  new rows (the shipped deletion log is unfiltered — a household is a trust
  unit) and is a hard requirement for the app's large-organization target.
- A deleted record cannot be checked against **live** grants — it doesn't
  exist. Visibility must therefore come from data captured *at event time*:
  the record's surface accounts and the people in its Related slice. That
  makes the answer stable after the record is edited, renamed, or deleted,
  and gives the audit the semantics "could this member have seen it *then*?"
  (a member granted access later cannot see earlier events).
- The **separate-tables** shape (over the agent's initial "fold
  deletion_log into one table" recommendation) was the user's call, driven
  by the visibility decision: the deletion log's natural viewing unit is a
  **bulk action** (`operation_id` — "show me this action's rows"), its rows
  carry failure reasons, and it is shipped D30 surface that smoke tests
  cover. Forcing one schema to serve entity-centric events and
  action-centric deletion attempts adds machinery without value. Each event
  keeps exactly one home: txn delete → deletion log; everything else in
  scope → `txn_history`.

## Consequences

- Visibility rule (shared by both pages): a member sees a row iff
  owner/admin, **or** they hold a read grant on some account in
  `account_ids` **and** (view mode All there **or** they are in
  `related_user_ids`). Legacy deletion-log rows (pre-000016, empty
  snapshots) keep today's all-members visibility — no regression.
- `GET /deletion-log?op=<id>` deep-links a bulk action: the post-bulk-delete
  flash points there, and the page renders only that action's visible rows.
  `operation_id` already exists (D30); only the link and filter are new.
- Two pages, two Clear buttons: each is owner/admin only, leaves its
  tombstone row, and has no automatic expiry (D30 house pattern).
- Cost of the shape: a new event type must choose its home deliberately
  (one trail, one table) — a small discipline, not a defect.
- Hard to reverse in one direction only: shipping **unfiltered** first and
  adding filtering later is impossible without the snapshot data; shipping
  filtered (this ADR) and relaxing to unfiltered later is trivial. The
  anticipated audit-UI redesign can rework presentation freely — the row
  shape and the rule stay.
