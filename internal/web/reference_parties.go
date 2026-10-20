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

// Party is a person or organisation money moves to/from outside
// the household (gifts, rent, loans to friends) — or a household member
// (whose party row is seeded by the membership trigger). Splits reference
// them via party_id; transactions can name one as an endpoint.
//
// D18: a party may optionally link to a user — household members
// are both users and parties, external parties are not.
// D23: the balance column is a computed view: the sum of expense split
// legs assigned to this party minus funds received from them
// (transactions whose origin is the party). No stored column.
type Party struct {
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

const partySelect = `party.id, party.name, party.type, party.relationship_type,
party.notes, party.is_active,
COALESCE(party.app_user_id, 0), COALESCE(un.display_name, '')`

const partyFrom = `FROM party
	LEFT JOIN app_user un ON un.id = party.app_user_id`

var partyTypes = []string{"person", "organisation"}

func (a *App) listParties(w http.ResponseWriter, r *http.Request) {
	hh := sessionFromContext(r.Context()).Household
	rows, err := a.Pool.Query(r.Context(),
		`SELECT `+partySelect+` `+partyFrom+` WHERE party.household_id = $1 ORDER BY party.name`, hh)
	if err != nil {
		http.Error(w, "list parties: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	var list []Party
	for rows.Next() {
		var p Party
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.Relationship, &p.Notes, &p.Active,
			&p.UserID, &p.UserName); err != nil {
			http.Error(w, "scan party: "+err.Error(), 500)
			return
		}
		// D23 computed balance (see type comment).
		var bal money.Money
		if err := a.Pool.QueryRow(r.Context(), `
			SELECT COALESCE((
				SELECT SUM(s.amount) FROM transaction_split s
				JOIN "transaction" t ON t.id = s.transaction_id
				WHERE t.household_id = $1 AND t.record_status <> 'void'
				  AND s.party_id = $2), 0)
			   - COALESCE((
				SELECT SUM(t.total_amount) FROM "transaction" t
				WHERE t.household_id = $1 AND t.record_status <> 'void'
				  AND t.origin_party_id = $2), 0)`,
			hh, p.ID).Scan(&bal); err != nil {
			http.Error(w, "party balance: "+err.Error(), 500)
			return
		}
		p.Balance, p.HasBalance = bal, true
		list = append(list, p)
	}
	a.render(w, r, 200, "reference_parties.html", map[string]any{
		"Title":   "Parties",
		"Flash":   flashFrom(r),
		"Section": "parties",
		"List":    list,
	})
}

type PartyForm struct {
	Party
	Err    string
	Users  []UserOption // household members linkable to this party (D18)
}

// UserOption is a minimal user row for the link select.
type UserOption struct {
	ID   int64
	Name string
}

func (a *App) fillUserOptions(ctx context.Context, f *PartyForm) {
	hh := sessionFromContext(ctx).Household
	rows, err := a.Pool.Query(ctx, `
		SELECT id, display_name FROM app_user
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

func (a *App) renderPartyForm(w http.ResponseWriter, r *http.Request, f PartyForm) {
	a.render(w, r, http.StatusOK, "reference_party_form.html", map[string]any{
		"Title":   "Party form",
		"Section": "parties",
		"F":       f,
		"Types":   partyTypes,
		"Editing": f.ID > 0,
	})
}

func (a *App) parsePartyForm(r *http.Request) PartyForm {
	f := PartyForm{}
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
			`SELECT count(*) FROM app_user WHERE id = $1 AND household_id = $2`,
			f.UserID, sessionFromContext(r.Context()).Household).Scan(&n)
		if err != nil || n == 0 {
			f.Err = "Unknown household member."
			return f
		}
	}
	return f
}

func (a *App) handlePartyCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		f := PartyForm{Party: Party{Type: "person"}}
		a.fillUserOptions(r.Context(), &f)
		a.renderPartyForm(w, r, f)
		return
	}
	f := a.parsePartyForm(r)
	a.fillUserOptions(r.Context(), &f)
	if f.Err != "" {
		a.renderPartyForm(w, r, f)
		return
	}
	hh := sessionFromContext(r.Context()).Household
	var userID any
	if f.UserID > 0 {
		userID = f.UserID
	}
	_, err := a.Pool.Exec(r.Context(),
		`INSERT INTO party (household_id, name, type, relationship_type, notes, app_user_id)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		hh, f.Name, f.Type, f.Relationship, f.Notes, userID)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "A party with this name already exists."
		} else {
			http.Error(w, "create party: "+err.Error(), 500)
			return
		}
		a.renderPartyForm(w, r, f)
		return
	}
	a.redirectRef(w, r, "/parties", "Party created.")
}

func (a *App) handlePartyEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	var cur Party
	err := a.Pool.QueryRow(ctx,
		`SELECT `+partySelect+` `+partyFrom+` WHERE party.household_id = $1 AND party.id = $2`, hh, id).
		Scan(&cur.ID, &cur.Name, &cur.Type, &cur.Relationship, &cur.Notes, &cur.Active,
			&cur.UserID, &cur.UserName)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load party: "+err.Error(), 500)
		return
	}
	if r.Method == http.MethodGet {
		f := PartyForm{Party: cur}
		a.fillUserOptions(ctx, &f)
		a.renderPartyForm(w, r, f)
		return
	}
	f := a.parsePartyForm(r)
	f.ID = cur.ID
	a.fillUserOptions(ctx, &f)
	if f.Err != "" {
		a.renderPartyForm(w, r, f)
		return
	}
	var userID any
	if f.UserID > 0 {
		userID = f.UserID
	}
	_, err = a.Pool.Exec(ctx,
		`UPDATE party SET name=$1, type=$2, relationship_type=$3, notes=$4, app_user_id=$5
		 WHERE id=$6 AND household_id=$7`,
		f.Name, f.Type, f.Relationship, f.Notes, userID, id, hh)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "A party with this name already exists."
		} else {
			http.Error(w, "update party: "+err.Error(), 500)
			return
		}
		a.renderPartyForm(w, r, f)
		return
	}
	a.redirectRef(w, r, "/parties", "Party saved.")
}

func (a *App) handlePartyDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	if _, err := a.Pool.Exec(ctx,
		`UPDATE party SET is_active = false WHERE id = $1 AND household_id = $2`, id, hh); err != nil {
		http.Error(w, "deactivate party: "+err.Error(), 500)
		return
	}
	a.redirectRef(w, r, "/parties", "Party deactivated — history keeps its reference.")
}
