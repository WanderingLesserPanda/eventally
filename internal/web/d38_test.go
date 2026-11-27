// D38 tests: the Categories page full-bleed + UI feedback round (goal doc
// 2026-11-27_D38-categories-full-bleed-goal.md; decisions-quickref D38).
// The mock-up (Penpot board "Categories - full page fill (to-be)", v4)
// settled: (1) the Categories window fills the Settings content pane
// (Categories only); (2) the subtitle moves out of the card into the pane
// header; (3) active tab radius 5px; (4) the selected row highlight is flat
// and runs edge-to-edge to the vertical divider; (5) the footer reads
// "Eventally - v<version>" with a version constant (v0.1); (6) empty-value
// placeholders are hyphens, not em dashes (Categories page only).
// The shape half asserts the shipped CSS/templates; the router half verifies
// the rendered page. Skipped unless EVENTALLY_TEST_DSN is set.
package web

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// --- static shape (no DB) -----------------------------------------------------

// TestD38Shape pins the D38 settled decisions in the shipped CSS + templates:
// the fixed patterns are present and the broken ones are gone.
func TestD38Shape(t *testing.T) {
	css := shapeRead(t, "static/app.css")
	page := shapeRead(t, "templates/reference_categories.html")
	layout := shapeRead(t, "templates/layout.html")
	rend := shapeRead(t, "render.go")

	// Item 3: the active tab is a 5px rounded rectangle, not a pill.
	// (Other pills in the app - tags, buttons, bars - keep their radius;
	// only the class tabs were settled to 5px.)
	if !hasRule(css, ".cat-tab", "border-radius: 5px") {
		t.Errorf("item3: .cat-tab missing border-radius: 5px")
	}
	if hasRule(css, ".cat-tab", "border-radius: 99px") {
		t.Errorf("item3: .cat-tab still has the 99px pill radius")
	}

	// Item 4: the selected row highlight is flat and runs to the divider.
	if !hasRule(css, ".cat-row.selected", "border-radius: 0") {
		t.Errorf("item4: .cat-row.selected missing border-radius: 0")
	}
	if !hasRule(css, ".cat-row.selected", "margin-right: -1.2rem") {
		t.Errorf("item4: .cat-row.selected missing the edge-to-divider margin")
	}
	// The mobile layout has no divider: the extension must be reset there.
	if !hasRule(css, ".cat-row.selected", "margin-right: 0") {
		t.Errorf("item4: mobile reset .cat-row.selected margin-right: 0 missing")
	}

	// Item 1: the bleed container drops the centered max-width.
	if !strings.Contains(css, ".container-bleed {") {
		t.Errorf("item1: .container-bleed rule missing")
	}

	// Item 2: the subtitle lives in the pane header (template), not the card.
	if !strings.Contains(layout, "pane-subtitle") {
		t.Errorf("item2: layout missing the pane-subtitle hook")
	}
	if strings.Contains(page, "A little order") {
		t.Errorf("item2: the subtitle is still inside the card template")
	}
	if strings.Contains(page, "cat-head") {
		t.Errorf("item2: the in-card cat-head block is still present")
	}

	// Item 5: the footer caption comes from the version constant.
	if !strings.Contains(rend, `const version = "0.1"`) {
		t.Errorf("item5: version constant missing from render.go")
	}
	if !strings.Contains(layout, `Eventally - v{{.Version}}`) {
		t.Errorf("item5: layout footer missing the Eventally - v{{.Version}} caption")
	}
	if strings.Contains(layout, "self-hosted household ledger") {
		t.Errorf("item5: the old footer caption is still present")
	}

	// Item 6: no em dashes anywhere in the Categories page templates (the
	// house rule is no em dashes in UI copy; the templates are the page).
	for _, name := range []string{"reference_categories", "reference_category_form"} {
		if f := shapeRead(t, "templates/"+name+".html"); strings.Contains(f, "—") {
			t.Errorf("item6: %s.html still contains an em dash", name)
		}
	}
}

// --- rendered page (DB-backed) --------------------------------------------------

// TestD38CategoriesPage verifies the settled layout in the rendered HTML:
// the bleed container, the subtitle in the pane header, the footer caption,
// and hyphen (not em dash) empty-value placeholders.
func TestD38CategoriesPage(t *testing.T) {
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := d37SeedBuild(t, pool)
	owner := d37Session(t, a, s.H, s.O)

	// One budgetable category with no budget: the Total Budget line renders
	// its empty placeholder (the hyphen check needs a rendered detail pane).
	var catID int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO category (household_id, name, parent_id, transaction_class, is_budgetable)
		 VALUES ($1,'D38 food',NULL,'expense',true) RETURNING id`, s.H).Scan(&catID); err != nil {
		t.Fatalf("seed category: %v", err)
	}

	body := d37Page(t, a, owner, "/settings/categories?sel="+strconv.FormatInt(catID, 10))

	// Item 1: the content pane is filled edge-to-edge, not centered.
	d37AssertIn(t, "/settings/categories", body, `class="container container-bleed"`)
	// Item 2: the subtitle sits in the pane header, ABOVE the card (not in it).
	sub := `<p class="pane-subtitle">A little order for where it all goes.</p>`
	if !strings.Contains(body, sub) {
		t.Errorf("GET /settings/categories: body missing the pane-header subtitle %q", sub)
	}
	if si, ci := strings.Index(body, sub), strings.Index(body, `<div class="cats"`); si < 0 || ci < 0 || si > ci {
		t.Errorf("GET /settings/categories: the subtitle must precede the .cats card")
	}
	// The card no longer carries the old head block.
	d37AssertNotIn(t, "/settings/categories", body, "cat-head")
	// Item 5: the versioned footer caption.
	d37AssertIn(t, "/settings/categories", body, "Eventally - v0.1")
	// Item 6: empty-value placeholders are hyphens; no em dash on the page.
	d37AssertIn(t, "/settings/categories", body, "Total Budget:</span>")
	d37AssertIn(t, "/settings/categories", body, "<b>-</b>")
	d37AssertNotIn(t, "/settings/categories", body, "—")
}

// TestD38ScopeAndDailyFooter: the bleed treatment is Categories-only (the
// other Settings pages keep the centered container), and the daily layout
// carries the same footer caption.
func TestD38ScopeAndDailyFooter(t *testing.T) {
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := d37SeedBuild(t, pool)
	owner := d37Session(t, a, s.H, s.O)

	// The other Settings pages stay centered (item 8: Categories only).
	for _, p := range []string{"/settings/household", "/settings/sync"} {
		body := d37Page(t, a, owner, p)
		d37AssertIn(t, p, body, `class="container"`)
		d37AssertNotIn(t, p, body, "container-bleed")
		d37AssertIn(t, p, body, "Eventally - v0.1")
	}
	// The daily layout carries the same footer caption.
	body := d37Page(t, a, owner, "/")
	d37AssertIn(t, "/", body, "Eventally - v0.1")
	d37AssertNotIn(t, "/", body, "container-bleed")
}
