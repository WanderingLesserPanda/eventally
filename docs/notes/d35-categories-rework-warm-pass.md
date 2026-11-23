# D35 — Categories rework + warm design system

Settled in the 2026-11-21 grill-with-docs session (four grill rounds + an
ezbookkeeping deep-reference round + four Penpot mockups: the main "Categories
(to-be)" board, the edge-states board, the budget-notes board, and the
form/checkbox-hint panel — all exported and vision-verified).
Status: **DESIGNED — SIGNED OFF 2026-11-22.** Implementation proceeds in
batches; each batch's push needs its own explicit approval, and migration
`000020` needs its own explicit green light (standing rule).

Reworks the Categories page from its current flat table to the "to-be"
two-pane layout, and — per the user's explicit "I would like to see what you
can do" — applies a new **warm terracotta design system app-wide in the same
wave**, not just on this page.

Reference apps consulted: ezbookkeeping (`mayswind/ezbookkeeping`) for the
list-page anatomy, the modal edit dialog, the icon-picker dropdown, and the
staged "Save Sort" reorder pattern. Eventally is an independent design
inspired by it, not a fork.

## Decisions (all settled)

| #  | Item                     | Decision |
|----|--------------------------|----------|
| Q1 | Palette                  | **Warm terracotta**, scoped first to Categories, then **app-wide this wave** (§B). One token re-point, not a per-page patch |
| Q2 | Row count                | Direct **subcategory** count (not subtree) |
| Q3 | Reordering               | **Real** reorder reusing the existing `category.sort_order` (already the `ORDER BY` on budget cards + recurring selects) — one global order honoured everywhere; zero migration for reordering itself |
| Q4 | Row actions + naming     | Per-sub-row **Edit + Archive** text buttons (Operation column); top-level actions live in the detail pane; **Archive / Unarchive** naming for categories *(rev. 2026-11-23: top-level Archive/Unarchive moved to the Edit modal; subcategory Archive stays inline)* |
| Q5 | Interaction              | **In-page** (Alpine `fetch`), page reloads minimised — standing preference, carried into the settings-page expansion |
| Q6 | Budget                   | **Envelope model**: per-child mode flag (§D); `Total = standalone subs + max(parent's own budget, Σ "part-of" slices)`; over-allocation and no-budget-parent both **warn + auto add-on**; effective total is **computed, not written back** |
| Q7 | Empty states             | Left pane keeps `No {class} categories yet.`; nudge text is `Please add a category first.` |
| Q8 | Deactivate placement     | Moved to the **detail pane** (as Archive), off the flat row *(rev. 2026-11-23: top-level Archive/Unarchive relocated to the Edit Category modal)* |
| Q9 | Create/edit              | In-page **modal** (see Q14) |
| Q10 | Warm scope               | **All pages**, one batch (full page set in §B) |
| Q11 | Row actions (final)      | **Edit + Archive only**; **Delete category is PARKED as an Open Item** (FK blast radius below) |
| Q12 | Archive naming scope     | **Categories only** — accounts keep the D30 Archive/Restore; merchants/parties/recurring keep Deactivate this wave (micro-inconsistency logged, not re-asked) |
| Q13 | Reorder surfaces         | **Both**: the left category list and the detail-pane sub-table |
| Q14 | Form architecture        | **Option B — modal dialog** (ezbookkeeping `EditDialog` pattern); the D32 icon picker **stays a modal** (accept one level of modal-over-modal); reorder commits via a staged **"Save Sort"** toolbar button |

## §A The page (two-pane, in-page)

- Layout is the approved "to-be" anatomy (user's Penpot screenshot is ground
  truth): H1 `Categories` + muted subtitle; toolbar with class tabs
  (Expenses / Income / Transfers) on the left and `+ Add Category` on the
  right; a full-width divider; then a two-pane body split by one vertical
  divider.
