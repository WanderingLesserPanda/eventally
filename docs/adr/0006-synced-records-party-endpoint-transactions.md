# Synced cross-household records as party-endpoint transactions

Decided 2026-11-16 (grill-with-docs session, D33 wave). A **synced record** —
a member's share of a related record from another household, pulled into the
current household for budget tracking — is a regular `"transaction"` row of
the target household: origin = a **party row representing the source
household**, `source='sync'`, source pointers, **no split rows, no account
endpoints**. It is neither a side table nor a transaction on an account.

## Why

- The user requires synced records to be first-class household records:
  visible on /txns, in History, in the Deletion log, and in the budget
  (settled in the interview; the agent's "budget-only data class"
  alternative was explicitly rejected).
- The money moved in the source household. Any account endpoint in the
  target is a lie: a real B-account misstates B's balances and
  outstanding, and an invented virtual/external account type pollutes the
  account list, the grants, and the balance cards with money that never
  moved in B. The party endpoint is the app's existing, legal shape for
  "no account on this side": `chk_txn_class_endpoints` accepts
  `origin_party_id` (000010), the /txns form already carries party
  selects, `perm.go` has an explicit party-only permission branch, and the
  imported real data contains party-origin rows that the list/detail pages
  already render.
- No split rows do two jobs at once: the deferred split-sum trigger is
  satisfied (sum 0), and settlement allocation — split-keyed (ADR-0004) —
  excludes synced records **by data**, so a foreign expense can never be
  funded again in the target household. Its money already moved in the
  source; settling it here would fabricate a ledger flow.
- The dashboard's category-actuals query needs no change (it sums visible,
  non-void, current-month expenses by category): a synced row is an
  ordinary row in its chosen category and enters the budget with zero query
  changes. That is the payoff of the "real transaction" choice.

## Consequences

- The "From" column of a synced row shows the source household's name (one
  party row per source household, `type 'organisation'`, auto-created on
  first sync) — provenance on the face of the row. If a user belongs to
  three households, the column distinguishes them.
- Visibility under D27: the syncer (Related via `recorded_by_user_id`) and
  owner/admin see the row; other members in Related mode do not (a
  party-only record exposes no account surface).
- Money facts (amount, date) on a synced row are locked against in-app
  editing — un-sync and re-sync is the refresh path; classification
  (category, merchant, description) stays freely editable.
- The source pointers are plain `BIGINT` pairs — **no cross-household FK**
  (the isolation convention). The wave's cross-household surfaces are
  exactly three and centralized: the `/sync` GET and POST handlers (reads)
  and the void-path cascade (the only write; it fires immediately when the
  source is voided, with standard History rows — an app-level cascade, not
  a DB trigger, because ADR-0005 gives every lifecycle event a home with
  actor).
- Hard to reverse in one direction only: once real users have synced rows
  living in /txns and History, demoting them to a side table would need a
  data migration and a visibility audit; starting from a side table and
  later promoting to first-class records changes the user-visible surface
  and is strictly harder. Shipping first-class now is the cheaper door.
