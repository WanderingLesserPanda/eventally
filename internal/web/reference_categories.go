package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/WanderingLesserPanda/eventally/internal/money"
	"github.com/jackc/pgx/v5"
)

// Category is an expense/income/transfer bucket. Categories may be nested
// one level via parent_id (e.g. "Food" > "Dining out") and carry an
// optional monthly budget (D10).

type Category struct {
	ID         int64
	Name       string
	ParentID   int64
	ParentName string
	Class      string
	Budget     money.Money
	HasBudget  bool
	Budgetable bool
	Active     bool
	// Icon is the D32 picked glyph key (la-*) from the registry; "" = unpicked.
	Icon string
	// BudgetMode is "standalone" (default; the sub's budget adds on) or
	// "part_of_parent" (a slice of the parent's envelope). Meaningful only on a
	// subcategory; a top-level category is always its own envelope.
	BudgetMode string
}

// categorySelectOne lists the unqualified columns for a single-row category
// SELECT (the edit handler and the detail pane load by id, no parent join).
const categorySelectOne = `id, name, COALESCE(parent_id, 0), '', transaction_class,
	COALESCE(budget_per_month, 0), budget_per_month IS NOT NULL, is_budgetable, is_active,
	COALESCE(icon, ''), COALESCE(budget_mode, 'standalone')`

var categoryClasses = []string{"expense", "income", "transfer"}

// catRow is one left-pane row: a top-level category of the active class.
type catRow struct {
	ID     int64
	Name   string
	Count  int // direct-subcategory count (Q2: direct, not subtree)
	Active bool
}

// catDetail is the right detail pane: the selected category, its direct
// subcategories (self row first), and the display-time total.
type catDetail struct {
	Cat      Category
	Subs     []Category
	Total    money.Money
	HasTotal bool
	Note     string // warning note (over-allocation / no-budget parent), "" if none
}

func (a *App) listCategories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	class := r.URL.Query().Get("class")
	if !containsStr(categoryClasses, class) {
		class = "expense" // D35 Q9(ii): the default tab is Expenses.
	}
	rows, err := a.catPaneRows(ctx, hh, class)
	if err != nil {
		http.Error(w, "list categories: "+err.Error(), 500)
		return
	}
	// Default selection: an explicit ?sel= wins; otherwise the first listed row
	// (active rows list before archived, so the first row is the first active).
	var selID int64
	if v := r.URL.Query().Get("sel"); v != "" {
		if n, aerr := strconv.Atoi(v); aerr == nil {
			selID = int64(n)
		}
	}
	if selID == 0 && len(rows) > 0 {
		selID = rows[0].ID
	}
	var detail catDetail
	if selID != 0 {
		var derr error
		detail, derr = a.catDetailData(ctx, hh, selID)
		if derr != nil {
			if errors.Is(derr, pgx.ErrNoRows) {
				selID = 0 // stale ?sel= — fall back to the empty detail
			} else {
				http.Error(w, "load category: "+derr.Error(), 500)
				return
			}
		}
	}
	a.render(w, r, 200, "reference_categories.html", map[string]any{
		"Title":      "Categories",
		"Flash":      flashFrom(r),
		"Section":    "categories",
		"Class":      class,
		"SelectedID": selID,
		"Rows":       rows,
		"Detail":     detail,
		"Classes":    categoryClasses,
	})
}

