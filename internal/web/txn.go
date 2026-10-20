package web

import (
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/money"
	"github.com/WanderingLesserPanda/eventally/internal/split"
)

// ---------------------------------------------------------------------------
// Phase P3: transactions.
//
//   - list with filters (date range, class, status, category, account, text)
//   - full form: class, date, merchant, category, total + FX fields,
//     origin/destination (account | merchant | party), and an N-way
//     transaction_split editor (equal / percent / exact amount; server-side
//     largest-remainder rounding via internal/split so shares always sum to
//     the total — D4)
//   - detail: facts, transaction_split table, status workflow (D9), receipt attach (D14)
//
// Conventions carried over from P2: household-scoped selects (active rows
// plus rows still referenced by history), flash messages via ?msg=, and
// COALESCE on every nullable column (NULL scanned into int64 panics).
// Polymorphic origin/destination refs are app-layer validated — the schema
// stores only type + ref.
// ---------------------------------------------------------------------------

var (
	txnClasses  = []string{"expense", "income", "transfer"}
	txnStatuses = []string{"pending", "approved", "void"}
)

// --- shared option lists ----------------------------------------------------

type opt struct {
	ID     int64
	Name   string
	Parent string // category only: parent's name, "" for top level
}

type merchantOpt struct {
	ID   int64
	Name string
	Cat  int64 // default_category_id, 0 when none
}

// refSel is one origin/destination dropdown pair: an account select and a
// party select. The endpoint is one account OR one party (never both); the
// template encodes the choice as "account:N" / "party:N" into a hidden
// field so parse stays uniform.
type refSel struct {
	Selected string // "account:3" / "party:7" / ""
	Accounts []opt
	Parties  []opt
}

// txnOptions is the shared option set behind the transaction form.
type txnOptions struct {
	Users     []opt
	Parties   []opt
	Merchants []merchantOpt
	Cats      []opt
	Accounts  []opt
}