- **Left pane**: one row per top-level category of the active class —
  `[drag grip] [name] [direct-subcategory count, muted]`. No per-category icon
  in the left pane (kept minimal to avoid bloating the UI; the category icon
  still renders in the detail-pane header and the sub-table). The drag grip is
  the reorder affordance (Q13/§C), rendered as a solid terracotta `la-braille`
  dot glyph at full opacity (revised 2026-11-23 from the faint grey
  `la-grip-vertical`); the specific row being dragged is highlighted (cream
  tint + left accent bar, house style). Selected row = cream tint + left accent
  bar + accent text. (2026-11-22, user instruction: the icon is dropped from the
  left pane; supersedes the earlier `[D32 icon, default la-tag]` row shorthand.)
- **Right pane (detail)**: `[name] [icon]` header with an `Edit Category`
  link; a `Total Budget:` line (envelope rule, §D, `—` when nothing is
  budgeted); a three-column sub-table (accent-tinted header: Subcategory
  Name / Budget Allocation / Operation) showing **only its subcategories in
  `sort_order`** (reversed 2026-11-23, user instruction: the selected category
  is no longer rendered as its own sub-table row — it is named in the pane
  header instead; a category with no subcategories shows a muted
  "No subcategories yet." row so the table never renders a bare header).
  `+ Add Subcategory` sits
  **directly below the table** (or the dividing line, if none) — a standing
  placement rule, it is NOT a footer of the pane.
- **One full page load.** Tab switch, row switch, create/edit, archive, and
  reorder are all `fetch` + in-place DOM swap (Alpine, already on the page).
  No new libraries. The D32 icon-picker modal is the existing proof that this
  pattern works in this codebase.
- Add / edit is a **modal dialog** (§G); the only overlay on the page.

## §B Warm design system (app-wide)

Re-point the `:root` tokens in `internal/web/static/app.css` (blue today) to
the warm family; audit the page-scoped hard-coded colors (setup page scoped
section first) so nothing stays blue. Restyling only the Categories page
would leave the rest of the app blue and create patchwork — hence the
app-wide batch the user approved.

| Token / surface        | From (blue)  | To (warm)  |
|------------------------|--------------|------------|
| `--accent`             | `#2f6df6`    | `#BD4B12` (terracotta) |
| page background        | cool grey    | `#f7f1e9` (warm paper) |
| `--line` (borders)     | cool grey    | `#e7dccc`  |
| selected-row tint      | blue tint    | `#FFF4EB` (cream) |
| table-header band      | blue tint    | `#FBE9DA` (peach) |
| `--ink`, `--danger`, `--ok` | unchanged | unchanged (kept functional) |

Reference-only sketch values (not tokens): apricot `#F8D3AF` (board bg),
ink `#1c2430`, muted `#6b7686`. The setup page's existing orange is harmonised
into the terracotta family, not left as a second accent.

**Page set (all restyled):** dashboard; txns + detail + form; accounts;
categories; merchants; parties; recurring; cards; reconcile; review; history;
deletion log; all forms; settings (household / grants / invites / sync);
D33 live view; login / setup / join; layout / topbar.

## §C Reordering (Save Sort)

- Reuses `category.sort_order INT NOT NULL DEFAULT 0` (already in
  `000001_init.sql`, already the `ORDER BY` on budget cards and the
  recurring-template selects) — **one order honoured everywhere** (consistency
  is the stated goal). No migration.
- Two reorderable surfaces (Q13): the left list (siblings = top-level
  categories of the active class) and the detail-pane sub-table (siblings =
  children of the selected category).
- **Staged commit (ezbookkeeping pattern):** drag reorders rows in place as a
  preview; a **`Save Sort`** button appears in the toolbar only while the
  order is dirty; clicking it sends **one POST** with the full ordered list for
  that class + level, which renumbers `sort_order` 0..N. Never click = no
  write. This replaces the earlier "immediate POST per drop" description — the
  surfaces are unchanged, only *when* the order commits.

