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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
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