func (a *App) txnFormOptions(ctx context.Context, hh int64) (*txnOptions, error) {
	o := &txnOptions{}
	refOrActive := "is_active OR id IN (SELECT %s FROM \"transaction\" WHERE household_id = $1 AND %s IS NOT NULL)"

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
		o.Users = append(o.Users, u)
	}
	rows.Close()

	// Parties (split participants: member rows + outsiders; D4).
	rows, err = a.Pool.Query(ctx, `
		SELECT id, name FROM party
		WHERE household_id = $1 AND (is_active
			OR id IN (SELECT party_id FROM transaction_split s
			         JOIN "transaction" t ON t.id = s.transaction_id
			         WHERE t.household_id = $1)
			OR id IN (SELECT origin_party_id FROM "transaction"
			         WHERE household_id = $1)
			OR id IN (SELECT destination_party_id FROM "transaction"
			         WHERE household_id = $1))
		ORDER BY name`, hh)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c opt
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			rows.Close()
			return nil, err
		}
		o.Parties = append(o.Parties, c)
	}
	rows.Close()

	// Merchants (with default category for prefill).
	rows, err = a.Pool.Query(ctx, `
		SELECT m.id, m.name, COALESCE(m.default_category_id, 0)
		FROM merchant m
		WHERE m.household_id = $1 AND (m.is_active
			OR m.id IN (SELECT merchant_id FROM "transaction"
			            WHERE household_id = $1 AND merchant_id IS NOT NULL))
		ORDER BY m.name`, hh)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m merchantOpt
		if err := rows.Scan(&m.ID, &m.Name, &m.Cat); err != nil {
			rows.Close()
			return nil, err
		}
		o.Merchants = append(o.Merchants, m)
	}
	rows.Close()

	// Categories (with parent name for the optgroup label).
	rows, err = a.Pool.Query(ctx, `
		SELECT c.id, c.name, COALESCE(p.name, '')
		FROM category c LEFT JOIN category p ON p.id = c.parent_id
		WHERE c.household_id = $1 AND (c.is_active
			OR c.id IN (SELECT category_id FROM "transaction"
			            WHERE household_id = $1 AND category_id IS NOT NULL))
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
		o.Cats = append(o.Cats, c)
	}
	rows.Close()
	_ = refOrActive // (documented; subqueries above are tailored per table)

	// Accounts (origin/destination choices — plus parties are shared).
	rows, err = a.Pool.Query(ctx, `
		SELECT id, name FROM account
		WHERE household_id = $1 AND (is_active
			OR id IN (SELECT origin_account_id FROM "transaction"
			         WHERE household_id = $1)
			OR id IN (SELECT destination_account_id FROM "transaction"
			         WHERE household_id = $1))
		ORDER BY name`, hh)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ac opt
		if err := rows.Scan(&ac.ID, &ac.Name); err != nil {
			rows.Close()
			return nil, err
		}
		o.Accounts = append(o.Accounts, ac)
	}
	rows.Close()
	return o, nil
}

func (o *txnOptions) fillRefSels(f *TxnForm) {
	f.OriginSel.Accounts, f.DestSel.Accounts = o.Accounts, o.Accounts
	f.OriginSel.Parties, f.DestSel.Parties = o.Parties, o.Parties
}

// --- form -------------------------------------------------------------------

// splitRow is one participant row of the transaction_split editor as posted back.
type splitRow struct {
	Key   string // "user:1" / "cp:4" / "" (not included)
	Value string // percent or amount, as typed (ignored in equal mode)
}

type TxnForm struct {
	Editing bool
	ID      int64
	Err     string
	Locked  bool // a settlement already references the splits -> cannot edit

	Class           string
	Date            string
	Total           string
	ChargedCurrency string
	AmountOriginal  string
	MerchantID      int64
	CategoryID      int64
	Description     string
	Notes           string
	Mode            string
	Rows            []splitRow
	ShareSum        string // amount mode: sum of typed shares

	Classes   []string
	Users     []opt
	Parties   []opt // split participants: member rows + outsiders
	Merchants []merchantOpt
	Cats      []opt
	OriginSel refSel
	DestSel   refSel
}

func (f *TxnForm) shareSumDisplay() string {
	sum := money.Money(0)
	for _, r := range f.Rows {
		if r.Key == "" {
			continue
		}
		if m, err := money.Parse(r.Value); err == nil {
			sum = sum.Add(m)
		}
	}
	if sum.IsZero() {
		return ""
	}
	return sum.String()
}

func (a *App) newTxnForm(ctx context.Context, hh int64) (*TxnForm, error) {
	f := &TxnForm{
		Class:   "expense",
		Date:    time.Now().Format("2006-01-02"),
		Mode:    "equal",
		Classes: txnClasses,
	}
	o, err := a.txnFormOptions(ctx, hh)
	if err != nil {
		return nil, err
	}
	f.Users, f.Parties = o.Users, o.Parties
	f.Merchants, f.Cats = o.Merchants, o.Cats
	o.fillRefSels(f)
	// One row per household user (the usual participants) plus one spare.
	for _, u := range o.Users {
		f.Rows = append(f.Rows, splitRow{Key: "user:" + strconv.FormatInt(u.ID, 10)})
	}
	f.Rows = append(f.Rows, splitRow{})
	return f, nil
}

// loadTxnForm builds a populated edit form. Editing is blocked (Locked) once
// a payment references this transaction's splits (P5 guard).
func (a *App) loadTxnForm(ctx context.Context, hh, id int64) (*TxnForm, error) {
	f, err := a.newTxnForm(ctx, hh)
	if err != nil {
		return nil, err
	}
	f.Editing = true
	f.ID = id

	var (
		t            txnDetail
		catsel, mers int64
		charged      string
		orig, rate   float64
	)
	var (
		origAcc   sql.NullInt64
		destAcc   sql.NullInt64
		origParty sql.NullInt64
		destParty sql.NullInt64
	)
	err = a.Pool.QueryRow(ctx, `
		SELECT t.id, t.transaction_date, t.class, t.description, t.notes,
		       t.total_amount, t.record_status, t.source,
		       COALESCE(t.category_id, 0), COALESCE(t.merchant_id, 0),
		       t.origin_account_id, t.destination_account_id,
		       t.origin_party_id, t.destination_party_id,
		       COALESCE(t.charged_currency, ''), COALESCE(t.amount_original, 0),
		       COALESCE(t.exchange_rate, 0)
		FROM "transaction" t
		WHERE t.id = $1 AND t.household_id = $2`, id, hh).
		Scan(&t.ID, &t.Date, &t.Class, &t.Description, &t.Notes, &t.Total,
			&t.Status, &t.Source, &catsel, &mers, &origAcc, &destAcc,
			&origParty, &destParty, &charged, &orig, &rate)
	if err != nil {
		return nil, err
	}

	f.Date = t.Date.Format("2006-01-02")
	f.Class = t.Class
	f.Total = t.Total.String()
	f.Description = t.Description
	f.Notes = t.Notes
	f.CategoryID = catsel
	f.MerchantID = mers
	f.ChargedCurrency = charged
	if orig != 0 {
		f.AmountOriginal = trimNum(orig)
	}
	if origAcc.Valid {
		f.OriginSel.Selected = "account:" + strconv.FormatInt(origAcc.Int64, 10)
	} else if origParty.Valid {
		f.OriginSel.Selected = "party:" + strconv.FormatInt(origParty.Int64, 10)
	}
	if destAcc.Valid {
		f.DestSel.Selected = "account:" + strconv.FormatInt(destAcc.Int64, 10)
	} else if destParty.Valid {
		f.DestSel.Selected = "party:" + strconv.FormatInt(destParty.Int64, 10)
	}

	// Existing splits pre-fill the editor in exact-amount mode: the safest
	// starting point for an edit (percent would silently re-round).
	f.Mode = "amount"
	f.Rows = nil
	rows, err := a.Pool.Query(ctx, `
		SELECT s.party_id, s.amount, COALESCE(p.app_user_id, 0)
		FROM transaction_split s
		LEFT JOIN party p ON p.id = s.party_id
		WHERE s.transaction_id = $1 ORDER BY s.id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var pid, uid int64
		var amt money.Money
		if err := rows.Scan(&pid, &amt, &uid); err != nil {
			rows.Close()
			return nil, err
		}
		// Members carry their USER id in the key ("user:N" resolves to the
		// member's party row at save time); outsiders carry the party id.
		var key string
		if uid > 0 {
			key = "user:" + strconv.FormatInt(uid, 10)
		} else {
			key = "cp:" + strconv.FormatInt(pid, 10)
		}
		f.Rows = append(f.Rows, splitRow{
			Key:   key,
			Value: amt.String(),
		})
	}
	rows.Close()
	if len(f.Rows) == 0 {
		for _, u := range f.Users {
			f.Rows = append(f.Rows, splitRow{Key: "user:" + strconv.FormatInt(u.ID, 10)})
		}
	}
	if f.Rows[len(f.Rows)-1].Key != "" {
		f.Rows = append(f.Rows, splitRow{})
	}
	f.ShareSum = f.shareSumDisplay()

	var n int
	_ = a.Pool.QueryRow(ctx, `
		SELECT count(*) FROM settlement_allocation pa
		JOIN transaction_split s ON s.id = pa.split_id
		WHERE s.transaction_id = $1`, id).Scan(&n)
	f.Locked = n > 0
	return f, nil
}

