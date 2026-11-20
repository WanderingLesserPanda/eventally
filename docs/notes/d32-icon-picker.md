# D32 — Category and account icons

Settled in the 2026-11-19 grill-with-docs session (two grill rounds + R2.4
deep-research round + two Penpot mockups, both approved "okay for MVP").
Status: **SHIPPED** (decisions signed off 2026-11-20; implemented exactly as
signed — migration 000019, vendored Line Awesome 2.1.0 (MIT, LICENSE.txt),
the 296-glyph code-managed registry (11 domain groups), the search/group
picker modal on both forms, app-side key validation with default
resolution, rendering on the six surfaces, the account-type emoji
retired; verified by the Go tests, smoke-d32.sh, and rehearse-d32.sh per
the section below).

Adds a selectable icon to every category and account. Icons render on the
six agreed surfaces, the existing account-type emoji is retired as the
display mechanism, and the selectable set of glyphs is a curated,
code-managed registry.

## Decisions (all settled)

| # | Item | Decision |
|---|------|----------|
| Q1 | Icon source | Vendored **Line Awesome 2.1.0** (Icons8, MIT) — font + CSS in the app, no CDN |
| Q2 | Registry | One curated file in code, domain-grouped, **296 glyphs** (§H). Expanding the set = file edit, no migration |
| Q3 | Storage | String glyph keys in `category.icon TEXT NULL` / `account.icon TEXT NULL`; NULL = unpicked → default renders (R2.4, Option A) |
| Q4 | Ownership | Household-level only; per-user icon overrides parked under permissions rework |
| Q5 | Custom uploads | Parked; the column shape keeps rework cheap (string can later point at a file reference) |
| Q6 | Permissions | Category icon: plain household membership (unchanged). Account icon: account Edit grant |
| Q7 | Defaults | Unpicked category → generic `tag` glyph; unpicked account → per-type default (§C). Emoji retired entirely |
| R2.1 | Permission tightening | Parked under permissions rework |
| R2.2 | Surfaces | All six: dashboard budget cards, /txns list + detail, review queue, forms, accounts grid, cards page |
| R2.3 | Generic-tag mockup | Approved — "okay for MVP, definitely needs improvement in the future" |
| R2.4 | Storage shape | String keys (Option A), after two research rounds |
| R2.5 | Picker mockup | Approved — "okay for MVP, might change in the future" |
| R2.6 | Curation | Assistant curates the initial registry (§H); user reviews it in this doc before sign-off |

## §A Storage (R2.4)

- `icon TEXT NULL` on both `category` and `account`. The stored value is the
  glyph's class name — second column of the §H tables (e.g. `la-coffee`).
  NULL means "unpicked": the UI renders the default and the picker shows
  the "use default" state. NULL and an explicit default render
  identically.
- **No CHECK constraint.** Keys are validated app-side against the registry.
  A CHECK would force DDL for every future registry expansion — the whole
  point of the code-managed registry is that adding icons is a file edit.
- **No backfill.** Existing rows stay NULL and render with defaults on
  day one.

## §B Migration

- `000019`: `ALTER TABLE category ADD COLUMN icon TEXT;`
  `ALTER TABLE account ADD COLUMN icon TEXT;` — additive, nullable, no
  backfill. Rehearsal + explicit green light, as every other migration.

## §C Rendering and defaults

- One registry entry per glyph: stored key, domain group, display label.
  Rendering is `<i class="la-…">` via the vendored font.
- Category default (icon IS NULL or unknown key): `la-tag`.
- Account default (icon IS NULL or unknown key), replacing the emoji
  lookup in `render.go` (`acctIcon`):

| Account type | Default glyph |
|---|---|
| savings | `la-piggy-bank` |
| ewallet | `la-mobile-alt` |
| credit_card | `la-credit-card` |
| loan | `la-file-invoice` |
| investment | `la-chart-line` |
| cash | `la-money-bill` |
| credits | `la-ticket-alt` |
| other / unknown | `la-tag` |

- An unknown key can only exist if someone hand-edits the DB (the registry
  only grows); it renders the default glyph, never a broken surface.

## §D Vendoring

- Into `internal/web/static/lineawesome/`: the two font files (woff,
  woff2), `line-awesome.min.css`, and `LICENSE.txt` (MIT, Icons8).
- Same pattern as the already-vendored alpine.min.js / htmx.min.js —
  served through the existing static route, no new web-stack dependency.

