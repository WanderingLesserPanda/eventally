# Categories page: one page load, in-page fetch, modal forms

Decided 2026-11-21 (grill-with-docs session, D35 wave, after an
ezbookkeeping reference round).

The Categories page loads **once** as a normal server-rendered page. Every
interaction after that — class-tab switch, category-row switch, create/edit
(opened as a **modal dialog**), archive, and reorder — is an Alpine `fetch`
that returns only the changed fragment and swaps it in place. No full page
reload, no scroll reset, no flash. Reordering is **staged**: drag previews the
new order in place, and a `Save Sort` toolbar button (visible only while the
order is dirty) commits it with one POST per class + level.

## Why

- Standing user preference: keep page reloads to a minimum; reloads get worse
  as the settings page expands. Everything in-page feels "snappier" and the
  user never "leaves" the page.
- The pattern already exists in the codebase: the D32 icon-picker modal
  opens and closes in place. This extends the same mechanism from one modal to
  the page's sections — no new libraries.
- **Modal dialog** (not a pane swap) for the form, matching ezbookkeeping's
  proven `EditDialog` anatomy (name / parent / icon / color / description /
  Save), and chosen by the user over a pane swap. A modal keeps one overlay at
  a time and gives the form the whole pane's room.
- **Staged Save Sort** (ezbookkeeping pattern) instead of an immediate POST
  per drop: one canonical endpoint ("send the ordered list for this class and
  level"), no network write for a tentative drag, and an abandonable reorder.
  It refines the earlier "one POST per drop" description; the reorderable
  surfaces are unchanged.

## Consequences

- New small endpoints (or handlers) for the in-page actions: tab/row content,
  form save, archive/unarchive, and Save Sort. The initial page is still a
  plain server render, so the page works without JS as it does today.
- Testing must cover the fragments and the round-trips (especially Save Sort),
  not just the initial HTML.
- The D32 icon picker stays a modal; inside the form modal it is a second,
  stacked modal (accepted). An anchored-dropdown picker (ezbookkeeping style)
  is parked as polish — converting would drop the signed-off D32 search box.

## Rejected alternatives

- **Full reload per interaction (links)**: simplest, but the explicit opposite
  of the user's stated preference.
- **Pane-swap form (Option A)**: the form replaces the detail pane in place.
  Viable, but the user preferred the modal (Option B), which also matches the
  reference app and avoids the form sharing space with the detail content.
- **Immediate POST per drag drop**: fewer moving parts on the client, but a
  write per tentative drop and no way to abandon a reorder without a
  compensating write.
