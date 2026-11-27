// D37 DB-backed tests: the two-layout navigation (goal doc
// 2026-11-27_d37-nav-rework-goal.md; decisions-quickref D37). Verifies at
// the router level (real middleware + routes, session cookie):
//   - every daily page renders the daily layout: flat pane, cogwheel to the
//     settings entry, active item; the "From other households" item only
//     for users with 2+ households (D33 SyncNav condition);
//   - every /settings/* page renders the settings layout: Back-to-Home
//     pill + logo link, no cogwheel, active item;
//   - Invites/Grants are hidden entirely (and 403) for non-admins;
//   - the pre-nesting URLs 302 into the nested routes (query preserved);
//   - the D35 two-pane Categories page + fragments live at the new URLs.
// Skipped unless EVENTALLY_TEST_DSN is set (harness: recurring_gate2_test.go).
package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

type d37Seed struct {
	H, H2 int64 // households (neutral names)
	O     int64 // owner of H (admin)
	M     int64 // plain member of H (non-admin)
	U     int64 // member of H and H2 (SyncNav + switcher)
}

// d37SeedBuild creates two households and three users (owner O, member M in
// H; U in H and H2), all in one pass.
func d37SeedBuild(t *testing.T, pool *pgxpool.Pool) d37Seed {
	t.Helper()
	ctx := context.Background()
	var s d37Seed

	mkHH := func(name string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO household (name, base_currency, language, first_day_of_week)
			 VALUES ($1,'MYR','en',1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatalf("household %q: %v", name, err)
		}
		return id
	}
	mkUser := func(hh int64, username string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
			 VALUES ($1,$2,$2,$2||'@test.local','x') RETURNING id`, hh, username).Scan(&id); err != nil {
			t.Fatalf("user %q: %v", username, err)
		}
		return id
	}
	member := func(user, hh int64, role string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO household_member (user_id, household_id, role) VALUES ($1,$2,$3)`,
			user, hh, role); err != nil {
			t.Fatalf("membership %d→%d: %v", user, hh, err)
		}
	}

	s.H = mkHH("Main household")
	s.H2 = mkHH("Second household")
	s.O = mkUser(s.H, "d37_owner")
	s.M = mkUser(s.H, "d37_member")
	s.U = mkUser(s.H, "d37_dual")
	member(s.O, s.H, "owner")
	member(s.M, s.H, "member")
	member(s.U, s.H, "member")
	member(s.U, s.H2, "owner")
	return s
}

