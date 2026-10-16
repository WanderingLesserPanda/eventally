package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/money"
)

// ---------------------------------------------------------------------------
// Phase P5: card view + payments with allocations (D5, D6).
//
//   - /cards: every credit card account with outstanding (unpaid expense
//     splits) and the computed recorded balance.
//   - /cards/{id}: outstanding per person, the unpaid split list (multi-select
//     for lump-sum payments, D5), and the payment history with allocations.
//   - /cards/{id}/pay: record a payment. Amount + date + method + paid_by,
//     allocation inputs pre-filled with each selected split's outstanding.
//     Optionally (D6) the same action writes the ledger transfer
//     savings → card so the recorded card balance stays in step with the
//     real one; the transfer is a split-less, approved balance movement and
//     is linked back via payment.ledger_transaction_id.
//
// Outstanding per split = split.amount − SUM(allocations) (D5: negative
// means overpaid, carries forward). Only class='expense' splits are
// payable; income/refund items are card credits (balance movements, not
// debt). record_status 'pending' still counts — approval happens at the
// monthly statement check (D9).
// ---------------------------------------------------------------------------

var payMethods = []string{"transfer", "online", "mobile", "cash", "other"}

// cardLine is one row of the /cards list.
type cardLine struct {
	ID          int64
	Name        string
	Institution string
	CardType    string
	CreditLimit money.Money
	Outstanding money.Money
	Recorded    money.Money
}

// byPerson is one row of the per-person outstanding summary.
type byPerson struct {
	Person      string
	UserID      int64 // >0 for household users (drives the Pay link)
	Share       money.Money
	Due         money.Money
}

// unpaidLine is one payable split row.
type unpaidLine struct {
	SplitID     int64
	Date        time.Time
	Label       string
	Participant string
	Status      string
	Share       money.Money
	Paid        money.Money
	Due         money.Money
}

// payLine is one payment-history row (allocations attached below).
type payLine struct {
	ID        int64
	Date      time.Time
	Amount    money.Money
	Method    string
	PaidBy    string
	Notes     string
	LedgerID  int64
	Allocs    []allocLine
}

type allocLine struct {
	Date        time.Time
	Label       string
	Participant string
	Amount      money.Money
}

// cardDetailData is the /cards/{id} page payload.
type cardDetailData struct {
	ID          int64
	Name        string
	Institution string
	CardType    string
	CreditLimit money.Money
	Outstanding money.Money
	Recorded    money.Money
	People      []byPerson
	Unpaid      []unpaidLine
	Payments    []payLine
}

// payRow is one allocation row of the pay form: the payable split plus the
// amount allocated to it as typed ("" on a fresh GET means "the whole
// outstanding" — the template pre-fills the input with Due).
type payRow struct {
	unpaidLine
	Alloc string
}

// cardPayForm is the /cards/{id}/pay form (GET and re-render).
type cardPayForm struct {
	Err         string
	CardID      int64
	CardName    string
	Amount      string
	Date        string
	Method      string
	PaidBy      int64
	Ledger      bool
	SourceID    int64
	Notes       string
	Rows        []payRow
	AllocSum    money.Money
	Users       []opt
	Sources     []opt // asset accounts (D6 source)
	Methods     []string
	NothingDue  bool
}

// --- shared queries ---------------------------------------------------------

// cardAccountRow loads one credit-card account scoped to the household.
func (a *App) cardAccountRow(ctx context.Context, hh, id int64) (int64, string, string, string, money.Money, error) {
	var (
		acID          int64
		name          string
		institution   string
		cardType      string
		creditLimit   money.Money
	)
	err := a.Pool.QueryRow(ctx, `
		SELECT id, name, institution, COALESCE(card_type, ''), COALESCE(credit_limit, 0)
		FROM account
		WHERE id = $1 AND household_id = $2 AND account_type = 'credit_card'`, id, hh).
		Scan(&acID, &name, &institution, &cardType, &creditLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", "", "", money.Money(0), sql.ErrNoRows
	}
	if err != nil {
		return 0, "", "", "", money.Money(0), err
	}
	return acID, name, institution, cardType, creditLimit, nil
}

