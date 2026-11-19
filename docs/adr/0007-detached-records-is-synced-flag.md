# Detach via an `is_synced` state flag, not a source value or pointer strip

Decided 2026-11-18 (grill-with-docs session, D34 wave). Supersedes the
2026-11-17 open-item sketch ("strip `source='sync'` + both pointers") and
the assistant's mid-grill `source='detached'` value proposal.

A **detached record** — a synced copy the target household kept but
un-linked from its source — is represented by `is_synced = false` plus
`detached_at`, while `source` stays `'sync'` and the source pointers stay
set. Every "is this row currently synced?" decision (edit lock, synced ✓
badge, A-side badge, void cascade, budget gate) reads the **flag**, not
`source`.

## Why

- `source` is a **birth marker** — how the row came to be
  (`manual`/`template`/`import`/`tracker`/`sync`). "Detached" is a
  **transient state**. Folding a state into the birth enum
  (`source='detached'`) overloads the column with a lifecycle stage that
  sits next to provenance classes, and it was the assistant's proposal —
  the user's flag model was adopted because it is semantically cleaner.
- Stripping the pointers (the 2026-11-17 sketch) would **destroy the
  re-link**: the upsert's "find the existing row" lookup could no longer
  find the row, so re-syncing the same source would insert a *second*
  record in B alongside the detached one.
- Keeping the pointers while moving `source` away from `'sync'` is
  **forbidden by the existing bidirectional CHECK**
  (`(source = 'sync') = (both pointers set)`, 000017) — that path needed
  constraint surgery plus a data rewrite of existing rows.
- A state flag makes migration 000018 purely additive (two columns + a
  one-line backfill), leaves the CHECK and the per-household UNIQUE
  untouched, and re-link falls out of `syncUpsert`'s existing found-row
  branch.

## Consequences

- Re-link = the upsert's found-row branch with `is_synced = false`: flip
  the flag, clear `detached_at`, reset amount/date from the source, keep
  classification/status. The UNIQUE constraint makes a second copy
  structurally impossible — the flagged "two records from one source"
  consequence no longer exists.
- The void cascade must filter `AND is_synced = true`: a detached copy
  survives a source void (it is now B's own record).
- The shipped D33 key points (edit-lock check, /txns list + detail badges,
  `aSyncedBy`) re-key from `source='sync'` to `is_synced` — a small,
  centralized change set.
- Refines ADR-0006 (synced records as party-endpoint transactions): its
  `source='sync'` marker survives as the birth marker; the lock's key moves
  to the flag.