// d37Session creates a real session row (cookie-authenticated, as the
// browser would be).
func d37Session(t *testing.T, a *App, household, user int64) *auth.Session {
	t.Helper()
	sess, err := a.Sessions.Create(context.Background(), household, user, "d37-test", "127.0.0.1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

// d37GET runs a GET through the full router (middleware + routes included).
func d37GET(t *testing.T, a *App, sess *auth.Session, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, r)
	return rec
}

func d37Body(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return rec.Body.String()
}

// d37Page asserts status 200, returns the body, and (when want contains "")
// checks nothing else — containment assertions live at the call sites so the
// failure message names the exact expected fragment.
func d37Page(t *testing.T, a *App, sess *auth.Session, path string) string {
	t.Helper()
	rec := d37GET(t, a, sess, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", path, rec.Code)
	}
	return d37Body(t, rec)
}

// d37AssertIn / d37AssertNotIn are the containment checks.
func d37AssertIn(t *testing.T, page, body, frag string) {
	t.Helper()
	if !strings.Contains(body, frag) {
		t.Errorf("GET %s: body missing %q", page, frag)
	}
}
func d37AssertNotIn(t *testing.T, page, body, frag string) {
	t.Helper()
	if strings.Contains(body, frag) {
		t.Errorf("GET %s: body unexpectedly contains %q", page, frag)
	}
}

// --- daily layout ------------------------------------------------------------

// TestD37DailyLayout: every day-to-day page renders in the daily layout with
// its pane item active; the cogwheel (settings entry) is present; the
// Back-to-Home pill is not.
func TestD37DailyLayout(t *testing.T) {
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := d37SeedBuild(t, pool)
	owner := d37Session(t, a, s.H, s.O)

	type dailyCase struct{ path, active string }
	cases := []dailyCase{
		{"/", `<a href="/" class="active">Dashboard</a>`},
		{"/capture", `<a href="/capture" class="active">Capture</a>`},
		{"/txns", `<a href="/txns" class="active">Transactions</a>`},
		{"/cards", `<a href="/cards" class="active">Cards</a>`},
		{"/reconcile", `<a href="/reconcile" class="active">Reconcile</a>`},
		{"/recurring", `<a href="/recurring" class="active">Recurring</a>`},
		{"/review", `<a href="/review" class="active">Review</a>`},
		{"/accounts", `<a href="/accounts" class="active">Accounts</a>`},
		{"/merchants", `<a href="/merchants" class="active">Merchants</a>`},
		{"/parties", `<a href="/parties" class="active">Parties</a>`},
		// Auxiliary pages: daily layout, no active pane item (the board has
		// no entries for them; they are reached from their parents).
		{"/history", ""},
		{"/deletion-log", ""},
	}
	for _, c := range cases {
		body := d37Page(t, a, owner, c.path)
		d37AssertIn(t, c.path, body, `<div class="shell">`)
		d37AssertIn(t, c.path, body, `<aside class="sidepane">`)
		// The flat pane carries the whole destination set (owner has one
		// household, so "From other households" is absent).
		d37AssertIn(t, c.path, body, `>Dashboard</a>`)
		d37AssertIn(t, c.path, body, `>Parties</a>`)
		// Settings entry = cogwheel to /settings/household; no back-home pill.
		d37AssertIn(t, c.path, body, `<a class="cog-btn" href="/settings/household"`)
		d37AssertNotIn(t, c.path, body, `back-home`)
		// Daily brand is an inert span (the settings variant is the link).
		d37AssertIn(t, c.path, body, `<span class="brand">Eventally</span>`)
		d37AssertNotIn(t, c.path, body, `<a class="brand" href="/">`)
		// Sign out lives in the pane footer.
		d37AssertIn(t, c.path, body, `<div class="side-foot">`)
		if c.active != "" {
			d37AssertIn(t, c.path, body, c.active)
		} else {
			// No item may be marked active on auxiliary pages.
			if strings.Contains(body, `class="active"`) {
				t.Errorf("GET %s: an unexpected pane item is active", c.path)
			}
		}
	}
}

// TestD37SyncNavConditional: "From other households" appears for a user with
// two households and is absent for a single-household user (D33 condition,
// now carried in the flat pane).
func TestD37SyncNavConditional(t *testing.T) {
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := d37SeedBuild(t, pool)

	single := d37Session(t, a, s.H, s.O)
	body := d37Page(t, a, single, "/")
	d37AssertNotIn(t, "/", body, `href="/sync"`)
	d37AssertNotIn(t, "/", body, `>From other households</a>`)

	dual := d37Session(t, a, s.H, s.U)
	body = d37Page(t, a, dual, "/")
	d37AssertIn(t, "/", body, `<a href="/sync" class="">From other households</a>`)
	// The switcher (2+ memberships) renders in the pane-head tools.
	d37AssertIn(t, "/", body, `hh-switcher`)
}

// TestD37DailySyncPage: /sync itself uses the daily layout with the pane item
// active (the two-surfaces split: the inbox stays daily).
func TestD37DailySyncPage(t *testing.T) {
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := d37SeedBuild(t, pool)
	dual := d37Session(t, a, s.H, s.U)

	body := d37Page(t, a, dual, "/sync")
	d37AssertIn(t, "/sync", body, `<a href="/sync" class="active">From other households</a>`)
	d37AssertIn(t, "/sync", body, `<a class="cog-btn" href="/settings/household"`)
	d37AssertNotIn(t, "/sync", body, `back-home`)
	// The settings Sync section is a separate entry in the settings layout
	// (the cogwheel target chain), not on this page's pane.
	d37AssertNotIn(t, "/sync", body, `<a class="brand" href="/">`)
}

// --- settings layout ----------------------------------------------------------

// TestD37SettingsLayout: every nested settings section renders in the
// settings layout (admin session): Back-to-Home pill + logo link present,
// cogwheel absent, the settings pane items present, the right item active.
func TestD37SettingsLayout(t *testing.T) {
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := d37SeedBuild(t, pool)
	owner := d37Session(t, a, s.H, s.O)

	type setCase struct{ path, active string }
	cases := []setCase{
		{"/settings/household", `<a href="/settings/household" class="active">Household</a>`},
		{"/settings/categories", `<a href="/settings/categories" class="active">Categories</a>`},
		{"/settings/invites", `<a href="/settings/invites" class="active">Invites</a>`},
		{"/settings/grants", `<a href="/settings/grants" class="active">Grants</a>`},
		{"/settings/sync", `<a href="/settings/sync" class="active">Sync</a>`},
	}
	for _, c := range cases {
		body := d37Page(t, a, owner, c.path)
		d37AssertIn(t, c.path, body, `<div class="shell">`)
		d37AssertIn(t, c.path, body, `<aside class="sidepane">`)
		// Way-back (variant 1+3c): labeled pill with the home glyph + logo link.
		d37AssertIn(t, c.path, body, `<a class="back-home" href="/"><i class="la la-home"`)
		d37AssertIn(t, c.path, body, `>Back to Home</a>`)
		d37AssertIn(t, c.path, body, `<a class="brand" href="/">Eventally</a>`)
		// No cogwheel on the settings page.
		d37AssertNotIn(t, c.path, body, `cog-btn`)
		// The admin's pane carries all five entries.
		for _, frag := range []string{
			`<a href="/settings/household"`,
			`<a href="/settings/categories"`,
			`<a href="/settings/invites"`,
			`<a href="/settings/grants"`,
			`<a href="/settings/sync"`,
		} {
			d37AssertIn(t, c.path, body, frag)
		}
		d37AssertIn(t, c.path, body, `<div class="side-foot">`)
		d37AssertIn(t, c.path, body, c.active)
	}
}

// TestD37NonAdminSettings: a plain member sees Household + Categories + Sync
// only (Invites/Grants hidden entirely — whole link, not disabled), and the
// two admin sections 403 on direct URL access.
func TestD37NonAdminSettings(t *testing.T) {
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := d37SeedBuild(t, pool)
	member := d37Session(t, a, s.H, s.M)

	for _, path := range []string{"/settings/household", "/settings/sync"} {
		body := d37Page(t, a, member, path)
		d37AssertIn(t, path, body, `<a href="/settings/household"`)
		d37AssertIn(t, path, body, `<a href="/settings/categories"`)
		d37AssertIn(t, path, body, `<a href="/settings/sync"`)
		d37AssertNotIn(t, path, body, `href="/settings/invites"`)
		d37AssertNotIn(t, path, body, `href="/settings/grants"`)
		d37AssertIn(t, path, body, `<a class="back-home" href="/">`)
	}

	for _, path := range []string{"/settings/invites", "/settings/grants"} {
		rec := d37GET(t, a, member, path)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s (non-admin): status %d, want 403", path, rec.Code)
		}
	}
}

