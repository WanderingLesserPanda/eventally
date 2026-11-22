// D35 Batch 2 tests (docs/notes/d35-categories-rework-warm-pass.md, ADR-0011):
// the two-pane Categories page. Covers the verification plan's Batch-2 items:
// per-class render + default selection, sub-table self-first order, the
// direct-subcategory count, empty states, archived-row demotion, the Batch-2
// total (parent + sub budgets, all standalone pre-000020), the in-page fragment
// endpoints, and the archive/unarchive round-trip. Scenario-dense synthetic
// seeds, no bulk data, no personal names. Skipped unless EVENTALLY_TEST_DSN is
// set (harness: recurring_gate2_test.go).
package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// d35App builds an App on a throwaway migrated DB and seeds a household with
// one user, returning the App, its pool, and an authenticated session.
func d35App(t *testing.T) (*App, *pgxpool.Pool, *auth.Session) {
	t.Helper()
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	var hh, u int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO household (name, base_currency, language, first_day_of_week)
		 VALUES ('D35','MYR','en',1) RETURNING id`).Scan(&hh); err != nil {
		t.Fatalf("household: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
		 VALUES ($1,'d35u','D35 U','d35@test.local','x') RETURNING id`, hh).Scan(&u); err != nil {
		t.Fatalf("user: %v", err)
	}
	return a, pool, &auth.Session{ID: "d35", Household: hh, UserID: u}
}

// d35Cat inserts a category and returns its id. parent==0 = top level;
// budget=="" = no budget; sortOrder is the (pre-envelope) sort column.
func d35Cat(t *testing.T, pool *pgxpool.Pool, hh int64, name, class string, parent int64, budget string, sortOrder int) int64 {
	t.Helper()
	var parentArg, budgetArg any
	if parent > 0 {
		parentArg = parent
	}
	if budget != "" {
		budgetArg = budget
	}
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO category (household_id, name, parent_id, transaction_class, budget_per_month, sort_order)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		hh, name, parentArg, class, budgetArg, sortOrder).Scan(&id); err != nil {
		t.Fatalf("insert category %s: %v", name, err)
	}
	return id
}

// d35CatMode is d35Cat with an explicit budget_mode ("standalone" or
// "part_of_parent"). parent==0 = top level; budget=="" = no budget; mode==""
// defaults to standalone.
func d35CatMode(t *testing.T, pool *pgxpool.Pool, hh int64, name, class string, parent int64, budget, mode string, sortOrder int) int64 {
	t.Helper()
	var parentArg, budgetArg any
	if parent > 0 {
		parentArg = parent
	}
	if budget != "" {
		budgetArg = budget
	}
	if mode == "" {
		mode = "standalone"
	}
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO category (household_id, name, parent_id, transaction_class, budget_per_month, budget_mode, sort_order)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		hh, name, parentArg, class, budgetArg, mode, sortOrder).Scan(&id); err != nil {
		t.Fatalf("insert category %s: %v", name, err)
	}
	return id
}