// cardOutstanding is the sum of unallocated expense splits on the card
// (pending + approved, never void, never transfer — transfers have no splits).
const cardUnpaidWhere = `t.household_id = $1 AND t.record_status <> 'void'
	AND t.class = 'expense' AND (
	  (t.origin_type = 'account' AND t.origin_ref = $2)
	  OR (t.destination_type = 'account' AND t.destination_ref = $2))`

func (a *App) cardOutstanding(ctx context.Context, hh, card int64) (money.Money, error) {
	var out money.Money
	err := a.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(s.amount - COALESCE(al.a, 0)), 0)
		FROM split s
		JOIN "transaction" t ON t.id = s.transaction_id
		LEFT JOIN (SELECT split_id, SUM(amount) AS a
			FROM payment_allocation GROUP BY split_id) al ON al.split_id = s.id
		WHERE `+cardUnpaidWhere, hh, card).Scan(&out)
	return out, err
}

// cardRecordedBalance computes the card's ledger liability:
//
//	opening + charges (card as origin) − credits (card as destination)
//	        − payments without a ledger link (D6: cash settled out-of-band)
//
// Payments that wrote a D6 transfer are already reflected in the
// destination=card total, so they are excluded from the payment sum.
func (a *App) cardRecordedBalance(ctx context.Context, hh, card int64) (money.Money, error) {
	var bal money.Money
	err := a.Pool.QueryRow(ctx, `
		SELECT COALESCE(a.opening_balance, 0)
		 + COALESCE((SELECT SUM(t.total_amount) FROM "transaction" t
		              WHERE t.household_id = $1 AND t.record_status <> 'void'
		        AND t.origin_type = 'account' AND t.origin_ref = $2), 0)
		 - COALESCE((SELECT SUM(t.total_amount) FROM "transaction" t
		              WHERE t.household_id = $1 AND t.record_status <> 'void'
		        AND t.destination_type = 'account' AND t.destination_ref = $2), 0)
		 - COALESCE((SELECT SUM(p.amount) FROM payment p
		              WHERE p.household_id = $1 AND p.account_id = $2
		        AND p.ledger_transaction_id IS NULL), 0)
		FROM account a WHERE a.id = $2 AND a.household_id = $1`, hh, card).Scan(&bal)
	return bal, err
}

// cardUnpaidLines lists the card's payable splits with outstanding > 0.
func (a *App) cardUnpaidLines(ctx context.Context, hh, card int64) ([]unpaidLine, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT s.id, t.transaction_date,
		       COALESCE(NULLIF(t.description, ''), m.name, '—'),
		       COALESCE(CASE s.participant_type
		                  WHEN 'household_user' THEN u.display_name
		                  ELSE cp.name END, 'Unknown'),
		       t.record_status, s.amount,
		       COALESCE(al.a, 0)
		FROM split s
		JOIN "transaction" t ON t.id = s.transaction_id
		LEFT JOIN merchant m ON m.id = t.merchant_id
		LEFT JOIN "user" u ON s.participant_type = 'household_user' AND u.id = s.participant_ref
		LEFT JOIN counterparty cp ON s.participant_type = 'counterparty' AND cp.id = s.participant_ref
		LEFT JOIN (SELECT split_id, SUM(amount) AS a
			FROM payment_allocation GROUP BY split_id) al ON al.split_id = s.id
		WHERE `+cardUnpaidWhere+`
		AND s.amount - COALESCE(al.a, 0) > 0
		ORDER BY t.transaction_date, s.id`, hh, card)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []unpaidLine
	for rows.Next() {
		var l unpaidLine
		var paid money.Money
		if err := rows.Scan(&l.SplitID, &l.Date, &l.Label, &l.Participant,
			&l.Status, &l.Share, &paid); err != nil {
			return nil, err
		}
		l.Paid = paid
		l.Due = l.Share.Sub(paid)
		out = append(out, l)
	}
	return out, nil
}