// --- redirects -----------------------------------------------------------------

// TestD37OldURLsRedirect: the pre-nesting URLs 302 into the nested routes;
// query strings are preserved.
func TestD37OldURLsRedirect(t *testing.T) {
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := d37SeedBuild(t, pool)
	owner := d37Session(t, a, s.H, s.O)

	type redirCase struct{ path, want string }
	cases := []redirCase{
		{"/settings", "/settings/household"},
		{"/settings?tab=invites", "/settings/invites"},
		{"/settings?tab=grants", "/settings/grants"},
		{"/settings?tab=sync", "/settings/sync"},
		{"/settings?tab=bogus", "/settings/household"},
		{"/categories", "/settings/categories"},
		{"/categories?class=income", "/settings/categories?class=income"},
		{"/categories/new", "/settings/categories/new"},
		{"/categories/42", "/settings/categories/42"},
	}
	for _, c := range cases {
		rec := d37GET(t, a, owner, c.path)
		if rec.Code != http.StatusFound {
			t.Errorf("GET %s: status %d, want 302", c.path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Location"); got != c.want {
			t.Errorf("GET %s: Location %q, want %q", c.path, got, c.want)
		}
	}
}

// --- categories at the new URLs -------------------------------------------------

// TestD37CategoriesAtNewURL: the D35 two-pane page and its fragment endpoints
// serve at /settings/categories*; the page carries the catsPage component
// wired to the new fragment URLs.
func TestD37CategoriesAtNewURL(t *testing.T) {
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := d37SeedBuild(t, pool)
	owner := d37Session(t, a, s.H, s.O)

	// Seed one category for the detail fragment.
	var catID int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO category (household_id, name, parent_id, transaction_class, is_budgetable)
		 VALUES ($1,'Test food',NULL,'expense',true) RETURNING id`, s.H).Scan(&catID); err != nil {
		t.Fatalf("seed category: %v", err)
	}

	body := d37Page(t, a, owner, "/settings/categories")
	d37AssertIn(t, "/settings/categories", body, `x-data="catsPage()"`)
	// The in-page machinery fetches the NEW fragment URLs (B3 update).
	d37AssertIn(t, "/settings/categories", body, "fetchText('/settings/categories/pane")
	d37AssertIn(t, "/settings/categories", body, "fetchText('/settings/categories/detail")
	// The form cancel link and the toolbar target the new paths.
	d37AssertIn(t, "/settings/categories", body, `href="/settings/categories/new"`)
	// The seeded category renders in the server-rendered pane.
	d37AssertIn(t, "/settings/categories", body, "Test food")

	// Fragments at the new URLs.
	pane := d37Page(t, a, owner, "/settings/categories/pane?class=expense")
	d37AssertIn(t, "/settings/categories/pane", pane, "Test food")
	detail := d37Page(t, a, owner, "/settings/categories/detail?id="+strconv.FormatInt(catID, 10))
	d37AssertIn(t, "/settings/categories/detail", detail, "Test food")
}