// d35Detail renders the in-page detail-fragment HTML for a category id
// (the cat_detail pane with the Total Budget line and the envelope note).
func d35Detail(t *testing.T, a *App, sess *auth.Session, id int64) string {
	t.Helper()
	rec := d32Get(t, a, sess, a.catDetailFragment, "/categories/detail?id="+strconv.FormatInt(id, 10), 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail id=%d: HTTP %d, body: %s", id, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// d35Page renders the full Categories page for a query string and returns the
// HTML (failing the test on a non-200).
func d35Page(t *testing.T, a *App, sess *auth.Session, query string) string {
	t.Helper()
	rec := d32Get(t, a, sess, a.listCategories, "/categories"+query, 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /categories%s: HTTP %d, body: %s", query, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestD35PerClassAndDefaultSelection: the page renders the two-pane anatomy,
// one pane of top-level categories per class, defaulting to the first row of
// the active class (Expenses, Q9(ii)).
func TestD35PerClassAndDefaultSelection(t *testing.T) {
	a, pool, sess := d35App(t)
	food := d35Cat(t, pool, sess.Household, "Food", "expense", 0, "1000", 0)
	d35Cat(t, pool, sess.Household, "Rent", "expense", 0, "", 1)
	d35Cat(t, pool, sess.Household, "Salary", "income", 0, "5000", 0)

	body := d35Page(t, a, sess, "")
	// Two-pane anatomy.
	for _, want := range []string{
		"A little order for where it all goes.",
		"cat-toolbar", "cat-tabs", "cat-pane", "cat-detail",
		"Expenses", "Income", "Transfers", "Add Category",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// Default class = expense: both expense top-levels present, income absent.
	if !strings.Contains(body, "Rent") {
		t.Errorf("default expense page should list Rent")
	}
	if strings.Contains(body, "Salary") {
		t.Errorf("default expense page should not list the income category Salary")
	}
	// First expense row (Food, sort 0) is the default selection.
	if !strings.Contains(body, `data-id="`+strconv.FormatInt(food, 10)+`"`) ||
		!strings.Contains(body, "cat-row selected") {
		t.Errorf("default selection should mark the first expense row (Food) selected")
	}
	if !strings.Contains(body, "Edit Category") || !strings.Contains(body, "Total Budget:") {
		t.Errorf("detail pane should show Edit Category and the Total Budget line")
	}
	// Income class shows only Salary.
	bodyInc := d35Page(t, a, sess, "?class=income")
	if !strings.Contains(bodyInc, "Salary") || strings.Contains(bodyInc, "Rent") {
		t.Errorf("income class page should show Salary only")
	}
}

// TestD35SubTableSelfFirstAndTotal: the detail sub-table lists the category's
// own row first, then its subcategories in sort_order; the Batch-2 total is the
// parent budget plus all sub budgets (all standalone pre-000020).
func TestD35SubTableSelfFirstAndTotal(t *testing.T) {
	a, pool, sess := d35App(t)
	food := d35Cat(t, pool, sess.Household, "Food", "expense", 0, "1000", 0)
	d35Cat(t, pool, sess.Household, "Groceries", "expense", food, "1500", 1)
	d35Cat(t, pool, sess.Household, "Dining Out", "expense", food, "500", 0)

	body := d35Page(t, a, sess, "")
	iSub := strings.Index(body, "cat-subtable")
	if iSub < 0 {
		t.Fatalf("detail sub-table not rendered: %s", body)
	}
	det := body[iSub:]
	iSelf := strings.Index(det, "Food")
	iDining := strings.Index(det, "Dining Out")
	iGroceries := strings.Index(det, "Groceries")
	if !(iSelf >= 0 && iSelf < iDining && iDining < iGroceries) {
		t.Fatalf("sub-table order: self=%d dining=%d groceries=%d (want self < dining < groceries)", iSelf, iDining, iGroceries)
	}
	if !strings.Contains(body, "3,000.00") {
		t.Errorf("expected Batch-2 total 3,000.00 (1000 + 500 + 1500)")
	}
}

// TestD35DirectSubcategoryCount: the left-pane count is the DIRECT subcategory
// count (a grandchild is not counted).
func TestD35DirectSubcategoryCount(t *testing.T) {
	a, pool, sess := d35App(t)
	food := d35Cat(t, pool, sess.Household, "Food", "expense", 0, "", 0)
	groceries := d35Cat(t, pool, sess.Household, "Groceries", "expense", food, "", 0)
	d35Cat(t, pool, sess.Household, "Dining Out", "expense", food, "", 1)
	d35Cat(t, pool, sess.Household, "Organics", "expense", groceries, "", 0) // grandchild

	body := d35Page(t, a, sess, "")
	iPane := strings.Index(body, "id=\"cat-pane\"")
	iDetail := strings.Index(body, "id=\"cat-detail\"")
	if iPane < 0 || iDetail < 0 || iDetail < iPane {
		t.Fatalf("could not delimit the panes")
	}
	pane := body[iPane:iDetail]
	iFood := strings.Index(pane, `data-id="`+strconv.FormatInt(food, 10)+`"`)
	if iFood < 0 {
		t.Fatalf("Food row not in the left pane")
	}
	rowEnd := strings.Index(pane[iFood:], "</li>")
	row := pane[iFood : iFood+rowEnd]
	if !strings.Contains(row, `<span class="cat-count">2</span>`) {
		t.Errorf("Food direct count should be 2 (grandchild not counted), row: %s", row)
	}
}

// TestD35EmptyStates: a class with no top-level categories renders the signed
// empty left pane and the detail nudge (which lives ONLY in the empty tab).
// The nudge string also appears in the page's <head> (the catsPage Alpine
// component's JS), so the detail-pane assertions delimit the region after the
// cat-detail element (which is past the head script).
func TestD35EmptyStates(t *testing.T) {
	a, pool, sess := d35App(t)
	d35Cat(t, pool, sess.Household, "Food", "expense", 0, "", 0)

	body := d35Page(t, a, sess, "?class=income")
	if !strings.Contains(body, "No Income categories yet.") {
		t.Errorf("empty income tab should read 'No Income categories yet.'")
	}
	iCat := strings.Index(body, `id="cat-detail"`)
	det := body[iCat:] // excludes the head <script> that also names the nudge
	if !strings.Contains(det, "Please add a category first.") {
		t.Errorf("empty tab detail should show the 'Please add a category first.' nudge")
	}

	bodyExp := d35Page(t, a, sess, "?class=expense")
	iExp := strings.Index(bodyExp, `id="cat-detail"`)
	expDet := bodyExp[iExp:]
	if strings.Contains(expDet, "Please add a category first.") {
		t.Errorf("a non-empty tab should not show the empty-tab nudge")
	}
	if strings.Contains(bodyExp, "No Expenses categories yet.") {
		t.Errorf("a non-empty tab should not show the empty left-pane line")
	}
}

// TestD35ArchivedRowDemotion: an archived top-level category is demoted below
// active rows (and muted) but stays listed.
func TestD35ArchivedRowDemotion(t *testing.T) {
	a, pool, sess := d35App(t)
	d35Cat(t, pool, sess.Household, "ActiveCat", "expense", 0, "", 0)
	archived := d35Cat(t, pool, sess.Household, "ArchivedCat", "expense", 0, "", 1)
	if _, err := pool.Exec(context.Background(),
		`UPDATE category SET is_active = false WHERE id = $1`, archived); err != nil {
		t.Fatalf("archive: %v", err)
	}

	body := d35Page(t, a, sess, "")
	iActive := strings.Index(body, "ActiveCat")
	iArchived := strings.Index(body, "ArchivedCat")
	if !(iActive >= 0 && iArchived > iActive) {
		t.Fatalf("archived row should be demoted below active rows: active=%d archived=%d", iActive, iArchived)
	}
	if !strings.Contains(body, "cat-row archived") {
		t.Errorf("the archived row should carry the muted 'archived' class")
	}
}

// TestD35FragmentEndpoints: the in-page swap endpoints return the pane/detail
// fragments (what the Alpine fetch swaps put into the DOM).
func TestD35FragmentEndpoints(t *testing.T) {
	a, pool, sess := d35App(t)
	food := d35Cat(t, pool, sess.Household, "Food", "expense", 0, "1000", 0)
	d35Cat(t, pool, sess.Household, "Groceries", "expense", food, "1500", 0)

	rec := d32Get(t, a, sess, a.catPaneFragment, "/categories/pane?class=expense", 0)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Food") ||
		!strings.Contains(rec.Body.String(), "cat-count") {
		t.Fatalf("pane fragment: HTTP %d body %s", rec.Code, rec.Body.String())
	}
	// The pane fragment must NOT be wrapped in the page layout.
	if strings.Contains(rec.Body.String(), "<header") || strings.Contains(rec.Body.String(), "class=\"topbar\"") {
		t.Errorf("pane fragment should be unwrapped (no layout/topbar)")
	}

	rec = d32Get(t, a, sess, a.catDetailFragment, "/categories/detail?id="+strconv.FormatInt(food, 10), 0)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cat-subtable") ||
		!strings.Contains(rec.Body.String(), "2,500.00") {
		t.Fatalf("detail fragment: HTTP %d body %s", rec.Code, rec.Body.String())
	}

	// A bad id 404s with the empty-detail nudge.
	rec = d32Get(t, a, sess, a.catDetailFragment, "/categories/detail?id=99999", 0)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "Please add a category first.") {
		t.Fatalf("detail fragment for a bad id: HTTP %d body %s", rec.Code, rec.Body.String())
	}
}

// TestD35ArchiveUnarchiveRoundTrip: the in-page archive/unarchive endpoints
// toggle is_active and return JSON (the client then re-fetches the panes).
func TestD35ArchiveUnarchiveRoundTrip(t *testing.T) {
	a, pool, sess := d35App(t)
	ctx := context.Background()
	id := d35Cat(t, pool, sess.Household, "Food", "expense", 0, "", 0)

	rec := httptest.NewRecorder()
	a.handleCategoryArchive(rec, d30Req(t, ctx, sess, "/categories/"+strconv.FormatInt(id, 10)+"/archive", id, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("archive: HTTP %d body %s", rec.Code, rec.Body.String())
	}
	var isActive bool
	if err := pool.QueryRow(ctx, `SELECT is_active FROM category WHERE id = $1`, id).Scan(&isActive); err != nil {
		t.Fatalf("read is_active: %v", err)
	}
	if isActive {
		t.Fatalf("archive should set is_active = false")
	}

	rec = httptest.NewRecorder()
	a.handleCategoryUnarchive(rec, d30Req(t, ctx, sess, "/categories/"+strconv.FormatInt(id, 10)+"/unarchive", id, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("unarchive: HTTP %d body %s", rec.Code, rec.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT is_active FROM category WHERE id = $1`, id).Scan(&isActive); err != nil {
		t.Fatalf("read is_active: %v", err)
	}
	if !isActive {
		t.Fatalf("unarchive should set is_active = true")
	}
}

// TestD35CreateParentPreselect: ?parent= preselects the parent (the
// + Add Subcategory link) and ?class= preselects the class in the form.
func TestD35CreateParentPreselect(t *testing.T) {
	a, pool, sess := d35App(t)
	parent := d35Cat(t, pool, sess.Household, "Food", "expense", 0, "", 0)

	rec := d32Get(t, a, sess, a.handleCategoryCreate,
		"/categories/new?parent="+strconv.FormatInt(parent, 10)+"&class=expense", 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /categories/new: HTTP %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="`+strconv.FormatInt(parent, 10)+`" selected`) {
		t.Errorf("the parent should be preselected in the form")
	}
	if !strings.Contains(body, `value="expense" selected`) {
		t.Errorf("the class should be preselected in the form")
	}
}

// TestD35SaveSortRoundTrip: the staged Save Sort endpoint renumbers sort_order
// 0..N over the posted id list, for the top level of a class and for a
// parent's children; a stale/mismatched list is a 400 (no partial write).
// D35 §C / Q3 / Q13.
func TestD35SaveSortRoundTrip(t *testing.T) {
	a, pool, sess := d35App(t)
	ctx := context.Background()
	aID := d35Cat(t, pool, sess.Household, "A", "expense", 0, "", 0)
	bID := d35Cat(t, pool, sess.Household, "B", "expense", 0, "", 1)
	cID := d35Cat(t, pool, sess.Household, "C", "expense", 0, "", 2)

	rec := httptest.NewRecorder()
	form := url.Values{"class": {"expense"}, "level": {"top"}, "ids": {fmt.Sprintf("%d,%d,%d", cID, aID, bID)}}
	a.handleCategorySort(rec, d30Req(t, ctx, sess, "/categories/sort", 0, form))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("top sort: HTTP %d body %s", rec.Code, rec.Body.String())
	}
	if got := d35SortOf(t, pool, cID, aID, bID); got != "0,1,2" {
		t.Errorf("top-level renumber c,a,b = %q, want 0,1,2", got)
	}

	// Sub level: a new parent with three children, reordered.
	p := d35Cat(t, pool, sess.Household, "Parent", "expense", 0, "", 3)
	x := d35Cat(t, pool, sess.Household, "X", "expense", p, "", 0)
	y := d35Cat(t, pool, sess.Household, "Y", "expense", p, "", 1)
	z := d35Cat(t, pool, sess.Household, "Z", "expense", p, "", 2)
	rec = httptest.NewRecorder()
	form = url.Values{"class": {"expense"}, "level": {"sub"}, "parent_id": {fmt.Sprintf("%d", p)}, "ids": {fmt.Sprintf("%d,%d,%d", y, z, x)}}
	a.handleCategorySort(rec, d30Req(t, ctx, sess, "/categories/sort", 0, form))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("sub sort: HTTP %d body %s", rec.Code, rec.Body.String())
	}
	if got := d35SortOf(t, pool, y, z, x); got != "0,1,2" {
		t.Errorf("sub renumber y,z,x = %q, want 0,1,2", got)
	}

	// A stale top-level list (categories dropped) is a 400, not a partial write.
	rec = httptest.NewRecorder()
	form = url.Values{"class": {"expense"}, "level": {"top"}, "ids": {fmt.Sprintf("%d,%d", aID, bID)}}
	a.handleCategorySort(rec, d30Req(t, ctx, sess, "/categories/sort", 0, form))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("stale top sort should be 400, got %d", rec.Code)
	}
}

// d35SortOf returns the sort_order of the given ids, joined by commas.
func d35SortOf(t *testing.T, pool *pgxpool.Pool, ids ...int64) string {
	t.Helper()
	var out []string
	for _, id := range ids {
		var s int
		if err := pool.QueryRow(context.Background(), `SELECT sort_order FROM category WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("read sort_order %d: %v", id, err)
		}
		out = append(out, strconv.Itoa(s))
	}
	return strings.Join(out, ",")
}

// --- D35 §G modal form: in-page fetch-marker request helpers ---------------
// The modal form GET/POST carry X-Requested-With: fetch; the handlers answer
// with a fragment/JSON instead of a full page/redirect. These helpers set the
// marker + session (middleware bypassed, like the other D35 tests).

func d35FetchGet(t *testing.T, a *App, sess *auth.Session, h http.HandlerFunc, path string, id int64) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Requested-With", "fetch")
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

func d35FetchPost(t *testing.T, a *App, sess *auth.Session, h http.HandlerFunc, path string, id int64, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(http.MethodPost, path, body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Requested-With", "fetch")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
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

// TestD35FormFragment: the in-page form GET returns just the cat_form fragment
// (unwrapped, no layout), preselected for new (?class=/?parent=) and pre-filled
// for edit (by id), with the csrf field present.
func TestD35FormFragment(t *testing.T) {
	a, pool, sess := d35App(t)
	parent := d35Cat(t, pool, sess.Household, "Food", "expense", 0, "1000", 0)

	rec := d35FetchGet(t, a, sess, a.handleCategoryCreate,
		"/categories/new?class=income&parent="+strconv.FormatInt(parent, 10), 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("new form fragment: HTTP %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`name="name"`, `name="transaction_class"`, `name="parent_id"`,
		`name="budget"`, `name="budgetable"`, `name="icon"`, `name="csrf"`} {
		if !strings.Contains(body, want) {
			t.Errorf("form fragment missing field %s", want)
		}
	}
	if !strings.Contains(body, `value="income" selected`) {
		t.Errorf("class should be preselected (income)")
	}
	if !strings.Contains(body, `value="`+strconv.FormatInt(parent, 10)+`" selected`) {
		t.Errorf("parent should be preselected")
	}
	if strings.Contains(body, `<header`) || strings.Contains(body, `class="topbar"`) || strings.Contains(body, "cat-toolbar") {
		t.Errorf("form fragment should be unwrapped (no layout/topbar)")
	}

	eid := d35Cat(t, pool, sess.Household, "Renamed", "expense", 0, "2500", 1)
	rec = d35FetchGet(t, a, sess, a.handleCategoryEdit, "/categories/"+strconv.FormatInt(eid, 10), eid)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit form fragment: HTTP %d", rec.Code)
	}
	eb := rec.Body.String()
	if !strings.Contains(eb, `value="Renamed"`) || !strings.Contains(eb, `value="2500.00"`) {
		t.Errorf("edit form not pre-filled; name-present=%v budget-present=%v",
			strings.Contains(eb, `value="Renamed"`), strings.Contains(eb, `value="2500.00"`))
	}
}

// TestD35InPageCreate: an in-page create POST replies JSON ok (not a redirect)
// and creates the row; a validation error re-renders the form fragment (html).
func TestD35InPageCreate(t *testing.T) {
	a, pool, sess := d35App(t)
	ctx := context.Background()
	form := url.Values{
		"name": {"Coffee"}, "transaction_class": {"expense"}, "parent_id": {""},
		"budget": {"75"}, "budgetable": {"on"}, "icon": {""},
	}
	rec := d35FetchPost(t, a, sess, a.handleCategoryCreate, "/categories", 0, form)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("in-page create: HTTP %d body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("in-page create should reply JSON, got %q", ct)
	}
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM category WHERE household_id=$1 AND name='Coffee'`, sess.Household).Scan(&id); err != nil {
		t.Fatalf("category not created: %v", err)
	}

	bad := url.Values{"name": {""}, "transaction_class": {"expense"}, "parent_id": {""}, "budget": {""}, "icon": {""}}
	rec = d35FetchPost(t, a, sess, a.handleCategoryCreate, "/categories", 0, bad)
	if rec.Code != http.StatusOK {
		t.Fatalf("in-page create error: HTTP %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "application/json") {
		t.Errorf("in-page create error should re-render the form (html), not JSON")
	}
	if !strings.Contains(rec.Body.String(), "Name is required.") {
		t.Errorf("in-page create error should show 'Name is required.'")
	}
}

// TestD35InPageEdit: an in-page edit POST replies JSON ok and updates the row.
func TestD35InPageEdit(t *testing.T) {
	a, pool, sess := d35App(t)
	ctx := context.Background()
	id := d35Cat(t, pool, sess.Household, "Old", "expense", 0, "100", 0)
	form := url.Values{
		"name": {"NewName"}, "transaction_class": {"expense"}, "parent_id": {""},
		"budget": {"900"}, "budgetable": {"on"}, "icon": {""},
	}
	rec := d35FetchPost(t, a, sess, a.handleCategoryEdit, "/categories/"+strconv.FormatInt(id, 10), id, form)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("in-page edit: HTTP %d body %s", rec.Code, rec.Body.String())
	}
	var name string
	if err := pool.QueryRow(ctx, `SELECT name FROM category WHERE id=$1`, id).Scan(&name); err != nil {
		t.Fatalf("read name: %v", err)
	}
	if name != "NewName" {
		t.Errorf("edit should rename to NewName, got %q", name)
	}
}

// TestD35Envelope exercises the two-mode envelope total (ADR-0012, wave doc §D):
//
//	total = (Σ standalone sub budgets) + max(parent budget, Σ part-of-parent slices)
//
// The five pinned scenarios plus the plural no-budget note branch.
func TestD35Envelope(t *testing.T) {
	type sub struct{ name, budget, mode string }
	cases := []struct {
		name      string
		parentB   string
		subs      []sub
		wantTotal string // rendered money on the Total Budget line
		wantNote  string // exact note text ("" = no note)
	}{
		{
			name: "all standalone", parentB: "1000",
			subs: []sub{{"Groceries", "500", "standalone"}, {"Dining", "300", "standalone"}},
			wantTotal: "RM 1,800.00", wantNote: "",
		},
		{
			name: "mixed standalone and part of parent", parentB: "1000",
			subs: []sub{{"Groceries", "500", "standalone"}, {"Dining", "400", "part_of_parent"}},
			wantTotal: "RM 1,500.00", wantNote: "",
		},
		{
			name: "over allocation", parentB: "1000",
			subs: []sub{{"Groceries", "800", "part_of_parent"}, {"Dining", "700", "part_of_parent"}},
			wantTotal: "RM 1,500.00",
			wantNote:  "Over-allocated by 500, the total budget for Food becomes 1,500.",
		},
		{
			name: "no budget parent single slice", parentB: "",
			subs: []sub{{"Groceries", "1500", "part_of_parent"}},
			wantTotal: "RM 1,500.00",
			wantNote:  "No budget on Food, 1,500 from Groceries adds to the total.",
		},
		{
			name: "all part of parent under envelope", parentB: "2000",
			subs: []sub{{"Groceries", "800", "part_of_parent"}, {"Dining", "900", "part_of_parent"}},
			wantTotal: "RM 2,000.00", wantNote: "",
		},
		{
			name: "no budget parent multiple slices", parentB: "",
			subs: []sub{{"Groceries", "600", "part_of_parent"}, {"Dining", "900", "part_of_parent"}},
			wantTotal: "RM 1,500.00",
			wantNote:  "No budget on Food, 1,500 from 2 subcategories adds to the total.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, pool, sess := d35App(t)
			parent := d35CatMode(t, pool, sess.Household, "Food", "expense", 0, tc.parentB, "standalone", 0)
			for _, s := range tc.subs {
				d35CatMode(t, pool, sess.Household, s.name, "expense", parent, s.budget, s.mode, 0)
			}
			body := d35Detail(t, a, sess, parent)
			wantTotalLine := ">Total Budget:</span>\n  <b>" + tc.wantTotal + "</b>"
			if !strings.Contains(body, wantTotalLine) {
				t.Fatalf("Total Budget line missing %q in:\n%s", tc.wantTotal, body)
			}
			if tc.wantNote == "" {
				if strings.Contains(body, `class="cat-note`) {
					t.Errorf("expected no envelope note, but found one in:\n%s", body)
				}
			} else if !strings.Contains(body, tc.wantNote) {
				t.Errorf("missing note %q in:\n%s", tc.wantNote, body)
			}
		})
	}
}

// TestD35FormBudgetModePrefill: the budget-mode checkbox pre-fills its checked
// state server-side (part_of_parent = checked; standalone / top-level = not).
// The checkbox is always in the markup (Alpine x-show="pid" hides it client-side
// when no parent is picked), so a static GET is enough to assert the checked bit.
func TestD35FormBudgetModePrefill(t *testing.T) {
	a, pool, sess := d35App(t)
	parent := d35Cat(t, pool, sess.Household, "Food", "expense", 0, "1000", 0)
	pop := d35CatMode(t, pool, sess.Household, "Groceries", "expense", parent, "500", "part_of_parent", 0)
	stand := d35CatMode(t, pool, sess.Household, "Dining", "expense", parent, "500", "standalone", 0)

	editForm := func(id int64) string {
		t.Helper()
		rec := d32Get(t, a, sess, a.handleCategoryEdit, "/categories/"+strconv.FormatInt(id, 10), id)
		if rec.Code != http.StatusOK {
			t.Fatalf("edit form id=%d: HTTP %d body %s", id, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	const checked = `name="budget_mode" value="part_of_parent" checked>`
	// A part_of_parent sub pre-fills the checkbox checked.
	if !strings.Contains(editForm(pop), checked) {
		t.Errorf("part_of_parent sub form should pre-fill the checkbox checked")
	}
	// A standalone sub does NOT.
	if strings.Contains(editForm(stand), checked) {
		t.Errorf("standalone sub form should NOT pre-fill the checkbox checked")
	}
	// A top-level category does NOT (it has no parent envelope).
	if strings.Contains(editForm(parent), checked) {
		t.Errorf("top-level form should NOT pre-fill the checkbox checked")
	}
}
