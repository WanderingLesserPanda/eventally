// D32 DB-backed tests (docs/notes/d32-icon-picker.md, ADR-0009): the icon
// registry (296 §H keys, 11 groups, all resolvable in the vendored Line
// Awesome 2.1.0 CSS, no duplicates, defaults for unknown/empty keys and the
// per-account-type map), the category form accepting any member's icon while
// rejecting unknown keys, and the account form's icon behind the Edit grant
// (no grant → 403, edit-all grant → 303 + persisted). Scenario-dense
// synthetic seeds, no bulk data (goal item). Skipped unless
// EVENTALLY_TEST_DSN is set (harness: recurring_gate2_test.go).
package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func d32App(t *testing.T) (*App, *pgxpool.Pool) {
	t.Helper()
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, pool
}

func d32Get(t *testing.T, a *App, sess *auth.Session, h http.HandlerFunc, path string, id int64) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if id > 0 {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", strconv.FormatInt(id, 10))
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	}
	r = r.WithContext(contextWithSession(r.Context(), sess))
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// --- registry (pure) --------------------------------------------------------

// TestD32RegistryComplete: the registry is exactly the design doc's §H —
// 296 entries in 11 groups, unique la-* keys, and every key resolvable in
// the vendored Line Awesome 2.1.0 CSS (plus the fonts and MIT LICENSE are
// vendored).
func TestD32RegistryComplete(t *testing.T) {
	if len(iconEntries) != IconCount {
		t.Fatalf("registry has %d entries, want %d (§H)", len(iconEntries), IconCount)
	}
	seen := map[string]bool{}
	var lastGroup string
	groupCount := 0
	for _, g := range iconEntries {
		if g.Key == "" || !strings.HasPrefix(g.Key, "la-") {
			t.Fatalf("entry %q is not an la-* glyph key", g.Key)
		}
		if seen[g.Key] {
			t.Fatalf("duplicate registry key %s", g.Key)
		}
		seen[g.Key] = true
		if g.Group == "" || g.Label == "" {
			t.Fatalf("key %s missing group or label", g.Key)
		}
		if g.Group != lastGroup {
			lastGroup = g.Group
			groupCount++
		}
	}
	if groupCount != 11 {
		t.Fatalf("registry spans %d groups, want 11 (§H)", groupCount)
	}
	// Every key must exist in the vendored CSS.
	cssPath := filepath.Join("static", "lineawesome", "css", "line-awesome.min.css")
	css, err := os.ReadFile(cssPath)
	if err != nil {
		t.Fatalf("read vendored CSS: %v", err)
	}
	for _, g := range iconEntries {
		if !strings.Contains(string(css), "."+g.Key+":before") {
			t.Fatalf("registry key %s not found in vendored CSS", g.Key)
		}
	}
	// The full font set is vendored (solid + regular + brands, woff + woff2).
	for _, f := range []string{
		"la-solid-900.woff2", "la-solid-900.woff",
		"la-regular-400.woff2", "la-regular-400.woff",
		"la-brands-400.woff2", "la-brands-400.woff",
	} {
		if _, err := os.Stat(filepath.Join("static", "lineawesome", "fonts", f)); err != nil {
			t.Fatalf("missing vendored font %s: %v", f, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join("static", "lineawesome", "LICENSE.txt")); err != nil ||
		!strings.Contains(string(b), "MIT") {
		t.Fatal("LICENSE.txt (MIT) must be vendored with Line Awesome")
	}
}

// TestD32Defaults: unknown or empty stored keys never break a surface —
// they render the default (category: la-tag; account: the per-type map).
func TestD32Defaults(t *testing.T) {
	if got := resolveCategoryIcon(""); got != "la-tag" {
		t.Fatalf("empty category key: %s, want la-tag", got)
	}
	if got := resolveCategoryIcon("la-bogus"); got != "la-tag" {
		t.Fatalf("unknown category key: %s, want la-tag", got)
	}
	if got := resolveCategoryIcon("la-coffee"); got != "la-coffee" {
		t.Fatalf("picked category key: %s, want la-coffee", got)
	}
	want := map[string]string{
		"savings":     "la-piggy-bank",
		"ewallet":     "la-mobile-alt",
		"credit_card": "la-credit-card",
		"loan":        "la-file-invoice",
		"investment":  "la-chart-line",
		"cash":        "la-money-bill",
		"credits":     "la-ticket-alt",
		"mystery":     "la-tag",
		"":            "la-tag",
	}
	for typ, k := range want {
		if got := resolveAccountIcon("", typ); got != k {
			t.Fatalf("account type %q default: %s, want %s", typ, got, k)
		}
	}
	if got := resolveAccountIcon("la-coffee", "savings"); got != "la-coffee" {
		t.Fatalf("picked account key must beat the type default: %s", got)
	}
	if got := resolveAccountIcon("la-ghost", "cash"); got != "la-money-bill" {
		t.Fatalf("unknown account key: %s, want la-money-bill", got)
	}
}

// --- forms over HTTP --------------------------------------------------------

// TestD32CategoryIconAnyMember: any household member may set a category's
// icon (no grant gate, unchanged by D32); unknown keys are rejected
// app-side; the form renders the picker with all 296 glyphs and the
// explicit "No icon (use default)" row.
func TestD32CategoryIconAnyMember(t *testing.T) {
	a, pool := d32App(t)
	ctx := context.Background()
	hh := d30Household(t, pool, "d32hh")
	o := d30User(t, pool, hh, "d32-owner", "")
	m := d30User(t, pool, hh, "d32-member", "")
	d30Membership(t, pool, o, hh, "owner")
	d30Membership(t, pool, m, hh, "member")
	om := &auth.Session{ID: "om", Household: hh, UserID: o}
	mm := &auth.Session{ID: "mm", Household: hh, UserID: m}

	// A PLAIN MEMBER creates a category with a registry icon.
	form := url.Values{"name": {"Coffee"}, "transaction_class": {"expense"}, "icon": {"la-coffee"}}
	rec := httptest.NewRecorder()
	a.handleCategoryCreate(rec, d30Req(t, ctx, mm, "/categories", 0, form))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("member category create: %d, want 303; body: %s", rec.Code, rec.Body.String()[:min(len(rec.Body.String()), 300)])
	}
	var catID int64
	var icon *string
	if err := pool.QueryRow(ctx,
		`SELECT id, icon FROM category WHERE household_id = $1 AND name = 'Coffee'`, hh).
		Scan(&catID, &icon); err != nil {
		t.Fatalf("load category: %v", err)
	}
	if icon == nil || *icon != "la-coffee" {
		t.Fatalf("icon = %v, want la-coffee", icon)
	}

	// An unknown key is rejected: form re-renders, no row is created.
	bad := url.Values{"name": {"Ghost"}, "transaction_class": {"expense"}, "icon": {"la-bogus"}}
	recBad := httptest.NewRecorder()
	a.handleCategoryCreate(recBad, d30Req(t, ctx, mm, "/categories", 0, bad))
	if recBad.Code != http.StatusOK || !strings.Contains(recBad.Body.String(), "Unknown icon.") {
		t.Fatalf("unknown icon: code %d, want 200 + 'Unknown icon.'", recBad.Code)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM category WHERE household_id = $1 AND name = 'Ghost'`, hh).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rejected key still created %d row(s)", n)
	}

	// The same plain member can EDIT the icon (membership, not a grant).
	edit := url.Values{
		"name": {"Coffee"}, "transaction_class": {"expense"}, "icon": {"la-leaf"},
		"budgetable": {"on"},
	}
	recEdit := httptest.NewRecorder()
	a.handleCategoryEdit(recEdit, d30Req(t, ctx, mm, "/categories/"+strconv.FormatInt(catID, 10), catID, edit))
	if recEdit.Code != http.StatusSeeOther {
		t.Fatalf("member category edit: %d, want 303", recEdit.Code)
	}
	if err := pool.QueryRow(ctx, `SELECT icon FROM category WHERE id = $1`, catID).Scan(&icon); err != nil {
		t.Fatal(err)
	}
	if icon == nil || *icon != "la-leaf" {
		t.Fatalf("edited icon = %v, want la-leaf", icon)
	}

	// The form renders the picker: 296 glyph cells + the "No icon" row,
	// preselected with the stored key, and the Line Awesome CSS is linked.
	recGet := d32Get(t, a, om, a.handleCategoryEdit, "/categories/"+strconv.FormatInt(catID, 10), catID)
	if recGet.Code != http.StatusOK {
		t.Fatalf("category form GET: %d", recGet.Code)
	}
	body := recGet.Body.String()
	if !strings.Contains(body, "/static/lineawesome/css/line-awesome.min.css") {
		t.Fatal("layout does not link the vendored Line Awesome CSS")
	}
	cells := strings.Count(body, `class="icon-cell" data-key="la-`)
	if cells != IconCount {
		t.Fatalf("picker renders %d glyph cells, want %d", cells, IconCount)
	}
	if !strings.Contains(body, `data-label="no icon (use default)"`) {
		t.Fatal("picker missing the 'No icon (use default)' row")
	}
	if !strings.Contains(body, `iconPicker('la-leaf', 'la')`) {
		t.Fatal("picker not preselected with the stored key")
	}
}

// TestD32AccountIconNeedsEditGrant: the account icon follows the account
// Edit gate — a member without it gets 403 (nothing written); the owner and
// an edit-all grantee succeed; unknown keys are rejected.
func TestD32AccountIconNeedsEditGrant(t *testing.T) {
	a, pool := d32App(t)
	ctx := context.Background()
	hh := d30Household(t, pool, "d32hh2")
	o := d30User(t, pool, hh, "d32-owner2", "")
	m := d30User(t, pool, hh, "d32-member2", "")
	e := d30User(t, pool, hh, "d32-editor2", "")
	d30Membership(t, pool, o, hh, "owner")
	d30Membership(t, pool, m, hh, "member")
	d30Membership(t, pool, e, hh, "member")
	om := &auth.Session{ID: "om2", Household: hh, UserID: o}
	mm := &auth.Session{ID: "mm2", Household: hh, UserID: m}
	em := &auth.Session{ID: "em2", Household: hh, UserID: e}
	// The account (created by SQL, so created_by_user_id is 0 — created-mode
	// can never match) and the editor's grants: an account-grant read (the
	// record-read gate in handleAccountEdit) + an edit-all household grant
	// (the POST gate: canEditAccountObject).
	acc := d30Account(t, pool, hh, "Main Savings", 0)
	d30Grant(t, pool, hh, e, acc, "Related", "")
	if _, err := pool.Exec(ctx,
		`INSERT INTO household_grant (user_id, household_id, can_read, can_create,
		 can_edit, can_delete, edit_mode, delete_mode)
		 VALUES ($1,$2,true,true,true,false,'all','created')`, e, hh); err != nil {
		t.Fatalf("editor grant: %v", err)
	}
	full := url.Values{
		"name": {"Main Savings"}, "account_type": {"savings"},
		"balance_type": {"asset"}, "icon": {"la-coins"},
	}
	getIcon := func() *string {
		t.Helper()
		var v *string
		if err := pool.QueryRow(ctx, `SELECT icon FROM account WHERE id = $1`, acc).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// No Edit grant: 403, nothing written.
	rec := httptest.NewRecorder()
	a.handleAccountEdit(rec, d30Req(t, ctx, mm, "/accounts/"+strconv.FormatInt(acc, 10), acc, full))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member without Edit grant: %d, want 403", rec.Code)
	}
	if v := getIcon(); v != nil {
		t.Fatalf("403 still wrote icon %v", *v)
	}

	// Owner: 303, persisted.
	rec = httptest.NewRecorder()
	a.handleAccountEdit(rec, d30Req(t, ctx, om, "/accounts/"+strconv.FormatInt(acc, 10), acc, full))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("owner edit: %d, want 303; body: %s", rec.Code, rec.Body.String()[:min(len(rec.Body.String()), 300)])
	}
	if v := getIcon(); v == nil || *v != "la-coins" {
		t.Fatalf("owner icon = %v, want la-coins", v)
	}

	// Edit-all grantee: 303, persisted.
	other := url.Values{
		"name": {"Main Savings"}, "account_type": {"savings"},
		"balance_type": {"asset"}, "icon": {"la-wallet"},
	}
	rec = httptest.NewRecorder()
	a.handleAccountEdit(rec, d30Req(t, ctx, em, "/accounts/"+strconv.FormatInt(acc, 10), acc, other))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit-all grantee: %d, want 303", rec.Code)
	}
	if v := getIcon(); v == nil || *v != "la-wallet" {
		t.Fatalf("editor icon = %v, want la-wallet", v)
	}

	// Unknown key: rejected even for the owner (form error, icon unchanged).
	bad := url.Values{
		"name": {"Main Savings"}, "account_type": {"savings"},
		"balance_type": {"asset"}, "icon": {"la-bogus"},
	}
	rec = httptest.NewRecorder()
	a.handleAccountEdit(rec, d30Req(t, ctx, om, "/accounts/"+strconv.FormatInt(acc, 10), acc, bad))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Unknown icon.") {
		t.Fatalf("unknown icon: code %d, want 200 + 'Unknown icon.'", rec.Code)
	}
	if v := getIcon(); v == nil || *v != "la-wallet" {
		t.Fatalf("rejected key overwrote icon: %v", v)
	}
}

