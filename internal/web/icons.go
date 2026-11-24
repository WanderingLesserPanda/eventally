// D32 icon registry (ADR-0009, docs/notes/d32-icon-picker.md §H).
//
// The curated, code-managed set of selectable glyphs: 296 Line Awesome
// 2.1.0 entries (Icons8, MIT — vendored in static/lineawesome/) in 11
// domain groups, transcribed exactly from the design doc's §H tables. This
// file is the single source of truth for the picker UI, the renderer, and
// app-side validation. Adding glyphs is a file edit here — no migration,
// no DB table (the icon columns hold the key as TEXT, validated here).
package web

import (
	"html"
	"html/template"
	"strings"
)

// iconGlyph is one registry entry. Key is the stored value and the CSS
// class name (e.g. "la-coffee"); Group the domain group; Label the
// picker's display text.
type iconGlyph struct {
	Key   string
	Group string
	Label string
}

// iconGroup is a domain group with its glyphs in §H order.
type iconGroup struct {
	Name   string
	Glyphs []iconGlyph
}

// iconEntries is the registry in §H order (group order, row order).
var iconEntries = []iconGlyph{
	// Food & Drink
	{"la-utensils", "Food & Drink", "Utensils"},
	{"la-coffee", "Food & Drink", "Coffee"},
	{"la-mug-hot", "Food & Drink", "Hot drink"},
	{"la-cocktail", "Food & Drink", "Cocktail"},
	{"la-glass-martini-alt", "Food & Drink", "Martini"},
	{"la-wine-glass", "Food & Drink", "Wine"},
	{"la-beer", "Food & Drink", "Beer"},
	{"la-pizza-slice", "Food & Drink", "Pizza"},
	{"la-hamburger", "Food & Drink", "Burger"},
	{"la-hotdog", "Food & Drink", "Hot dog"},
	{"la-bread-slice", "Food & Drink", "Bread"},
	{"la-ice-cream", "Food & Drink", "Ice cream"},
	{"la-candy-cane", "Food & Drink", "Candy"},
	{"la-apple-alt", "Food & Drink", "Apple"},
	{"la-lemon", "Food & Drink", "Lemon"},
	{"la-carrot", "Food & Drink", "Carrot"},
	{"la-egg", "Food & Drink", "Egg"},
	{"la-cheese", "Food & Drink", "Cheese"},
	{"la-fish", "Food & Drink", "Fish"},
	{"la-seedling", "Food & Drink", "Produce"},
	{"la-concierge-bell", "Food & Drink", "Service bell"},
	{"la-table", "Food & Drink", "Table"},
	// Shopping & Groceries
	{"la-shopping-basket", "Shopping & Groceries", "Basket"},
	{"la-shopping-cart", "Shopping & Groceries", "Cart"},
	{"la-cart-plus", "Shopping & Groceries", "Add to cart"},
	{"la-cart-arrow-down", "Shopping & Groceries", "Checkout"},
	{"la-shopping-bag", "Shopping & Groceries", "Bag"},
	{"la-store", "Shopping & Groceries", "Store"},
	{"la-store-alt", "Shopping & Groceries", "Corner store"},
	{"la-box", "Shopping & Groceries", "Box"},
	{"la-boxes", "Shopping & Groceries", "Boxes"},
	{"la-box-open", "Shopping & Groceries", "Open box"},
	{"la-archive", "Shopping & Groceries", "Archive"},
	{"la-recycle", "Shopping & Groceries", "Recycling"},
	{"la-trash-alt", "Shopping & Groceries", "Trash"},
	{"la-clipboard-list", "Shopping & Groceries", "List"},
	{"la-clipboard-check", "Shopping & Groceries", "Checklist"},
	{"la-receipt", "Shopping & Groceries", "Receipt"},
	{"la-file-invoice", "Shopping & Groceries", "Invoice"},
	{"la-file-invoice-dollar", "Shopping & Groceries", "Paid invoice"},
	{"la-barcode", "Shopping & Groceries", "Barcode"},
	{"la-ticket-alt", "Shopping & Groceries", "Ticket"},
	{"la-qrcode", "Shopping & Groceries", "QR code"},
	{"la-tag", "Shopping & Groceries", "Tag"},
	{"la-tags", "Shopping & Groceries", "Tags"},
	{"la-percentage", "Shopping & Groceries", "Discount"},
	{"la-gem", "Shopping & Groceries", "Gem"},
	{"la-hand-holding-usd", "Shopping & Groceries", "Payment"},
	// Transport
	{"la-car", "Transport", "Car"},
	{"la-car-side", "Transport", "Car (side)"},
	{"la-car-battery", "Transport", "Battery car"},
	{"la-car-crash", "Transport", "Crash"},
	{"la-taxi", "Transport", "Taxi"},
	{"la-bus", "Transport", "Bus"},
	{"la-bus-alt", "Transport", "Bus (alt)"},
	{"la-motorcycle", "Transport", "Motorcycle"},
	{"la-bicycle", "Transport", "Bicycle"},
	{"la-truck", "Transport", "Truck"},
	{"la-truck-loading", "Transport", "Loading"},
	{"la-truck-moving", "Transport", "Moving"},
	{"la-plane", "Transport", "Plane"},
	{"la-plane-departure", "Transport", "Departure"},
	{"la-plane-arrival", "Transport", "Arrival"},
	{"la-ship", "Transport", "Ship"},
	{"la-train", "Transport", "Train"},
	{"la-subway", "Transport", "Subway"},
	{"la-gas-pump", "Transport", "Fuel"},
	{"la-road", "Transport", "Road"},
	{"la-map", "Transport", "Map"},
	{"la-map-marked", "Transport", "Marked map"},
	{"la-map-marker-alt", "Transport", "Location pin"},
	{"la-directions", "Transport", "Directions"},
	{"la-location-arrow", "Transport", "Arrow"},
	{"la-compass", "Transport", "Compass"},
	{"la-traffic-light", "Transport", "Traffic light"},
	{"la-parking", "Transport", "Parking"},
	{"la-walking", "Transport", "Walking"},
	// Travel & Stays
	{"la-hotel", "Travel & Stays", "Hotel"},
	{"la-bed", "Travel & Stays", "Bed"},
	{"la-suitcase", "Travel & Stays", "Suitcase"},
	{"la-luggage-cart", "Travel & Stays", "Luggage"},
	{"la-passport", "Travel & Stays", "Passport"},
	{"la-globe", "Travel & Stays", "World"},
	{"la-globe-americas", "Travel & Stays", "Americas"},
	{"la-campground", "Travel & Stays", "Campground"},
	{"la-mountain", "Travel & Stays", "Mountain"},
	{"la-water", "Travel & Stays", "Water"},
	{"la-swimmer", "Travel & Stays", "Swimming"},
	{"la-umbrella-beach", "Travel & Stays", "Beach"},
	{"la-spa", "Travel & Stays", "Spa"},
	{"la-anchor", "Travel & Stays", "Anchor"},
	// Home & Bills
	{"la-home", "Home & Bills", "Home"},
	{"la-warehouse", "Home & Bills", "Warehouse"},
	{"la-building", "Home & Bills", "Building"},
	{"la-city", "Home & Bills", "City"},
	{"la-church", "Home & Bills", "Church"},
	{"la-mosque", "Home & Bills", "Mosque"},
	{"la-synagogue", "Home & Bills", "Synagogue"},
	{"la-couch", "Home & Bills", "Sofa"},
	{"la-chair", "Home & Bills", "Chair"},
	{"la-door-open", "Home & Bills", "Door"},
	{"la-window-close", "Home & Bills", "Window"},
	{"la-lightbulb", "Home & Bills", "Bulb"},
	{"la-plug", "Home & Bills", "Plug"},
	{"la-bolt", "Home & Bills", "Electricity"},
	{"la-battery-full", "Home & Bills", "Battery"},
	{"la-wifi", "Home & Bills", "Wi-Fi"},
	{"la-blender", "Home & Bills", "Blender"},
	{"la-fan", "Home & Bills", "Fan"},
	{"la-fire", "Home & Bills", "Fire"},
	{"la-fire-extinguisher", "Home & Bills", "Extinguisher"},
	{"la-tools", "Home & Bills", "Tools"},
	{"la-wrench", "Home & Bills", "Wrench"},
	{"la-hammer", "Home & Bills", "Hammer"},
	{"la-paint-roller", "Home & Bills", "Paint roller"},
	{"la-paint-brush", "Home & Bills", "Paint brush"},
	{"la-broom", "Home & Bills", "Broom"},
	{"la-spray-can", "Home & Bills", "Spray can"},
	{"la-toilet", "Home & Bills", "Toilet"},
	{"la-shower", "Home & Bills", "Shower"},
	{"la-key", "Home & Bills", "Key"},
	// Work & Career
	{"la-briefcase", "Work & Career", "Briefcase"},
	{"la-briefcase-medical", "Work & Career", "Medical case"},
	{"la-user-tie", "Work & Career", "Professional"},
	{"la-user-graduate", "Work & Career", "Graduate"},
	{"la-graduation-cap", "Work & Career", "Graduation"},
	{"la-school", "Work & Career", "School"},
	{"la-chalkboard-teacher", "Work & Career", "Teacher"},
	{"la-book", "Work & Career", "Book"},
	{"la-book-open", "Work & Career", "Open book"},
	{"la-book-reader", "Work & Career", "Reading"},
	{"la-laptop", "Work & Career", "Laptop"},
	{"la-desktop", "Work & Career", "Desktop"},
	{"la-tablet-alt", "Work & Career", "Tablet"},
	{"la-mobile-alt", "Work & Career", "Phone"},
	{"la-server", "Work & Career", "Server"},
	{"la-database", "Work & Career", "Database"},
	{"la-network-wired", "Work & Career", "Network"},
	{"la-cloud", "Work & Career", "Cloud"},
	{"la-cloud-upload-alt", "Work & Career", "Upload"},
	{"la-cloud-download-alt", "Work & Career", "Download"},
	{"la-file", "Work & Career", "File"},
	{"la-file-alt", "Work & Career", "Document"},
	{"la-file-contract", "Work & Career", "Contract"},
	{"la-file-signature", "Work & Career", "Signature"},
	{"la-folder", "Work & Career", "Folder"},
	{"la-folder-open", "Work & Career", "Open folder"},
	{"la-inbox", "Work & Career", "Inbox"},
	{"la-envelope", "Work & Career", "Envelope"},
	{"la-envelope-open", "Work & Career", "Open mail"},
	{"la-comment", "Work & Career", "Message"},
	{"la-comments", "Work & Career", "Messages"},
	{"la-phone", "Work & Career", "Phone"},
	{"la-print", "Work & Career", "Print"},
	{"la-calculator", "Work & Career", "Calculator"},
	{"la-chart-line", "Work & Career", "Chart"},
	{"la-tasks", "Work & Career", "Tasks"},
	{"la-project-diagram", "Work & Career", "Diagram"},
	{"la-sitemap", "Work & Career", "Structure"},
	{"la-list", "Work & Career", "List"},
	// Health & Fitness
	{"la-heart", "Health & Fitness", "Heart"},
	{"la-heart-broken", "Health & Fitness", "Broken heart"},
	{"la-heartbeat", "Health & Fitness", "Pulse"},
	{"la-first-aid", "Health & Fitness", "First aid"},
	{"la-ambulance", "Health & Fitness", "Ambulance"},
	{"la-hospital", "Health & Fitness", "Hospital"},
	{"la-hospital-alt", "Health & Fitness", "Hospital (alt)"},
	{"la-prescription-bottle-alt", "Health & Fitness", "Pharmacy"},
	{"la-notes-medical", "Health & Fitness", "Medical notes"},
	{"la-allergies", "Health & Fitness", "Allergies"},
	{"la-capsules", "Health & Fitness", "Capsules"},
	{"la-pills", "Health & Fitness", "Pill"},
	{"la-syringe", "Health & Fitness", "Syringe"},
	{"la-user-md", "Health & Fitness", "Doctor"},
	{"la-stethoscope", "Health & Fitness", "Stethoscope"},
	{"la-dna", "Health & Fitness", "DNA"},
	{"la-brain", "Health & Fitness", "Brain"},
	{"la-bone", "Health & Fitness", "Bone"},
	{"la-wheelchair", "Health & Fitness", "Wheelchair"},
	{"la-running", "Health & Fitness", "Running"},
	{"la-football-ball", "Health & Fitness", "Football"},
	{"la-futbol", "Health & Fitness", "Soccer"},
	{"la-basketball-ball", "Health & Fitness", "Basketball"},
	{"la-baseball-ball", "Health & Fitness", "Baseball"},
	{"la-golf-ball", "Health & Fitness", "Golf"},
	{"la-dumbbell", "Health & Fitness", "Dumbbell"},
	{"la-bath", "Health & Fitness", "Bath"},
	{"la-hand-paper", "Health & Fitness", "Hand"},
	{"la-hand-holding-heart", "Health & Fitness", "Charity hand"},
	{"la-praying-hands", "Health & Fitness", "Praying hands"},
	// Lifestyle & Leisure
	{"la-gamepad", "Lifestyle & Leisure", "Gamepad"},
	{"la-chess", "Lifestyle & Leisure", "Chess"},
	{"la-chess-knight", "Lifestyle & Leisure", "Chess knight"},
	{"la-dice", "Lifestyle & Leisure", "Dice"},
	{"la-dice-d20", "Lifestyle & Leisure", "D20"},
	{"la-theater-masks", "Lifestyle & Leisure", "Theater"},
	{"la-music", "Lifestyle & Leisure", "Music"},
	{"la-guitar", "Lifestyle & Leisure", "Guitar"},
	{"la-drum", "Lifestyle & Leisure", "Drum"},
	{"la-film", "Lifestyle & Leisure", "Film"},
	{"la-video", "Lifestyle & Leisure", "Video"},
	{"la-camera", "Lifestyle & Leisure", "Camera"},
	{"la-camera-retro", "Lifestyle & Leisure", "Retro camera"},
	{"la-image", "Lifestyle & Leisure", "Photo"},
	{"la-palette", "Lifestyle & Leisure", "Palette"},
	{"la-brush", "Lifestyle & Leisure", "Brush"},
	{"la-pencil-alt", "Lifestyle & Leisure", "Pencil"},
	{"la-cut", "Lifestyle & Leisure", "Scissors"},
	{"la-ruler", "Lifestyle & Leisure", "Ruler"},
	{"la-newspaper", "Lifestyle & Leisure", "Newspaper"},
	{"la-blog", "Lifestyle & Leisure", "Blog"},
	{"la-podcast", "Lifestyle & Leisure", "Podcast"},
	{"la-microphone", "Lifestyle & Leisure", "Mic"},
	{"la-microphone-alt", "Lifestyle & Leisure", "Mic (alt)"},
	{"la-headphones", "Lifestyle & Leisure", "Headphones"},
	{"la-headphones-alt", "Lifestyle & Leisure", "Headphones (alt)"},
	{"la-tv", "Lifestyle & Leisure", "TV"},
	{"la-satellite", "Lifestyle & Leisure", "Satellite"},
	{"la-satellite-dish", "Lifestyle & Leisure", "Dish"},
	{"la-rocket", "Lifestyle & Leisure", "Rocket"},
	{"la-star", "Lifestyle & Leisure", "Star"},
	{"la-star-half", "Lifestyle & Leisure", "Star (half)"},
	{"la-crown", "Lifestyle & Leisure", "Crown"},
	{"la-medal", "Lifestyle & Leisure", "Medal"},
	{"la-trophy", "Lifestyle & Leisure", "Trophy"},
	{"la-award", "Lifestyle & Leisure", "Award"},
	// Family & Gifts
	{"la-users", "Family & Gifts", "Family"},
	{"la-user-friends", "Family & Gifts", "Friends"},
	{"la-user", "Family & Gifts", "Person"},
	{"la-baby", "Family & Gifts", "Baby"},
	{"la-child", "Family & Gifts", "Child"},
	{"la-baby-carriage", "Family & Gifts", "Stroller"},
	{"la-gift", "Family & Gifts", "Gift"},
	{"la-handshake", "Family & Gifts", "Handshake"},
	{"la-user-shield", "Family & Gifts", "Protected person"},
	{"la-id-card", "Family & Gifts", "ID"},
	{"la-id-card-alt", "Family & Gifts", "ID (alt)"},
	{"la-fingerprint", "Family & Gifts", "Fingerprint"},
	{"la-dog", "Family & Gifts", "Dog"},
	{"la-cat", "Family & Gifts", "Cat"},
	{"la-paw", "Family & Gifts", "Paw"},
	{"la-feather", "Family & Gifts", "Feather"},
	{"la-leaf", "Family & Gifts", "Leaf"},
	{"la-tree", "Family & Gifts", "Tree"},
	// Finance & Money
	{"la-piggy-bank", "Finance & Money", "Piggy bank"},
	{"la-wallet", "Finance & Money", "Wallet"},
	{"la-credit-card", "Finance & Money", "Card"},
	{"la-coins", "Finance & Money", "Coins"},
	{"la-money-bill", "Finance & Money", "Cash"},
	{"la-money-bill-wave", "Finance & Money", "Cash (alt)"},
	{"la-money-check", "Finance & Money", "Check"},
	{"la-money-check-alt", "Finance & Money", "Bank check"},
	{"la-dollar-sign", "Finance & Money", "Dollar"},
	{"la-exchange-alt", "Finance & Money", "Exchange"},
	{"la-landmark", "Finance & Money", "Bank"},
	{"la-university", "Finance & Money", "Bank (university)"},
	{"la-balance-scale", "Finance & Money", "Balance"},
	{"la-gavel", "Finance & Money", "Court"},
	{"la-bitcoin", "Finance & Money", "Bitcoin"},
	{"la-ethereum", "Finance & Money", "Ethereum"},
	{"la-cc-visa", "Finance & Money", "Visa"},
	{"la-cc-mastercard", "Finance & Money", "Mastercard"},
	{"la-cc-amex", "Finance & Money", "Amex"},
	{"la-cc-discover", "Finance & Money", "Discover"},
	{"la-cc-paypal", "Finance & Money", "PayPal"},
	{"la-cc-apple-pay", "Finance & Money", "Apple Pay"},
	{"la-cc-stripe", "Finance & Money", "Stripe"},
	// Other
	{"la-question", "Other", "Question"},
	{"la-question-circle", "Other", "Question (circle)"},
	{"la-info", "Other", "Info"},
	{"la-exclamation-triangle", "Other", "Alert"},
	{"la-ban", "Other", "Prohibited"},
	{"la-check", "Other", "Check"},
	{"la-check-circle", "Other", "Confirmed"},
	{"la-times", "Other", "Close"},
	{"la-times-circle", "Other", "Close (circle)"},
	{"la-plus", "Other", "Add"},
	{"la-minus", "Other", "Remove"},
	{"la-search", "Other", "Search"},
	{"la-filter", "Other", "Filter"},
	{"la-sliders-h", "Other", "Filters"},
	{"la-cog", "Other", "Settings"},
	{"la-cogs", "Other", "Gears"},
	{"la-clock", "Other", "Clock"},
	{"la-hourglass", "Other", "Timer"},
	{"la-calendar", "Other", "Calendar"},
	{"la-download", "Other", "Download"},
	{"la-upload", "Other", "Upload"},
	{"la-share-alt", "Other", "Share"},
	{"la-link", "Other", "Link"},
	{"la-bookmark", "Other", "Bookmark"},
	{"la-flag", "Other", "Flag"},
	{"la-moon", "Other", "Moon"},
	{"la-sun", "Other", "Sun"},
	{"la-snowflake", "Other", "Snow"},
	{"la-bluetooth", "Other", "Bluetooth"},
}

