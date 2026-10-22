# Settlement funding: legs table is the source of truth, single payer column dropped

Decided 2026-10-22 (grill-with-docs session, D31 wave). A settlement's
funding is modeled as a `settlement_funding` legs table (payer + optional
source account + method + amount per leg, Σ legs = settlement amount). The
single `settlement.paid_by_user_id` column — and `settlement.method`, which
moves to the leg — are **dropped** in migration 000016 after backfilling
one leg per existing settlement.

## Why

- Multi-payer funding makes the payer a **set**, not a column. Keeping
  `paid_by_user_id` as a denormalized "primary payer" would create a second
  source of truth that every write must keep in sync with the legs.
- The column would not save any query: the settled Related rule is "the
  user paid **any** leg", so the hot Related-slice EXISTS (run per
  transaction row on every /txns list load) must read the legs table under
  *either* option. The column's remaining consumers are the RESTRICT
  delete-guard and display convenience — both provided by the legs table
  (its `user_id` FK carries the same RESTRICT guard).
- The denormalization's failure mode is a **silent per-user permission
  under-report**: any future path that writes legs without updating the
  column makes one user's Related slice quietly shrink, with no error
  anywhere. That risk grows with the user base — the app targets
  large-organization scale, where it is worst.
- The alternative (keep the column, treat legs as advisory) was rejected
  for the same reason; dropping the column costs one safe
  `ALTER TABLE ... DROP COLUMN` after the backfill and removes a dead index
  plus a permanent "column or legs?" decision for every future query.

## Consequences

- Migration 000016 order: create `settlement_funding` → backfill exactly
  one leg per settlement (payer from the old column; source from the linked
  D6 transfer's `origin_account_id` when present) → drop
  `paid_by_user_id`, `method`, and the old payer index.
- Five query sites re-point to legs: `relatedSliceSQL`'s payer clause,
  `canSeeSettlement`, card payment-history display, reconcile payment list
  + recorded balance, and the txn-detail payment display.
- `settlement.amount` stays a column and equals Σ legs, app-enforced in
  the pay flow's single transaction — no partial state.
- Hard to reverse: the column is gone from the live table; restoring it
  would require a re-backfill from legs. A future permissions rework (user
  anticipates one) must therefore read funding exclusively from the legs
  table.