// --- parse ------------------------------------------------------------------

// txnspec is the fully parsed, validated form payload.
type txnspec struct {
	class       string
	date        string
	merchantID  int64
	categoryID  int64
	description string
	notes       string
	total       money.Money
	charged     string
	original    float64
	rate        float64 // original units per base unit (D13)
	originType  string
	originRef   int64
	destType    string
	destRef     int64
	parts       []split.Part
	shares      []money.Money
}

// parseTxnForm reads and validates the form, including the transaction_split editor.
// On failure it returns the re-renderable form plus a user message.
func parseTxnForm(r *http.Request) (*txnspec, *TxnForm, string) {
	f := &TxnForm{
		Class:           r.FormValue("class"),
		Date:            strings.TrimSpace(r.FormValue("date")),
		Total:           strings.TrimSpace(r.FormValue("total")),
		ChargedCurrency: strings.ToUpper(strings.TrimSpace(r.FormValue("charged_currency"))),
		AmountOriginal:  strings.TrimSpace(r.FormValue("amount_original")),
		Description:     strings.TrimSpace(r.FormValue("description")),
		Notes:           strings.TrimSpace(r.FormValue("notes")),
		Mode:            r.FormValue("split_mode"),
		OriginSel:       refSel{Selected: r.FormValue("origin_ref")},
		DestSel:         refSel{Selected: r.FormValue("destination_ref")},
	}
	if m := r.FormValue("merchant_id"); m != "" {
		f.MerchantID, _ = strconv.ParseInt(m, 10, 64)
	}
	if c := r.FormValue("category_id"); c != "" {
		f.CategoryID, _ = strconv.ParseInt(c, 10, 64)
	}

	keys := r.Form["part_key[]"]
	vals := r.Form["part_value[]"]
	if len(keys) != len(vals) {
		return nil, f, "Split form is malformed (participant rows out of sync)."
	}
	f.Rows = make([]splitRow, 0, len(keys))
	for i := range keys {
		f.Rows = append(f.Rows, splitRow{Key: keys[i], Value: vals[i]})
	}
	f.ShareSum = f.shareSumDisplay()

	s := &txnspec{
		class:       f.Class,
		date:        f.Date,
		merchantID:  f.MerchantID,
		categoryID:  f.CategoryID,
		description: f.Description,
		notes:       f.Notes,
	}
	if !inSlice(s.class, txnClasses) {
		return nil, f, "Class must be expense, income or transfer."
	}
	if !isDateStr(s.date) {
		return nil, f, "Date is required (YYYY-MM-DD)."
	}
	total, err := money.Parse(f.Total)
	if err != nil {
		return nil, f, "Total amount is invalid."
	}
	if total <= 0 {
		return nil, f, "Total amount must be greater than zero."
	}
	s.total = total

	if f.ChargedCurrency != "" {
		orig, err := strconv.ParseFloat(f.AmountOriginal, 64)
		if err != nil || orig <= 0 {
			return nil, f, "When a charged currency is set, the amount in that currency must be a positive number."
		}
		s.charged = f.ChargedCurrency
		s.original = orig
		s.rate = orig / (float64(total.AsCents()) / 100)
	}

	ot, oid, err := parseRef(f.OriginSel.Selected)
	if err != nil {
		return nil, f, "Choose an origin (where does this come from?). Account or party."
	}
	dt, did, err := parseRef(f.DestSel.Selected)
	if err != nil {
		return nil, f, "Choose a destination (where does it go?). Account or party."
	}
	if ot == dt && oid == did {
		return nil, f, "Origin and destination cannot be the same entry."
	}
	if s.class == "transfer" && (ot != "account" || dt != "account") {
		return nil, f, "A transfer moves money between two accounts."
	}
	s.originType, s.originRef = ot, oid
	s.destType, s.destRef = dt, did

	// Split participants: rows with a selected key.
	var parts []split.Part
	seen := map[string]bool{}
	for i, row := range f.Rows {
		if row.Key == "" {
			continue
		}
		if _, _, perr := parseParticipantKey(row.Key); perr != nil {
			return nil, f, "A participant row is invalid."
		}
		if seen[row.Key] {
			return nil, f, "A participant appears more than once in the split."
		}
		seen[row.Key] = true
		p := split.Part{Key: row.Key}
		v := strings.TrimSpace(row.Value)
		switch f.Mode {
		case "equal":
			p.Mode = split.ModePercent
			p.Percent = 100.0
		case "percent":
			if v == "" {
				return nil, f, fmt.Sprintf("Split row %d: enter a percentage.", i+1)
			}
			pv, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return nil, f, fmt.Sprintf("Split row %d: the percentage must be a number.", i+1)
			}
			p.Mode = split.ModePercent
			p.Percent = pv
		case "amount":
			if v == "" {
				return nil, f, fmt.Sprintf("Split row %d: enter an amount.", i+1)
			}
			am, err := money.Parse(v)
			if err != nil {
				return nil, f, fmt.Sprintf("Split row %d: the amount is invalid.", i+1)
			}
			if am <= 0 {
				return nil, f, fmt.Sprintf("Split row %d: the amount must be greater than zero.", i+1)
			}
			p.Mode = split.ModeAmount
			p.Amount = am
		default:
			return nil, f, "Split mode must be equal, percent or exact amount."
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return nil, f, "Choose at least one participant for the split."
	}
	switch f.Mode {
	case "equal":
		s.shares = split.EqualSplit(total, len(parts))
	default:
		var err error
		s.shares, err = split.Compute(total, parts)
		if err != nil {
			return nil, f, err.Error()
		}
	}
	s.parts = parts
	return s, f, ""
}