## §D Budget envelope model

The Categories page's `Total Budget:` and the dashboard budget roll-up adopt a
two-mode subcategory budget.

**New field.** One **additive** column on `category` (§H): `budget_mode
TEXT NOT NULL DEFAULT 'standalone'`, app-side validated against
`standalone | part_of_parent`. It is meaningful only on a subcategory
(`parent_id IS NOT NULL`); a top-level category is always its own envelope.
This is the "new separate field" the user proposed.

**The two modes** (a checkbox in the subcategory form, shown only when a
parent is selected):
- **standalone** (default): the sub's budget is its own allocation; it
  **adds on** to the parent's number.
- **part_of_parent** (a "slice"): the sub's budget is carved **out of** the
  parent's envelope; it does **not** increase the total.

**One rule.** A category's effective monthly total is:

    total = (Σ standalone sub budgets) + max(parent's own budget, Σ part-of-parent sub budgets)

which covers every requested scenario:
1. Parent budget only, no children → total = parent's budget.
2. Parent + some standalone subs → total = parent + standalone subs.
3. Parent + slices within the envelope → total = the envelope (unchanged).
4. Slices **over** the envelope → total = Σ slices (the excess **auto add-ons**).
5. Parent has **no** budget + slices → total = Σ slices (slices add on).

**Over-allocation and no-budget-parent: warn + auto add-on, never block.**
The excess simply increases the total; the UI shows a note so it is never
silent. Wording (user-settled 2026-11-21; house style: commas, no em-dashes,
no possessives):
- Over-allocated: `Over-allocated by 500, the total budget for Food & Drink becomes 3,000.`
- No parent budget: `No budget on Food & Drink, 1,500 from Groceries adds to the total.`

**Computed, not written back.** The effective total is **not** persisted into
the parent's `budget_per_month`. The parent keeps the envelope the user typed;
the total is derived at display time in the existing budget roll-up path
(`internal/web/card.go`), consistent with D10's "data in, display in" — the
dashboard already computes roll-ups at query time, so the auto add-on is just
a new rule in that same calculation. Writing the total back would create a
second number (envelope vs effective) that must stay in sync on every child
edit; with the per-child flag, shrinking a slice automatically shrinks the
total with no write-back cascade.

**Display.** Detail pane: `Total Budget: 3,000` + the note when over-allocated
or no-budget. Sub-table: a slice renders `1,500.00 (of parent)`; a standalone
sub renders its plain amount.

## §E Archive / Unarchive (categories)

- **Archive** (the old Deactivate) soft-inactivates a category
  (`is_active = false`): it stays in history (transactions keep their
  category, budget history intact) and is demoted/muted on the Categories
  page. **Unarchive** restores it. One-line confirm on Archive (reversible);
  Unarchive is a plain link.
- Placement: per-sub-row **Archive** text button (Operation column); top-level
  categories' Archive/Unarchive live in the **Edit Category modal** (reversed
  2026-11-23, user instruction, from the detail pane; subcategory Archive stays
  inline in the sub-table).
- Naming is **categories-only** (Q12). The app already had "Archived account"
  from D30; this wave makes categories consistent with it. Merchants, parties,
  and recurring keep "Deactivate" this wave.

## §F Empty states / edge states

- **Empty tab** (no top-level category of the active class): left pane
  `No {class} categories yet.` (kept); detail pane nudge `Please add a
  category first.` (user-confirmed against the full-page mockup); `+ Add
  Category` still visible; no detail content.
- **Category with no subcategories**: the sub-table shows the category's own
  single row (always first); directly below the table (or the dividing line,
  if none) sits the signed `+ Add Subcategory` link (→ the form with the
  parent pre-selected) — the parent exists, so the action is available; the Q7
  nudge lives only in the empty tab (2026-11-21, superseding the draft that had
  put the nudge string in this spot too).