// catPaneRows lists the top-level categories of a class, active rows first
// then archived (each group by sort_order, name), with the direct-subcategory
// count (Q2: direct, not subtree).
func (a *App) catPaneRows(ctx context.Context, hh int64, class string) ([]catRow, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT c.id, c.name, c.is_active,
		       (SELECT count(*) FROM category s WHERE s.parent_id = c.id)
		FROM category c
		WHERE c.household_id = $1 AND c.transaction_class = $2 AND c.parent_id IS NULL
		ORDER BY c.is_active DESC, c.sort_order, c.name`, hh, class)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []catRow
	for rows.Next() {
		var r catRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Active, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// catDetailData loads a category and its direct subcategories (active first,
// then archived, each by sort_order, name), with the display-time total.
func (a *App) catDetailData(ctx context.Context, hh, id int64) (catDetail, error) {
	var d catDetail
	err := a.Pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s FROM category WHERE id = $2 AND household_id = $1`, categorySelectOne), hh, id).
		Scan(&d.Cat.ID, &d.Cat.Name, &d.Cat.ParentID, &d.Cat.ParentName, &d.Cat.Class,
			&d.Cat.Budget, &d.Cat.HasBudget, &d.Cat.Budgetable, &d.Cat.Active, &d.Cat.Icon, &d.Cat.BudgetMode)
	if err != nil {
		return d, err
	}
	rows, err := a.Pool.Query(ctx, `
		SELECT id, name, COALESCE(parent_id,0), '', transaction_class,
		       COALESCE(budget_per_month,0), budget_per_month IS NOT NULL, is_budgetable, is_active, COALESCE(icon,''), COALESCE(budget_mode,'standalone')
		FROM category WHERE household_id = $1 AND parent_id = $2
		ORDER BY is_active DESC, sort_order, name`, hh, id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var s Category
		if err := rows.Scan(&s.ID, &s.Name, &s.ParentID, &s.ParentName, &s.Class,
			&s.Budget, &s.HasBudget, &s.Budgetable, &s.Active, &s.Icon, &s.BudgetMode); err != nil {
			return d, err
		}
		d.Subs = append(d.Subs, s)
	}
	d.Total, d.HasTotal, d.Note = catEnvelopeTotal(d.Cat, d.Subs)
	return d, nil
}

// catEnvelopeTotal computes the detail-pane "Total Budget" line with the full
// two-mode envelope rule (ADR-0012, wave doc §D):
//
//	total = (Σ standalone sub budgets) + max(parent's own budget, Σ part-of-parent slices)
//
// It returns a warning note for the two overflow cases (wave doc §D, verbatim
// wording, house style: commas, no em-dashes, no possessives):
//   - slices exceed the parent's budget  -> over-allocation
//   - the parent has no budget + slices  -> the slices add on (informational)
//
// The total is computed at display time and never written back.
func catEnvelopeTotal(cat Category, subs []Category) (total money.Money, hasTotal bool, note string) {
	var own, standalone, slice money.Money
	if cat.HasBudget {
		own = cat.Budget
	}
	var firstSlice *Category
	sliceCount := 0
	for i := range subs {
		s := &subs[i]
		if !s.HasBudget {
			continue
		}
		if s.BudgetMode == "part_of_parent" {
			slice = slice.Add(s.Budget)
			sliceCount++
			if firstSlice == nil {
				firstSlice = s
			}
		} else {
			standalone = standalone.Add(s.Budget)
		}
	}
	total = standalone
	if own >= slice {
		total = total.Add(own)
	} else {
		total = total.Add(slice)
	}
	hasTotal = cat.HasBudget || total > 0

	switch {
	case cat.HasBudget && slice > own:
		// Slices exceed the parent's envelope: the excess simply auto add-ons.
		note = fmt.Sprintf("Over-allocated by %s, the total budget for %s becomes %s.",
			moneyNote(slice-own), cat.Name, moneyNote(total))
	case !cat.HasBudget && sliceCount > 0:
		// No parent budget: the slices add on, so the total is informational.
		if sliceCount == 1 {
			note = fmt.Sprintf("No budget on %s, %s from %s adds to the total.",
				cat.Name, moneyNote(firstSlice.Budget), firstSlice.Name)
		} else {
			note = fmt.Sprintf("No budget on %s, %s from %d subcategories adds to the total.",
				cat.Name, moneyNote(slice), sliceCount)
		}
	}
	return total, hasTotal, note
}