func inSlice(s string, list []string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// parseRef splits and validates an encoded "type:id" endpoint reference
// ("account:3" / "party:7"). The merchant endpoint died in Gate 3c — the
// merchant is a display attribute (merchant_id), never an endpoint.
func parseRef(v string) (string, int64, error) {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 || !inSlice(parts[0], []string{"account", "party"}) {
		return "", 0, fmt.Errorf("bad reference %q", v)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return "", 0, fmt.Errorf("bad reference %q", v)
	}
	return parts[0], id, nil
}

// parseParticipantKey splits a split-editor key ("user:N" / "cp:N") into a
// schema participant_type ("household_user"/"party") and ref.
func parseParticipantKey(key string) (string, int64, error) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("bad participant key %q", key)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return "", 0, fmt.Errorf("bad participant key %q", key)
	}
	switch parts[0] {
	case "user":
		return "household_user", id, nil
	case "cp":
		return "party", id, nil
	default:
		return "", 0, fmt.Errorf("bad participant key %q", key)
	}
}

// keysOf maps split parts to their editor keys.
func keysOf(parts []split.Part) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.Key
	}
	return out
}

// validateRefs checks that every referenced row exists and belongs to the
// household. The DB now has real FKs for accounts/parties; this is the
// pre-save UX layer (friendlier errors before the INSERT).
func (a *App) validateRefs(ctx context.Context, hh int64, s *txnspec) error {
	refOK := func(table string, id int64) bool {
		if id <= 0 {
			return true
		}
		var n int
		err := a.Pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE id = $1 AND household_id = $2`, table),
			id, hh).Scan(&n)
		return err == nil && n > 0
	}
	if !refOK("category", s.categoryID) {
		return fmt.Errorf("category not found")
	}
	if !refOK("merchant", s.merchantID) {
		return fmt.Errorf("merchant not found")
	}
	if !refOK(s.originType, s.originRef) {
		return fmt.Errorf("origin not found")
	}
	if !refOK(s.destType, s.destRef) {
		return fmt.Errorf("destination not found")
	}
	if s.originType == s.destType && s.originRef == s.destRef {
		return fmt.Errorf("origin and destination are the same entry")
	}
	for _, p := range s.parts {
		typ, id, err := parseParticipantKey(p.Key)
		if err != nil {
			return fmt.Errorf("bad participant %q", p.Key)
		}
		table := "party"
		if typ == "household_user" {
			table = `app_user`
		}
		if !refOK(table, id) {
			return fmt.Errorf("participant not found")
		}
	}
	return nil
}

// --- save -------------------------------------------------------------------

func (a *App) saveTxn(w http.ResponseWriter, r *http.Request, hh, id int64) {
	sess := sessionFromContext(r.Context())
	spec, f, perr := parseTxnForm(r)
	if perr != "" {
		a.reRenderForm(w, r, hh, id, f, perr)
		return
	}
	if err := a.validateRefs(r.Context(), hh, spec); err != nil {
		a.reRenderForm(w, r, hh, id, f, err.Error())
		return
	}

	// Guard: once a settlement has allocated against this transaction's
	// transaction_split rows, the splits can no longer be rewritten —
	// settlement_allocation holds the old transaction_split ids.
	if id > 0 {
		var n int
		if err := a.Pool.QueryRow(r.Context(), `
			SELECT count(*) FROM settlement_allocation pa
			JOIN transaction_split s ON s.id = pa.split_id
			WHERE s.transaction_id = $1`, id).Scan(&n); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		if n > 0 {
			http.Redirect(w, r, fmt.Sprintf("/txns/%d?msg=%s", id,
				url.QueryEscape("Cannot edit: a settlement already references this transaction's splits.")),
				http.StatusSeeOther)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	defer tx.Rollback(ctx)

	var cat, merch any
	if spec.categoryID > 0 {
		cat = spec.categoryID
	}
	if spec.merchantID > 0 {
		merch = spec.merchantID
	}
	var charged, orig, rate any
	if spec.charged != "" {
		charged, orig, rate = spec.charged, spec.original, spec.rate
	}

	// The four concrete endpoint columns: each side holds one kind.
	var originAcc, destAcc, originParty, destParty any
	switch spec.originType {
	case "account":
		originAcc = spec.originRef
	case "party":
		originParty = spec.originRef
	}
	switch spec.destType {
	case "account":
		destAcc = spec.destRef
	case "party":
		destParty = spec.destRef
	}

	// Split participants resolve to party ids up front (members -> their
	// seeded member party row; outsiders -> their party id).
	partyIDs, err := a.splitEditorKeys(r.Context(), hh, keysOf(spec.parts))
	if err != nil {
		a.reRenderForm(w, r, hh, id, f, err.Error())
		return
	}

	txnID := id
	if id == 0 {
		err = tx.QueryRow(ctx, `
			INSERT INTO "transaction" (
				household_id, recorded_by_user_id, transaction_date, class,
				category_id, merchant_id, description, description_extra,
				total_amount, charged_currency, amount_original, exchange_rate,
				origin_account_id, destination_account_id,
				origin_party_id, destination_party_id,
				record_status, source, notes
			) VALUES ($1, $2, $3, $4, $5, $6, $7, '', $8, $9, $10, $11,
				$12, $13, $14, $15, 'pending', 'manual', $16)
			RETURNING id`,
			hh, sess.UserID, spec.date, spec.class,
			cat, merch, spec.description, spec.total, charged, orig, rate,
			originAcc, destAcc, originParty, destParty,
			spec.notes).Scan(&txnID)
		if err != nil {
			http.Error(w, "create transaction: "+err.Error(), 500)
			return
		}
	} else {
		res, err := tx.Exec(ctx, `
			UPDATE "transaction" SET
				transaction_date = $2, class = $3,
				category_id = $4, merchant_id = $5, description = $6, notes = $7,
				total_amount = $8, charged_currency = $9, amount_original = $10,
				exchange_rate = $11,
				origin_account_id = $12, destination_account_id = $13,
				origin_party_id = $14, destination_party_id = $15
			WHERE id = $1 AND household_id = $16`,
			txnID, spec.date, spec.class, cat, merch, spec.description,
			spec.notes, spec.total, charged, orig, rate,
			originAcc, destAcc, originParty, destParty, hh)
		if err != nil {
			http.Error(w, "update transaction: "+err.Error(), 500)
			return
		}
		if res.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
	}

	// Splits: replace wholesale. The deferred split-sum trigger (migration
	// 001) re-checks parent consistency at commit.
	if _, err := tx.Exec(ctx, `DELETE FROM transaction_split WHERE transaction_id = $1`, txnID); err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	for i := range spec.parts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO transaction_split (household_id, transaction_id, party_id, amount)
			VALUES ($1, $2, $3, $4)`,
			hh, txnID, partyIDs[i], spec.shares[i]); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "could not save transaction: "+err.Error(), 500)
		return
	}

	msg := "Transaction saved."
	if id == 0 {
		msg = "Transaction created."
	}
	http.Redirect(w, r, fmt.Sprintf("/txns/%d?msg=%s", txnID, url.QueryEscape(msg)),
		http.StatusSeeOther)
}