// iconByKey indexes the registry (built once).
var iconByKey = func() map[string]iconGlyph {
	m := make(map[string]iconGlyph, len(iconEntries))
	for _, g := range iconEntries {
		m[g.Key] = g
	}
	return m
}()

// IconCount is the number of registry entries (296 — asserted by the tests
// against the design doc and the vendored CSS).
const IconCount = 296

// iconGroups returns the registry grouped for the picker, in §H order.
func iconGroups() []iconGroup {
	var out []iconGroup
	for _, g := range iconEntries {
		if n := len(out); n > 0 && out[n-1].Name == g.Group {
			out[n-1].Glyphs = append(out[n-1].Glyphs, g)
		} else {
			out = append(out, iconGroup{Name: g.Group, Glyphs: []iconGlyph{g}})
		}
	}
	return out
}

// iconValid reports whether key is a registry key — the app-side validation
// for the icon form fields (spec §A: no DB CHECK constraint).
func iconValid(key string) bool {
	_, ok := iconByKey[key]
	return ok
}

// categoryIconDefault is the default glyph key for an unpicked category.
const categoryIconDefault = "la-tag"

// accountIconDefault is the per-type default glyph key (spec §C) — the
// replacement for the retired emoji lookup.
func accountIconDefault(typ string) string {
	switch typ {
	case "savings":
		return "la-piggy-bank"
	case "ewallet":
		return "la-mobile-alt"
	case "credit_card":
		return "la-credit-card"
	case "loan":
		return "la-file-invoice"
	case "investment":
		return "la-chart-line"
	case "cash":
		return "la-money-bill"
	case "credits":
		return "la-ticket-alt"
	default:
		return "la-tag"
	}
}

