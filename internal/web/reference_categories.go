package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
}

// categorySelect lists columns with c.* qualified for the list query
// (which joins the parent). The edit handler uses its own unqualified
// SELECT against the single row.
const categorySelectList = `c.id, c.name, COALESCE(c.parent_id, 0), COALESCE(p.name, ''), c.transaction_class,
	COALESCE(c.budget_per_month, 0), c.budget_per_month IS NOT NULL, c.budgetable, c.is_active`

const categorySelectOne = `id, name, COALESCE(parent_id, 0), '', transaction_class,
	COALESCE(budget_per_month, 0), budget_per_month IS NOT NULL, budgetable, is_active`

var categoryClasses = []string{"expense", "income", "transfer"}

func (a *App) listCategories(w http.ResponseWriter, r *http.Request) {
	hh := sessionFromContext(r.Context()).Household
	rows, err := a.Pool.Query(r.Context(),
		fmt.Sprintf(`SELECT %s FROM category c LEFT JOIN category p ON p.id = c.parent_id
			WHERE c.household_id = $1 ORDER BY c.transaction_class, c.name`, categorySelectList), hh)
	if err != nil {
		http.Error(w, "list categories: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	var list []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.ParentID, &c.ParentName, &c.Class,
			&c.Budget, &c.HasBudget, &c.Budgetable, &c.Active); err != nil {
			http.Error(w, "scan category: "+err.Error(), 500)
			return
		}
		list = append(list, c)
	}
	a.render(w, r, 200, "reference_categories.html", map[string]any{
		"Title":   "Categories",
		"Flash":   flashFrom(r),
		"Section": "categories",
		"List":    list,
	})
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
		"Title":   "Category form",
		"Section": "categories",
		"F":       f,
		"Classes": categoryClasses,
		"Editing": f.ID > 0,
	})
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
		f.ParentOptions = a.categoryParentOptions(r.Context(), hh, 0)
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
		a.renderCategoryForm(w, r, f)
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
		`INSERT INTO category (household_id, name, parent_id, transaction_class, budget_per_month, budgetable)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		hh, f.Name, parentID, f.Class, budget, f.Budgetable)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "A category with this name already exists."
		} else {
			http.Error(w, "create category: "+err.Error(), 500)
			return
		}
		f.ParentOptions = a.categoryParentOptions(r.Context(), hh, 0)
		a.renderCategoryForm(w, r, f)
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
			&cur.Budget, &cur.HasBudget, &cur.Budgetable, &cur.Active)
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
		a.renderCategoryForm(w, r, f)
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
		 budget_per_month=$4, budgetable=$5 WHERE id=$6 AND household_id=$7`,
		f.Name, parentID, f.Class, budget, f.Budgetable, id, hh)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "A category with this name already exists."
		} else {
			http.Error(w, "update category: "+err.Error(), 500)
			return
		}
		f.ParentOptions = a.categoryParentOptions(ctx, hh, id)
		a.renderCategoryForm(w, r, f)
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
