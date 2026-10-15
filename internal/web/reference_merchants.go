package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Merchant is a place where money leaves (or enters) the household.
// Carries a default category so quick capture can prefill.

type Merchant struct {
	ID         int64
	Name       string
	CategoryID int64
	Category   string
	Country    string
	Notes      string
	Active     bool
}

const merchantSelect = `m.id, m.name, COALESCE(m.default_category_id, 0), COALESCE(c.name, ''),
	m.country, m.notes, m.is_active`

func (a *App) listMerchants(w http.ResponseWriter, r *http.Request) {
	hh := sessionFromContext(r.Context()).Household
	rows, err := a.Pool.Query(r.Context(),
		fmt.Sprintf(`SELECT %s FROM merchant m LEFT JOIN category c ON c.id = m.default_category_id
			WHERE m.household_id = $1 ORDER BY m.name`, merchantSelect), hh)
	if err != nil {
		http.Error(w, "list merchants: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	var list []Merchant
	for rows.Next() {
		var m Merchant
		if err := rows.Scan(&m.ID, &m.Name, &m.CategoryID, &m.Category,
			&m.Country, &m.Notes, &m.Active); err != nil {
			http.Error(w, "scan merchant: "+err.Error(), 500)
			return
		}
		list = append(list, m)
	}
	a.render(w, r, 200, "reference_merchants.html", map[string]any{
		"Title":   "Merchants",
		"Flash":   flashFrom(r),
		"Section": "merchants",
		"List":    list,
	})
}

type MerchantForm struct {
	Merchant
	CategoryOptions []Category
	Err             string
}

// categoryOptionsForMerchants returns active categories (any class) for
// the default-category picker.
func (a *App) categoryOptionsForMerchants(ctx context.Context, hh int64) []Category {
	rows, err := a.Pool.Query(ctx,
		`SELECT id, name FROM category WHERE household_id = $1 AND is_active
		 ORDER BY transaction_class, name`, hh)
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

func (a *App) renderMerchantForm(w http.ResponseWriter, r *http.Request, f MerchantForm) {
	a.render(w, r, http.StatusOK, "reference_merchant_form.html", map[string]any{
		"Title":   "Merchant form",
		"Section": "merchants",
		"F":       f,
		"Editing": f.ID > 0,
	})
}

func (a *App) parseMerchantForm(r *http.Request, f MerchantForm) MerchantForm {
	f.Name = strings.TrimSpace(r.FormValue("name"))
	f.Country = strings.TrimSpace(r.FormValue("country"))
	f.Notes = strings.TrimSpace(r.FormValue("notes"))
	if f.Name == "" {
		f.Err = "Name is required."
		return f
	}
	if cid, err := formInt(r, "category_id", 0, 1<<31); err != nil {
		f.Err = err.Error()
		return f
	} else {
		f.CategoryID = int64(cid)
	}
	return f
}

func (a *App) handleMerchantCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		hh := sessionFromContext(r.Context()).Household
		f := MerchantForm{}
		f.CategoryOptions = a.categoryOptionsForMerchants(r.Context(), hh)
		a.renderMerchantForm(w, r, f)
		return
	}
	hh := sessionFromContext(r.Context()).Household
	f := a.parseMerchantForm(r, MerchantForm{})
	if f.Err == "" && f.CategoryID > 0 {
		var ok bool
		err := a.Pool.QueryRow(r.Context(),
			`SELECT true FROM category WHERE id = $1 AND household_id = $2`,
			f.CategoryID, hh).Scan(&ok)
		if errors.Is(err, pgx.ErrNoRows) {
			f.Err = "Default category not found."
		} else if err != nil {
			http.Error(w, "category check: "+err.Error(), 500)
			return
		}
	}
	if f.Err != "" {
		f.CategoryOptions = a.categoryOptionsForMerchants(r.Context(), hh)
		a.renderMerchantForm(w, r, f)
		return
	}
	var catID any
	if f.CategoryID > 0 {
		catID = f.CategoryID
	}
	_, err := a.Pool.Exec(r.Context(),
		`INSERT INTO merchant (household_id, name, default_category_id, country, notes)
		 VALUES ($1,$2,$3,$4,$5)`,
		hh, f.Name, catID, f.Country, f.Notes)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "A merchant with this name already exists."
		} else {
			http.Error(w, "create merchant: "+err.Error(), 500)
			return
		}
		f.CategoryOptions = a.categoryOptionsForMerchants(r.Context(), hh)
		a.renderMerchantForm(w, r, f)
		return
	}
	a.redirectRef(w, r, "/merchants", "Merchant created.")
}

func (a *App) handleMerchantEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	var cur Merchant
	err := a.Pool.QueryRow(ctx,
		`SELECT id, name, COALESCE(default_category_id, 0), COALESCE(c.name,''), country, notes, is_active
		 FROM merchant m LEFT JOIN category c ON c.id = m.default_category_id
		 WHERE m.id = $2 AND m.household_id = $1`, hh, id).
		Scan(&cur.ID, &cur.Name, &cur.CategoryID, &cur.Category,
			&cur.Country, &cur.Notes, &cur.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load merchant: "+err.Error(), 500)
		return
	}
	if r.Method == http.MethodGet {
		f := MerchantForm{Merchant: cur}
		f.CategoryOptions = a.categoryOptionsForMerchants(ctx, hh)
		a.renderMerchantForm(w, r, f)
		return
	}
	f := a.parseMerchantForm(r, MerchantForm{Merchant: cur})
	f.ID = cur.ID
	if f.Err == "" && f.CategoryID > 0 {
		var ok bool
		err := a.Pool.QueryRow(ctx,
			`SELECT true FROM category WHERE id = $1 AND household_id = $2`,
			f.CategoryID, hh).Scan(&ok)
		if errors.Is(err, pgx.ErrNoRows) {
			f.Err = "Default category not found."
		} else if err != nil {
			http.Error(w, "category check: "+err.Error(), 500)
			return
		}
	}
	if f.Err != "" {
		f.CategoryOptions = a.categoryOptionsForMerchants(ctx, hh)
		a.renderMerchantForm(w, r, f)
		return
	}
	var catID any
	if f.CategoryID > 0 {
		catID = f.CategoryID
	}
	_, err = a.Pool.Exec(ctx,
		`UPDATE merchant SET name=$1, default_category_id=$2, country=$3, notes=$4
		 WHERE id=$5 AND household_id=$6`,
		f.Name, catID, f.Country, f.Notes, id, hh)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "A merchant with this name already exists."
		} else {
			http.Error(w, "update merchant: "+err.Error(), 500)
			return
		}
		f.CategoryOptions = a.categoryOptionsForMerchants(ctx, hh)
		a.renderMerchantForm(w, r, f)
		return
	}
	a.redirectRef(w, r, "/merchants", "Merchant saved.")
}

func (a *App) handleMerchantDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	if _, err := a.Pool.Exec(ctx,
		`UPDATE merchant SET is_active = false WHERE id = $1 AND household_id = $2`, id, hh); err != nil {
		http.Error(w, "deactivate merchant: "+err.Error(), 500)
		return
	}
	a.redirectRef(w, r, "/merchants", "Merchant deactivated — history keeps its reference.")
}