// reRenderForm re-renders the form with the posted values, the full option
// lists (so the selects still render) and the error.
func (a *App) reRenderForm(w http.ResponseWriter, r *http.Request, hh, id int64, f *TxnForm, msg string) {
	o, err := a.txnFormOptions(r.Context(), hh)
	if err != nil {
		http.Error(w, "load options: "+err.Error(), 500)
		return
	}
	f.Editing = id > 0
	f.ID = id
	f.Mode = strings.TrimSpace(r.FormValue("split_mode"))
	f.Classes = txnClasses
	f.Users, f.Parties = o.Users, o.Parties
	f.Merchants, f.Cats = o.Merchants, o.Cats
	o.fillRefSels(f)
	f.ShareSum = f.shareSumDisplay()
	f.Err = msg
	title := "New transaction"
	if f.Editing {
		title = "Edit transaction"
	}
	a.render(w, r, http.StatusOK, "txn_form.html", map[string]any{
		"Title": title,
		"F":     f,
	})
}

// --- list -------------------------------------------------------------------

// txnLine is one row of the transactions list.
type txnLine struct {
	ID        int64
	Date      time.Time
	Label     string
	Category  string
	Total     money.Money
	Charged   string // e.g. "JPY", "" when base currency
	Original  string // e.g. "400 JPY" when Charged != ""
	FromTo    string
	Status    string
}

type listFilters struct {
	From, To, Class, Status, Q string
	CategoryID, AccountID      int64
}

