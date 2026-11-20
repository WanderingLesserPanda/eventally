// Package web — P7: recurring expense templates (D11).
//
// A recurring template generates ONE pending expense transaction per month,
// on the day-of-month set on the template (or, if the server was down that
// day, on the first page load after it). Generation is idempotent per
// month: recurring_template.last_generated_date records the month (as its
// first day) an entry was created for, and is checked before inserting.
//
// Generated entries land in the pending-review queue with source='template'
// (D9); the reviewer approves/edits/voids them like any other pending row.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/money"
	"github.com/WanderingLesserPanda/eventally/internal/split"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RecurringLine is one row of the template list.
type RecurringLine struct {
	ID            int64
	Name          string
	Merchant      string
	Category      string
	Account       string
	Amount        money.Money
	DayOfMonth    int
	Participants  string // e.g. "tester, Partner" — display only
	Active        bool
	LastGenerated *time.Time
	AccountID     int64
	Creator       int64 // 0 = unknown (pre-000012 rows); R5 delete needs a real creator
	CanDelete     bool
	CanEdit       bool
}

// recurringSpec is the validated create/edit form payload.
type recurringSpec struct {
	name       string
	merchantID int64
	categoryID int64
	accountID  int64
	amount     money.Money
	dayOfMonth int
	partKeys   []string // "user:N" / "cp:N"
}

// --- split_spec (JSONB array of participant keys) ---------------------------