// resolveCategoryIcon returns the effective category glyph key: the stored
// key when it is in the registry, otherwise the default. An unknown key can
// only exist via a hand-edited DB row — a stale key renders the default,
// never a broken surface.
func resolveCategoryIcon(stored string) string {
	if iconValid(stored) {
		return stored
	}
	return categoryIconDefault
}

// resolveAccountIcon returns the effective account glyph key: the stored
// key when it is in the registry, otherwise the per-type default.
func resolveAccountIcon(stored, typ string) string {
	if iconValid(stored) {
		return stored
	}
	return accountIconDefault(typ)
}

// brandGlyphs lists the registry keys whose glyph lives in the Line Awesome
// Brands font rather than the Free solid font. Derived 2026-11-20 by reading
// the vendored woff2 cmaps (10 of the 296 keys). The style class selects the
// font family (vendor CSS: .la = Free 900, .lar = Free 400, .lab = Brands
// 400); rendering a brand key with the plain "la" class loads the solid
// font, which lacks the glyph, so the icon renders as a blank box.
var brandGlyphs = map[string]bool{
	"la-bitcoin":       true,
	"la-bluetooth":     true,
	"la-cc-amex":       true,
	"la-cc-apple-pay":  true,
	"la-cc-discover":   true,
	"la-cc-mastercard": true,
	"la-cc-paypal":     true,
	"la-cc-stripe":     true,
	"la-cc-visa":       true,
	"la-ethereum":      true,
}