// cardByPerson summarises outstanding per participant.
func (a *App) cardByPerson(ctx context.Context, hh, card int64) ([]byPerson, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT COALESCE(CASE s.participant_type
		                  WHEN 'household_user' THEN u.display_name
		                  ELSE cp.name END, 'Unknown'),
		       CASE WHEN s.participant_type = 'household_user'
		            THEN s.participant_ref ELSE 0 END,
		       SUM(s.amount),
		       SUM(s.amount - COALESCE(al.a, 0))
		FROM split s
		JOIN "transaction" t ON t.id = s.transaction_id
		LEFT JOIN "user" u ON s.participant_type = 'household_user' AND u.id = s.participant_ref
		LEFT JOIN counterparty cp ON s.participant_type = 'counterparty' AND cp.id = s.participant_ref
		LEFT JOIN (SELECT split_id, SUM(amount) AS a
			FROM payment_allocation GROUP BY split_id) al ON al.split_id = s.id
		WHERE `+cardUnpaidWhere+`
		GROUP BY 1, 2
		HAVING SUM(s.amount - COALESCE(al.a, 0)) > 0
		ORDER BY 4 DESC, 1`, hh, card)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []byPerson
	for rows.Next() {
		var p byPerson
		if err := rows.Scan(&p.Person, &p.UserID, &p.Share, &p.Due); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// cardPayments loads the card's payment history with allocations attached.
func (a *App) cardPayments(ctx context.Context, hh, card int64) ([]payLine, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT p.id, p.payment_date, p.amount, p.method, u.display_name,
		       p.notes, COALESCE(p.ledger_transaction_id, 0)
		FROM payment p
		JOIN "user" u ON u.id = p.paid_by
		WHERE p.household_id = $1 AND p.account_id = $2
		ORDER BY p.payment_date DESC, p.id DESC`, hh, card)
	if err != nil {
		return nil, err
	}
	var pays []payLine
	for rows.Next() {
		var p payLine
		if err := rows.Scan(&p.ID, &p.Date, &p.Amount, &p.Method, &p.PaidBy,
			&p.Notes, &p.LedgerID); err != nil {
			rows.Close()
			return nil, err
		}
		pays = append(pays, p)
	}
	rows.Close()
	if len(pays) == 0 {
		return pays, nil
	}

	ids := make([]int64, len(pays))
	for i, p := range pays {
		ids[i] = p.ID
	}
	// IN-list placeholders via $1..$N built once.
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)+2)
	args = append(args, hh, card)
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", i+3)
		args = append(args, id)
	}
	arows, err := a.Pool.Query(ctx, `
		SELECT pa.payment_id, pa.amount, t.transaction_date,
		       COALESCE(NULLIF(t.description, ''), m.name, '—'),
		       COALESCE(CASE s.participant_type
		                  WHEN 'household_user' THEN u.display_name
		                  ELSE cp.name END, '')
		FROM payment_allocation pa
		JOIN payment p ON p.id = pa.payment_id
		JOIN split s ON s.id = pa.split_id
		JOIN "transaction" t ON t.id = s.transaction_id
		LEFT JOIN merchant m ON m.id = t.merchant_id
		LEFT JOIN "user" u ON s.participant_type = 'household_user' AND u.id = s.participant_ref
		LEFT JOIN counterparty cp ON s.participant_type = 'counterparty' AND cp.id = s.participant_ref
		WHERE p.household_id = $1 AND p.account_id = $2
		  AND p.id IN (`+strings.Join(ph, ", ")+`)
		ORDER BY p.payment_date, p.id, s.id`, args...)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	byPay := make(map[int64]*payLine, len(pays))
	for i := range pays {
		byPay[pays[i].ID] = &pays[i]
	}
	for arows.Next() {
		var (
			pid     int64
			alloc   allocLine
		)
		if err := arows.Scan(&pid, &alloc.Amount, &alloc.Date, &alloc.Label,
			&alloc.Participant); err != nil {
			return nil, err
		}
		if p := byPay[pid]; p != nil {
			p.Allocs = append(p.Allocs, alloc)
		}
	}
	return pays, nil
}