- **No budget, no subcategories**: own single row with `—`, total `—`. The
  sub-table never renders empty — the `+ Add Subcategory` link always has a
  home below it.
- **Subcategory of a different class than its parent**: still shown (structure
  is truth; tabs filter the left list only).

## §G The form (modal)

- Add / edit category is a **modal dialog** (Option B, Q14): Name; Class
  (expense/income/transfer); **Parent** (select, only when a subcategory);
  **Monthly Budget** + the **budget-mode checkbox** (§D) when a parent is set;
  **Budgetable**; the **D32 icon picker**. Footer: Cancel + Save. Saving a sub
  pre-selects its parent.
- **The D32 icon picker stays a modal** (already built, tested, and
  live-verified: search, grouped rows, the default no-icon row). Inside the
  form modal it opens as a second modal — an accepted, ordinary modal-over-modal
  stack (the form survives the picker closing intact). Converting it to
  ezbookkeeping's anchored **dropdown** would drop the signed-off D32 search
  box, so it is **parked as a polish Open Item** to revisit after the live
  check if the stacking feels heavy.
- The modal is the only overlay on the page; `+ Add Category` (toolbar),
  `Edit Category` (detail header), and `+ Add Subcategory` (directly below the
  sub-table) all open it.

## §H Migration

One additive, nullable-by-default column, no backfill (existing rows stay
`standalone`, which is today's behaviour — a sub's budget already adds on):

- `000020`: `ALTER TABLE category ADD COLUMN budget_mode TEXT NOT NULL DEFAULT 'standalone';`
  App-side validation against `standalone | part_of_parent` (no CHECK, matching
  the D32 app-side-validation convention). Rehearsal + explicit green light,
  as every other migration.

Reordering needs **no** migration (§C).

## Parked / Open Items (not built this wave)

- **Delete category** (Q11, explicitly parked by the user). FK blast radius
  for when it is built: `txn` / `recurring` / merchant-default category refs
  are `ON DELETE SET NULL` (silent uncategorisation), D33 sync mappings are
  `CASCADE`, and deleting a parent promotes its children to top-level. Needs a
  consequences modal + a decision on where the childless-orphaned records go.
- **Icon picker as an anchored dropdown** (ezbookkeeping style) — polish; the
  D32 modal stands for this wave.
- **Per-slice budget tracking / per-slice actuals** — the model stores the mode
  but does not yet break actuals down per slice.
- **Preset category import** (ezbookkeeping's "Add Default Categories") —
  observed, not requested.
- **Merchants/parties/recurring "Archive" naming** — kept as "Deactivate" this
  wave (Q12).

## Verification / rehearsal (planned, post sign-off)

- Go tests: per-class render, selection default, **envelope total math**
  (all five §D scenarios incl. over-allocation and no-budget), sub-table order
  self-first, direct-subcategory count, empty states, archived row demotion,
  and a **Save Sort round-trip** (POST ordered list → `sort_order` renumbered).
- A copy-of-`eventally_dev` rehearsal for the `000020` migration (the standing
  rule: live data migrations need explicit green light, distinct from any
  engine work).
- Smoke: load the Categories page once, then tab switch / row switch / create /
  edit / archive / Save Sort all in-page with no full reload.
- Live-image pixel verification of the warm pass rides on the user's next live
  pull (container browser backend is currently broken).

## Sign-off

Decisions signed off 2026-11-21 across four grill rounds plus the
ezbookkeeping reference round; final user sign-off given 2026-11-22,
authorising commit/push of this doc batch and the start of implementation.
The two budget-note wordings and the `Please add a category first.` nudge
were user-confirmed in their final form (2026-11-21); the `+ Add Subcategory`
placement (directly below the sub-table, not a pane footer) was settled
2026-11-22 and applied to the mockups. Implementation batches follow,
each with its own push approval; `000020` rehearsal + green light still
outstanding.
