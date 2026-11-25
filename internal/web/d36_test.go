package web

// D36 — icon colours for categories and accounts (ADR-0013,
// docs/notes/d36-icon-colour-palette.md). B4's verification suite.
//
// It covers: the curated 16-swatch palette's integrity, the normalise +
// colourValid accept/reject contract, the tinted iconTag/caseIcon/acctIcon
// HTML output, and the DB write paths — a category colour set by a plain
// member, an account colour gated by the account's Edit grant (403 without,
// 303 + persisted with), and an invalid hex silently cleared to NULL (the
// row is still created: only the colour value is rejected, per ADR-0013).

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
)

func d36App(t *testing.T) (*App, *pgxpool.Pool) {
	t.Helper()
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, pool
}

// d36ColourOfCategory returns the stored colour for a category by name
// ("" when NULL).
func d36ColourOfCategory(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var colour string
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(colour, '') FROM category WHERE name = $1`, name).Scan(&colour)
	if err != nil {
		t.Fatalf("read category colour %q: %v", name, err)
	}
	return colour
}

// d36ColourOfAccount returns the stored colour for an account ("" when NULL).
func d36ColourOfAccount(t *testing.T, pool *pgxpool.Pool, hh, acc int64) string {
	t.Helper()
	var colour string
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(colour, '') FROM account WHERE id = $1 AND household_id = $2`, acc, hh).Scan(&colour)
	if err != nil {
		t.Fatalf("read account colour: %v", err)
	}
	return colour
}

// TestD36_PaletteIntegrity locks the curated 16-swatch palette: exactly 16,
// every entry a valid normalised 6-hex, all hexes distinct, all names
// distinct and non-empty.
func TestD36_PaletteIntegrity(t *testing.T) {
	if len(iconColours) != 16 {
		t.Fatalf("palette has %d swatches, want 16", len(iconColours))
	}
	seenHex := map[string]int{}
	seenName := map[string]int{}
	for i, c := range iconColours {
		if c.Name == "" {
			t.Errorf("swatch %d has an empty name", i)
		}
		if len(c.Hex) != 6 || !colourValid(c.Hex) {
			t.Errorf("swatch %q (%q) has an invalid/stored-form hex", c.Name, c.Hex)
		}
		seenHex[c.Hex]++
		seenName[strings.ToLower(c.Name)]++
	}
	for h, n := range seenHex {
		if n > 1 {
			t.Errorf("hex %q used by %d swatches", h, n)
		}
	}
	for n, c := range seenName {
		if c > 1 {
			t.Errorf("name %q used by %d swatches", n, c)
		}
	}
}