## §E Picker UX (R2.5 mockup, approved for MVP)

- Modal from the category and account forms (new + edit). Content: search
  box, domain-grouped glyph grid, preview chip of the current selection,
  an explicit **"No icon (use default)"** row, footer with save.
- Search matches glyph label and group name.
- All household members see the same registry (Q4 — household-level).

## §F Surfaces (R2.2)

1. Dashboard budget cards — category icon beside each category row.
2. /txns list + detail — category icon beside the category name.
3. Review queue — category icon on pending rows.
4. Forms — icon in the category select; the icon picker itself lives on
   the category and account forms.
5. Accounts grid — account icon (picked, or per-type default).
6. Cards page — account icon on card rows.

## §G Permissions (Q6)

- Setting a category icon: any household member (category CRUD already has
  no grant gate — unchanged).
- Setting an account icon: requires the account Edit grant (same gate as
  editing the account).
- Tightening (e.g. owner-only) parked under permissions rework (R2.1).

## §H Initial registry — 296 glyphs, 11 groups

Curated from Line Awesome 2.1.0 (the package contains 1,555 glyphs; this
set selects the 296 that fit a personal-finance household). Every key was
machine-validated against the package's `icons.json` — nothing in this table
is a phantom. **Review requested: groups, picks, and labels before
sign-off.**

### Food & Drink

| Stored key | Glyph | Display label |
|---|---|---|
| `utensils` | `la-utensils` | Utensils |
| `coffee` | `la-coffee` | Coffee |
| `mug-hot` | `la-mug-hot` | Hot drink |
| `cocktail` | `la-cocktail` | Cocktail |
| `glass-martini-alt` | `la-glass-martini-alt` | Martini |
| `wine-glass` | `la-wine-glass` | Wine |
| `beer` | `la-beer` | Beer |
| `pizza-slice` | `la-pizza-slice` | Pizza |
| `hamburger` | `la-hamburger` | Burger |
| `hotdog` | `la-hotdog` | Hot dog |
| `bread-slice` | `la-bread-slice` | Bread |
| `ice-cream` | `la-ice-cream` | Ice cream |
| `candy-cane` | `la-candy-cane` | Candy |
| `apple-alt` | `la-apple-alt` | Apple |
| `lemon` | `la-lemon` | Lemon |
| `carrot` | `la-carrot` | Carrot |
| `egg` | `la-egg` | Egg |
| `cheese` | `la-cheese` | Cheese |
| `fish` | `la-fish` | Fish |
| `seedling` | `la-seedling` | Produce |
| `concierge-bell` | `la-concierge-bell` | Service bell |
| `table` | `la-table` | Table |
### Shopping & Groceries

| Stored key | Glyph | Display label |
|---|---|---|
| `shopping-basket` | `la-shopping-basket` | Basket |
| `shopping-cart` | `la-shopping-cart` | Cart |
| `cart-plus` | `la-cart-plus` | Add to cart |
| `cart-arrow-down` | `la-cart-arrow-down` | Checkout |
| `shopping-bag` | `la-shopping-bag` | Bag |
| `store` | `la-store` | Store |
| `store-alt` | `la-store-alt` | Corner store |
| `box` | `la-box` | Box |
| `boxes` | `la-boxes` | Boxes |
| `box-open` | `la-box-open` | Open box |
| `archive` | `la-archive` | Archive |
| `recycle` | `la-recycle` | Recycling |
| `trash-alt` | `la-trash-alt` | Trash |
| `clipboard-list` | `la-clipboard-list` | List |
| `clipboard-check` | `la-clipboard-check` | Checklist |
| `receipt` | `la-receipt` | Receipt |
| `file-invoice` | `la-file-invoice` | Invoice |
| `file-invoice-dollar` | `la-file-invoice-dollar` | Paid invoice |
| `barcode` | `la-barcode` | Barcode |
| `ticket-alt` | `la-ticket-alt` | Ticket |
| `qrcode` | `la-qrcode` | QR code |
| `tag` | `la-tag` | Tag |
| `tags` | `la-tags` | Tags |
| `percentage` | `la-percentage` | Discount |
| `gem` | `la-gem` | Gem |
| `hand-holding-usd` | `la-hand-holding-usd` | Payment |
### Transport

