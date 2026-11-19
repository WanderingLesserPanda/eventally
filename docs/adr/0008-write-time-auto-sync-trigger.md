# Auto-sync triggers on the source's write path, best-effort

Decided 2026-11-18 (grill-with-docs session, D34 wave).

An auto-synced copy is created by a best-effort call to the centralized
`syncUpsert` **in the source household's write path**: after a source
record's own commit (create/edit via the `saveTxn` path; voids through the
existing one-way cascade), for every member with the toggle on who is in
the record's Related slice. A trigger failure never fails the member's own
save; a missed copy shows as "syncing…" in the Live view, and the manual
Sync button on that row is the fallback.

## Why

- **B's budget reads B's own table.** A copy must exist before the
  dashboard can count it. Render-time lazy (the `generateDueRecurring`
  precedent) would leave B's budget stale until someone happened to load a
  triggering surface — "the copy appeared when I opened the dashboard" is
  a surprise behaviour for a number the household budgets against.
- **A periodic background sweep** adds a goroutine, a cadence, and an
  idempotency story to a self-hosted single-process app to buy
  seconds-to-minutes of staleness tolerance.
- **Write-time composes with what already exists**: the same centralized
  upsert the manual button calls (the D33 seam), the existing one-way void
  cascade, and the Live view as a **free recovery surface** — a missed
  auto-sync is simply an unsynced row with a manual button on it.
- Best-effort, after commit: a cross-household write must never roll back
  or delay the member's own record in their own household.

## Consequences

- Switching the toggle **on** runs the same upsert over the currently
  syncable backlog (catch-up) — which retires the parked "bulk sync"
  button; no separate bulk-sync surface is built.
- The trigger's read of `auto_sync` rows (which live in the target
  household's context) is a **new named cross-household surface**, added to
  the isolation probe alongside the source-category list and the
  household-detail reads.
- Two members of B both toggled on for the same source record: the last
  upsert processed wins (deterministic `user.id` order) — the existing
  re-sync semantics, no new conflict machinery.
- Auto-synced copies arrive as `pending` (the D33-reserved path);
  `syncUpsert` gains a status parameter (`'approved'` manual / `'pending'`
  auto).