// iconStyle returns the LA style class for a key: the font family that
// actually contains the key's glyph.
func iconStyle(key string) string {
	if brandGlyphs[key] {
		return "lab"
	}
	return "la"
}

// iconColours is the curated D36 palette (design doc §B, user-locked
// 2026-11-24). It backs the picker's "Predefined colours" tab — a UX
// convenience, not a constraint: any valid #rrggbb the user types is accepted
// identically (ADR-0013). The stored form is the normalised 6-hex string
// without "#".
var iconColours = []struct{ Name, Hex string }{
	{"Terracotta", "b84810"},
	{"Coral", "e86a4a"},
	{"Amber", "ff9500"},
	{"Honey", "f2b32e"},
	{"Leaf", "7ba32e"},
	{"Fern", "2e8b57"},
	{"Teal", "009688"},
	{"Lagoon", "4fb3d9"},
	{"Ocean", "2196f3"},
	{"Indigo", "3f51b5"},
	{"Violet", "673ab7"},
	{"Orchid", "9c27b0"},
	{"Rose", "e0559d"},
	{"Brick", "d64545"},
	{"Slate", "8e8e93"},
	{"Ink", "3a3a3a"},
}

// colourHex returns a palette entry's normalised hex by name ("" if none).
func colourHex(name string) string {
	for _, c := range iconColours {
		if c.Name == name {
			return c.Hex
		}
	}
	return ""
}