| Stored key | Glyph | Display label |
|---|---|---|
| `car` | `la-car` | Car |
| `car-side` | `la-car-side` | Car (side) |
| `car-battery` | `la-car-battery` | Battery car |
| `car-crash` | `la-car-crash` | Crash |
| `taxi` | `la-taxi` | Taxi |
| `bus` | `la-bus` | Bus |
| `bus-alt` | `la-bus-alt` | Bus (alt) |
| `motorcycle` | `la-motorcycle` | Motorcycle |
| `bicycle` | `la-bicycle` | Bicycle |
| `truck` | `la-truck` | Truck |
| `truck-loading` | `la-truck-loading` | Loading |
| `truck-moving` | `la-truck-moving` | Moving |
| `plane` | `la-plane` | Plane |
| `plane-departure` | `la-plane-departure` | Departure |
| `plane-arrival` | `la-plane-arrival` | Arrival |
| `ship` | `la-ship` | Ship |
| `train` | `la-train` | Train |
| `subway` | `la-subway` | Subway |
| `gas-pump` | `la-gas-pump` | Fuel |
| `road` | `la-road` | Road |
| `map` | `la-map` | Map |
| `map-marked` | `la-map-marked` | Marked map |
| `map-marker-alt` | `la-map-marker-alt` | Location pin |
| `directions` | `la-directions` | Directions |
| `location-arrow` | `la-location-arrow` | Arrow |
| `compass` | `la-compass` | Compass |
| `traffic-light` | `la-traffic-light` | Traffic light |
| `parking` | `la-parking` | Parking |
| `walking` | `la-walking` | Walking |
### Travel & Stays

| Stored key | Glyph | Display label |
|---|---|---|
| `hotel` | `la-hotel` | Hotel |
| `bed` | `la-bed` | Bed |
| `suitcase` | `la-suitcase` | Suitcase |
| `luggage-cart` | `la-luggage-cart` | Luggage |
| `passport` | `la-passport` | Passport |
| `globe` | `la-globe` | World |
| `globe-americas` | `la-globe-americas` | Americas |
| `campground` | `la-campground` | Campground |
| `mountain` | `la-mountain` | Mountain |
| `water` | `la-water` | Water |
| `swimmer` | `la-swimmer` | Swimming |
| `umbrella-beach` | `la-umbrella-beach` | Beach |
| `spa` | `la-spa` | Spa |
| `anchor` | `la-anchor` | Anchor |
### Home & Bills

| Stored key | Glyph | Display label |
|---|---|---|
| `home` | `la-home` | Home |
| `warehouse` | `la-warehouse` | Warehouse |
| `building` | `la-building` | Building |
| `city` | `la-city` | City |
| `church` | `la-church` | Church |
| `mosque` | `la-mosque` | Mosque |
| `synagogue` | `la-synagogue` | Synagogue |
| `couch` | `la-couch` | Sofa |
| `chair` | `la-chair` | Chair |
| `door-open` | `la-door-open` | Door |
| `window-close` | `la-window-close` | Window |
| `lightbulb` | `la-lightbulb` | Bulb |
| `plug` | `la-plug` | Plug |
| `bolt` | `la-bolt` | Electricity |
| `battery-full` | `la-battery-full` | Battery |
| `wifi` | `la-wifi` | Wi-Fi |
| `blender` | `la-blender` | Blender |
| `fan` | `la-fan` | Fan |
| `fire` | `la-fire` | Fire |
| `fire-extinguisher` | `la-fire-extinguisher` | Extinguisher |
| `tools` | `la-tools` | Tools |
| `wrench` | `la-wrench` | Wrench |
| `hammer` | `la-hammer` | Hammer |
| `paint-roller` | `la-paint-roller` | Paint roller |
| `paint-brush` | `la-paint-brush` | Paint brush |
| `broom` | `la-broom` | Broom |
| `spray-can` | `la-spray-can` | Spray can |
| `toilet` | `la-toilet` | Toilet |
| `shower` | `la-shower` | Shower |
| `key` | `la-key` | Key |
### Work & Career