// --- list + detail -----------------------------------------------------------

func (a *App) handleCards(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household

	rows, err := a.Pool.Query(ctx, `
		SELECT id, name, institution, COALESCE(card_type, ''), COALESCE(credit_limit, 0)
		FROM account
		WHERE household_id = $1 AND account_type = 'credit_card'
		ORDER BY is_active DESC, name`, hh)
	if err != nil {
		http.Error(w, "list cards: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	var cards []cardLine
	for rows.Next() {
		var c cardLine
		if err := rows.Scan(&c.ID, &c.Name, &c.Institution, &c.CardType,
			&c.CreditLimit); err != nil {
			http.Error(w, "scan card: "+err.Error(), 500)
			return
		}
		out, err := a.cardOutstanding(ctx, hh, c.ID)
		if err != nil {
			http.Error(w, "card outstanding: "+err.Error(), 500)
			return
		}
		bal, err := a.cardRecordedBalance(ctx, hh, c.ID)
		if err != nil {
			http.Error(w, "card balance: "+err.Error(), 500)
			return
		}
		c.Outstanding, c.Recorded = out, bal
		cards = append(cards, c)
	}
	a.render(w, r, http.StatusOK, "cards.html", map[string]any{
		"Title": "Cards",
		"Flash": flashFrom(r),
		"Cards": cards,
	})
}

func (a *App) handleCardDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household

	_, name, inst, ctype, limit, err := a.cardAccountRow(ctx, hh, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "load card: "+err.Error(), 500)
		return
	}
	out, err := a.cardOutstanding(ctx, hh, id)
	if err != nil {
		http.Error(w, "card outstanding: "+err.Error(), 500)
		return
	}
	bal, err := a.cardRecordedBalance(ctx, hh, id)
	if err != nil {
		http.Error(w, "card balance: "+err.Error(), 500)
		return
	}
	people, err := a.cardByPerson(ctx, hh, id)
	if err != nil {
		http.Error(w, "card by person: "+err.Error(), 500)
		return
	}
	unpaid, err := a.cardUnpaidLines(ctx, hh, id)
	if err != nil {
		http.Error(w, "card unpaid: "+err.Error(), 500)
		return
	}
	pays, err := a.cardPayments(ctx, hh, id)
	if err != nil {
		http.Error(w, "card payments: "+err.Error(), 500)
		return
	}
	a.render(w, r, http.StatusOK, "card.html", map[string]any{
		"Title": name,
		"Flash": flashFrom(r),
		"C": cardDetailData{
			ID: id, Name: name, Institution: inst, CardType: ctype,
			CreditLimit: limit, Outstanding: out, Recorded: bal,
			People: people, Unpaid: unpaid, Payments: pays,
		},
	})
}

// --- pay flow (D5 + D6) -------------------------------------------------------

