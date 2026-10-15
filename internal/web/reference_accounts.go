package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/money"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Reference data CRUD (phase P2): accounts, categories, merchants,
// counterparties. All lists are household-scoped; delete is a soft
// deactivation (is_active=false) so historical transactions keep their
// references.

// --- generic helpers ------------------------------------------------------

// pathID extracts the :id route parameter (chi).
func pathID(r *http.Request) (int64, bool) {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(rctx.URLParam("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// redirectRef 303s back to the list with a flash message in the query.
func (a *App) redirectRef(w http.ResponseWriter, r *http.Request, list, msg string) {
	http.Redirect(w, r, list+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func flashFrom(r *http.Request) string {
	return r.URL.Query().Get("msg")
}

// isUniqueViolation detects Postgres 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// isDateStr is a cheap YYYY-MM-DD validator for form input.
func isDateStr(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// --- accounts --------------------------------------------------------------

// Account is one ledger account (the "where the money sits" side of a
// dual-entry transaction).
type Account struct {
	ID           int64
	Name         string
	Institution  string
	Type         string
	BalanceType  string
	CardType     string
	CreditLimit  money.Money
	HasCreditLim bool
	StatementDay int
	HasStmtDay   bool
	PaymentDue   int
	HasPayDue    bool
	Opening      money.Money
	OpeningDate  string
	Active       bool
	Notes        string
}

const accountSelect = `id, name, institution, account_type, balance_type,
	COALESCE(card_type, ''), COALESCE(credit_limit, 0), credit_limit IS NOT NULL,
	COALESCE(statement_day, 0), statement_day IS NOT NULL,
	COALESCE(payment_due_day, 0), payment_due_day IS NOT NULL,
	opening_balance, COALESCE(to_char(opening_balance_date, 'YYYY-MM-DD'), ''), is_active, notes`

var accountTypes = []string{"savings", "ewallet", "credit_card", "loan", "investment", "cash", "credits"}

func (a *App) listAccounts(w http.ResponseWriter, r *http.Request) {
	hh := sessionFromContext(r.Context()).Household
	rows, err := a.Pool.Query(r.Context(),
		fmt.Sprintf(`SELECT %s FROM account WHERE household_id = $1 ORDER BY is_active DESC, sort_order, name`, accountSelect), hh)
	if err != nil {
		http.Error(w, "list accounts: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	var list []Account
	for rows.Next() {
		var ac Account
		if err := rows.Scan(&ac.ID, &ac.Name, &ac.Institution, &ac.Type, &ac.BalanceType,
			&ac.CardType, &ac.CreditLimit, &ac.HasCreditLim,
			&ac.StatementDay, &ac.HasStmtDay,
			&ac.PaymentDue, &ac.HasPayDue,
			&ac.Opening, &ac.OpeningDate, &ac.Active, &ac.Notes); err != nil {
			http.Error(w, "scan account: "+err.Error(), 500)
			return
		}
		list = append(list, ac)
	}
	a.render(w, r, 200, "reference_accounts.html", map[string]any{
		"Title":   "Accounts",
		"Flash":   flashFrom(r),
		"Section": "accounts",
		"List":    list,
	})
}

// AccountForm carries parsed form state for create/edit.
type AccountForm struct {
	Account
	Err string
}

func (a *App) renderAccountForm(w http.ResponseWriter, r *http.Request, f AccountForm) {
	a.render(w, r, http.StatusOK, "reference_account_form.html", map[string]any{
		"Title":   "Account form",
		"Section": "accounts",
		"F":       f,
		"Types":   accountTypes,
		"Editing": f.ID > 0,
	})
}

func (a *App) parseAccountForm(r *http.Request) AccountForm {
	f := AccountForm{}
	f.Name = strings.TrimSpace(r.FormValue("name"))
	f.Institution = strings.TrimSpace(r.FormValue("institution"))
	f.Type = r.FormValue("account_type")
	f.BalanceType = r.FormValue("balance_type")
	f.CardType = r.FormValue("card_type")
	f.OpeningDate = strings.TrimSpace(r.FormValue("opening_date"))
	f.Notes = strings.TrimSpace(r.FormValue("notes"))
	if f.Name == "" {
		f.Err = "Name is required."
		return f
	}
	switch f.Type {
	case "savings", "ewallet", "credit_card", "loan", "investment", "cash", "credits":
	default:
		f.Err = "Choose a valid account type."
		return f
	}
	switch f.BalanceType {
	case "asset", "liability":
	default:
		f.Err = "Choose asset or liability."
		return f
	}
	if f.CardType != "" && f.CardType != "principal" && f.CardType != "supplementary" {
		f.Err = "Card type must be principal or supplementary."
		return f
	}
	if v := strings.TrimSpace(r.FormValue("credit_limit")); v != "" {
		m, err := money.Parse(v)
		if err != nil {
			f.Err = "Credit limit: " + err.Error()
			return f
		}
		f.CreditLimit, f.HasCreditLim = m, true
	}
	if v, err := formInt(r, "statement_day", 1, 31); err != nil {
		f.Err = err.Error()
		return f
	} else if v > 0 {
		f.StatementDay, f.HasStmtDay = v, true
	}
	if v, err := formInt(r, "payment_due_day", 1, 31); err != nil {
		f.Err = err.Error()
		return f
	} else if v > 0 {
		f.PaymentDue, f.HasPayDue = v, true
	}
	if v := strings.TrimSpace(r.FormValue("opening_balance")); v != "" {
		m, err := money.Parse(v)
		if err != nil {
			f.Err = "Opening balance: " + err.Error()
			return f
		}
		f.Opening = m
	}
	if f.OpeningDate != "" && !isDateStr(f.OpeningDate) {
		f.Err = "Opening balance date must be YYYY-MM-DD."
	}
	return f
}

func (a *App) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.renderAccountForm(w, r, AccountForm{})
		return
	}
	f := a.parseAccountForm(r)
	if f.Err != "" {
		a.renderAccountForm(w, r, f)
		return
	}
	hh := sessionFromContext(r.Context()).Household
	var id int64
	err := a.Pool.QueryRow(r.Context(),
		`INSERT INTO account (household_id, name, institution, account_type, balance_type,
		 card_type, credit_limit, statement_day, payment_due_day,
		 opening_balance, opening_balance_date, is_active, notes)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		hh, f.Name, f.Institution, f.Type, f.BalanceType,
		f.cardOrNil(), f.creditLimitOrNil(), f.stmtDayOrNil(), f.payDueOrNil(),
		f.Opening, f.openingDateOrNil(), true, f.Notes).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "An account with this name already exists."
		} else {
			http.Error(w, "create account: "+err.Error(), 500)
			return
		}
		a.renderAccountForm(w, r, f)
		return
	}
	a.redirectRef(w, r, "/accounts", "Account created.")
}

func (a *App) handleAccountEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	var cur Account
	err := a.Pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s FROM account WHERE id = $2 AND household_id = $1`, accountSelect), hh, id).
		Scan(&cur.ID, &cur.Name, &cur.Institution, &cur.Type, &cur.BalanceType,
			&cur.CardType, &cur.CreditLimit, &cur.HasCreditLim,
			&cur.StatementDay, &cur.HasStmtDay,
			&cur.PaymentDue, &cur.HasPayDue,
			&cur.Opening, &cur.OpeningDate, &cur.Active, &cur.Notes)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load account: "+err.Error(), 500)
		return
	}
	if r.Method == http.MethodGet {
		a.renderAccountForm(w, r, AccountForm{Account: cur})
		return
	}
	f := a.parseAccountForm(r)
	f.ID = cur.ID
	if f.Err != "" {
		a.renderAccountForm(w, r, f)
		return
	}
	_, err = a.Pool.Exec(ctx,
		`UPDATE account SET name=$1, institution=$2, account_type=$3, balance_type=$4,
		 card_type=$5, credit_limit=$6, statement_day=$7, payment_due_day=$8,
		 opening_balance=$9, opening_balance_date=$10, notes=$11
		 WHERE id=$12 AND household_id=$13`,
		f.Name, f.Institution, f.Type, f.BalanceType,
		f.cardOrNil(), f.creditLimitOrNil(), f.stmtDayOrNil(), f.payDueOrNil(),
		f.Opening, f.openingDateOrNil(), f.Notes, id, hh)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "An account with this name already exists."
		} else {
			http.Error(w, "update account: "+err.Error(), 500)
			return
		}
		a.renderAccountForm(w, r, f)
		return
	}
	a.redirectRef(w, r, "/accounts", "Account saved.")
}

func (a *App) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	if _, err := a.Pool.Exec(ctx,
		`UPDATE account SET is_active = false WHERE id = $1 AND household_id = $2`, id, hh); err != nil {
		http.Error(w, "deactivate account: "+err.Error(), 500)
		return
	}
	a.redirectRef(w, r, "/accounts", "Account deactivated — history keeps its reference.")
}

// AccountForm nullable-field helpers.

func (f *AccountForm) cardOrNil() any {
	if f.CardType == "" {
		return nil
	}
	return f.CardType
}
func (f *AccountForm) creditLimitOrNil() any {
	if !f.HasCreditLim {
		return nil
	}
	return f.CreditLimit
}
func (f *AccountForm) stmtDayOrNil() any {
	if !f.HasStmtDay {
		return nil
	}
	return f.StatementDay
}
func (f *AccountForm) payDueOrNil() any {
	if !f.HasPayDue {
		return nil
	}
	return f.PaymentDue
}
func (f *AccountForm) openingDateOrNil() any {
	if f.OpeningDate == "" {
		return nil
	}
	return f.OpeningDate
}