| Stored key | Glyph | Display label |
|---|---|---|
| `briefcase` | `la-briefcase` | Briefcase |
| `briefcase-medical` | `la-briefcase-medical` | Medical case |
| `user-tie` | `la-user-tie` | Professional |
| `user-graduate` | `la-user-graduate` | Graduate |
| `graduation-cap` | `la-graduation-cap` | Graduation |
| `school` | `la-school` | School |
| `chalkboard-teacher` | `la-chalkboard-teacher` | Teacher |
| `book` | `la-book` | Book |
| `book-open` | `la-book-open` | Open book |
| `book-reader` | `la-book-reader` | Reading |
| `laptop` | `la-laptop` | Laptop |
| `desktop` | `la-desktop` | Desktop |
| `tablet-alt` | `la-tablet-alt` | Tablet |
| `mobile-alt` | `la-mobile-alt` | Phone |
| `server` | `la-server` | Server |
| `database` | `la-database` | Database |
| `network-wired` | `la-network-wired` | Network |
| `cloud` | `la-cloud` | Cloud |
| `cloud-upload-alt` | `la-cloud-upload-alt` | Upload |
| `cloud-download-alt` | `la-cloud-download-alt` | Download |
| `file` | `la-file` | File |
| `file-alt` | `la-file-alt` | Document |
| `file-contract` | `la-file-contract` | Contract |
| `file-signature` | `la-file-signature` | Signature |
| `folder` | `la-folder` | Folder |
| `folder-open` | `la-folder-open` | Open folder |
| `inbox` | `la-inbox` | Inbox |
| `envelope` | `la-envelope` | Envelope |
| `envelope-open` | `la-envelope-open` | Open mail |
| `comment` | `la-comment` | Message |
| `comments` | `la-comments` | Messages |
| `phone` | `la-phone` | Phone |
| `print` | `la-print` | Print |
| `calculator` | `la-calculator` | Calculator |
| `chart-line` | `la-chart-line` | Chart |
| `tasks` | `la-tasks` | Tasks |
| `project-diagram` | `la-project-diagram` | Diagram |
| `sitemap` | `la-sitemap` | Structure |
| `list` | `la-list` | List |
### Health & Fitness

| Stored key | Glyph | Display label |
|---|---|---|
| `heart` | `la-heart` | Heart |
| `heart-broken` | `la-heart-broken` | Broken heart |
| `heartbeat` | `la-heartbeat` | Pulse |
| `first-aid` | `la-first-aid` | First aid |
| `ambulance` | `la-ambulance` | Ambulance |
| `hospital` | `la-hospital` | Hospital |
| `hospital-alt` | `la-hospital-alt` | Hospital (alt) |
| `prescription-bottle-alt` | `la-prescription-bottle-alt` | Pharmacy |
| `notes-medical` | `la-notes-medical` | Medical notes |
| `allergies` | `la-allergies` | Allergies |
| `capsules` | `la-capsules` | Capsules |
| `pills` | `la-pills` | Pill |
| `syringe` | `la-syringe` | Syringe |
| `user-md` | `la-user-md` | Doctor |
| `stethoscope` | `la-stethoscope` | Stethoscope |
| `dna` | `la-dna` | DNA |
| `brain` | `la-brain` | Brain |
| `bone` | `la-bone` | Bone |
| `wheelchair` | `la-wheelchair` | Wheelchair |
| `running` | `la-running` | Running |
| `football-ball` | `la-football-ball` | Football |
| `futbol` | `la-futbol` | Soccer |
| `basketball-ball` | `la-basketball-ball` | Basketball |
| `baseball-ball` | `la-baseball-ball` | Baseball |
| `golf-ball` | `la-golf-ball` | Golf |
| `dumbbell` | `la-dumbbell` | Dumbbell |
| `bath` | `la-bath` | Bath |
| `hand-paper` | `la-hand-paper` | Hand |
| `hand-holding-heart` | `la-hand-holding-heart` | Charity hand |
| `praying-hands` | `la-praying-hands` | Praying hands |
### Lifestyle & Leisure

