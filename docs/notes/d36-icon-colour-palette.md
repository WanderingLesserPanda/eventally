# D36 — Icon colours for categories and accounts

Settled in the 2026-11-24 grill-with-docs round (one grill round + a rendered
16-colour comparison board + an ezBookkeeping reference read). Status:
**DESIGNED / signed off 2026-11-24** — the design is complete and signed off;
implementation is **gated on the user's green light** (no code yet; docs
committed first).

Extends D32 (icon picker, SHIPPED). Adds a tint colour to the glyph a user
already picks for a category or account: the chosen icon can be coloured from
a curated 16-swatch palette or any custom hex value (ezBookkeeping-style),
stored alongside the icon key. The glyph itself and the picker modal are
unchanged — colour is a **separate field + anchored popover** on the same two
forms.

## Decisions (all settled)

| # | Item | Decision |
|---|------|----------|
| C1 | Scope | Both **categories** and **accounts** (the same six surfaces as D32) |
| C2 | Storage | `colour TEXT NULL` on `category` and `account`; value = 6 lowercase hex digits, no `#` (e.g. `b84810`); NULL / empty = no tint (glyph inherits the normal text colour) |
| C3 | Presets | **16** curated colours (§B) — the warm design system widened to the full spectrum; a curated Go slice (one source of truth, no CSS table, no DB) |
| C4 | Custom | Free-form hex (`#rrggbb`), ezBookkeeping-style — preset and custom stored **identically** as a hex string; no flag distinguishes them |
| C5 | Placement | Separate **Colour** field + an **anchored popover** on the category and account forms — NOT inside the D32 icon-picker modal (which stays glyph-only) |
| C6 | Popover | Two tabs — **Predefined colours** (16-swatch grid + "No colour" clear) and **Custom colour** (hex input + native `<input type="color">`, live tint preview) |
| C7 | Validation | App-side: normalise (trim, strip a leading `#`, lowercase); accept only `^[0-9a-f]{6}$`; reject anything else (mirrors the D32 icon-key validation) |
| C8 | Rendering | Tint applied inline as `style="color:#<hex>"` on the existing `<i class="la la-…">` glyph, on all six D32 surfaces; no per-swatch CSS custom properties (the value is per-record data, not a theme token) |
| C9 | Permissions | Category colour = plain household membership; account colour = account **Edit** grant (both mirror their icon, D32 §G) |
| C10 | Migration | `000021_icon_colour.sql` — two purely additive `ALTER TABLE … ADD COLUMN colour TEXT NULL` |
| C11 | Language | British English "**colour**" throughout (Go identifiers, the column, template text, copy) unless embedded in an unchangeable third-party token |

## §A Storage (C2, C7)

- `category.colour` / `account.colour` — `TEXT NULL`.
- Stored value: **6 lowercase hex digits, no `#`** — `b84810`, not `#B84810`.
- NULL or empty = "no tint": the glyph renders in the inherited text colour.
- No CHECK constraint. Validation is app-side (C7), the D32 icon-key pattern
  and ADR-0009 philosophy (curated set in code, free value validated, no DB
  constraint).

## §B The 16 predefined colours (C3)

A single Go slice (name + hex) in `internal/web/icons.go` — the source of
truth for the popover grid and for the app-side "is this a known preset"
lookup. Order = board order (warm → cool → neutral).

| # | Name | Hex |
|---|------|-----|
| 1 | Terracotta | `#B84810` |
| 2 | Coral | `#E86A4A` |
| 3 | Amber | `#FF9500` |
| 4 | Honey | `#F2B32E` |
| 5 | Leaf | `#7BA32E` |
| 6 | Fern | `#2E8B57` |
| 7 | Teal | `#009688` |
| 8 | Lagoon | `#4FB3D9` |
| 9 | Ocean | `#2196F3` |
| 10 | Indigo | `#3F51B5` |
| 11 | Violet | `#673AB7` |
| 12 | Orchid | `#9C27B0` |
| 13 | Rose | `#E0559D` |
| 14 | Brick | `#D64545` |
| 15 | Slate | `#8E8E93` |
| 16 | Ink | `#3A3A3A` |

The palette widens the D35 warm pass into the full spectrum (the system was
warm-only; the full wheel lets a user match a category's mood — green for
groceries, blue for utilities, etc.). Confirmed "keep as-is" by the user on
the rendered comparison board (2026-11-24).

## §C Custom colour (C4, C6)

- "Custom colour" tab: a `#rrggbb` text input **and** a native
  `<input type="color">` for convenience; a live tint preview of the current
  icon.