// splitSpecToJSONB serializes a participant-key list into the JSONB array
// stored on recurring_template.split_spec, e.g. ["user:3","cp:2"].
func splitSpecToJSONB(keys []string) ([]byte, error) {
	if len(keys) == 0 {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// splitSpecKeys decodes a stored split_spec JSONB array back into a key list.
// (Legacy ';' -text values no longer exist after migration 000007.)
func splitSpecKeys(specJSON []byte) []string {
	var keys []string
	if len(specJSON) == 0 {
		return keys
	}
	_ = json.Unmarshal(specJSON, &keys)
	return keys
}

func (a *App) handleRecurring(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, perr := a.permFor(r)
	if perr != nil {
		http.Error(w, "permission load: "+perr.Error(), 500)
		return
	}
	a.generateDueRecurring(ctx, hh, time.Now())

	rows, err := a.Pool.Query(ctx, fmt.Sprintf(`
		SELECT t.id, t.name, COALESCE(m.name, ''), COALESCE(c.name, ''), a.name,
		       t.amount, t.day_of_month, t.is_active, t.last_generated_date, t.split_spec,
		       t.account_id, COALESCE(t.created_by_user_id, 0)
		FROM recurring_template t
		LEFT JOIN merchant m ON m.id = t.merchant_id
		LEFT JOIN category c ON c.id = t.category_id
		JOIN account a ON a.id = t.account_id
		WHERE t.household_id = $1%s
		ORDER BY t.name`, recurringVisibleWhere(p, 2)), hh, sess.UserID)
	if err != nil {
		http.Error(w, "list recurring: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	// Participant display names, resolved once for all rows.
	nameByRef := map[string]string{}
	prs, err := a.Pool.Query(r.Context(), `
		SELECT 'user:' || u.id, u.display_name
		FROM app_user u
		JOIN household_member m ON m.user_id = u.id AND m.household_id = $1
		UNION ALL
		SELECT 'cp:'||id, name FROM party WHERE household_id = $1`, hh)
	if err != nil {
		http.Error(w, "participant names: "+err.Error(), 500)
		return
	}
	for prs.Next() {
		var key, nm string
		if err := prs.Scan(&key, &nm); err != nil {
			prs.Close()
			http.Error(w, "participant names: "+err.Error(), 500)
			return
		}
		nameByRef[key] = nm
	}
	prs.Close()
	var list []RecurringLine
	for rows.Next() {
		var l RecurringLine
		var specJSON []byte
		if err := rows.Scan(&l.ID, &l.Name, &l.Merchant, &l.Category, &l.Account,
			&l.Amount, &l.DayOfMonth, &l.Active, &l.LastGenerated, &specJSON,
			&l.AccountID, &l.Creator); err != nil {
			http.Error(w, "scan recurring: "+err.Error(), 500)
			return
		}
		var names []string
		for _, k := range splitSpecKeys(specJSON) {
			if nm, ok2 := nameByRef[k]; ok2 {
				names = append(names, nm)
			} else {
				names = append(names, k)
			}
		}
		l.Participants = strings.Join(names, ", ")
		// Per-row actions (Gate 5): editing needs the account's edit grant
		// (Related mode must also be in the spec); deleting is author-scoped
		// (R5) — the creator or the owner.
		inSpec := l.Creator == sess.UserID || isSplitParty(sess.UserID, splitSpecKeys(specJSON))
		canE, mode := p.canEditRecordMode(l.AccountID)
		l.CanEdit = canE && (mode == "All" || inSpec)
		l.CanDelete = p.canDeleteRecurring(l.Creator)
		list = append(list, l)
	}
	a.render(w, r, 200, "recurring.html", map[string]any{
		"Title":   "Recurring",
		"Flash":   flashFrom(r),
		"Section": "recurring",
		"List":    list,
	})
}

// recurringVisibleWhere is the visibility predicate for recurring templates
// (alias `t`): the creator sees their own, otherwise the template's account
// must carry a read grant with an All view mode — or a Related view mode and
// the user is a split party of the template (R5 split-party visibility).
// $1 = household, $uN = user.
func recurringVisibleWhere(p *perms, uN int) string {
	if p.isOwner() {
		// The owner sees every template. The fragment still consumes the
		// $uN placeholder (a tautology) so callers can bind their args
		// uniformly — pgx rejects queries with more bound args than
		// placeholders.
		return fmt.Sprintf(" AND ($%d::bigint IS NOT NULL)", uN)
	}
	u := fmt.Sprintf("$%d", uN)
	// JSONB array of "user:N"/"cp:M" strings: a user is a party when any
	// element equals "user:<id>" (exact match avoids prefix collisions).
	// The id is concatenated, never embedded in the literal: quoting %s
	// would bake "$2" into the string and the comparison would never match.
	exact := fmt.Sprintf(
		"EXISTS (SELECT 1 FROM jsonb_array_elements_text(COALESCE(t.split_spec, '[]'::jsonb)) e(e) WHERE e = 'user:' || %s::text)", u)
	return fmt.Sprintf(
		" AND (COALESCE(t.created_by_user_id, 0) = %s OR EXISTS (SELECT 1 FROM account_grant ag WHERE ag.user_id = %s AND ag.household_id = $1 AND ag.can_read AND ag.account_id = t.account_id AND ag.view_mode = 'All') OR (EXISTS (SELECT 1 FROM account_grant ag WHERE ag.user_id = %s AND ag.household_id = $1 AND ag.can_read AND ag.account_id = t.account_id AND ag.view_mode = 'Related') AND %s))",
		u, u, u, exact)
}

// recurringForm carries the form state for create + edit.
type recurringForm struct {
	ID           int64
	Err          string
	Name         string
	MerchantID   int64
	CategoryID   int64
	AccountID    int64
	Amount       string
	DayOfMonth   string
	SelectedKeys []string
	Active       bool

	Merchants  []opt
	Cats       []opt
	Accounts   []opt
	Users      []opt
	Parties    []opt
	DayOptions []int
}

func (a *App) recurringFormOptions(ctx context.Context, hh int64) (*recurringForm, error) {
	f := &recurringForm{Active: true}
	for d := 1; d <= 28; d++ {
		f.DayOptions = append(f.DayOptions, d)
	}
	// Users (split participants).
	rows, err := a.Pool.Query(ctx,
		`SELECT u.id, u.display_name
		 FROM app_user u JOIN household_member m
		   ON m.user_id = u.id AND m.household_id = $1
		 ORDER BY u.id`, hh)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u opt
		if err := rows.Scan(&u.ID, &u.Name); err != nil {
			rows.Close()
			return nil, err
		}
		f.Users = append(f.Users, u)
	}
	rows.Close()
	// Parties (split participants: member rows + outsiders).
	rows, err = a.Pool.Query(ctx,
		`SELECT id, name FROM party WHERE household_id = $1 ORDER BY name`, hh)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c opt
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			rows.Close()
			return nil, err
		}
		f.Parties = append(f.Parties, c)
	}
	rows.Close()
	// Merchants (active or ever referenced).
	rows, err = a.Pool.Query(ctx, `
		SELECT id, name FROM merchant
		WHERE household_id = $1 AND (is_active
			OR id IN (SELECT merchant_id FROM "transaction" WHERE household_id = $1
			AND merchant_id IS NOT NULL))
		ORDER BY name`, hh)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m opt
		if err := rows.Scan(&m.ID, &m.Name); err != nil {
			rows.Close()
			return nil, err
		}
		f.Merchants = append(f.Merchants, m)
	}
	rows.Close()
	// Categories.
	rows, err = a.Pool.Query(ctx, `
		SELECT c.id, c.name, COALESCE(p.name, '')
		FROM category c LEFT JOIN category p ON p.id = c.parent_id
		WHERE c.household_id = $1
		ORDER BY c.transaction_class, c.sort_order, c.name`, hh)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c opt
		if err := rows.Scan(&c.ID, &c.Name, &c.Parent); err != nil {
			rows.Close()
			return nil, err
		}
		f.Cats = append(f.Cats, c)
	}
	rows.Close()
	// Accounts (the charge lands on one of them — normally a card).
	rows, err = a.Pool.Query(ctx, `
		SELECT id, name FROM account WHERE household_id = $1 ORDER BY name`, hh)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a2 opt
		if err := rows.Scan(&a2.ID, &a2.Name); err != nil {
			rows.Close()
			return nil, err
		}
		f.Accounts = append(f.Accounts, a2)
	}
	rows.Close()
	return f, nil
}

func (a *App) renderRecurringForm(w http.ResponseWriter, r *http.Request, f *recurringForm) {
	a.render(w, r, http.StatusOK, "recurring_form.html", map[string]any{
		"Title": "Recurring template",
		"Flash": flashFrom(r),
		"F":     f,
	})
}