| Stored key | Glyph | Display label |
|---|---|---|
| `gamepad` | `la-gamepad` | Gamepad |
| `chess` | `la-chess` | Chess |
| `chess-knight` | `la-chess-knight` | Chess knight |
| `dice` | `la-dice` | Dice |
| `dice-d20` | `la-dice-d20` | D20 |
| `theater-masks` | `la-theater-masks` | Theater |
| `music` | `la-music` | Music |
| `guitar` | `la-guitar` | Guitar |
| `drum` | `la-drum` | Drum |
| `film` | `la-film` | Film |
| `video` | `la-video` | Video |
| `camera` | `la-camera` | Camera |
| `camera-retro` | `la-camera-retro` | Retro camera |
| `image` | `la-image` | Photo |
| `palette` | `la-palette` | Palette |
| `brush` | `la-brush` | Brush |
| `pencil-alt` | `la-pencil-alt` | Pencil |
| `cut` | `la-cut` | Scissors |
| `ruler` | `la-ruler` | Ruler |
| `newspaper` | `la-newspaper` | Newspaper |
| `blog` | `la-blog` | Blog |
| `podcast` | `la-podcast` | Podcast |
| `microphone` | `la-microphone` | Mic |
| `microphone-alt` | `la-microphone-alt` | Mic (alt) |
| `headphones` | `la-headphones` | Headphones |
| `headphones-alt` | `la-headphones-alt` | Headphones (alt) |
| `tv` | `la-tv` | TV |
| `satellite` | `la-satellite` | Satellite |
| `satellite-dish` | `la-satellite-dish` | Dish |
| `rocket` | `la-rocket` | Rocket |
| `star` | `la-star` | Star |
| `star-half` | `la-star-half` | Star (half) |
| `crown` | `la-crown` | Crown |
| `medal` | `la-medal` | Medal |
| `trophy` | `la-trophy` | Trophy |
| `award` | `la-award` | Award |
### Family & Gifts

| Stored key | Glyph | Display label |
|---|---|---|
| `users` | `la-users` | Family |
| `user-friends` | `la-user-friends` | Friends |
| `user` | `la-user` | Person |
| `baby` | `la-baby` | Baby |
| `child` | `la-child` | Child |
| `baby-carriage` | `la-baby-carriage` | Stroller |
| `gift` | `la-gift` | Gift |
| `handshake` | `la-handshake` | Handshake |
| `user-shield` | `la-user-shield` | Protected person |
| `id-card` | `la-id-card` | ID |
| `id-card-alt` | `la-id-card-alt` | ID (alt) |
| `fingerprint` | `la-fingerprint` | Fingerprint |
| `dog` | `la-dog` | Dog |
| `cat` | `la-cat` | Cat |
| `paw` | `la-paw` | Paw |
| `feather` | `la-feather` | Feather |
| `leaf` | `la-leaf` | Leaf |
| `tree` | `la-tree` | Tree |
### Finance & Money

| Stored key | Glyph | Display label |
|---|---|---|
| `piggy-bank` | `la-piggy-bank` | Piggy bank |
| `wallet` | `la-wallet` | Wallet |
| `credit-card` | `la-credit-card` | Card |
| `coins` | `la-coins` | Coins |
| `money-bill` | `la-money-bill` | Cash |
| `money-bill-wave` | `la-money-bill-wave` | Cash (alt) |
| `money-check` | `la-money-check` | Check |
| `money-check-alt` | `la-money-check-alt` | Bank check |
| `dollar-sign` | `la-dollar-sign` | Dollar |
| `exchange-alt` | `la-exchange-alt` | Exchange |
| `landmark` | `la-landmark` | Bank |
| `university` | `la-university` | Bank (university) |
| `balance-scale` | `la-balance-scale` | Balance |
| `gavel` | `la-gavel` | Court |
| `bitcoin` | `la-bitcoin` | Bitcoin |
| `ethereum` | `la-ethereum` | Ethereum |
| `cc-visa` | `la-cc-visa` | Visa |
| `cc-mastercard` | `la-cc-mastercard` | Mastercard |
| `cc-amex` | `la-cc-amex` | Amex |
| `cc-discover` | `la-cc-discover` | Discover |
| `cc-paypal` | `la-cc-paypal` | PayPal |
| `cc-apple-pay` | `la-cc-apple-pay` | Apple Pay |
| `cc-stripe` | `la-cc-stripe` | Stripe |
### Other

| Stored key | Glyph | Display label |
|---|---|---|
| `question` | `la-question` | Question |
| `question-circle` | `la-question-circle` | Question (circle) |
| `info` | `la-info` | Info |
| `exclamation-triangle` | `la-exclamation-triangle` | Alert |
| `ban` | `la-ban` | Prohibited |
| `check` | `la-check` | Check |
| `check-circle` | `la-check-circle` | Confirmed |
| `times` | `la-times` | Close |
| `times-circle` | `la-times-circle` | Close (circle) |
| `plus` | `la-plus` | Add |
| `minus` | `la-minus` | Remove |
| `search` | `la-search` | Search |
| `filter` | `la-filter` | Filter |
| `sliders-h` | `la-sliders-h` | Filters |
| `cog` | `la-cog` | Settings |
| `cogs` | `la-cogs` | Gears |
| `clock` | `la-clock` | Clock |
| `hourglass` | `la-hourglass` | Timer |
| `calendar` | `la-calendar` | Calendar |
| `download` | `la-download` | Download |
| `upload` | `la-upload` | Upload |
| `share-alt` | `la-share-alt` | Share |
| `link` | `la-link` | Link |
| `bookmark` | `la-bookmark` | Bookmark |
| `flag` | `la-flag` | Flag |
| `moon` | `la-moon` | Moon |
| `sun` | `la-sun` | Sun |
| `snowflake` | `la-snowflake` | Snow |
| `bluetooth` | `la-bluetooth` | Bluetooth |
## §I Parked / out of scope

