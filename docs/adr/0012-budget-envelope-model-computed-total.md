# Category budgets: envelope model with per-child mode, computed total

Decided 2026-11-21 (grill-with-docs session, D35 wave).

A subcategory's monthly budget has a **mode** stored in a new additive column
`category.budget_mode TEXT NOT NULL DEFAULT 'standalone'` (`standalone` |
`part_of_parent`):

- **standalone** (default): the sub's budget is its own allocation and **adds
  on** to the parent's number.
- **part_of_parent** (a "slice"): the sub's budget is carved **out of** the
  parent's envelope and does not increase the total.

A category's effective monthly total is computed as:

    total = (Σ standalone sub budgets) + max(parent's own budget, Σ part-of-parent sub budgets)

Over-allocation (slices exceed the parent's budget) and a parent with no budget
both **auto add-on**: the total becomes Σ slices, the UI shows a warning note,
and nothing is blocked. The effective total is **computed at display time** in
the existing budget roll-up path — it is **never written back** into the
parent's stored `budget_per_month`, which keeps the envelope the user typed.

## Why

- The user wants three shapes to all work: budget on the parent only; parent +
  some subs; all subs summing to the parent. The `max(envelope, Σ slices)` rule
  covers all of them, plus the two overflow cases, with one formula instead of
  per-case branches.
- **Computed, not persisted**, because the dashboard already computes roll-ups
  at query time (D10, "data in, display in"). The auto add-on is a new rule in
  that existing calculation, not a new engine.
- Writing the effective total back to the parent (or a stored "total" field)
  would create a second number — envelope vs effective — that must stay in
  sync on every child edit, with write-back cascades and stale-state risk if a
  write fails. With only the per-child flag persisted, shrinking a slice
  automatically shrinks the total, and a crash can never leave a stale total.
- The flag is a product choice per subcategory (the form's checkbox), so it
  must be stored; a two-value mode is cheap and additive with a safe default
  that preserves today's behaviour (subs add on).

## Consequences

- One additive migration (`000020`), no backfill: existing subs are
  `standalone`, which is exactly how budgets behave today.
- The budget roll-up (dashboard cards, `internal/web/card.go`) gains the
  mode-aware rule; every consumer reads the same computation, so surfaces
  cannot disagree.
- The detail pane shows the note cases: `Over-allocated by 500, the total
  budget for Food & Drink becomes 3,000.` and `No budget on Food & Drink,
  1,500 from Groceries adds to the total.` (house style per the user:
  commas, no em-dashes, no possessives).
- Per-slice **actuals** tracking is out of scope (parked); the model stores
  the mode and computes the total, but does not yet break spending down per
  slice.

## Rejected alternatives

- **Write the effective total back to the parent** (or a new stored-total
  field): second source of truth, sync + cascade + stale-state costs; the
  computed rule gives identical display with none of it. The user's "new
  separate field" is satisfied by the per-child mode flag rather than a stored
  total.
- **Block over-allocation on save**: fights the flexibility that is the point
  of the feature; budgets are planning numbers.
- **A separate allocation table** (parent → slice amounts): more schema for a
  single-level hierarchy the app already has with `parent_id`; the per-child
  column expresses the same thing where the budget already lives.