func (a *App) listTxns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	q := r.URL.Query()
	f := listFilters{
		From:   q.Get("from"),
		To:     q.Get("to"),
		Class:  q.Get("class"),
		Status: q.Get("status"),
		Q:      q.Get("q"),
	}
	if s := q.Get("category"); s != "" {
		f.CategoryID, _ = strconv.ParseInt(s, 10, 64)
	}
	if s := q.Get("account"); s != "" {
		f.AccountID, _ = strconv.ParseInt(s, 10, 64)
	}

	conds := []string{"t.household_id = $1"}
	args := []any{hh}
	addCond := func(sql string, arg any) {
		args = append(args, arg)
		conds = append(conds, fmt.Sprintf(sql, len(args)))
	}
	if f.From != "" && isDateStr(f.From) {
		addCond("t.transaction_date >= $%d", f.From)
	}
	if f.To != "" && isDateStr(f.To) {
		addCond("t.transaction_date <= $%d", f.To)
	}
	if f.Class != "" {
		addCond("t.class = $%d", f.Class)
	}
	if f.Status != "" {
		addCond("t.record_status = $%d", f.Status)
	}
	if f.CategoryID > 0 {
		addCond("t.category_id = $%d", f.CategoryID)
	}
	if f.AccountID > 0 {
		args = append(args, f.AccountID, f.AccountID)
		conds = append(conds, fmt.Sprintf(
			"(t.origin_account_id = $%d) OR (t.destination_account_id = $%d)",
			len(args)-1, len(args)))
	}
	if f.Q != "" {
		like := "%" + f.Q + "%"
		args = append(args, like, like)
		conds = append(conds, fmt.Sprintf(
			"(t.description ILIKE $%d OR m.name ILIKE $%d)", len(args)-1, len(args)))
	}

	rows, err := a.Pool.Query(ctx, `
		SELECT t.id, t.transaction_date, t.description, t.total_amount,
		       COALESCE(t.charged_currency, ''), COALESCE(t.amount_original, 0),
		       t.record_status, COALESCE(c.name, ''), COALESCE(m.name, ''),
		       COALESCE(oa.name, COALESCE(oc.name, COALESCE(m.name, ''))),
		       COALESCE(da.name, COALESCE(dc.name, ''))
		FROM "transaction" t
		LEFT JOIN category c      ON c.id = t.category_id
		LEFT JOIN merchant m      ON m.id = t.merchant_id
		LEFT JOIN account oa ON oa.id = t.origin_account_id
		LEFT JOIN party oc       ON oc.id = t.origin_party_id
		LEFT JOIN account da ON da.id = t.destination_account_id
		LEFT JOIN party dc       ON dc.id = t.destination_party_id
		WHERE ` + strings.Join(conds, " AND ") + `
		ORDER BY t.transaction_date DESC, t.id DESC
		LIMIT 200`, args...)
	if err != nil {
		http.Error(w, "list transactions: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	var list []txnLine
	for rows.Next() {
		var (
			l     txnLine
			desc  string
			merch string
			orig  float64
			ot, dt string
			_ = ot
		)
		if err := rows.Scan(&l.ID, &l.Date, &desc, &l.Total, &l.Charged, &orig,
			&l.Status, &l.Category, &merch, &ot, &dt); err != nil {
			http.Error(w, "scan transaction: "+err.Error(), 500)
			return
		}
		l.Label = desc
		if l.Label == "" {
			l.Label = merch
		}
		if l.Charged != "" {
			l.Original = trimNum(orig) + " " + l.Charged
		}
		from, to := strings.TrimSpace(ot), strings.TrimSpace(dt)
		if from != "" || to != "" {
			if from == "" {
				from = "—"
			}
			if to == "" {
				to = "—"
			}
			l.FromTo = from + " → " + to
		}
		list = append(list, l)
	}

	a.render(w, r, http.StatusOK, "txns.html", map[string]any{
		"Title":    "Transactions",
		"Flash":    flashFrom(r),
		"List":     list,
		"F":        f,
		"Statuses": txnStatuses,
		"Classes":  txnClasses,
		"Cats":     a.activeCategoryOptions(ctx, hh),
		"Accts":    a.activeAccountOptions(ctx, hh),
	})
}

// activeCategoryOptions / activeAccountOptions feed the filter bar.
func (a *App) activeCategoryOptions(ctx context.Context, hh int64) []opt {
	var out []opt
	rows, err := a.Pool.Query(ctx,
		`SELECT id, name FROM category WHERE household_id = $1 AND is_active
		 ORDER BY sort_order, name`, hh)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c opt
		_ = rows.Scan(&c.ID, &c.Name)
		out = append(out, c)
	}
	return out
}

func (a *App) activeAccountOptions(ctx context.Context, hh int64) []opt {
	var out []opt
	rows, err := a.Pool.Query(ctx,
		`SELECT id, name FROM account WHERE household_id = $1 AND is_active ORDER BY name`, hh)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c opt
		_ = rows.Scan(&c.ID, &c.Name)
		out = append(out, c)
	}
	return out
}

// --- handlers: form ---------------------------------------------------------

func (a *App) handleTxnCreate(w http.ResponseWriter, r *http.Request) {
	hh := sessionFromContext(r.Context()).Household
	if r.Method == http.MethodGet {
		f, err := a.newTxnForm(r.Context(), hh)
		if err != nil {
			http.Error(w, "load options: "+err.Error(), 500)
			return
		}
		// Optional prefill: /txns/new?merchant_id=N also preselects the
		// merchant's default category.
		if s := r.URL.Query().Get("merchant_id"); s != "" {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil && id > 0 {
				f.MerchantID = id
				for _, m := range f.Merchants {
					if m.ID == id && m.Cat > 0 {
						f.CategoryID = m.Cat
					}
				}
			}
		}
		a.render(w, r, http.StatusOK, "txn_form.html", map[string]any{
			"Title": "New transaction",
			"F":     f,
		})
		return
	}
	a.saveTxn(w, r, hh, 0)
}

func (a *App) handleTxnEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := a.loadTxnForm(r.Context(), sessionFromContext(r.Context()).Household, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.render(w, r, http.StatusOK, "txn_form.html", map[string]any{
		"Title": "Edit transaction",
		"F":     f,
	})
}

func (a *App) handleTxnEditPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.saveTxn(w, r, sessionFromContext(r.Context()).Household, id)
}

// --- handlers: detail + status ---------------------------------------------

type splitLine struct {
	Participant string
	Amount      money.Money
	Pct         string
}

type txnDetail struct {
	ID           int64
	Date         time.Time
	Class        string
	Description  string
	Merchant     string
	Category     string
	Notes        string
	Total        money.Money
	Charged      string
	Original     string
	Rate         string
	OriginLabel  string
	DestLabel    string
	Status       string
	Source       string
	RecordedBy   string
	Splits       []splitLine
	HasAllocs    bool
	Settlements  []settlementDetailLine
	ReceiptPath  string
}

// settlementDetailLine is one settlement allocated against this
// transaction's splits (the detail page's "Settlements" section).
type settlementDetailLine struct {
	Date   time.Time
	Payer  string
	Amount money.Money
}