func (a *App) handleCardPayGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	sess := sessionFromContext(ctx)

	_, name, inst, ctype, limit, err := a.cardAccountRow(ctx, hh, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "load card: "+err.Error(), 500)
		return
	}
	all, err := a.cardUnpaidLines(ctx, hh, id)
	if err != nil {
		http.Error(w, "card unpaid: "+err.Error(), 500)
		return
	}

	// Selection: explicit split[] params win, then who=<user id>, then all.
	q := r.URL.Query()
	sel := map[int64]bool{}
	noSel := true
	if splits := q["split"]; len(splits) > 0 {
		noSel = false
		for _, s := range splits {
			if v, perr := strconv.ParseInt(s, 10, 64); perr == nil && v > 0 {
				sel[v] = true
			}
		}
	} else if who := q.Get("who"); who != "" {
		noSel = false
		if uid, perr := strconv.ParseInt(who, 10, 64); perr == nil && uid > 0 {
			rows2, qerr := a.Pool.Query(ctx, `
				SELECT s.id FROM split s
				JOIN "transaction" t ON t.id = s.transaction_id
				WHERE `+cardUnpaidWhere+`
				  AND s.participant_type = 'household_user' AND s.participant_ref = $3
				  AND s.amount > COALESCE(
				    (SELECT SUM(pa.amount) FROM payment_allocation pa WHERE pa.split_id = s.id), 0)`,
				hh, id, uid)
			if qerr == nil {
				for rows2.Next() {
					var sid int64
					if rows2.Scan(&sid) == nil {
						sel[sid] = true
					}
				}
				rows2.Close()
			}
		}
	}

	f := &cardPayForm{
		CardID:     id,
		CardName:   name,
		Date:       time.Now().Format("2006-01-02"),
		Method:     "transfer",
		PaidBy:     sess.UserID,
		Ledger:     true,
		Methods:    payMethods,
		NothingDue: len(all) == 0,
	}
	f.Users, _ = a.userOptions(ctx, hh)
	f.Sources, _ = a.assetAccountOptions(ctx, hh)

	var total money.Money
	for _, l := range all {
		if !noSel && !sel[l.SplitID] {
			continue
		}
		// Alloc left empty: the template pre-fills each input with the
		// outstanding, so a fresh GET means "pay everything selected".
		f.Rows = append(f.Rows, payRow{unpaidLine: l})
		total = total.Add(l.Due)
	}
	if len(f.Rows) == 0 {
		f.Err = "Nothing selected (or nothing is due on this card)."
	} else {
		f.Amount = total.String()
		f.AllocSum = total
	}
	if inst != "" {
		f.CardName += " · " + inst
	}
	_ = ctype
	_ = limit
	a.render(w, r, http.StatusOK, "card_pay.html", map[string]any{
		"Title": "Record payment — " + name,
		"F":     f,
	})
}