func (a *App) handleRecurringCreate(w http.ResponseWriter, r *http.Request) {
	hh := sessionFromContext(r.Context()).Household
	if r.Method == http.MethodGet {
		f, err := a.recurringFormOptions(r.Context(), hh)
		if err != nil {
			http.Error(w, "load options: "+err.Error(), 500)
			return
		}
		a.renderRecurringForm(w, r, f)
		return
	}
	a.saveRecurring(w, r, hh, 0)
}

func (a *App) handleRecurringEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	if r.Method == http.MethodPost {
		hh := sessionFromContext(r.Context()).Household
		a.saveRecurring(w, r, hh, id)
		return
	}
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, perr := a.permFor(r)
	if perr != nil {
		http.Error(w, "permission load: "+perr.Error(), 500)
		return
	}
	f, err := a.recurringFormOptions(ctx, hh)
	if err != nil {
		http.Error(w, "load options: "+err.Error(), 500)
		return
	}
	// Gate 5: the edit page is only reachable with an edit grant.
	accID, _, inSpec, err := a.recurringAccess(ctx, hh, sess.UserID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load recurring: "+err.Error(), 500)
		return
	}
	if !p.canEditRecurring(accID, inSpec) {
		http.Error(w, "You do not have permission to edit this template.", http.StatusForbidden)
		return
	}
	var (
		name, amountStr string
		specJSON        []byte
		accountID       int64
		merch, cat      int64
		day             int
		active          bool
	)
	err = a.Pool.QueryRow(ctx, `
		SELECT t.name, t.amount, t.split_spec, t.account_id,
		       COALESCE(t.merchant_id, 0), COALESCE(t.category_id, 0),
		       t.day_of_month, t.is_active
		FROM recurring_template t WHERE t.id = $1 AND t.household_id = $2`, id, hh).
		Scan(&name, &amountStr, &specJSON, &accountID, &merch, &cat, &day, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load recurring: "+err.Error(), 500)
		return
	}
	f.ID = id
	f.Name = name
	f.Amount = amountStr
	f.MerchantID = merch
	f.CategoryID = cat
	f.AccountID = accountID
	f.DayOfMonth = strconv.Itoa(day)
	f.Active = active
	f.SelectedKeys = splitSpecKeys(specJSON)
	a.renderRecurringForm(w, r, f)
}

// recurringAccess loads a template's account id, creator and whether the
// user is a split party in its spec.
func (a *App) recurringAccess(ctx context.Context, hh, user, id int64) (accountID, creator int64, inSpec bool, err error) {
	var specJSON []byte
	err = a.Pool.QueryRow(ctx, `
		SELECT t.account_id, COALESCE(t.created_by_user_id, 0), COALESCE(t.split_spec, '[]'::jsonb)
		FROM recurring_template t WHERE t.id = $1 AND t.household_id = $2`, id, hh).
		Scan(&accountID, &creator, &specJSON)
	if err != nil {
		return 0, 0, false, err
	}
	inSpec = creator == user || isSplitParty(user, splitSpecKeys(specJSON))
	return accountID, creator, inSpec, nil
}