- Preset and custom are stored **identically** as a 6-hex string — there is
  no "preset vs custom" flag. (This is the ezBookkeeping model: a `color`
  string column; the picker offers a scheme list *and* free hex; both land in
  the same column.)

## §D Placement (C5, C6)

- A new **Colour** field + anchored popover on
  `reference_category_form.html` and `reference_account_form.html`, sibling to
  the existing icon control.
- A new Alpine component `colourPicker` (parallel to the D32 `iconPicker`),
  an **anchored popover** (not a modal), opened from the Colour field. The
  D32 icon-picker modal is **untouched**.
- Selecting a swatch or committing a hex sets a hidden `<input name="colour">`
  (6 lowercase hex, or empty to clear). The form posts `colour` alongside
  `icon`.

## §E Rendering (C8)

- Home: `internal/web/icons.go` (the D32 icon helpers already live here:
  `iconValid`, `resolveCategoryIcon`, `resolveAccountIcon`, `iconStyle`,
  `iconTag`).
- Extend the glyph helper to take a colour:
  `iconTag(key, colour string) template.HTML` →
  `<i class="la la-<key>" style="color:#<colour>"></i>` (style omitted when
  colour is empty). `iconStyle(key)` gains the tint too.
- The tint is inline CSS from the stored value — **no per-swatch CSS custom
  properties** (the value is per-record data, not a design token, so nothing
  goes in `app.css`).
- Colours render wherever the icon already renders — all **six D32 surfaces**:
  dashboard budget cards, /txns list + detail, review queue, forms (icon in
  the category select + the picker on both forms), accounts grid, cards page.

## §F Permissions (C9)

- **Category** colour: plain household membership (any member) — same as the
  category icon (D32 §G).
- **Account** colour: account **Edit** grant — same as the account icon
  (D32 §G).
- No new permission kinds; colour piggybacks on the icon's existing gate.

## §G Migration (C10)

`internal/db/migrations/000021_icon_colour.sql`:

```sql
ALTER TABLE category ADD COLUMN colour TEXT NULL;
ALTER TABLE account  ADD COLUMN colour TEXT NULL;
```

Purely additive. Existing rows → NULL = no tint = current behaviour,
bit-identical rendering until a colour is set.

## §H British English (C11)

"colour" (not "color") everywhere D36 introduces text: Go identifiers
(`colourValid`, `colourStyle`, `colourPicker`), the `colour` column and hidden
input, template labels ("Colour"), and any user-facing copy. Only a token
embedded in an unchangeable third-party library would keep its original
spelling — not applicable here (the Line Awesome class names don't use the
word).

## Parked / Open (not built this wave)

- **Swatch reordering / user-defined palette edits** — the 16 are fixed for
  now; the palette is a Go slice, so reordering is a file edit later.
- **Contrast guard** — no automatic contrast check against the panel; the user
  picks what they can see. (Optional future: warn when a chosen hex has low
  contrast on the light panel.)
- **Per-surface tint override** — colour is one value per record, applied
  everywhere (no per-surface tinting).

## Verification / rehearsal (planned, post sign-off)

- `internal/web/d36_test.go` — palette integrity (16 unique valid hex,
  distinct names); the `colourValid` accept/reject matrix (`#B84810` → ok after
  normalise; `b8481` → reject; `zzz` → reject; `#GG0000` → reject; empty →
  clear); category colour set by a plain member; account colour gated by the
  Edit grant (403 without); an invalid hex rejected with no row; tinted
  `iconTag` output (style present when set, absent when NULL).
- `smoke-d36.sh` — scratch-DB HTTP pass: both forms carry the Colour popover
  (16 swatches + "No colour" + custom hex + native picker) and the hidden
  `colour` input; a category/account saved with a preset and with a custom hex
  render tinted on all six surfaces; an invalid hex is rejected with no write;
  clearing returns the glyph to the inherited text colour.
- `rehearse-d36.sh` — cutover rehearsal on a `eventally_dev` template copy:
  auto-migration to the new version, the `colour` columns additive and
  all-NULL, budget totals bit-identical, full Go suite green. The live
  `eventally_dev` migration stays gated on an explicit green light + fresh
  backup (standing rule).

## Sign-off

Sign-off (given 2026-11-24, on the rendered 16-colour comparison board)
authorizes **implementation**: migration 000021, the palette + validation in
`icons.go`, the `colourPicker` popover on both forms, and the tinted rendering
on the six surfaces — tracked under ADR 0013 (extends ADR 0009).
Implementation is separately gated on the user's green light.

Locked as decided: the 16 predefined colours ("keep as-is") and the placement
(separate Colour field + anchored popover, per the recommendation the user
followed).
