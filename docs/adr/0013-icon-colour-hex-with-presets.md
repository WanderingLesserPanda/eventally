# Icon colours are free hex with curated presets

Decided 2026-11-24 (grill-with-docs session, D36 wave). Extends ADR 0009.

`category.colour` and `account.colour` are `TEXT NULL` columns holding a
6-digit lowercase hex string (`b84810`), no `#`; NULL means "no tint — render
the glyph in the inherited text colour". A curated 16-colour preset is offered
in the picker, but the user may also enter any custom hex (ezBookkeeping-
style). Preset and custom are stored identically. No DB CHECK; the value is
normalised and validated app-side.

## Why

- **The value space is unbounded, so it is not a registry.** ADR 0009's
  curated string-key registry works for icons because the selectable set is
  closed and product-controlled. Colours are different: the user can name
  *any* hex, so the stored value is a free string, not a key into a fixed set.
  A registry of "allowed" colours would either reject legitimate input or
  balloon to the whole colour space. So colour is stored as the hex itself.
- **Presets are UX, not a constraint.** The 16 curated colours are a
  convenience (one tap, harmonised with the warm design system), not the whole
  domain. ezBookkeeping proves the shape: a `color` string column + a scheme
  list *and* free hex in the picker, both stored the same way. We follow that.
- **Preset and custom are indistinguishable on read.** No flag says "this was
  a preset." The read side only ever needs a hex to paint the glyph. That
  keeps the column, the renderer, and validation to one code path.
- **No CSS custom properties.** The colour is per-record data, not a theme
  token, so it is applied inline (`style="color:#…"`) from the stored value.
  Nothing enters `app.css`; a re-themed app leaves user colours intact.
- **App-side validation, no CHECK.** Normalise (trim, strip `#`, lowercase)
  then accept only `^[0-9a-f]{6}$`. Mirrors the D32 icon-key validation and
  ADR 0009's "validate in code, no DB constraint" stance.

## Consequences

- One additive migration (`000021_icon_colour.sql`): `colour TEXT NULL` on
  `category` and `account`; existing rows stay NULL (no tint) and render
  unchanged.
- `internal/web/icons.go` gains the 16-colour palette slice,
  `colourValid`/normalisation, and the tinted glyph helper (an extension of the
  D32 `iconTag`/`iconStyle`).
- A new `colourPicker` Alpine component (anchored popover, two tabs: predefined
  grid + custom hex) on the category and account forms; the D32 icon-picker
  modal is unchanged.
- Rendering tints the existing `<i class="la la-…">` glyph inline on all six
  D32 surfaces.
- Permissions mirror the icon: category colour = membership; account colour =
  Edit grant.
- The curated 16 are a product decision kept in code (a Go slice); reordering
  or retuning them is a file edit, not a migration.

## Rejected alternatives

- **Curated colour registry (ADR 0009 style)** — rejected: the domain is open
  (any hex), so a fixed key set either rejects valid input or is pointless.
- **Store the colour name ("terracotta") and resolve to hex** — rejected:
  custom colours have no name; and it couples the stored value to a registry
  that cannot cover custom hex.
- **`<input type="color">` only (no free hex)** — rejected: the user wants
  ezbk-style free hex, not just the native picker's limited input.
- **Per-swatch CSS custom properties in `app.css`** — rejected: the value is
  per-record data, not a design token; inline CSS from the stored hex is the
  correct home.

## British English

The column, identifiers, template text and copy use "colour" (British), per
the user's standing preference, except where embedded in an unchangeable
third-party token (none here).
