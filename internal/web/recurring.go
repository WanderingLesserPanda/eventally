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
)

// RecurringLine is one row of the template list.
type RecurringLine struct {
	ID              int64
	Name            string
	Merchant        string
	Category        string
	Account         string
	Amount          money.Money
	DayOfMonth      int
	Participants    string // e.g. "tester, Partner" — display only
	Active          bool
	LastGenerated   *time.Time
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
	hh := sessionFromContext(r.Context()).Household
	a.generateDueRecurring(r.Context(), hh, time.Now())

	rows, err := a.Pool.Query(r.Context(), `
		SELECT t.id, t.name, COALESCE(m.name, ''), COALESCE(c.name, ''), a.name,
		       t.amount, t.day_of_month, t.is_active, t.last_generated_date, t.split_spec
		FROM recurring_template t
		LEFT JOIN merchant m ON m.id = t.merchant_id
		LEFT JOIN category c ON c.id = t.category_id
		JOIN account a ON a.id = t.account_id
		WHERE t.household_id = $1
		ORDER BY t.name`, hh)
	if err != nil {
		http.Error(w, "list recurring: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	// Participant display names, resolved once for all rows.
	nameByRef := map[string]string{}
	prs, err := a.Pool.Query(r.Context(), `
		SELECT 'user:'||id, display_name FROM app_user WHERE household_id = $1
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
			&l.Amount, &l.DayOfMonth, &l.Active, &l.LastGenerated, &specJSON); err != nil {
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
		list = append(list, l)
	}
	a.render(w, r, 200, "recurring.html", map[string]any{
		"Title":   "Recurring",
		"Flash":   flashFrom(r),
		"Section": "recurring",
		"List":    list,
	})
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

	Merchants   []opt
	Cats        []opt
	Accounts    []opt
	Users       []opt
	Parties     []opt
	DayOptions  []int
}

func (a *App) recurringFormOptions(ctx context.Context, hh int64) (*recurringForm, error) {
	f := &recurringForm{Active: true}
	for d := 1; d <= 28; d++ {
		f.DayOptions = append(f.DayOptions, d)
	}
	// Users (split participants).
	rows, err := a.Pool.Query(ctx,
		`SELECT id, display_name FROM app_user WHERE household_id = $1 ORDER BY id`, hh)
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
	hh := sessionFromContext(ctx).Household
	f, err := a.recurringFormOptions(ctx, hh)
	if err != nil {
		http.Error(w, "load options: "+err.Error(), 500)
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

func (a *App) saveRecurring(w http.ResponseWriter, r *http.Request, hh, id int64) {
	ctx := r.Context()
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
		var n int
		if err := a.Pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE id = $1 AND household_id = $2`, table),
			pid, hh).Scan(&n); err != nil || n == 0 {
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
				 amount, day_of_month, split_spec, is_active)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			hh, f.Name, merch, cat, f.AccountID, amount, day,
			specJSON, f.Active)
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
	hh := sessionFromContext(r.Context()).Household
	res, err := a.Pool.Exec(r.Context(),
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
		var n int
		if err := a.Pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE id = $1 AND household_id = $2`, table),
			pid, hh).Scan(&n); err != nil || n == 0 {
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

// handleReview lists pending transactions — the monthly review surface
// (D9: entries get approved when the statement check happens).
func (a *App) handleReview(w http.ResponseWriter, r *http.Request) {
	hh := sessionFromContext(r.Context()).Household
	a.generateDueRecurring(r.Context(), hh, time.Now())

	rows, err := a.Pool.Query(r.Context(), `
		SELECT t.id, t.transaction_date, COALESCE(t.description,
		       COALESCE(m.name, 'transaction')),
		       COALESCE(c.name, ''), t.total_amount,
		       COALESCE(t.source, 'manual'),
		       COALESCE(u.display_name, '')
		FROM "transaction" t
		LEFT JOIN merchant m ON m.id = t.merchant_id
		LEFT JOIN category c ON c.id = t.category_id
		LEFT JOIN app_user u ON u.id = t.recorded_by_user_id
		WHERE t.household_id = $1 AND t.record_status = 'pending'
		ORDER BY t.transaction_date DESC, t.id DESC`, hh)
	if err != nil {
		http.Error(w, "list pending: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	type reviewLine struct {
		ID           int64
		Date         time.Time
		Label        string
		Category     string
		Total        money.Money
		Source       string
		RecordedBy   string
	}
	var list []reviewLine
	for rows.Next() {
		var l reviewLine
		if err := rows.Scan(&l.ID, &l.Date, &l.Label, &l.Category, &l.Total,
			&l.Source, &l.RecordedBy); err != nil {
			http.Error(w, "scan pending: "+err.Error(), 500)
			return
		}
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
func (a *App) handleReviewBulk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
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

	// Which of these already have payment allocations?
	rows, err := a.Pool.Query(ctx, `
		SELECT s.transaction_id
		FROM settlement_allocation pa
		JOIN transaction_split s ON s.id = pa.split_id
		WHERE s.transaction_id = ANY($1)`, ids)
	if err != nil {
		http.Error(w, "check allocations: "+err.Error(), 500)
		return
	}
	locked := map[int64]bool{}
	for rows.Next() {
		var tid int64
		if err := rows.Scan(&tid); err != nil {
			rows.Close()
			http.Error(w, "check allocations: "+err.Error(), 500)
			return
		}
		locked[tid] = true
	}
	rows.Close()

	// Only pending rows may change status here (the queue is pending-only).
	var ok []int64
	for _, id := range ids {
		if !locked[id] {
			ok = append(ok, id)
		}
	}
	var n int64
	if len(ok) > 0 {
		res, err := a.Pool.Exec(ctx, `
			UPDATE "transaction" SET record_status = $1
			WHERE id = ANY($2) AND household_id = $3 AND record_status = 'pending'`,
			target, ok, hh)
		if err != nil {
			http.Error(w, "bulk status: "+err.Error(), 500)
			return
		}
		n = res.RowsAffected()
	}
	verb := "approved"
	if target == "void" {
		verb = "voided"
	}
	msg := fmt.Sprintf("%d of %d selected transactions %s.", n, len(ids), verb)
	if len(locked) > 0 {
		msg += fmt.Sprintf(" %d skipped — a payment references their splits.", len(locked))
	}
	a.redirectReview(w, r, msg)
}

func (a *App) redirectReview(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/review?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}