// classLabel is the human class name for tabs and empty states.
func classLabel(c string) string {
	switch c {
	case "expense":
		return "Expenses"
	case "income":
		return "Income"
	case "transfer":
		return "Transfers"
	default:
		return c
	}
}

// queryInt reads a positive integer from the query string.
func queryInt(r *http.Request, name string) (int64, bool) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

type CategoryForm struct {
	Category
	ParentOptions []Category
	Err           string
}

func (a *App) categoryParentOptions(ctx context.Context, hh, excludeID int64) []Category {
	rows, err := a.Pool.Query(ctx,
		`SELECT id, name FROM category WHERE household_id = $1 AND parent_id IS NULL AND id <> $2
		 ORDER BY name`, hh, excludeID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (a *App) renderCategoryForm(w http.ResponseWriter, r *http.Request, f CategoryForm) {
	a.render(w, r, http.StatusOK, "reference_category_form.html", map[string]any{
		"Title":      "Category form",
		"Section":    "categories",
		"F":          f,
		"Classes":    categoryClasses,
		"Editing":    f.ID > 0,
		"IconGroups": iconGroups(),
	})
}

// inPage reports whether the request is an in-page fetch (the modal form
// GET/POST and the fragment swaps), so handlers return a JSON/fragment payload
// instead of a full page or a redirect.
func inPage(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "fetch"
}

// renderCategoryFormFragment renders just the cat_form define (no layout) for
// the in-page modal (D35 §G). Session is passed so the csrf field renders.
func (a *App) renderCategoryFormFragment(w http.ResponseWriter, r *http.Request, f CategoryForm, editing bool) {
	a.renderFragment(w, r, http.StatusOK, "cat_form", map[string]any{
		"Session":    sessionFromContext(r.Context()),
		"F":          f,
		"Classes":    categoryClasses,
		"Editing":    editing,
		"IconGroups": iconGroups(),
	})
}

// renderCategoryFormError re-renders the form with its validation error: as a
// fragment when in-page (the modal re-injects it to show the error), else as a
// full page.
func (a *App) renderCategoryFormError(w http.ResponseWriter, r *http.Request, f CategoryForm, editing bool) {
	if inPage(r) {
		a.renderCategoryFormFragment(w, r, f, editing)
		return
	}
	a.renderCategoryForm(w, r, f)
}

// categoryFormOK is the in-page success reply: the modal closes and re-fetches
// the changed panes.
func categoryFormOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (a *App) parseCategoryForm(r *http.Request, f CategoryForm) CategoryForm {
	f.Name = strings.TrimSpace(r.FormValue("name"))
	f.Class = r.FormValue("transaction_class")
	if f.Name == "" {
		f.Err = "Name is required."
		return f
	}
	switch f.Class {
	case "expense", "income", "transfer":
	default:
		f.Err = "Choose a valid class."
		return f
	}
	if pid, err := formInt(r, "parent_id", 0, 1<<31); err != nil {
		f.Err = err.Error()
		return f
	} else {
		f.ParentID = int64(pid)
	}
	f.Budgetable = r.FormValue("budgetable") == "on"
	if v := strings.TrimSpace(r.FormValue("budget")); v != "" {
		m, err := money.Parse(v)
		if err != nil {
			f.Err = "Monthly budget: " + err.Error()
			return f
		}
		f.Budget, f.HasBudget = m, true
	}
	// D32: icon glyph key — validated against the registry; "" = unpicked.
	f.Icon = strings.TrimSpace(r.FormValue("icon"))
	if f.Icon != "" && !iconValid(f.Icon) {
		f.Err = "Unknown icon."
		return f
	}
	// D35 §D: budget mode. The form sends "part_of_parent" only when the
	// subcategory's budget-mode checkbox is checked; otherwise it is absent
	// (standalone). App-side validation (standalone | part_of_parent). A
	// top-level category has no parent envelope, so it is always standalone.
	switch m := r.FormValue("budget_mode"); m {
	case "", "standalone":
		f.BudgetMode = "standalone"
	case "part_of_parent":
		f.BudgetMode = "part_of_parent"
	default:
		f.Err = "Unknown budget mode."
		return f
	}
	if f.ParentID == 0 {
		f.BudgetMode = "standalone"
	}
	return f
}

// verifyParent ensures parentID (if any) is a top-level category of the
// household. Returns an error message on failure, "" on success.
func (a *App) verifyParent(ctx context.Context, hh, parentID int64) string {
	if parentID <= 0 {
		return ""
	}
	var ok bool
	err := a.Pool.QueryRow(ctx,
		`SELECT true FROM category WHERE id = $1 AND household_id = $2 AND parent_id IS NULL`,
		parentID, hh).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return "Parent category not found."
	}
	if err != nil {
		return "parent check: " + err.Error()
	}
	return ""
}

func (a *App) handleCategoryCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		hh := sessionFromContext(r.Context()).Household
		f := CategoryForm{Category: Category{Class: "expense", Budgetable: true}}
		// D35: ?class= / ?parent= preselect (+ Add Subcategory passes the parent).
		if c := r.URL.Query().Get("class"); containsStr(categoryClasses, c) {
			f.Class = c
		}
		if pid, ok := queryInt(r, "parent"); ok {
			if a.verifyParent(r.Context(), hh, pid) == "" {
				f.ParentID = pid
			}
		}
		f.ParentOptions = a.categoryParentOptions(r.Context(), hh, 0)
		if inPage(r) {
			a.renderCategoryFormFragment(w, r, f, false)
			return
		}
		a.renderCategoryForm(w, r, f)
		return
	}
	hh := sessionFromContext(r.Context()).Household
	f := a.parseCategoryForm(r, CategoryForm{})
	if f.Err == "" {
		if msg := a.verifyParent(r.Context(), hh, f.ParentID); msg != "" {
			f.Err = msg
		}
	}
	if f.Err != "" {
		f.ParentOptions = a.categoryParentOptions(r.Context(), hh, 0)
		a.renderCategoryFormError(w, r, f, false)
		return
	}
	var parentID any
	if f.ParentID > 0 {
		parentID = f.ParentID
	}
	var budget any
	if f.HasBudget {
		budget = f.Budget
	}
	_, err := a.Pool.Exec(r.Context(),
		`INSERT INTO category (household_id, name, parent_id, transaction_class, budget_per_month, is_budgetable, icon, budget_mode)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		hh, f.Name, parentID, f.Class, budget, f.Budgetable, iconOrNil(f.Icon), f.BudgetMode)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "A category with this name already exists."
		} else {
			http.Error(w, "create category: "+err.Error(), 500)
			return
		}
		f.ParentOptions = a.categoryParentOptions(r.Context(), hh, 0)
		a.renderCategoryFormError(w, r, f, false)
		return
	}
	if inPage(r) {
		categoryFormOK(w)
		return
	}
	a.redirectRef(w, r, "/categories", "Category created.")
}

func (a *App) handleCategoryEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	var cur Category
	err := a.Pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s FROM category WHERE id = $2 AND household_id = $1`, categorySelectOne), hh, id).
		Scan(&cur.ID, &cur.Name, &cur.ParentID, &cur.ParentName, &cur.Class,
			&cur.Budget, &cur.HasBudget, &cur.Budgetable, &cur.Active, &cur.Icon, &cur.BudgetMode)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load category: "+err.Error(), 500)
		return
	}
	if r.Method == http.MethodGet {
		f := CategoryForm{Category: cur}
		f.ParentOptions = a.categoryParentOptions(ctx, hh, id)
		if inPage(r) {
			a.renderCategoryFormFragment(w, r, f, true)
			return
		}
		a.renderCategoryForm(w, r, f)
		return
	}
	f := a.parseCategoryForm(r, CategoryForm{Category: cur})
	f.ID = cur.ID
	if f.Err == "" && f.ParentID == id {
		f.Err = "A category cannot be its own parent."
	}
	if f.Err == "" {
		if msg := a.verifyParent(ctx, hh, f.ParentID); msg != "" {
			f.Err = msg
		}
	}
	if f.Err != "" {
		f.ParentOptions = a.categoryParentOptions(ctx, hh, id)
		a.renderCategoryFormError(w, r, f, true)
		return
	}
	var parentID any
	if f.ParentID > 0 {
		parentID = f.ParentID
	}
	var budget any
	if f.HasBudget {
		budget = f.Budget
	}
	_, err = a.Pool.Exec(ctx,
		`UPDATE category SET name=$1, parent_id=$2, transaction_class=$3,
		 budget_per_month=$4, is_budgetable=$5, icon=$6, budget_mode=$7
		 WHERE id=$8 AND household_id=$9`,
		f.Name, parentID, f.Class, budget, f.Budgetable, iconOrNil(f.Icon), f.BudgetMode, id, hh)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "A category with this name already exists."
		} else {
			http.Error(w, "update category: "+err.Error(), 500)
			return
		}
		f.ParentOptions = a.categoryParentOptions(ctx, hh, id)
		a.renderCategoryFormError(w, r, f, true)
		return
	}
	if inPage(r) {
		categoryFormOK(w)
		return
	}
	a.redirectRef(w, r, "/categories", "Category saved.")
}

