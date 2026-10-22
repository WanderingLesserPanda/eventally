package web

import (
	"context"
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
	ParentID     int64
	ParentName   string
	CreatedBy    int64 // created_by_user_id (household-level created-mode checks, R5)
}

// accountListRow is one card in the accounts grid: the stored account
// plus its live ledger balance (opening + activity since).
type accountListRow struct {
	Account
	Live      money.Money
	HasLive   bool
	IsCredit  bool // liability account: balance reads as owed, not owned
	IsChild   bool // supplementary card hidden from the top-level grid
	Utilized  float64 // credit cards only: balance/limit, 0..1+
	Children  int    // card family: supplementary cards under this parent
}

// accountSummary is the top-of-page strip: what the household owns,
// owes, and has in total, across active accounts.
type accountSummary struct {
	Assets    money.Money
	Liabilities money.Money
	NetWorth  money.Money
	ActiveCount int
}

const accountSelect = `a.id, a.name, a.institution, a.account_type, a.balance_type,
	COALESCE(a.card_type, ''), COALESCE(a.credit_limit, 0), a.credit_limit IS NOT NULL,
	COALESCE(a.statement_day, 0), a.statement_day IS NOT NULL,
	COALESCE(a.payment_due_day, 0), a.payment_due_day IS NOT NULL,
	a.opening_balance, COALESCE(to_char(a.opening_balance_date, 'YYYY-MM-DD'), ''), a.is_active, a.notes,
	COALESCE(a.parent_id, 0), COALESCE(pn.name, ''), COALESCE(a.created_by_user_id, 0)`

const accountFrom = `FROM account a
	LEFT JOIN account pn ON pn.id = a.parent_id`

var accountTypes = []string{"savings", "ewallet", "credit_card", "loan", "investment", "cash", "credits"}