func (a *App) handleTxnDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household

	var t txnDetail
	var (
		catsel, mers int64
		charged      string
		orig, rate   float64
		merch, cat   string
	)
	err := a.Pool.QueryRow(ctx, `
		SELECT t.id, t.transaction_date, t.class, t.description, t.notes,
		       t.total_amount, t.record_status, t.source,
		       COALESCE(u.display_name, ''),
		       COALESCE(oa.name, COALESCE(oc.name, COALESCE(m2.name, ''))),
		       COALESCE(da.name, COALESCE(dc.name, '')),
		       COALESCE(t.charged_currency, ''), COALESCE(t.amount_original, 0),
		       COALESCE(t.exchange_rate, 0), COALESCE(t.merchant_id, 0), COALESCE(t.category_id, 0),
		       COALESCE(t.receipt_path, ''),
		       COALESCE(m2.name, ''), COALESCE(c2.name, '')
		FROM "transaction" t
		LEFT JOIN app_user u ON u.id = t.recorded_by_user_id
		LEFT JOIN account oa ON oa.id = t.origin_account_id
		LEFT JOIN party oc       ON oc.id = t.origin_party_id
		LEFT JOIN account da ON da.id = t.destination_account_id
		LEFT JOIN party dc       ON dc.id = t.destination_party_id
		LEFT JOIN merchant m2 ON m2.id = t.merchant_id
		LEFT JOIN category c2 ON c2.id = t.category_id
		WHERE t.id = $1 AND t.household_id = $2`, id, hh).
		Scan(&t.ID, &t.Date, &t.Class, &t.Description, &t.Notes, &t.Total,
			&t.Status, &t.Source, &t.RecordedBy,
			&t.OriginLabel, &t.DestLabel,
			&charged, &orig, &rate, &mers, &catsel, &t.ReceiptPath,
			&merch, &cat)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	t.Merchant, t.Category = merch, cat
	t.Charged = charged
	if orig != 0 {
		t.Original = trimNum(orig) + " " + charged
	}
	if rate != 0 {
		t.Rate = trimNum(rate) + " " + charged + " per 1 base unit"
	}
	_ = catsel

	// Splits.
	rows, err := a.Pool.Query(ctx, `
		SELECT s.amount, COALESCE(p.name, 'Unknown participant')
		FROM transaction_split s
		LEFT JOIN party p ON p.id = s.party_id
		WHERE s.transaction_id = $1
		ORDER BY COALESCE(p.app_user_id, 0) = 0, s.id`, id)
	if err != nil {
		http.Error(w, "load splits: "+err.Error(), 500)
		return
	}
	for rows.Next() {
		var sl splitLine
		if err := rows.Scan(&sl.Amount, &sl.Participant); err != nil {
			rows.Close()
			http.Error(w, "scan split: "+err.Error(), 500)
			return
		}
		if t.Total.AsCents() > 0 {
			sl.Pct = fmt.Sprintf("%.2f%%", float64(sl.Amount.AsCents())/float64(t.Total.AsCents())*100)
		}
		t.Splits = append(t.Splits, sl)
	}
	rows.Close()

	var an int
	_ = a.Pool.QueryRow(ctx, `
		SELECT count(*) FROM settlement_allocation pa
		JOIN transaction_split s ON s.id = pa.split_id
		WHERE s.transaction_id = $1`, id).Scan(&an)
	t.HasAllocs = an > 0

	// Settlements allocated against this transaction's splits (the new
	// "Settlements" section: transaction -> its settlements).
	t.Settlements = nil
	srows, err := a.Pool.Query(ctx, `
		SELECT p.payment_date, COALESCE(u.display_name, '—'), SUM(pa.amount)
		FROM settlement_allocation pa
		JOIN settlement p ON p.id = pa.payment_id
		JOIN transaction_split s ON s.id = pa.split_id
		LEFT JOIN app_user u ON u.id = p.paid_by_user_id
		WHERE s.transaction_id = $1
		GROUP BY p.payment_date, p.id, u.display_name
		ORDER BY p.payment_date, p.id`, id)
	if err != nil {
		http.Error(w, "load settlements: "+err.Error(), 500)
		return
	}
	for srows.Next() {
		var sl settlementDetailLine
		if err := srows.Scan(&sl.Date, &sl.Payer, &sl.Amount); err != nil {
			srows.Close()
			http.Error(w, "scan settlement: "+err.Error(), 500)
			return
		}
		t.Settlements = append(t.Settlements, sl)
	}
	srows.Close()

	a.render(w, r, http.StatusOK, "txn_detail.html", map[string]any{
		"Title":    "Transaction",
		"Flash":    flashFrom(r),
		"T":        t,
		"Statuses": txnStatuses,
	})
}