// TestD32BrandStyle: brand glyphs must render with the lab style class,
// which selects the Line Awesome Brands font. Regression (2026-11-20 live
// report): ethereum/apple-pay rendered blank because the plain la class
// loads the solid Free font, which has no brand codepoints. The brand set
// was derived from the vendored woff2 cmaps and is pinned here.
func TestD32BrandStyle(t *testing.T) {
	brands := []string{
		"la-bitcoin", "la-bluetooth", "la-cc-amex", "la-cc-apple-pay",
		"la-cc-discover", "la-cc-mastercard", "la-cc-paypal", "la-cc-stripe",
		"la-cc-visa", "la-ethereum",
	}
	brandSet := make(map[string]bool, len(brands))
	for _, k := range brands {
		brandSet[k] = true
		if got := iconStyle(k); got != "lab" {
			t.Fatalf("iconStyle(%s) = %q, want lab", k, got)
		}
		if h := string(iconTag(k)); h != `<i class="lab `+k+`"></i>` {
			t.Fatalf("iconTag(%s) = %s, want lab-prefixed", k, h)
		}
	}
	// Every registry key classifies to exactly one style; non-brands solid.
	for _, g := range iconEntries {
		want := "la"
		if brandSet[g.Key] {
			want = "lab"
		}
		if got := iconStyle(g.Key); got != want {
			t.Fatalf("iconStyle(%s) = %q, want %q", g.Key, got, want)
		}
	}
	if n := len(brandGlyphs); n != len(brands) {
		t.Fatalf("brandGlyphs has %d keys, test pins %d", n, len(brands))
	}
	// The vendor CSS actually assigns the Brands family to .lab (if a future
	// re-vendor drops this, the lab class would silently render nothing).
	cssPath := filepath.Join("static", "lineawesome", "css", "line-awesome.min.css")
	css, err := os.ReadFile(cssPath)
	if err != nil {
		t.Fatalf("read vendored CSS: %v", err)
	}
	if !strings.Contains(string(css), `.lab{font-family:'Line Awesome Brands'}`) {
		t.Fatal("vendor CSS no longer assigns the Brands family to .lab")
	}
}
