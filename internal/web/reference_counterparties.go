package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/WanderingLesserPanda/eventally/internal/money"
)

// Counterparty is a person or organisation money moves to/from outside
// the household (gifts, rent, loans to friends). Splits reference them
// via participant_type='counterparty'.
//
// D18: a counterparty may optionally link to a user — household members
// are both users and counterparties, external parties are not.
// D23: the balance column is a computed view: the sum of expense split
// legs assigned to this counterparty minus funds received from them
// (ledger rows whose origin is the counterparty). No stored column.
type Counterparty struct {
	ID           int64
	Name         string
	Type         string // person | organisation
	Relationship string
	Notes        string
	Active       bool
	UserID       int64
	UserName     string
	Balance      money.Money
	HasBalance   bool
}

const counterpartySelect = `counterparty.id, counterparty.name, counterparty.type, counterparty.relationship_type,
	counterparty.notes, counterparty.is_active,
	COALESCE(counterparty.user_id, 0), COALESCE(un.display_name, '')`

const counterpartyFrom = `FROM counterparty
	LEFT JOIN "user" un ON un.id = counterparty.user_id`

var counterpartyTypes = []string{"person", "organisation"}

func (a *App) listCounterparties(w http.ResponseWriter, r *http.Request) {
	hh := sessionFromContext(r.Context()).Household
	rows, err := a.Pool.Query(r.Context(),
		`SELECT `+counterpartySelect+` `+counterpartyFrom+` WHERE counterparty.household_id = $1 ORDER BY counterparty.name`, hh)
	if err != nil {
		http.Error(w, "list counterparties: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	var list []Counterparty
	for rows.Next() {
		var c Counterparty
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &c.Relationship, &c.Notes, &c.Active,
			&c.UserID, &c.UserName); err != nil {
			http.Error(w, "scan counterparty: "+err.Error(), 500)
			return
		}
		// D23 computed balance (see type comment).
		var bal money.Money
		if err := a.Pool.QueryRow(r.Context(), `
			SELECT COALESCE((
				SELECT SUM(s.amount) FROM split s
				JOIN "transaction" t ON t.id = s.transaction_id
				WHERE t.household_id = $1 AND t.record_status <> 'void'
				  AND s.participant_type = 'counterparty' AND s.participant_ref = $2), 0)
			   - COALESCE((
				SELECT SUM(t.total_amount) FROM "transaction" t
				WHERE t.household_id = $1 AND t.record_status <> 'void'
				  AND t.origin_type = 'counterparty' AND t.origin_ref = $2), 0)`,
			hh, c.ID).Scan(&bal); err != nil {
			http.Error(w, "counterparty balance: "+err.Error(), 500)
			return
		}
		c.Balance, c.HasBalance = bal, true
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
	Err    string
	Users  []UserOption // household members linkable to this counterparty (D18)
}

// UserOption is a minimal user row for the link select.
type UserOption struct {
	ID   int64
	Name string
}

func (a *App) fillUserOptions(ctx context.Context, f *CounterpartyForm) {
	hh := sessionFromContext(ctx).Household
	rows, err := a.Pool.Query(ctx, `
		SELECT id, display_name FROM "user"
		WHERE household_id = $1 ORDER BY display_name`, hh)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var u UserOption
		if err := rows.Scan(&u.ID, &u.Name); err != nil {
			return
		}
		f.Users = append(f.Users, u)
	}
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
	if v, perr := strconv.ParseInt(strings.TrimSpace(r.FormValue("user_id")), 10, 64); perr == nil && v > 0 {
		f.UserID = v
	}
	if f.Name == "" {
		f.Err = "Name is required."
		return f
	}
	if f.Type != "person" && f.Type != "organisation" {
		f.Err = "Choose person or organisation."
		return f
	}
	if f.UserID > 0 {
		var n int
		err := a.Pool.QueryRow(r.Context(),
			`SELECT count(*) FROM "user" WHERE id = $1 AND household_id = $2`,
			f.UserID, sessionFromContext(r.Context()).Household).Scan(&n)
		if err != nil || n == 0 {
			f.Err = "Unknown household member."
			return f
		}
	}
	return f
}

func (a *App) handleCounterpartyCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		f := CounterpartyForm{Counterparty: Counterparty{Type: "person"}}
		a.fillUserOptions(r.Context(), &f)
		a.renderCounterpartyForm(w, r, f)
		return
	}
	f := a.parseCounterpartyForm(r)
	a.fillUserOptions(r.Context(), &f)
	if f.Err != "" {
		a.renderCounterpartyForm(w, r, f)
		return
	}
	hh := sessionFromContext(r.Context()).Household
	_, err := a.Pool.Exec(r.Context(),
		`INSERT INTO counterparty (household_id, name, type, relationship_type, notes, user_id)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		hh, f.Name, f.Type, f.Relationship, f.Notes, f.userOrNil())
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
		`SELECT `+counterpartySelect+` `+counterpartyFrom+` WHERE counterparty.household_id = $1 AND counterparty.id = $2`, hh, id).
		Scan(&cur.ID, &cur.Name, &cur.Type, &cur.Relationship, &cur.Notes, &cur.Active,
			&cur.UserID, &cur.UserName)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load counterparty: "+err.Error(), 500)
		return
	}
	if r.Method == http.MethodGet {
		f := CounterpartyForm{Counterparty: cur}
		a.fillUserOptions(ctx, &f)
		a.renderCounterpartyForm(w, r, f)
		return
	}
	f := a.parseCounterpartyForm(r)
	f.ID = cur.ID
	a.fillUserOptions(ctx, &f)
	if f.Err != "" {
		a.renderCounterpartyForm(w, r, f)
		return
	}
	_, err = a.Pool.Exec(ctx,
		`UPDATE counterparty SET name=$1, type=$2, relationship_type=$3, notes=$4, user_id=$5
		 WHERE id=$6 AND household_id=$7`,
		f.Name, f.Type, f.Relationship, f.Notes, f.userOrNil(), id, hh)
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

func (f *CounterpartyForm) userOrNil() any {
	if f.UserID == 0 {
		return nil
	}
	return f.UserID
}
