package web

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shapeRead reads a file relative to this test's package directory.
func shapeRead(t *testing.T, rel string) string {
	t.Helper()
	_, this, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(this), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// hasRule reports whether a single-line CSS declaration block for `sel`
// contains `want` (the shipped app.css keeps each rule on one line).
func hasRule(css, sel, want string) bool {
	for _, line := range strings.Split(css, "\n") {
		if strings.Contains(line, sel+" {") && strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// TestCatUI_B5_shape pins the 4th-review UI refinements (D35) in the shipped
// CSS + templates. These are static "shape" checks: they assert the fixed
// patterns are present and the broken ones are gone, independent of a running
// browser (which the headless-Chromium runtime test complements).
func TestCatUI_B5_shape(t *testing.T) {
	css := shapeRead(t, "static/app.css")
	page := shapeRead(t, "templates/reference_categories.html")
	form := shapeRead(t, "templates/reference_category_form.html")

	// Item 2: sub-table headers read in normal case, not bold (override the
	// global th uppercase/bold).
	for _, want := range []string{"font-weight: 400", "text-transform: none", "letter-spacing: 0"} {
		if !hasRule(css, ".cat-subtable th", want) {
			t.Errorf("item2: .cat-subtable th missing %q", want)
		}
	}

	// Item 9: the budget column (money) is left-aligned inside the sub-table.
	for _, want := range []string{
		".cat-subtable th.money { text-align: left; }",
		".cat-subtable td.money { text-align: left; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("item9: css missing %q", want)
		}
	}
	// Item 9 (drag card): the floating card's budget cell must match the
	// table's left-aligned budget column. Regression: item 9 left-aligned the
	// table cell but the card's .dc-money stayed right-aligned, so the card's
	// budget value sat at the wrong edge during a reorder drag.
	if !hasRule(css, ".drag-card-sub .dc-money", "text-align: left") {
		t.Error("item9: drag-card budget cell (.dc-money) must be left-aligned like the table")
	}

	// Item 1 + 4: one coherent alignment (right-aligned header + cell) with a
	// gap between the Edit / Archive / grip cluster. The cell is a NORMAL
	// table cell (not a flex container — a flex <td> breaks the table's row
	// borders); it right-aligns the inline cluster and spaces it with a
	// sibling margin.
	if !hasRule(css, "th.cat-op", "text-align: right") {
		t.Error("item1: operation header must be right-aligned")
	}
	if strings.Contains(css, "th.cat-op { text-align: center") {
		t.Error("item1: the old centred operation header is still present")
	}
	if !hasRule(css, "td.cat-op", "text-align: right") {
		t.Error("item1: operation cell must be right-aligned")
	}
	if !hasRule(css, "td.cat-op > * + *", "margin-left: 0.5rem") {
		t.Error("item4: Edit/Archive/grip must keep a 0.5rem gap")
	}
	if strings.Contains(css, "td.cat-op { display: flex") {
		t.Error("item1: the flex <td> (which breaks table row borders) is still present")
	}

	// Item 5: detail-head icon sized to the title; Item 7: sub-row icon enlarged.
	if !hasRule(css, ".cat-detail-title .la", "font-size: 1.25rem") {
		t.Error("item5: detail-title icon not sized to the title")
	}
	if !hasRule(css, ".cat-subtable .cat-cell-name .la", "font-size: 1.15rem") {
		t.Error("item7: sub-row icon not enlarged")
	}

	// Item 3: explicit column widths via a <colgroup> + CSS widths.
	if !strings.Contains(page, "<colgroup>") || !strings.Contains(page, "col-name") ||
		!strings.Contains(page, "col-budget") || !strings.Contains(page, "col-op") {
		t.Error("item3: sub-table missing <colgroup> with col-name/col-budget/col-op")
	}
	for _, want := range []string{
		".col-name { width: 45%", ".col-budget { width: 30%", ".col-op { width: 25%",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("item3: css missing %q", want)
		}
	}

	// Items 6/10/11: modal Parent hidden for top-level; locked parent/class
	// carries a hidden input (a disabled select does not submit).
	if !strings.Contains(form, "{{if .F.ParentID}}") {
		t.Error("item6: Parent block must be wrapped in {{if .F.ParentID}}")
	}
	if !strings.Contains(form, `<input type="hidden" name="parent_id" value="{{.F.ParentID}}">`) {
		t.Error("item10: locked parent must carry a hidden parent_id input")
	}
	if !strings.Contains(form, `<input type="hidden" name="transaction_class" value="{{.F.Class}}">`) {
		t.Error("item11: locked class must carry a hidden transaction_class input")
	}
	if !strings.Contains(form, `<select name="transaction_class" disabled aria-disabled="true">`) {
		t.Error("item11: locked class select must be disabled")
	}
	if !strings.Contains(form, `<select name="parent_id" disabled aria-disabled="true">`) {
		t.Error("item10: locked parent select must be disabled")
	}
}