func (a *App) handleCardPayPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household

	_, name, inst, _, _, err := a.cardAccountRow(ctx, hh, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "load card: "+err.Error(), 500)
		return
	}

	all, err := a.cardUnpaidLines(ctx, hh, id)
	if err != nil {
		http.Error(w, "card unpaid: "+err.Error(), 500)
		return
	}
	dueByID := make(map[int64]unpaidLine, len(all))
	for _, l := range all {
		dueByID[l.SplitID] = l
	}

	amountStr := strings.TrimSpace(r.FormValue("amount"))
	amount, aerr := money.Parse(amountStr)
	date := strings.TrimSpace(r.FormValue("payment_date"))
	method := r.FormValue("method")
	if method == "" {
		method = "transfer"
	}
	paidBy, _ := strconv.ParseInt(r.FormValue("paid_by"), 10, 64)
	ledger := r.FormValue("write_ledger") == "on"
	sourceID, _ := strconv.ParseInt(r.FormValue("source_account"), 10, 64)
	notes := strings.TrimSpace(r.FormValue("notes"))

	// Parse allocation rows: paired alloc_split[]/alloc[] values. An empty
	// value means the row is not allocated.
	type allocSpec struct {
		SplitID int64
		Typed   string
		Amount  money.Money
	}
	var allocs []allocSpec
	var allocSum money.Money
	malformed := false
	allocKeys := r.Form["alloc_split[]"]
	allocValues := r.Form["alloc[]"]
	if len(allocValues) != len(allocKeys) {
		malformed = true
	}
	lineErr := ""
	for i, key := range allocKeys {
		sid, kerr := strconv.ParseInt(key, 10, 64)
		if kerr != nil || sid <= 0 {
			continue
		}
		v := strings.TrimSpace(allocValues[i])
		if v == "" {
			continue // row left empty = not allocated
		}
		am, verr := money.Parse(v)
		if verr != nil || am <= 0 {
			lineErr = "Allocation amounts must be positive numbers."
			allocs = append(allocs, allocSpec{SplitID: sid, Typed: v})
			break
		}
		line, ok := dueByID[sid]
		if !ok {
			lineErr = "A selected item is not payable on this card."
			allocs = append(allocs, allocSpec{SplitID: sid, Typed: v})
			break
		}
		if am.Cmp(line.Due) > 0 {
			lineErr = fmt.Sprintf("Allocation for %s exceeds its outstanding %s.",
				line.Label, line.Due.String())
			allocs = append(allocs, allocSpec{SplitID: sid, Typed: v})
			break
		}
		allocs = append(allocs, allocSpec{SplitID: sid, Typed: v, Amount: am})
		allocSum = allocSum.Add(am)
	}

	// Rebuild the re-renderable form with the typed values (so a failure
	// keeps everything the user entered).
	f := &cardPayForm{
		CardID:   id,
		CardName: name,
		Amount:   amountStr,
		Date:     date,
		Method:   method,
		PaidBy:   paidBy,
		Ledger:   ledger,
		SourceID: sourceID,
		Notes:    notes,
		Methods:  payMethods,
	}
	if inst != "" {
		f.CardName += " · " + inst
	}
	// Rows: every selected (typed) split, then the rest of the unpaid list
	// as empty rows, so the user can add more without losing the page.
	rowOrder := make([]int64, 0, len(all))
	seenRow := make(map[int64]bool, len(allocs))
	for _, as := range allocs {
		if seenRow[as.SplitID] {
			continue
		}
		seenRow[as.SplitID] = true
		rowOrder = append(rowOrder, as.SplitID)
	}
	for _, l := range all {
		if !seenRow[l.SplitID] {
			rowOrder = append(rowOrder, l.SplitID)
		}
	}
	typedByID := make(map[int64]string, len(allocs))
	for _, as := range allocs {
		typedByID[as.SplitID] = as.Typed
	}
	for _, sid := range rowOrder {
		l := dueByID[sid]
		f.Rows = append(f.Rows, payRow{unpaidLine: l, Alloc: typedByID[sid]})
	}
	f.AllocSum = allocSum

	if lineErr != "" {
		f.Err = lineErr
	} else if aerr != nil || amount <= 0 {
		f.Err = "Payment amount is required."
	} else if malformed {
		f.Err = "Allocation form is malformed."
	} else if !isDateStr(date) {
		f.Err = "Payment date is required (YYYY-MM-DD)."
	} else if !inSlice(method, payMethods) {
		f.Err = "Payment method is not recognised."
	} else if paidBy <= 0 {
		f.Err = "Choose who made the payment."
	} else if len(allocs) == 0 {
		f.Err = "Allocate the payment to at least one item."
	} else if allocSum.Cmp(amount) != 0 {
		f.Err = fmt.Sprintf("Allocations sum to %s, payment is %s — they must match exactly.",
			allocSum.String(), amount.String())
	} else {
		var n int
		if err := a.Pool.QueryRow(ctx,
			`SELECT count(*) FROM "user" WHERE id = $1 AND household_id = $2`,
			paidBy, hh).Scan(&n); err != nil || n == 0 {
			f.Err = "Payer not found."
		}
	}

	if f.Err == "" && ledger {
		if sourceID <= 0 {
			f.Err = "Choose the account the payment is made from (ledger transfer)."
		} else {
			var sBalance string
			if err := a.Pool.QueryRow(ctx, `
				SELECT balance_type FROM account
				WHERE id = $1 AND household_id = $2`, sourceID, hh).
				Scan(&sBalance); err != nil {
				f.Err = "Source account not found."
			} else if sourceID == id {
				f.Err = "The source account is the card itself."
			} else if sBalance != "asset" {
				f.Err = "The ledger transfer must come from an asset account (savings, cash or e-wallet)."
			}
		}
	}

	if f.Err != "" {
		f.Users, _ = a.userOptions(ctx, hh)
		f.Sources, _ = a.assetAccountOptions(ctx, hh)
		a.render(w, r, http.StatusOK, "card_pay.html", map[string]any{
			"Title": "Record payment — " + name,
			"F":     f,
		})
		return
	}

	// Commit: payment + allocations (+ optional D6 transfer) in one tx.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	defer tx.Rollback(ctx)

	var ledgerTxnID any
	if ledger {
		tid := 0
		desc := fmt.Sprintf("Card payment: %s (%s)", name, date)
		if err := tx.QueryRow(ctx, `
			INSERT INTO "transaction" (
				household_id, recorded_by, transaction_date, class,
				description, total_amount,
				origin_type, origin_ref, destination_type, destination_ref,
				record_status, source, notes
			) VALUES ($1, $2, $3, 'transfer', $4, $5,
				'account', $6, 'account', $7,
				'approved', 'manual', $8)
			RETURNING id`,
			hh, paidBy, date, desc, amount,
			sourceID, id,
			"Written by the payment flow (D6) to keep the recorded card balance in step.").
			Scan(&tid); err != nil {
			http.Error(w, "ledger transfer: "+err.Error(), 500)
			return
		}
		ledgerTxnID = tid
	}

	paymentID := 0
	if err := tx.QueryRow(ctx, `
		INSERT INTO payment (household_id, account_id, paid_by, amount,
		                      payment_date, method, notes, ledger_transaction_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		hh, id, paidBy, amount, date, method, notes, ledgerTxnID).Scan(&paymentID); err != nil {
		http.Error(w, "create payment: "+err.Error(), 500)
		return
	}
	for _, as := range allocs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment_allocation (payment_id, split_id, amount)
			VALUES ($1, $2, $3)`, paymentID, as.SplitID, as.Amount); err != nil {
			http.Error(w, "allocate payment: "+err.Error(), 500)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "could not record payment: "+err.Error(), 500)
		return
	}

	msg := "Payment of " + amount.String() + " recorded."
	http.Redirect(w, r, fmt.Sprintf("/cards/%d?msg=%s", id, url.QueryEscape(msg)),
		http.StatusSeeOther)
}