// TestD36_ColourValidMatrix covers the accept/reject contract of the
// normalise + colourValid pair (icons.go).
func TestD36_ColourValidMatrix(t *testing.T) {
	valid := []struct{ in, want string }{
		{"#B84810", "b84810"},     // canonical, with '#'
		{"#b84810", "b84810"},     // canonical, lowercase
		{"B84810", "b84810"},      // no '#', uppercase
		{"  #FF0000  ", "ff0000"}, // surrounding whitespace + '#'
		{"#000000", "000000"},
		{"#ffffff", "ffffff"},
	}
	for _, tc := range valid {
		got := normaliseColour(tc.in)
		if got != tc.want {
			t.Errorf("normaliseColour(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !colourValid(got) {
			t.Errorf("colourValid(%q) = false, want true (from %q)", got, tc.in)
		}
	}
	// Empty is the "clear / no tint" value — it must be valid (stores NULL).
	if got := normaliseColour(""); got != "" || !colourValid("") {
		t.Errorf("empty colour: normalise=%q colourValid=%v, want \"\"/true", got, colourValid(""))
	}
	// Invalid hexes are rejected.
	for _, bad := range []string{"b8481", "zzz", "#GG0000", "12345", "1234567", "rgb(1,2,3)"} {
		if colourValid(normaliseColour(bad)) {
			t.Errorf("colourValid is true for invalid input %q", bad)
		}
	}
}

// TestD36_IconTagTint checks the tinted HTML output (no DB): a valid colour
// adds style="color:#<hex>"; empty/invalid drop the style so the glyph
// inherits the surrounding text colour.
func TestD36_IconTagTint(t *testing.T) {
	tinted := regexp.MustCompile(`style="color:#([0-9a-f]{6})"`)
	cases := []struct {
		name        string
		key, colour string
		wantTinted  bool
		wantHex     string
	}{
		{"valid 6-hex", "ri-coffee-cup", "b84810", true, "b84810"},
		{"valid with hash + upper", "ri-coffee-cup", "#B84810", true, "b84810"},
		{"no colour", "ri-coffee-cup", "", false, ""},
		{"invalid hex", "ri-coffee-cup", "#GG0000", false, ""},
		{"empty key", "", "b84810", false, ""},
	}
	for _, c := range cases {
		out := string(iconTag(c.key, c.colour))
		if c.wantTinted {
			m := tinted.FindStringSubmatch(out)
			if m == nil {
				t.Fatalf("%s: no tinted style in %q", c.name, out)
			}
			if m[1] != c.wantHex {
				t.Errorf("%s: tint %q, want %q", c.name, m[1], c.wantHex)
			}
		} else if strings.Contains(out, "style=") {
			t.Errorf("%s: unexpected style in %q", c.name, out)
		}
	}
	// catIcon / acctIcon thread the colour through to iconTag. Empty stored
	// keys resolve to a non-empty default, so the tint appears deterministically.
	if !strings.Contains(string(catIcon("", "16a34a")), `style="color:#16a34a"`) {
		t.Errorf("catIcon with colour not tinted: %q", string(catIcon("", "16a34a")))
	}
	if strings.Contains(string(catIcon("")), "style=") {
		t.Errorf("catIcon without colour should not be tinted: %q", string(catIcon("")))
	}
	if !strings.Contains(string(acctIcon("", "Savings", "2b6cb0")), `style="color:#2b6cb0"`) {
		t.Errorf("acctIcon with colour not tinted: %q", string(acctIcon("", "Savings", "2b6cb0")))
	}
	if strings.Contains(string(acctIcon("", "Savings")), "style=") {
		t.Errorf("acctIcon without colour should not be tinted: %q", string(acctIcon("", "Savings")))
	}
	// XSS-safe: returns a template.HTML carrying an <i> glyph element.
	var _ template.HTML = iconTag("ri-coffee-cup", "b84810")
	if !strings.Contains(string(iconTag("ri-coffee-cup", "b84810")), "<i class=") {
		t.Errorf("iconTag output missing glyph element: %q", string(iconTag("ri-coffee-cup", "b84810")))
	}
}

// TestD36_CategoryColourPlainMember: categories are household-scoped (no
// per-object grant), so a plain member sets a category's colour directly.
// A valid colour persists; an empty one stores NULL.
func TestD36_CategoryColourPlainMember(t *testing.T) {
	a, pool := d36App(t)
	ctx := context.Background()
	hID := d30Household(t, pool, "d36 cat hh")
	memID := d30User(t, pool, hID, "d36-cat-member", "")
	d30Membership(t, pool, memID, hID, "member")
	sess := &auth.Session{ID: "d36-cat", Household: hID, UserID: memID}

	for name, colour := range map[string]string{
		"d36 Cat Coffee": "b84810",
		"d36 Cat Clear":  "",
	} {
		form := url.Values{
			"name":              {name},
			"transaction_class": {"expense"},
			"budgetable":        {"on"},
			"colour":            {colour},
		}
		w := httptest.NewRecorder()
		a.handleCategoryCreate(w, d30Req(t, ctx, sess, "/categories/create", 0, form))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("create %q: got %d, want 303", name, w.Code)
		}
		if got := d36ColourOfCategory(t, pool, name); got != colour {
			t.Errorf("category %q: stored colour %q, want %q", name, got, colour)
		}
	}
}