// accountLiveBalance is the ledger position of one account:
// opening + activity with the account as origin − activity as
// destination (non-void transactions only). Card families are summed
// across their members before netting, so a parent's roll-up matches
// the card view (D19).
func (a *App) accountLiveBalance(ctx context.Context, hh int64, ids []int64) (money.Money, error) {
	ph := inPh(ids)
	var bal money.Money
	err := a.Pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT SUM(x.opening_balance) FROM account x
		                  WHERE x.household_id = $1 AND x.id IN (`+ph+`)), 0)
		 + COALESCE((SELECT SUM(t.total_amount) FROM "transaction" t
			           WHERE t.household_id = $1 AND t.record_status <> 'void'
			             AND t.origin_account_id IN (`+ph+`)), 0)
		 - COALESCE((SELECT SUM(t.total_amount) FROM "transaction" t
			           WHERE t.household_id = $1 AND t.record_status <> 'void'
			             AND t.destination_account_id IN (`+ph+`)), 0)`,
		append([]any{hh}, toAny(ids)...)...).Scan(&bal)
	return bal, err
}

func (a *App) listAccounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, err := a.permFor(r)
	if err != nil {
		http.Error(w, "permission load: "+err.Error(), 500)
		return
	}
	if !permCanHRead(p) {
		http.Error(w, "You do not have permission to view accounts.", http.StatusForbidden)
		return
	}
	// Read-filter (Q8: the account grant is the only key): members see the
	// account objects they have a read grant on; the owner sees everything.
	rows, err := a.Pool.Query(ctx,
		fmt.Sprintf(`SELECT %s %s WHERE a.household_id = $1
		 AND ($3 OR EXISTS (SELECT 1 FROM account_grant ag
		                   WHERE ag.user_id = $2 AND ag.household_id = $1
		                     AND ag.can_read AND ag.account_id = a.id))
		 ORDER BY a.is_active DESC, a.sort_order, a.name`, accountSelect, accountFrom),
		hh, sess.UserID, p.isOwner())
	if err != nil {
		http.Error(w, "list accounts: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	var list []accountListRow
	for rows.Next() {
		var r accountListRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Institution, &r.Type, &r.BalanceType,
			&r.CardType, &r.CreditLimit, &r.HasCreditLim,
			&r.StatementDay, &r.HasStmtDay,
			&r.PaymentDue, &r.HasPayDue,
			&r.Opening, &r.OpeningDate, &r.Active, &r.Notes,
			&r.ParentID, &r.ParentName, &r.CreatedBy); err != nil {
			http.Error(w, "scan account: "+err.Error(), 500)
			return
		}
		r.IsCredit = r.BalanceType == "liability"
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "list accounts: "+err.Error(), 500)
		return
	}

	// Card families: a parent rolls up its active supplementary cards
	// (D19); children are hidden from the grid to avoid double-counting
	// the balance, but stay listed under their family.
	childOf := map[int64]bool{}
	childCount := map[int64]int{}
	crows, cerr := a.Pool.Query(ctx, `
		SELECT parent_id, count(*) FROM account
		WHERE household_id = $1 AND parent_id IS NOT NULL AND is_active
		GROUP BY parent_id`, hh)
	if cerr == nil {
		for crows.Next() {
			var pid int64
			var n int
			if crows.Scan(&pid, &n) == nil {
				childCount[pid] = n
			}
		}
		crows.Close()
	}
	for _, r := range list {
		if r.Active && r.ParentID > 0 {
			childOf[r.ID] = true
		}
	}
	for i := range list {
		if list[i].ParentID > 0 {
			list[i].Children = childCount[list[i].ParentID]
		}
		list[i].IsChild = childOf[list[i].ID]
	}

	// Live balances: per top-level account, card families summed first.
	for i, r := range list {
		if childOf[r.ID] {
			continue
		}
		fam := []int64{r.ID}
		for _, c := range list {
			if c.ParentID == r.ID && c.Active {
				fam = append(fam, c.ID)
			}
		}
		bal, err := a.accountLiveBalance(ctx, hh, fam)
		if err != nil {
			http.Error(w, "live balance: "+err.Error(), 500)
			return
		}
		list[i].Live = bal
		list[i].HasLive = true
		if list[i].IsCredit && list[i].HasCreditLim && list[i].CreditLimit > 0 {
			list[i].Utilized = float64(bal) / float64(list[i].CreditLimit)
		}
	}

	// Summary strip over active top-level accounts only.
	var sum accountSummary
	for _, r := range list {
		if !r.Active || childOf[r.ID] {
			continue
		}
		sum.ActiveCount++
		if r.IsCredit {
			sum.Liabilities = sum.Liabilities.Add(r.Live)
		} else {
			sum.Assets = sum.Assets.Add(r.Live)
		}
	}
	sum.NetWorth = sum.Assets.Sub(sum.Liabilities)

	a.render(w, r, 200, "reference_accounts.html", map[string]any{
		"Title":   "Accounts",
		"Flash":   flashFrom(r),
		"Section": "accounts",
		"List":    list,
		"ChildOf": childOf,
		"Summary": sum,
	})
}

// AccountForm carries parsed form state for create/edit.
type AccountForm struct {
	Account
	Err        string
	Parents    []Account // credit-card accounts eligible to be a parent (D19)
	ParentOpts bool      // true when the parent select is relevant (credit card)
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
	if v, perr := strconv.ParseInt(strings.TrimSpace(r.FormValue("parent_id")), 10, 64); perr == nil && v > 0 {
		f.ParentID = v
	}
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
	// D19: the parent link only applies to credit cards, and is exactly
	// one level deep — a parent may never itself have a parent.
	if f.Type != "credit_card" {
		f.ParentID = 0
	} else if f.ParentID > 0 {
		if f.ParentID == f.ID {
			f.Err = "An account cannot be its own parent."
			return f
		}
		var pType string
		var pParent int64
		err := a.Pool.QueryRow(r.Context(),
			`SELECT account_type, COALESCE(parent_id, 0) FROM account
			 WHERE id = $1 AND household_id = $2`, f.ParentID,
			sessionFromContext(r.Context()).Household).Scan(&pType, &pParent)
		if errors.Is(err, pgx.ErrNoRows) || err != nil {
			f.Err = "Unknown parent account."
			return f
		}
		if pType != "credit_card" {
			f.Err = "The parent must be a credit card account."
			return f
		}
		if pParent > 0 {
			f.Err = "The parent account already has its own parent — only one level of nesting is supported."
			return f
		}
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
		f := AccountForm{}
		a.fillParentOptions(r.Context(), &f)
		a.renderAccountForm(w, r, f)
		return
	}
	f := a.parseAccountForm(r)
	a.fillParentOptions(r.Context(), &f)
	if f.Err != "" {
		a.renderAccountForm(w, r, f)
		return
	}
	hh := sessionFromContext(r.Context()).Household
	sess := sessionFromContext(r.Context())
	p, err := a.permFor(r)
	if err != nil {
		http.Error(w, "permission load: "+err.Error(), 500)
		return
	}
	if !p.canCreateAccount() {
		http.Error(w, "You do not have permission to create accounts.", http.StatusForbidden)
		return
	}
	var id int64
	err = a.Pool.QueryRow(r.Context(),
		`INSERT INTO account (household_id, name, institution, account_type, balance_type,
		 card_type, credit_limit, statement_day, payment_due_day,
		 opening_balance, opening_balance_date, is_active, notes, parent_id,
		 created_by_user_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
		hh, f.Name, f.Institution, f.Type, f.BalanceType,
		f.cardOrNil(), f.creditLimitOrNil(), f.stmtDayOrNil(), f.payDueOrNil(),
		f.Opening, f.openingDateOrNil(), true, f.Notes, f.parentOrNil(), sess.UserID).Scan(&id)
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
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, err := a.permFor(r)
	if err != nil {
		http.Error(w, "permission load: "+err.Error(), 500)
		return
	}
	var cur Account
	err = a.Pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s %s WHERE a.id = $2 AND a.household_id = $1`, accountSelect, accountFrom), hh, id).
		Scan(&cur.ID, &cur.Name, &cur.Institution, &cur.Type, &cur.BalanceType,
			&cur.CardType, &cur.CreditLimit, &cur.HasCreditLim,
			&cur.StatementDay, &cur.HasStmtDay,
			&cur.PaymentDue, &cur.HasPayDue,
			&cur.Opening, &cur.OpeningDate, &cur.Active, &cur.Notes,
			&cur.ParentID, &cur.ParentName, &cur.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load account: "+err.Error(), 500)
		return
	}
	if !p.canReadRecords(cur.ID) {
		http.Error(w, "You do not have permission to view this account.", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost && !p.canEditAccountObject(cur.ID, cur.CreatedBy) {
		http.Error(w, "You do not have permission to edit this account.", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet {
		f := AccountForm{Account: cur}
		a.fillParentOptions(ctx, &f)
		a.renderAccountForm(w, r, f)
		return
	}
	f := a.parseAccountForm(r)
	f.ID = cur.ID
	a.fillParentOptions(ctx, &f)
	if f.Err != "" {
		a.renderAccountForm(w, r, f)
		return
	}
	_, err = a.Pool.Exec(ctx,
		`UPDATE account SET name=$1, institution=$2, account_type=$3, balance_type=$4,
		 card_type=$5, credit_limit=$6, statement_day=$7, payment_due_day=$8,
		 opening_balance=$9, opening_balance_date=$10, notes=$11, parent_id=$12
		 WHERE id=$13 AND household_id=$14`,
		f.Name, f.Institution, f.Type, f.BalanceType,
		f.cardOrNil(), f.creditLimitOrNil(), f.stmtDayOrNil(), f.payDueOrNil(),
		f.Opening, f.openingDateOrNil(), f.Notes, f.parentOrNil(), id, hh)
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

// handleAccountDelete is the STRICT delete (Q4): an account is only removable
// when it has zero tied records (no non-void transactions, no settlements, no
// child cards). Recurring templates are NOT tied records — they cascade with
// the account (migration 000015). It requires the household-level delete
// grant scoped by the created|all mode. (Archiving is the separate, reversible
// soft-inactivate — see handleAccountArchive.)
func (a *App) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, err := a.permFor(r)
	if err != nil {
		http.Error(w, "permission load: "+err.Error(), 500)
		return
	}
	var createdBy int64
	err = a.Pool.QueryRow(ctx,
		`SELECT COALESCE(created_by_user_id, 0) FROM account
		 WHERE id = $1 AND household_id = $2`, id, hh).Scan(&createdBy)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load account: "+err.Error(), 500)
		return
	}
	if !p.canDeleteAccountObject(id, createdBy) {
		http.Error(w, "You do not have permission to delete this account.", http.StatusForbidden)
		return
	}
	// Strict rule: refuse if any ledger record still ties to the account.
	tied, err := a.strictAccountHasTiedRecords(ctx, hh, id)
	if err != nil {
		http.Error(w, "check tied records: "+err.Error(), 500)
		return
	}
	if tied {
		http.Error(w, "This account still has records tied to it (transactions, "+
			"settlements, or linked card). Remove or move them first — an "+
			"account with history cannot be deleted.", http.StatusBadRequest)
		return
	}
	if _, err := a.Pool.Exec(ctx, `DELETE FROM account WHERE id = $1 AND household_id = $2`, id, hh); err != nil {
		http.Error(w, "delete account: "+err.Error(), 500)
		return
	}
	a.redirectRef(w, r, "/accounts", "Account deleted.")
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
func (f *AccountForm) parentOrNil() any {
	if f.ParentID == 0 {
		return nil
	}
	return f.ParentID
}

// fillParentOptions loads the credit-card accounts eligible to be a parent
// for the form's select (D19): all credit cards except the account being
// edited, and no grand-children (parents with their own parent are excluded).
// On a blank create form (type not chosen yet) the select is shown too —
// the handler silently ignores parent_id for non-credit-card types.
func (a *App) fillParentOptions(ctx context.Context, f *AccountForm) {
	if f.Type != "" && f.Type != "credit_card" {
		return
	}
	f.ParentOpts = true
	hh := sessionFromContext(ctx).Household
	excl := `a.id <> $2`
	exclID := f.ID
	if exclID == 0 {
		// Creating: no exclusion; $2 = 0 matches nothing.
		exclID = 0
	}
	rows, err := a.Pool.Query(ctx, `
		SELECT id, name FROM account a
		WHERE household_id = $1 AND account_type = 'credit_card' AND is_active
		  AND COALESCE(parent_id, 0) = 0 AND `+excl+`
		ORDER BY name`, hh, exclID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var p Account
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			return
		}
		f.Parents = append(f.Parents, p)
	}
}
