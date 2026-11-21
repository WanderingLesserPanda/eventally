# Warm terracotta design system, applied app-wide in one batch

Decided 2026-11-21 (grill-with-docs session, D35 wave).

The app's `:root` design tokens in `internal/web/static/app.css` are
re-pointed from the blue family to a warm terracotta family — `--accent`
`#2f6df6` → `#BD4B12`, page background → `#f7f1e9`, `--line` → `#e7dccc`,
selected-row tint → `#FFF4EB` (cream), table-header band → `#FBE9DA`.
`--ink`, `--danger`, `--ok` stay as-is. The change lands on **every page in
one batch** (dashboard, txns, accounts, categories, merchants, parties,
recurring, cards, reconcile, review, history, deletion log, all forms,
settings, D33 live view, auth, layout), not just the Categories page being
reworked.

## Why

- The user asked for the warm treatment first on Categories and then
  explicitly approved applying it app-wide in the same wave ("I would like to
  see what you can do"). A single token re-point is how the app already works:
  every page reads the same `:root` variables.
- Restyling one page would leave the rest of the app blue — a permanent
  patchwork of two design systems in one product. Doing it partially creates
  more work later, not less.
- One batch keeps the change reviewable as a single diff (tokens + an audit of
  page-scoped hard-coded colors, starting with the setup page's scoped section)
  instead of N half-changes scattered across future waves.

## Consequences

- Every page's look changes in this wave; regression testing must cover the
  full page set, not just Categories.
- The setup page's existing orange is harmonised into the terracotta family
  so the app has one accent, not two.
- Functional colors (danger/ok) are untouched, so error and success semantics
  are unchanged.

## Rejected alternatives

- **Scoped warm on Categories only**: what the page first asked for, but it
  strands the rest of the app in blue and contradicts the user's app-wide
  approval.
- **A full new palette with new functional colors**: unnecessary risk; the
  request is a warm shift, not a redesign of semantic colors.
