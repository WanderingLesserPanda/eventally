# Icon keys are stable strings from a code-managed registry

Decided 2026-11-19 (grill-with-docs session, D32 wave).

`category.icon` and `account.icon` are `TEXT NULL` columns whose value is a
glyph key from a curated, code-managed registry (Line Awesome 2.1.0). The
stored value is the glyph's class name (e.g. `la-coffee`); NULL means
"unpicked — render the default". The registry is a single Go file, the
source of truth for the picker UI, the renderer, and app-side validation.
Adding icons is a file edit, not a migration.

## Why

- The DB needs a **stable identity** for a chosen glyph. The field
  converges on one of two shapes: human-readable strings over an
  app-managed set (Iconify `prefix:name`, GitHub reaction strings, Notion
  emoji strings), or stable opaque integers (ezbookkeeping's banded codes).
  Nobody stores font codepoints or list positions — both shift under the
  feet of the data.
- Strings are **self-describing in a SQL dump** (`icon = 'la-coffee'`),
  debuggable without the registry open, and immune to reordering the file.
- Code-managed (not a DB table) because the selectable set is a **product
  decision, not user data**: one file, one review diff, zero migrations to
  grow. A DB table + FK would need a migration for every added icon and
  treats a constant as mutable data.
- No CHECK constraint: validation is app-side against the registry, so
  expansion stays a pure file edit.

## Consequences

- `icon` can hold a value no longer in the registry only if someone
  hand-edits the DB (the registry only grows); the renderer falls back to
  the default glyph — a stale key never breaks a surface.
- The picker, the renderer, and the tests all read the same Go registry —
  no drift between what can be picked and what can be rendered.
- Custom icons (user uploads) remain parked. When built, the column
  already holds a string that can later point at a file reference under a
  separate namespace.

## Rejected alternatives

- **Font Unicode codepoint**: implementation detail; can shift between
  font versions.
- **Integer index into the registry list**: position is not identity;
  reordering the file would silently remap every stored icon.
- **Stable integer codes** (ezbookkeeping style): equally safe, but opaque
  in the DB and still requires a parallel mapping table in code. Noted as a
  proven alternative, not chosen.
- **DB table + FK**: a migration per added icon; contradicts the settled
  "one registry file, no migration" decision.
- **Lucide (ISC, SVG set)** as the icon source (evaluated 2026-11-20): a
  full 296-icon side-by-side board was built for the decision. Lucide had
  an equivalent for 285/296 picks (54 closest-match only), but the 11
  payment-brand glyphs have no equivalent — Lucide ships no brand icons —
  and the user chose Line Awesome on visual preference. String-key storage
  keeps this reversible at the rendering layer.