func (a *App) handleTxnStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	target := r.FormValue("status")
	if !inSlice(target, txnStatuses) {
		http.Error(w, "bad status", 400)
		return
	}
	var cur string
	err := a.Pool.QueryRow(ctx,
		`SELECT record_status FROM "transaction" WHERE id = $1 AND household_id = $2`,
		id, hh).Scan(&cur)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if cur == "void" {
		http.Redirect(w, r, fmt.Sprintf("/txns/%d?msg=%s", id,
			url.QueryEscape("A voided transaction cannot be changed.")), http.StatusSeeOther)
		return
	}
	if target != "void" {
		var n int
		_ = a.Pool.QueryRow(ctx, `
			SELECT count(*) FROM settlement_allocation pa
			JOIN transaction_split s ON s.id = pa.split_id
			WHERE s.transaction_id = $1`, id).Scan(&n)
		if n > 0 {
			http.Redirect(w, r, fmt.Sprintf("/txns/%d?msg=%s", id,
				url.QueryEscape("Cannot change status: a payment already references this transaction's splits.")),
				http.StatusSeeOther)
			return
		}
	}
	if _, err := a.Pool.Exec(ctx,
		`UPDATE "transaction" SET record_status = $3 WHERE id = $1 AND household_id = $2`,
		id, hh, target); err != nil {
		http.Error(w, "update status: "+err.Error(), 500)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/txns/%d?msg=%s", id,
		url.QueryEscape("Status set to "+target+".")), http.StatusSeeOther)
}

// --- receipts (D14: upload-only, served by direct link) ---------------------

var receiptTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".gif":  "image/gif",
	".pdf":  "application/pdf",
}

func sanitizeReceiptName(name string) string {
	base := filepath.Base(name)
	if base == "." || base == "" {
		base = "receipt"
	}
	var b strings.Builder
	for _, r := range base {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			continue
		}
		b.WriteRune(r)
	}
	name = b.String()
	if name == "" {
		name = "receipt"
	}
	// Insert the uniqueness suffix before the extension so the stored
	// filename keeps it (serveReceipts derives the Content-Type from it).
	ext := filepath.Ext(name)
	base = strings.TrimSuffix(name, ext)
	if len(base) > 95 { // keep base+suffix+ext under the 100-char limit
		base = base[:95]
	}
	return base + "_" + strconv.FormatInt(time.Now().Unix(), 36) + ext
}

func (a *App) handleTxnReceiptUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	hh := sessionFromContext(r.Context()).Household
	// Confirm the transaction exists first (404 before parsing the body).
	var n int
	if err := a.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM "transaction" WHERE id = $1 AND household_id = $2`, id, hh).Scan(&n); err != nil || n == 0 {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseMultipartForm(11 << 20); err != nil {
		http.Error(w, "file too large or malformed", 413)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file uploaded", 400)
		return
	}
	defer file.Close()
	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if _, ok := receiptTypes[ext]; !ok {
		http.Redirect(w, r, fmt.Sprintf("/txns/%d?msg=%s", id,
			url.QueryEscape("Receipt must be a JPG, PNG, WebP, GIF or PDF.")), http.StatusSeeOther)
		return
	}
	if hdr.Size > 10<<20 {
		http.Redirect(w, r, fmt.Sprintf("/txns/%d?msg=%s", id,
			url.QueryEscape("Receipt must be at most 10 MB.")), http.StatusSeeOther)
		return
	}
	name := sanitizeReceiptName(hdr.Filename)
	// receipt_path stores the bare filename; the file itself lives under
	// DataDir/receipts/ and is served at /receipts/<name>.
	rel := name
	if err := os.MkdirAll(filepath.Join(a.DataDir, "receipts"), 0o755); err != nil {
		log.Printf("receipt dir: %v", err)
		http.Error(w, "could not store file", 500)
		return
	}
	dst, err := os.Create(filepath.Join(a.DataDir, "receipts", rel))
	if err != nil {
		log.Printf("receipt create: %v", err)
		http.Error(w, "could not store file", 500)
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		log.Printf("receipt write: %v", err)
		http.Error(w, "could not store file", 500)
		return
	}
	if _, err := a.Pool.Exec(r.Context(),
		`UPDATE "transaction" SET receipt_path = $3 WHERE id = $1 AND household_id = $2`,
		id, hh, rel); err != nil {
		http.Error(w, "update receipt: "+err.Error(), 500)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/txns/%d?msg=%s", id,
		url.QueryEscape("Receipt attached.")), http.StatusSeeOther)
}

func (a *App) handleTxnReceiptDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	hh := sessionFromContext(r.Context()).Household
	var rp string
	err := a.Pool.QueryRow(r.Context(),
		`SELECT COALESCE(receipt_path, '') FROM "transaction" WHERE id = $1 AND household_id = $2`,
		id, hh).Scan(&rp)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if rp != "" {
		full := filepath.Join(a.DataDir, "receipts", rp)
		// Only delete within the data dir.
		if strings.HasPrefix(full, filepath.Clean(a.DataDir)+string(filepath.Separator)) {
			_ = os.Remove(full)
		}
	}
	if _, err := a.Pool.Exec(r.Context(),
		`UPDATE "transaction" SET receipt_path = NULL WHERE id = $1 AND household_id = $2`,
		id, hh); err != nil {
		http.Error(w, "delete receipt: "+err.Error(), 500)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/txns/%d?msg=%s", id,
		url.QueryEscape("Receipt removed.")), http.StatusSeeOther)
}

// serveReceipts streams uploaded receipts by direct link (D14).
func (a *App) serveReceipts(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/receipts/")
	if p == "" || strings.Contains(p, "..") || strings.Contains(p, "/") {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(filepath.Ext(p))
	if ct, ok := receiptTypes[ext]; ok {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", mime.TypeByExtension(ext))
	}
	http.ServeFile(w, r, filepath.Join(a.DataDir, "receipts", p))
}

// trimNum renders a float with up to 6 decimals, trimming trailing zeros
// (NUMERIC(16,4)/(18,6) come back as e.g. 38.760000 -> "38.76").
func trimNum(f float64) string {
	s := strconv.FormatFloat(f, 'f', 6, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}