- User-uploaded custom icons (Q5) — column shaped so rework is cheap.
- Per-user icon overrides (Q4) — parked under permissions rework.
- Permission tightening for icons (R2.1) — parked under permissions rework.
- Picker UX refinements (R2.3 / R2.5) — "okay for MVP", improve later.
- Expanding the icon set: edit the one registry file, no migration.

## §J Research notes (R2.4 — why string keys)

Two research rounds (2026-11-19) on how icon identity is stored in the wild:

- **Iconify** (the largest icon registry, 200k+ icons): every icon is
  addressed by a stable `prefix:name` string. The name is the identity;
  codepoints and indices are implementation details.
- **Font Awesome / Line Awesome**: the public identity of a glyph is its
  CSS class name. The Unicode codepoint is internal detail that can shift
  between font versions — storing codepoints would couple the DB to font
  internals.
- **GitHub reactions**: a fixed, app-managed list of strings (`"heart"`,
  `"rocket"`, …). **Notion page icons**: the emoji character string itself
  is the identity. Both are string identity over an app-managed set —
  exactly this use case.
- **ezbookkeeping** (closest analogue, also uses Line Awesome): stable
  *banded* integer codes (1–99 Food, 1000–1999 Currency, …) mapping to
  `la-*` classes. A proven design — but the integers are opaque in the DB
  and the mapping lives in code anyway. Equally safe, less readable.
  Noted as a legitimate alternative, not chosen.
- **Actual Budget** (cautionary tale): embeds the emoji *in the category
  name string* with no identity system → ambiguity and rendering bugs.

**Decision: Option A — string glyph keys** (e.g. `la-coffee`), `TEXT NULL`.
Option B (ezbookkeeping-style banded int codes) noted as a proven
alternative; Option C (DB table + FK) ruled out — contradicts the settled
"one registry file, no migration" decision.

## Verification

- `internal/web/d32_test.go` — `TestD32Registry` (296 unique `la-*` keys, 11
  groups, every key resolvable in the vendored CSS, no CSS duplicates,
  default resolution for unknown/empty keys + the per-account-type map),
  `TestD32CategoryIconAnyMember` (plain member set + unknown key rejected),
  `TestD32AccountIconNeedsEditGrant` (403 without the Edit grant, edit-all
  grantee succeeds, unknown key rejected),
  `TestD32AccountDefaultIcons` (per-type defaults render).
- `smoke-d32.sh` — scratch-DB HTTP pass: LA assets serve (CSS + font +
  LICENSE), picker markup on both forms (296 cells + "No icon" row + the
  registered `iconPicker` Alpine component), category icon set/edited by a
  plain member, unknown key rejected with no row, account icon set by the
  owner, 403 without the Edit grant (and no write), icons (picked +
  defaults) render on all six surfaces, no emoji glyphs on the accounts
  grid. The picker's client-side behavior (select, save-to-hidden-input,
  no-icon clear, search by label and group) is verified headlessly with a
  Node harness driving the real component code against the rendered
  markup.
- `rehearse-d32.sh` — cutover rehearsal on a `eventally_dev` template copy:
  auto-migration to v19 (000017/18/19), the icon columns additive with every
  pre-existing row NULL, budget totals bit-identical before/after, full Go
  suite green. The live `eventally_dev` migration (16 → 19) is still
  gated on explicit approval + fresh backup, per the standing rule.

## Sign-off

User reviews §H (the registry) and the decisions above. Sign-off authorizes
implementation: vendoring (§D), migration 000019 (§B), the registry file,
picker (§E), and the six render surfaces (§F) — tracked under ADR 0009.

Signed off 2026-11-20 (icon source confirmed as Line Awesome 2.1.0 after a
full 296-icon comparison board against Lucide; see ADR 0009 rejected
alternatives).