// normaliseColour trims, strips one leading "#", and lowercases a colour into
// D36's stored form: a 6-hex string without "#". Empty stays empty.
func normaliseColour(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	return strings.ToLower(s)
}

// colourValid reports whether c is a storable colour: empty (no tint) or a
// normalised 6-hex string. App-side validation — no DB CHECK (ADR-0009/0013).
func colourValid(c string) bool {
	if c == "" {
		return true
	}
	if len(c) != 6 {
		return false
	}
	for i := 0; i < 6; i++ {
		d := c[i]
		if !((d >= '0' && d <= '9') || (d >= 'a' && d <= 'f')) {
			return false
		}
	}
	return true
}

// colourOrNil maps a stored colour to a SQL value: empty clears the tint
// (NULL), a non-empty value is stored as-is.
func colourOrNil(c string) any {
	if c == "" {
		return nil
	}
	return c
}

// iconTag renders a resolved glyph key as an <i> element. Keys reach here
// only through resolve* (registry membership checked), so the key is a
// registry token, never free user input. colour (optional) tints the glyph
// inline as style="color:#<hex>"; an empty or invalid value leaves the style
// off so the glyph inherits the surrounding text colour.
func iconTag(key string, colour ...string) template.HTML {
	if key == "" {
		return ""
	}
	style := ""
	if len(colour) > 0 {
		if c := normaliseColour(colour[0]); c != "" && colourValid(c) {
			style = ` style="color:#` + c + `"`
		}
	}
	return template.HTML(`<i class="` + iconStyle(key) + ` ` + html.EscapeString(key) + `"` + style + `></i>`)
}

// iconOrNil maps the form value to its DB value: "" (unpicked) → NULL.
func iconOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// catIcon is the category template function (dashboard, /txns, review,
// forms): the picked glyph, or the tag default. The optional second arg tints
// the glyph (D36).
func catIcon(stored string, colour ...string) template.HTML {
	return iconTag(resolveCategoryIcon(stored), colour...)
}

// acctIcon is the account template function (accounts grid, cards page,
// forms): the picked glyph, or the per-type default. The optional third arg
// tints the glyph (D36).
func acctIcon(stored, typ string, colour ...string) template.HTML {
	return iconTag(resolveAccountIcon(stored, typ), colour...)
}
