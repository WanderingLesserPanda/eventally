package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Counterparty is a person or organisation money moves to/from outside
// the household (gifts, rent, loans to friends). Splits reference them
// via participant_type='counterparty'.

type Counterparty struct {
	ID           int64
	Name         string
	Type         string // person | organisation
	Relationship string
	Notes        string
	Active       bool
}

const counterpartySelect = `id, name, type, relationship_type, notes, is_active`

var counterpartyTypes = []string{"person", "organisation"}

func (a *App) listCounterparties(w http.ResponseWriter, r *http.Request) {
	hh := sessionFromContext(r.Context()).Household
	rows, err := a.Pool.Query(r.Context(),
		`SELECT `+counterpartySelect+` FROM counterparty WHERE household_id = $1 ORDER BY name`, hh)
	if err != nil {
		http.Error(w, "list counterparties: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	var list []Counterparty
	for rows.Next() {
		var c Counterparty
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &c.Relationship, &c.Notes, &c.Active); err != nil {
			http.Error(w, "scan counterparty: "+err.Error(), 500)
			return
		}
		list = append(list, c)
	}
	a.render(w, r, 200, "reference_counterparties.html", map[string]any{
		"Title":   "Counterparties",
		"Flash":   flashFrom(r),
		"Section": "counterparties",
		"List":    list,
	})
}

type CounterpartyForm struct {
	Counterparty
	Err string
}

func (a *App) renderCounterpartyForm(w http.ResponseWriter, r *http.Request, f CounterpartyForm) {
	a.render(w, r, http.StatusOK, "reference_counterparty_form.html", map[string]any{
		"Title":   "Counterparty form",
		"Section": "counterparties",
		"F":       f,
		"Types":   counterpartyTypes,
		"Editing": f.ID > 0,
	})
}

func (a *App) parseCounterpartyForm(r *http.Request) CounterpartyForm {
	f := CounterpartyForm{}
	f.Name = strings.TrimSpace(r.FormValue("name"))
	f.Type = r.FormValue("type")
	f.Relationship = strings.TrimSpace(r.FormValue("relationship"))
	f.Notes = strings.TrimSpace(r.FormValue("notes"))
	if f.Name == "" {
		f.Err = "Name is required."
		return f
	}
	if f.Type != "person" && f.Type != "organisation" {
		f.Err = "Choose person or organisation."
		return f
	}
	return f
}

func (a *App) handleCounterpartyCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.renderCounterpartyForm(w, r, CounterpartyForm{Counterparty: Counterparty{Type: "person"}})
		return
	}
	f := a.parseCounterpartyForm(r)
	if f.Err != "" {
		a.renderCounterpartyForm(w, r, f)
		return
	}
	hh := sessionFromContext(r.Context()).Household
	_, err := a.Pool.Exec(r.Context(),
		`INSERT INTO counterparty (household_id, name, type, relationship_type, notes)
		 VALUES ($1,$2,$3,$4,$5)`,
		hh, f.Name, f.Type, f.Relationship, f.Notes)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "A counterparty with this name already exists."
		} else {
			http.Error(w, "create counterparty: "+err.Error(), 500)
			return
		}
		a.renderCounterpartyForm(w, r, f)
		return
	}
	a.redirectRef(w, r, "/counterparties", "Counterparty created.")
}

func (a *App) handleCounterpartyEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	var cur Counterparty
	err := a.Pool.QueryRow(ctx,
		`SELECT `+counterpartySelect+` FROM counterparty WHERE id = $2 AND household_id = $1`, hh, id).
		Scan(&cur.ID, &cur.Name, &cur.Type, &cur.Relationship, &cur.Notes, &cur.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load counterparty: "+err.Error(), 500)
		return
	}
	if r.Method == http.MethodGet {
		a.renderCounterpartyForm(w, r, CounterpartyForm{Counterparty: cur})
		return
	}
	f := a.parseCounterpartyForm(r)
	f.ID = cur.ID
	if f.Err != "" {
		a.renderCounterpartyForm(w, r, f)
		return
	}
	_, err = a.Pool.Exec(ctx,
		`UPDATE counterparty SET name=$1, type=$2, relationship_type=$3, notes=$4
		 WHERE id=$5 AND household_id=$6`,
		f.Name, f.Type, f.Relationship, f.Notes, id, hh)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "A counterparty with this name already exists."
		} else {
			http.Error(w, "update counterparty: "+err.Error(), 500)
			return
		}
		a.renderCounterpartyForm(w, r, f)
		return
	}
	a.redirectRef(w, r, "/counterparties", "Counterparty saved.")
}

func (a *App) handleCounterpartyDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	if _, err := a.Pool.Exec(ctx,
		`UPDATE counterparty SET is_active = false WHERE id = $1 AND household_id = $2`, id, hh); err != nil {
		http.Error(w, "deactivate counterparty: "+err.Error(), 500)
		return
	}
	a.redirectRef(w, r, "/counterparties", "Counterparty deactivated — history keeps its reference.")
}