func (a *App) saveRecurring(w http.ResponseWriter, r *http.Request, hh, id int64) {
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	p, perr := a.permFor(r)
	if perr != nil {
		http.Error(w, "permission load: "+perr.Error(), 500)
		return
	}
	f, err := a.recurringFormOptions(ctx, hh)
	if err != nil {
		http.Error(w, "load options: "+err.Error(), 500)
		return
	}
	if id > 0 {
		f.ID = id
	}

	f.Name = strings.TrimSpace(r.FormValue("name"))
	if f.Name == "" {
		f.Err = "Name is required."
		a.renderRecurringForm(w, r, f)
		return
	}
	if m := r.FormValue("merchant_id"); m != "" {
		f.MerchantID, _ = strconv.ParseInt(m, 10, 64)
	}
	if c := r.FormValue("category_id"); c != "" {
		f.CategoryID, _ = strconv.ParseInt(c, 10, 64)
	}
	// D34: the template's generated records must carry a category — the
	// single shared rule, required at template creation (an edit keeps its
	// pick; legacy templates without one stay as they are).
	if id == 0 {
		if err := categoryRequiredErr(f.CategoryID, f.Cats); err != nil {
			f.Err = err.Error()
			a.renderRecurringForm(w, r, f)
			return
		}
	}
	if acc := strings.TrimSpace(r.FormValue("account_id")); acc == "" {
		f.Err = "Choose the account the charge lands on."
		a.renderRecurringForm(w, r, f)
		return
	}
	a2, err := formInt(r, "account_id", 1, 1<<31)
	if err != nil {
		f.Err = "Choose the account the charge lands on."
		a.renderRecurringForm(w, r, f)
		return
	}
	f.AccountID = int64(a2)
	amount, err := money.Parse(r.FormValue("amount"))
	if err != nil || amount <= 0 {
		f.Err = "Amount: enter a number greater than zero."
		a.renderRecurringForm(w, r, f)
		return
	}
	f.Amount = amount.String()
	day, err := formInt(r, "day_of_month", 1, 28)
	if err != nil {
		f.Err = "Day of month must be between 1 and 28."
		a.renderRecurringForm(w, r, f)
		return
	}
	f.DayOfMonth = strconv.Itoa(day)
	f.Active = r.FormValue("active") == "on"

	// Participants: checkboxes, de-duplicated, existence-verified.
	seen := map[string]bool{}
	for _, k := range r.Form["part[]"] {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		typ, pid, perr := parseParticipantKey(k)
		if perr != nil {
			f.Err = "A participant is invalid."
			a.renderRecurringForm(w, r, f)
			return
		}
		table := "party"
		if typ == "household_user" {
			table = `app_user`
		}
		if !refExists(ctx, a.Pool, table, pid, hh) {
			f.Err = "A participant is not valid for this household."
			a.renderRecurringForm(w, r, f)
			return
		}
		seen[k] = true
		f.SelectedKeys = append(f.SelectedKeys, k)
	}
	if len(f.SelectedKeys) == 0 {
		f.Err = "Choose at least one person who splits this expense."
		a.renderRecurringForm(w, r, f)
		return
	}

	// Gate 5 write checks.
	if id == 0 {
		if !p.canCreateRecurring(f.AccountID) {
			f.Err = "You do not have permission to create a template on that account."
			a.renderRecurringForm(w, r, f)
			return
		}
	} else {
		accID, _, inSpec, err := a.recurringAccess(ctx, hh, sess.UserID, id)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", 404)
			return
		}
		if err != nil {
			http.Error(w, "load recurring: "+err.Error(), 500)
			return
		}
		if !p.canEditRecurring(accID, inSpec) {
			f.Err = "You do not have permission to edit this template."
			a.renderRecurringForm(w, r, f)
			return
		}
		// Moving the template to another account also needs a write grant
		// on the new account.
		if f.AccountID != accID && !p.canEditRecurring(f.AccountID, inSpec) {
			f.Err = "You do not have permission to move this template to that account."
			a.renderRecurringForm(w, r, f)
			return
		}
	}

	var merch, cat any
	if f.MerchantID > 0 {
		merch = f.MerchantID
	}
	if f.CategoryID > 0 {
		cat = f.CategoryID
	}
	if id == 0 {
		specJSON, jerr := splitSpecToJSONB(f.SelectedKeys)
		if jerr != nil {
			f.Err = "Could not encode participants."
			a.renderRecurringForm(w, r, f)
			return
		}
		_, err = a.Pool.Exec(ctx, `
			INSERT INTO recurring_template
				(household_id, name, merchant_id, category_id, account_id,
				 amount, day_of_month, split_spec, is_active, created_by_user_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			hh, f.Name, merch, cat, f.AccountID, amount, day,
			specJSON, f.Active, sess.UserID)
		if err != nil {
			if isUniqueViolation(err) {
				f.Err = "A recurring template with this name already exists."
				a.renderRecurringForm(w, r, f)
				return
			}
			http.Error(w, "create recurring: "+err.Error(), 500)
			return
		}
		a.redirectRef(w, r, "/recurring", "Recurring template created.")
		return
	}
	specJSON2, jerr := splitSpecToJSONB(f.SelectedKeys)
	if jerr != nil {
		f.Err = "Could not encode participants."
		a.renderRecurringForm(w, r, f)
		return
	}
	res, err := a.Pool.Exec(ctx, `
		UPDATE recurring_template SET
			name=$1, merchant_id=$2, category_id=$3, account_id=$4,
			amount=$5, day_of_month=$6, split_spec=$7, is_active=$8
		WHERE id=$9 AND household_id=$10`,
		f.Name, merch, cat, f.AccountID, amount, day,
		specJSON2, f.Active, id, hh)
	if err != nil {
		if isUniqueViolation(err) {
			f.Err = "A recurring template with this name already exists."
		} else {
			http.Error(w, "update recurring: "+err.Error(), 500)
			return
		}
		a.renderRecurringForm(w, r, f)
		return
	}
	if res.RowsAffected() == 0 {
		http.Error(w, "not found", 404)
		return
	}
	a.redirectRef(w, r, "/recurring", "Recurring template saved.")
}

func (a *App) handleRecurringDeactivate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
		return
	}
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, perr := a.permFor(r)
	if perr != nil {
		http.Error(w, "permission load: "+perr.Error(), 500)
		return
	}
	// R5: deactivating a template is an author-scoped action (creator or
	// owner). A split party can see/edit it but not stop it.
	_, creator, _, err := a.recurringAccess(ctx, hh, sess.UserID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load recurring: "+err.Error(), 500)
		return
	}
	if !p.canDeleteRecurring(creator) {
		http.Error(w, "Only the template's creator can stop it.", http.StatusForbidden)
		return
	}
	res, err := a.Pool.Exec(ctx,
		`UPDATE recurring_template SET is_active = false WHERE id = $1 AND household_id = $2`,
		id, hh)
	if err != nil {
		http.Error(w, "deactivate recurring: "+err.Error(), 500)
		return
	}
	if res.RowsAffected() == 0 {
		http.Error(w, "not found", 404)
		return
	}
	a.redirectRef(w, r, "/recurring", "Template stopped — already-generated entries are untouched.")
}

// --- month-start generation (D11) -------------------------------------------

// dueTemplate is one recurring row selected for generation this month.
type dueTemplate struct {
	id, account, merch, cat int64
	name                    string
	amount                  money.Money
	day                     int
	specJSON                []byte
}

// generateDueRecurring creates this month's pending entry for every active
// template that has not yet been generated for the current month. Called on
// page loads (recurring list + dashboard) so a down-at-month-start server
// catches up on the first visit. Missing months are NOT back-filled (D11:
// simple recurrence only).
func (a *App) generateDueRecurring(ctx context.Context, hh int64, now time.Time) {
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	rows, err := a.Pool.Query(ctx, `
		SELECT t.id, t.name, t.amount, t.day_of_month, t.split_spec,
		       t.account_id, COALESCE(t.merchant_id, 0), COALESCE(t.category_id, 0)
		FROM recurring_template t
		WHERE t.household_id = $1 AND t.is_active
		  AND (t.last_generated_date IS NULL OR t.last_generated_date < $2)
		ORDER BY t.id`, hh, monthStart)
	if err != nil {
		return
	}
	var dues []dueTemplate
	for rows.Next() {
		var d dueTemplate
		if err := rows.Scan(&d.id, &d.name, &d.amount, &d.day, &d.specJSON,
			&d.account, &d.merch, &d.cat); err != nil {
			rows.Close()
			return
		}
		dues = append(dues, d)
	}
	rows.Close()

	// Generate date = the template's day of the current month (1..28 is
	// always valid); if that day is in the future the entry is still dated
	// to it (it represents a standing charge, e.g. a rental on the 1st).
	for _, d := range dues {
		date := monthStart.AddDate(0, 0, d.day-1).Format("2006-01-02")
		if err := a.generateRecurringEntry(ctx, hh, d, date); err != nil {
			if errors.Is(err, errSiblingGenerated) {
				log.Printf("recurring: %q for %s already generated by another worker", d.name, date)
				continue
			}
			// Leave last_generated_date unset so the next page load
			// retries (a vanished participant, for instance).
			logRecurringSkip(d.name, err)
			continue
		}
	}
}

// logRecurringSkip records a generation failure without failing the page.
// The template stays ungenerated, so the next page load retries it.
func logRecurringSkip(name string, err error) {
	log.Printf("recurring: skipped %q: %v", name, err)
}

// errSiblingGenerated: a concurrent worker already generated this month's
// occurrence (lost the row-lock race). Not a failure: log and move on.
var errSiblingGenerated = errors.New("sibling worker already generated this occurrence")

func (a *App) generateRecurringEntry(ctx context.Context, hh int64, d dueTemplate, date string) error {
	// Serialize concurrent generation for this template. The row lock is
	// the fast path: two workers loading the page at once queue up here,
	// the second one then re-checks and sees the occurrence already exists.
	// The unique index uq_txn_recurring_occurrence is the safety net the
	// lock is not (crash between lock release and commit, etc.): the loser
	// hits 23505 and is skipped.
	//
	// Validate transaction_split participants; skip (and retry later) if any vanished.
	// (parts is de-duplicated — the stored array already is, but be safe.)
	var parts []string
	seen := map[string]bool{}
	for _, k := range splitSpecKeys(d.specJSON) {
		if k == "" || seen[k] {
			continue
		}
		typ, pid, perr := parseParticipantKey(k)
		if perr != nil {
			return fmt.Errorf("recurring %s: bad participant %q", d.name, k)
		}
		table := "party"
		if typ == "household_user" {
			table = `app_user`
		}
		if !refExists(ctx, a.Pool, table, pid, hh) {
			return fmt.Errorf("recurring %s: participant %q no longer exists", d.name, k)
		}
		seen[k] = true
		parts = append(parts, k)
	}
	if len(parts) == 0 {
		return fmt.Errorf("recurring %s: no participants in split_spec", d.name)
	}
	shares := split.EqualSplit(d.amount, len(parts))

	partyIDs, err := a.splitEditorKeys(ctx, hh, parts)
	if err != nil {
		return fmt.Errorf("recurring %s: %v", d.name, err)
	}

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Row lock: serializes concurrent generation for this template. The
	// loser's INSERT below then hits the void-aware unique index (23505).
	if err := tx.QueryRow(ctx,
		`SELECT id FROM recurring_template WHERE id = $1 FOR UPDATE`, d.id).Scan(&d.id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Template vanished mid-generation; next load re-queries dues.
			return fmt.Errorf("recurring %s: template no longer exists", d.name)
		}
		return err
	}
	var merch, cat any
	if d.merch > 0 {
		merch = d.merch
	}
	if d.cat > 0 {
		cat = d.cat
	}
	var txnID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO "transaction" (
			household_id, recorded_by_user_id, transaction_date, class,
				category_id, merchant_id, description, description_extra,
				total_amount, origin_account_id,
				record_status, source, notes,
				recurring_template_id
		) VALUES ($1, NULL, $2, 'expense', $3, $4, $5, '', $6,
			$7, 'pending', 'template', $8, $9)
		RETURNING id`,
		hh, date, cat, merch, d.name, d.amount, d.account,
		"Auto-generated from recurring template. Review and approve.", d.id).
		Scan(&txnID)
	if err != nil {
		if isUniqueViolation(err) {
			// Lost the race: a sibling worker committed this month's
			// occurrence first. The void-aware unique index is the
			// guarantee; the row lock is just the fast path.
			return errSiblingGenerated
		}
		return err
	}
	for i := range parts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO transaction_split (household_id, transaction_id, party_id, amount)
			VALUES ($1, $2, $3, $4)`, hh, txnID, partyIDs[i], shares[i]); err != nil {
			return err
		}
	}
	gdate, err := time.ParseInLocation("2006-01-02", date, time.UTC)
	if err != nil {
		return err
	}
	monthStart := time.Date(gdate.Year(), gdate.Month(), 1, 0, 0, 0, 0, time.UTC)
	if _, err := tx.Exec(ctx, `
		UPDATE recurring_template SET last_generated_date = $2
		WHERE id = $1 AND household_id = $3
		  AND (last_generated_date IS NULL OR last_generated_date < $2)`,
		d.id, monthStart, hh); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- pending-review queue (D9) ----------------------------------------------

// reviewLine is one row of the pending-review queue (D9) and of the D34
// bulk modal.
type reviewLine struct {
	ID       int64
	Date     time.Time
	Label    string
	Category string
	// Icon is the D32 category glyph key ("" = unpicked or no category).
	Icon       string
	CatID      int64
	NoCat      bool // D34: no category — the explicit marker (legacy rows)
	Synced     bool // D34: currently synced (is_synced) — the bulk modal lists these
	Total      money.Money
	Source     string
	RecordedBy string
}

// handleReview lists pending transactions — the monthly review surface
// (D9: entries get approved when the statement check happens).
func (a *App) handleReview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, perr := a.permFor(r)
	if perr != nil {
		http.Error(w, "permission load: "+perr.Error(), 500)
		return
	}
	a.generateDueRecurring(ctx, hh, time.Now())

	rows, err := a.Pool.Query(ctx, fmt.Sprintf(`
		SELECT t.id, t.transaction_date, COALESCE(t.description,
		       COALESCE(m.name, 'transaction')),
		       COALESCE(t.category_id, 0), COALESCE(c.name, ''), COALESCE(c.icon, ''),
		       t.total_amount, COALESCE(t.is_synced, false),
		       COALESCE(t.source, 'manual'),
		       COALESCE(u.display_name, '')
		FROM "transaction" t
		LEFT JOIN merchant m ON m.id = t.merchant_id
		LEFT JOIN category c ON c.id = t.category_id
		LEFT JOIN app_user u ON u.id = t.recorded_by_user_id
		WHERE t.household_id = $1 AND t.record_status = 'pending'
		  AND %s
		ORDER BY t.transaction_date DESC, t.id DESC`, txnVisibleWhere(p, 2, 1)), hh, sess.UserID)
	if err != nil {
		http.Error(w, "list pending: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	var list []reviewLine
	for rows.Next() {
		var l reviewLine
		var catID int64
		var catNameV string
		if err := rows.Scan(&l.ID, &l.Date, &l.Label, &catID, &catNameV, &l.Icon, &l.Total,
			&l.Synced, &l.Source, &l.RecordedBy); err != nil {
			http.Error(w, "scan pending: "+err.Error(), 500)
			return
		}
		l.CatID, l.Category, l.NoCat = catID, catNameV, catID == 0
		list = append(list, l)
	}
	a.render(w, r, 200, "review.html", map[string]any{
		"Title":   "Review pending",
		"Flash":   flashFrom(r),
		"Section": "review",
		"List":    list,
	})
}

// handleReviewBulk approves or voids the selected pending transactions in
// one POST. The same guards as the single-row status change apply: a row
// referenced by a payment allocation is skipped (and reported), never
// force-changed.
//
// D34 (§B): approving a pending row without a category is rejected until
// one is supplied. The BULK workflow (Q17 as revised): the category modal
// surfaces ONLY when the selection includes ≥1 currently-synced record
// (is_synced = true), and it lists ONLY those sync records (pre-filled
// editable, missing blank + required, no save-as-mapping). Non-sync records
// approve straight through (category unchanged); a non-sync row missing a
// category (legacy only) is SKIPPED + REPORTED and is approved individually
// via the single-row pop-up. Apply → the summary page; Confirm → one
// all-or-nothing commit (cancel anywhere = nothing commits; skips are
// reported, never silent). Void stays a safe reject (the one-way cascade
// does the rest); there is no "reject and keep pending" state.
func (a *App) handleReviewBulk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, perr := a.permFor(r)
	if perr != nil {
		http.Error(w, "permission load: "+perr.Error(), 500)
		return
	}
	target := r.FormValue("status")
	if !inSlice(target, txnStatuses) {
		http.Error(w, "bad status", 400)
		return
	}
	var ids []int64
	for _, s := range r.Form["id[]"] {
		if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		a.redirectReview(w, r, "Choose at least one transaction.")
		return
	}

	// Classify every selected id: row state (pending / synced / category),
	// payment locks (skipped, reported), and the Edit grant (skipped,
	// counted).
	type rowState struct {
		pending bool
		synced  bool
		catID   int64
		label   string
	}
	states := map[int64]rowState{}
	rows, err := a.Pool.Query(ctx, `
		SELECT t.id, t.record_status, COALESCE(t.is_synced, false),
		       COALESCE(t.category_id, 0),
		       COALESCE(t.description, COALESCE(m.name, 'transaction'))
		FROM "transaction" t
		LEFT JOIN merchant m ON m.id = t.merchant_id
		WHERE t.id = ANY($1) AND t.household_id = $2`, ids, hh)
	if err != nil {
		http.Error(w, "load selected: "+err.Error(), 500)
		return
	}
	for rows.Next() {
		var (
			id    int64
			st    string
			sync  bool
			cat   int64
			label string
		)
		if err := rows.Scan(&id, &st, &sync, &cat, &label); err != nil {
			rows.Close()
			http.Error(w, "load selected: "+err.Error(), 500)
			return
		}
		states[id] = rowState{pending: st == "pending", synced: sync, catID: cat, label: label}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		http.Error(w, "load selected: "+err.Error(), 500)
		return
	}
	rows.Close()

	// Which of these already have payment allocations?
	allocRows, err := a.Pool.Query(ctx, `
		SELECT s.transaction_id
		FROM settlement_allocation pa
		JOIN transaction_split s ON s.id = pa.split_id
		WHERE s.transaction_id = ANY($1)`, ids)
	if err != nil {
		http.Error(w, "check allocations: "+err.Error(), 500)
		return
	}
	locked := map[int64]bool{}
	for allocRows.Next() {
		var tid int64
		if err := allocRows.Scan(&tid); err != nil {
			allocRows.Close()
			http.Error(w, "check allocations: "+err.Error(), 500)
			return
		}
		locked[tid] = true
	}
	allocRows.Close()

	// Only pending rows may change status here (the queue is pending-only),
	// and only ones the caller holds an Edit grant on (Gate 5: a status move
	// is an edit).
	denied, lockedN := 0, 0
	var syncRows, straightRows, noCatRows []reviewLine
	for _, id := range ids {
		st, ok := states[id]
		if !ok || !st.pending {
			continue
		}
		if locked[id] {
			lockedN++
			continue
		}
		surface, inRel, err := a.txnAccess(ctx, hh, sess.UserID, id)
		if err != nil {
			http.Error(w, "load access: "+err.Error(), 500)
			return
		}
		if !p.canEditTxn(surface, inRel) {
			denied++
			continue
		}
		line := reviewLine{ID: id, Category: catName(ctx, a.Pool, hh, st.catID), CatID: st.catID,
			NoCat: st.catID == 0, Synced: st.synced, Label: st.label}
		switch {
		case st.synced:
			syncRows = append(syncRows, line)
		case st.catID == 0:
			noCatRows = append(noCatRows, line)
		default:
			straightRows = append(straightRows, line)
		}
	}

	// Void: the safe reject — straight commit of the eligible rows; the
	// one-way cascade reaches their synced copies.
	if target == "void" {
		var ok []int64
		for _, id := range ids {
			st, known := states[id]
			if known && st.pending && !locked[id] && canEditForBulk(ctx, a, hh, sess.UserID, id, p) {
				ok = append(ok, id)
			}
		}
		var n int64
		if len(ok) > 0 {
			res, err := a.Pool.Exec(ctx, `
				UPDATE "transaction" SET record_status = 'void'
				WHERE id = ANY($1) AND household_id = $2 AND record_status = 'pending'`,
				ok, hh)
			if err != nil {
				http.Error(w, "bulk status: "+err.Error(), 500)
				return
			}
			n = res.RowsAffected()
		}
		msg := fmt.Sprintf("%d of %d selected transactions voided.", n, len(ids))
		if lockedN > 0 {
			msg += fmt.Sprintf(" %d skipped — a payment references their splits.", lockedN)
		}
		if denied > 0 {
			msg += fmt.Sprintf(" %d skipped — no edit permission.", denied)
		}
		a.redirectReview(w, r, msg)
		return
	}

	// Approve.
	if r.FormValue("phase") != "confirm" {
		if len(syncRows) == 0 {
			// No synced record in the selection: straight commit of the
			// rows that carry a category; legacy no-category rows are
			// skipped + reported (approved individually on the record
			// page).
			_, err := a.reviewApproveTx(ctx, hh, sess.UserID, straightRows, nil)
			if errors.Is(err, pgx.ErrNoRows) {
				a.redirectReview(w, r, "One or more rows were no longer pending — nothing was committed.")
				return
			}
			if err != nil {
				http.Error(w, "bulk approve: "+err.Error(), 500)
				return
			}
			msg := fmt.Sprintf("%d of %d selected transactions approved.", len(straightRows), len(ids))
			if len(noCatRows) > 0 {
				msg += fmt.Sprintf(" %d skipped — no category (approve them individually on the record page).", len(noCatRows))
			}
			if lockedN > 0 {
				msg += fmt.Sprintf(" %d skipped — a payment references their splits.", lockedN)
			}
			if denied > 0 {
				msg += fmt.Sprintf(" %d skipped — no edit permission.", denied)
			}
			a.redirectReview(w, r, msg)
			return
		}
		// The modal: the summary of everything that WOULD commit (nothing
		// is committed yet — Cancel approves nothing).
		a.renderBulkModal(w, r, hh, "review_bulk_modal.html", map[string]any{
			"Title": "Approve selected records", "Err": "",
			"Cats":     a.syncCategoryOptions(ctx, hh),
			"SyncRows": syncRows, "StraightRows": straightRows, "Skipped": noCatRows,
			"LockedN": lockedN, "DeniedN": denied, "IDs": ids,
		})
		return
	}

	// Confirm: validate the sync rows' categories (required; the picker is
	// the household's active expense categories), then ONE all-or-nothing
	// commit of every row in the modal.
	catFor := map[int64]int64{}
	missing := 0
	for _, l := range syncRows {
		cat, _ := formInt64(r, "cat_"+strconv.FormatInt(l.ID, 10))
		if cat == 0 || !a.syncCatFits(ctx, hh, cat) {
			missing++
			continue
		}
		catFor[l.ID] = cat
	}
	if missing > 0 {
		a.renderBulkModal(w, r, hh, "review_bulk_modal.html", map[string]any{
			"Title":    "Approve selected records",
			"Err":      fmt.Sprintf("%d synced record(s) still need a category — pick one for each.", missing),
			"Cats":     a.syncCategoryOptions(ctx, hh),
			"SyncRows": syncRows, "StraightRows": straightRows, "Skipped": noCatRows,
			"LockedN": lockedN, "DeniedN": denied, "IDs": ids,
		})
		return
	}
	toCommit := append(append([]reviewLine{}, syncRows...), straightRows...)
	n, err := a.reviewApproveTx(ctx, hh, sess.UserID, toCommit, catFor)
	if errors.Is(err, pgx.ErrNoRows) {
		a.redirectReview(w, r, "One or more rows were no longer pending — nothing was committed.")
		return
	}
	if err != nil {
		http.Error(w, "bulk approve: "+err.Error(), 500)
		return
	}
	msg := fmt.Sprintf("%d selected transactions approved.", n)
	if len(noCatRows) > 0 {
		msg += fmt.Sprintf(" %d skipped — no category (approve them individually on the record page).", len(noCatRows))
	}
	if lockedN > 0 {
		msg += fmt.Sprintf(" %d skipped — a payment references their splits.", lockedN)
	}
	if denied > 0 {
		msg += fmt.Sprintf(" %d skipped — no edit permission.", denied)
	}
	a.redirectReview(w, r, msg)
}

// catName resolves a category id to its "Parent › child" label ("" for 0) —
// household-local.
func catName(ctx context.Context, pool *pgxpool.Pool, hh, catID int64) string {
	if catID == 0 {
		return ""
	}
	var name string
	err := pool.QueryRow(ctx, `
		SELECT CASE WHEN pc.name IS NOT NULL AND pc.name <> ''
		           THEN pc.name || ' › ' || c.name ELSE c.name END
		FROM category c LEFT JOIN category pc ON pc.id = c.parent_id
		WHERE c.id = $1 AND c.household_id = $2`, catID, hh).Scan(&name)
	if err != nil {
		return ""
	}
	return name
}

// reviewApproveTx commits the given pending rows to 'approved' in ONE
// all-or-nothing transaction (D34 §B): per row, the status flip (plus the
// category update for a sync row that got one) and a standard History row.
// Any row that stopped being pending fails the whole commit (the caller
// reports "nothing was committed"). catFor maps a row id to the category to
// apply (absent/0 = keep the row's own; only synced rows carry one).
func (a *App) reviewApproveTx(ctx context.Context, hh, actor int64, lines []reviewLine, catFor map[int64]int64) (int, error) {
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	for _, l := range lines {
		var upErr error
		if cat, has := catFor[l.ID]; has && cat > 0 {
			upErr = tx.QueryRow(ctx, `
				UPDATE "transaction"
				SET record_status = 'approved', category_id = $3
				WHERE id = $1 AND household_id = $2 AND record_status = 'pending'
				RETURNING id`, l.ID, hh, cat).Scan(&l.ID)
		} else {
			upErr = tx.QueryRow(ctx, `
				UPDATE "transaction"
				SET record_status = 'approved'
				WHERE id = $1 AND household_id = $2 AND record_status = 'pending'
				RETURNING id`, l.ID, hh).Scan(&l.ID)
		}
		if upErr != nil {
			return 0, upErr
		}
		summary := "status pending → approved"
		if cat, has := catFor[l.ID]; has && cat > 0 && cat != l.CatID {
			summary += " · category set to " + catName(ctx, a.Pool, hh, cat)
		}
		if err := a.writeTxnHistoryEvent(ctx, tx, hh, l.ID, "edited", summary, actor); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(lines), nil
}

func (a *App) redirectReview(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/review?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// renderBulkModal renders the D34 bulk-approval modal page (the summary of
// everything the Confirm would commit — nothing is committed by the GET).
func (a *App) renderBulkModal(w http.ResponseWriter, r *http.Request, hh int64, name string, data map[string]any) {
	data["Flash"] = flashFrom(r)
	data["Section"] = "review"
	a.render(w, r, 200, name, data)
}

// canEditForBulk reports the Edit grant on one record (the bulk-void path's
// eligibility check) — the same Gate 5 rule as the status endpoint.
func canEditForBulk(ctx context.Context, a *App, hh, user, id int64, p *perms) bool {
	surface, inRel, err := a.txnAccess(ctx, hh, user, id)
	if err != nil {
		return false
	}
	return p.canEditTxn(surface, inRel)
}