// --- payment delete ------------------------------------------------------------

func (a *App) handlePaymentDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household

	var cardID int64
	err := a.Pool.QueryRow(ctx,
		`SELECT COALESCE(account_id, 0) FROM payment
		 WHERE id = $1 AND household_id = $2`, id, hh).Scan(&cardID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "load payment: "+err.Error(), 500)
		return
	}
	if _, err := a.Pool.Exec(ctx, `DELETE FROM payment WHERE id = $1 AND household_id = $2`,
		id, hh); err != nil {
		http.Error(w, "delete payment: "+err.Error(), 500)
		return
	}
	target := "/"
	if cardID > 0 {
		target = fmt.Sprintf("/cards/%d", cardID)
	}
	http.Redirect(w, r, target+"?msg="+url.QueryEscape("Payment deleted (allocations removed)."),
		http.StatusSeeOther)
}

// --- option helpers -------------------------------------------------------------

func (a *App) userOptions(ctx context.Context, hh int64) ([]opt, error) {
	rows, err := a.Pool.Query(ctx,
		`SELECT id, display_name FROM "user" WHERE household_id = $1 ORDER BY id`, hh)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []opt
	for rows.Next() {
		var u opt
		if err := rows.Scan(&u.ID, &u.Name); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func (a *App) assetAccountOptions(ctx context.Context, hh int64) ([]opt, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT id, name FROM account
		WHERE household_id = $1 AND is_active AND balance_type = 'asset'
		ORDER BY name`, hh)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []opt
	for rows.Next() {
		var c opt
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