// TestD36_InvalidHexSilentlyCleared: an invalid colour is rejected app-side
// (ADR-0013) and stored as NULL — but the row itself is still created,
// because only the colour value is invalid, not the rest of the form. This
// locks in the committed behaviour (row created, colour cleared).
func TestD36_InvalidHexSilentlyCleared(t *testing.T) {
	a, pool := d36App(t)
	ctx := context.Background()
	hID := d30Household(t, pool, "d36 invalid hh")
	memID := d30User(t, pool, hID, "d36-invalid-member", "")
	d30Membership(t, pool, memID, hID, "member")
	sess := &auth.Session{ID: "d36-inv", Household: hID, UserID: memID}

	name := "d36 Cat Invalid"
	form := url.Values{
		"name":              {name},
		"transaction_class": {"expense"},
		"budgetable":        {"on"},
		"colour":            {"#GG0000"},
	}
	w := httptest.NewRecorder()
	a.handleCategoryCreate(w, d30Req(t, ctx, sess, "/categories/create", 0, form))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create with invalid colour: got %d, want 303 (row still created)", w.Code)
	}
	if got := d36ColourOfCategory(t, pool, name); got != "" {
		t.Fatalf("invalid colour: stored %q, want NULL/empty", got)
	}
}

// TestD36_AccountColourGated: an account's colour is gated by the account's
// Edit grant — 403 without, 303 + persisted with, and clearing stores NULL.
func TestD36_AccountColourGated(t *testing.T) {
	a, pool := d36App(t)
	ctx := context.Background()
	hID := d30Household(t, pool, "d36 acct hh")
	ownerID := d30User(t, pool, hID, "d36-acct-owner", "")
	d30Membership(t, pool, ownerID, hID, "owner")
	memID := d30User(t, pool, hID, "d36-acct-member", "")
	d30Membership(t, pool, memID, hID, "member")
	accID := d30Account(t, pool, hID, "d36 Acct Main", 0)

	editForm := func(colour string) url.Values {
		return url.Values{
			"name":         {"d36 Acct Main"},
			"account_type": {"savings"},
			"balance_type": {"asset"},
			"colour":       {colour},
		}
	}

	// Member without a grant on the account → 403 (the gate precedes the write).
	w := httptest.NewRecorder()
	sess := &auth.Session{ID: "d36-acct", Household: hID, UserID: memID}
	a.handleAccountEdit(w, d30Req(t, ctx, sess, "/accounts/edit", accID, editForm("16a34a")))
	if w.Code != http.StatusForbidden {
		t.Fatalf("before grant: member edit got %d, want 403", w.Code)
	}

	// The account gate needs TWO grants (D32 pattern): an account-grant read
	// (the record-read gate, canReadRecords — it only honours account_grant,
	// not household read) + an edit-all household grant (the POST edit gate,
	// canEditAccountObject). Both are missing above, so the member gets 403;
	// with both, the same member now writes 303 + persisted.
	d30Grant(t, pool, hID, memID, accID, "Related", "")
	if _, err := pool.Exec(ctx,
		`INSERT INTO household_grant (user_id, household_id, can_read, can_create,
		 can_edit, can_delete, edit_mode, delete_mode)
		 VALUES ($1,$2,true,true,true,false,'all','created')`, memID, hID); err != nil {
		t.Fatalf("household edit grant: %v", err)
	}
	w = httptest.NewRecorder()
	a.handleAccountEdit(w, d30Req(t, ctx, sess, "/accounts/edit", accID, editForm("16a34a")))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("after grant: member edit got %d, want 303", w.Code)
	}
	if got := d36ColourOfAccount(t, pool, hID, accID); got != "16a34a" {
		t.Fatalf("account colour: stored %q, want 16a34a", got)
	}

	// Clearing the colour stores NULL.
	w = httptest.NewRecorder()
	a.handleAccountEdit(w, d30Req(t, ctx, sess, "/accounts/edit", accID, editForm("")))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("clear colour: got %d, want 303", w.Code)
	}
	if got := d36ColourOfAccount(t, pool, hID, accID); got != "" {
		t.Fatalf("cleared account colour: stored %q, want NULL/empty", got)
	}
}