func (a *App) handleCategoryDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	if _, err := a.Pool.Exec(ctx,
		`UPDATE category SET is_active = false WHERE id = $1 AND household_id = $2`, id, hh); err != nil {
		http.Error(w, "deactivate category: "+err.Error(), 500)
		return
	}
	a.redirectRef(w, r, "/categories", "Category deactivated — history keeps its reference.")
}

// --- D35 in-page fragments + archive/unarchive (ADR-0011) ------------------
// The Categories page loads once; tab/row/archive/unarchive are fetch swaps.
// The two GET fragment endpoints return a single section; the archive and
// unarchive POSTs return JSON, after which the client re-fetches the sections.

// catPaneFragment returns the left-pane list for a class (in-page tab switch).
func (a *App) catPaneFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	class := r.URL.Query().Get("class")
	if !containsStr(categoryClasses, class) {
		class = "expense"
	}
	rows, err := a.catPaneRows(ctx, hh, class)
	if err != nil {
		http.Error(w, "list categories: "+err.Error(), 500)
		return
	}
	var selID int64
	if len(rows) > 0 {
		selID = rows[0].ID
	}
	a.renderFragment(w, r, http.StatusOK, "cat_pane", map[string]any{
		"Class": class, "Rows": rows, "SelectedID": selID,
	})
}

// catDetailFragment returns the detail pane for a category (in-page row switch).
func (a *App) catDetailFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	id, ok := queryInt(r, "id")
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	detail, err := a.catDetailData(ctx, hh, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			a.renderFragment(w, r, http.StatusNotFound, "cat_detail", map[string]any{"Detail": catDetail{}})
			return
		}
		http.Error(w, "load category: "+err.Error(), 500)
		return
	}
	a.renderFragment(w, r, http.StatusOK, "cat_detail", map[string]any{"Detail": detail})
}

// setCatActive toggles a category's is_active (Archive = false, Unarchive =
// true, the old "Deactivate" renamed per D35 Q4/Q8). It is reversible and keeps
// all history. Returns JSON so the in-page client can re-fetch the changed
// sections instead of reloading.
func (a *App) setCatActive(w http.ResponseWriter, r *http.Request, active bool) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	tag := "archive"
	if active {
		tag = "unarchive"
	}
	if _, err := a.Pool.Exec(ctx,
		`UPDATE category SET is_active = $1 WHERE id = $2 AND household_id = $3`,
		active, id, hh); err != nil {
		http.Error(w, tag+" category: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// handleCategoryArchive soft-inactivates a category (the old "Deactivate").
func (a *App) handleCategoryArchive(w http.ResponseWriter, r *http.Request) {
	a.setCatActive(w, r, false)
}

// handleCategoryUnarchive restores an archived category.
func (a *App) handleCategoryUnarchive(w http.ResponseWriter, r *http.Request) {
	a.setCatActive(w, r, true)
}

// --- D35 Save Sort (ADR-0011, §C / Q3 / Q13) --------------------------------
// Reordering reuses category.sort_order (already the ORDER BY on budget cards
// and the recurring-template selects, so one order is honoured everywhere).
// The surfaces stage the order client-side as a drag preview; this endpoint is
// the only place it persists. One POST carries the full ordered id list for a
// class + level, and sort_order is renumbered 0..N in that order, atomically.
// The posted ids must be exactly the set at that level — a stale or dropped
// row would leave a gap, so a mismatch is a 400. No click = no write.

// catLevelIDs lists the category ids at a reorder level: "top" = the top-level
// categories of the class; "sub" = the children of parentID.
func (a *App) catLevelIDs(ctx context.Context, hh int64, class, level string, parentID int64) ([]int64, error) {
	var (
		q    string
		args []any
	)
	if level == "top" {
		q = `SELECT id FROM category WHERE household_id=$1 AND transaction_class=$2 AND parent_id IS NULL
			ORDER BY sort_order, name`
		args = []any{hh, class}
	} else {
		q = `SELECT id FROM category WHERE household_id=$1 AND parent_id=$2
			ORDER BY sort_order, name`
		args = []any{hh, parentID}
	}
	rows, err := a.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// handleCategorySort commits a staged reorder: renumber sort_order 0..N over
// the posted id list for a class + level, in a single transaction.
func (a *App) handleCategorySort(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	class := r.PostFormValue("class")
	level := r.PostFormValue("level")
	if !containsStr(categoryClasses, class) {
		http.Error(w, "bad class", 400)
		return
	}
	if level != "top" && level != "sub" {
		http.Error(w, "bad level", 400)
		return
	}
	var parentID int64
	if level == "sub" {
		pid, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("parent_id")), 10, 64)
		if err != nil || pid <= 0 {
			http.Error(w, "bad parent_id", 400)
			return
		}
		parentID = pid
		if msg := a.verifyParent(ctx, hh, parentID); msg != "" {
			http.Error(w, msg, 400)
			return
		}
	}
	posted := parseIDList(r.PostFormValue("ids"))
	cur, err := a.catLevelIDs(ctx, hh, class, level, parentID)
	if err != nil {
		http.Error(w, "list level: "+err.Error(), 500)
		return
	}
	if !sameIDSet(posted, cur) {
		http.Error(w, "the reordered list is stale, reload and try again", 400)
		return
	}
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "begin sort: "+err.Error(), 500)
		return
	}
	defer tx.Rollback(ctx)
	for i, id := range posted {
		if _, err := tx.Exec(ctx,
			`UPDATE category SET sort_order=$1 WHERE id=$2 AND household_id=$3`,
			i, id, hh); err != nil {
			http.Error(w, "renumber: "+err.Error(), 500)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "commit sort: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// parseIDList splits a comma-separated id list into int64s (empties skipped).
func parseIDList(csv string) []int64 {
	var out []int64
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.ParseInt(part, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// sameIDSet reports whether a and b hold the same multiset of ids (same
// length, same membership, order-insensitive).
func sameIDSet(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[int64]int, len(a))
	for _, x := range a {
		counts[x]++
	}
	for _, x := range b {
		if counts[x] == 0 {
			return false
		}
		counts[x]--
	}
	return true
}
