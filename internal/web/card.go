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

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/WanderingLesserPanda/eventally/internal/money"
)

// pgErrCode returns the Postgres SQLSTATE of err, or "" if it is not a
// Postgres error.
func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// ---------------------------------------------------------------------------
// Phase P5: card view + payments with allocations (D5, D6).
//
//   - /cards: every credit card account with outstanding (unpaid expense
//     splits) and the computed recorded balance.
//   - /cards/{id}: outstanding per person, the unpaid transaction_split list (multi-select
//     for lump-sum payments, D5), and the payment history with allocations.
//   - /cards/{id}/pay: record a payment. Amount + date + method + paid_by_user_id,
//     allocation inputs pre-filled with each selected split's outstanding.
//     Optionally (D6) the same action writes the ledger transfer
//     savings → card so the recorded card balance stays in step with the
//     real one; the transfer is a split-less, approved balance movement and
//     is linked back via payment.ledger_transaction_id.
//
// Outstanding per transaction_split = split.amount − SUM(allocations) (D5: negative
// means overpaid, carries forward). Only class='expense' splits are
// payable; income/refund items are card credits (balance movements, not
// debt). record_status 'pending' still counts — approval happens at the
// monthly statement check (D9).
// ---------------------------------------------------------------------------

var payMethods = []string{"transfer", "online", "mobile", "cash", "other"}

// cardLine is one row of the /cards list.
type cardLine struct {
	ID            int64
	Name          string
	Institution   string
	CardType      string
	CreditLimit   money.Money
	Outstanding   money.Money
	Recorded      money.Money
	ChildrenCount int
}

// byPerson is one row of the per-person outstanding summary.
type byPerson struct {
	Person      string
	UserID      int64 // >0 for household users (drives the Pay link)
	Share       money.Money
	Due         money.Money
}

// unpaidLine is one payable transaction_split row.
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
// Credit is the unapplied portion of the payment: amount − Σ allocations.
// A settlement may pay more than its allocations (option (a), Gate 4) —
// the excess is carried as credit until later allocations absorb it.
type payLine struct {
	ID       int64
	Date     time.Time
	Amount   money.Money
	Credit   money.Money
	Method   string
	PaidBy   string
	Notes    string
	LedgerID int64
	Allocs   []allocLine
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
	// UnappliedCredit is the sum of (payment − allocations) over the
	// family's settlements: money taken in that was not yet absorbed by
	// split allocations (Gate 4, option (a)).
	UnappliedCredit money.Money
	People          []byPerson
	Unpaid          []unpaidLine
	Payments        []payLine
	Family          *cardFamily
}

// payRow is one allocation row of the pay form: the payable transaction_split plus the
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
	// Unapplied is the part of the typed payment not covered by the
	// allocations (shown on the record button when > 0, Gate 4 option (a)).
	Unapplied   money.Money
	Users       []opt
	Sources     []opt // asset accounts (D6 source)
	Methods     []string
	NothingDue  bool
}

// --- shared queries ---------------------------------------------------------

// cardFamilyLine is one card in a family roll-up (self, parent or child).
type cardFamilyLine struct {
	ID          int64
	Name        string
	CardType    string
	Outstanding money.Money
	Recorded    money.Money
}

// cardFamily is the one-level parent/child family of a card (D19): the
// card itself plus its parent or its children.
type cardFamily struct {
	ID          int64
	Name        string
	IsParent    bool
	ParentID    int64
	ParentName  string
	Children    []cardFamilyLine
	TotalOut    money.Money // roll-up when IsParent
	TotalRec    money.Money // roll-up when IsParent
}

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

// inPh builds an IN-list of placeholders like "$2, $3, $4" starting at
// placeholder 2 for len(v) values (placeholder 1 is always the household
// id).
func inPh(v []int64) string {
	if len(v) == 0 {
		return "0" // matches nothing
	}
	ph := make([]string, len(v))
	for i := range v {
		ph[i] = fmt.Sprintf("$%d", i+2)
	}
	return strings.Join(ph, ", ")
}

// toAny converts []int64 into []any for pgx arguments.
func toAny(v []int64) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

// cardUnpaidWhere is the card's outstanding transaction_split filter (pending +
// approved, never void, never transfer — transfers have no splits). The
// IN_ACCOUNTS marker is replaced by inPh(fam) via unpaidWhereFor;
// placeholders 2..1+len(fam) hold the family account ids.
const cardUnpaidWhere = `t.household_id = $1 AND t.record_status <> 'void'
	AND t.class = 'expense' AND (
	  (t.origin_account_id IN (IN_ACCOUNTS))
	  OR (t.destination_account_id IN (IN_ACCOUNTS)))`

// unpaidWhereFor scopes cardUnpaidWhere to a card family's account ids.
func unpaidWhereFor(fam []int64) string {
	return strings.ReplaceAll(cardUnpaidWhere, "IN_ACCOUNTS", inPh(fam))
}

// cardOutstanding is the sum of unallocated expense splits on the card
// family (the roll-up for a parent, per D19).
func (a *App) cardOutstanding(ctx context.Context, hh int64, fam []int64) (money.Money, error) {
	var out money.Money
	err := a.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(s.amount - COALESCE(al.a, 0)), 0)
		FROM transaction_split s
		JOIN "transaction" t ON t.id = s.transaction_id
		LEFT JOIN (SELECT split_id, SUM(amount) AS a
			FROM settlement_allocation GROUP BY split_id) al ON al.split_id = s.id
		WHERE `+unpaidWhereFor(fam),
		append([]any{hh}, toAny(fam)...)...).Scan(&out)
	return out, err
}

// cardRecordedBalance computes the card family's ledger liability (D19
// roll-up for a parent):
//
//	opening + charges (card as origin) − credits (card as destination)
//	        − payments without a ledger link (D6: cash settled out-of-band)
//
// Payments that wrote a D6 transfer are already reflected in the
// destination=card total, so they are excluded from the payment sum.
// Each term is an independent scalar subquery — never join account rows
// to transactions here (that multiplies every amount per account).
func (a *App) cardRecordedBalance(ctx context.Context, hh int64, fam []int64) (money.Money, error) {
	var bal money.Money
	ph := inPh(fam)
	err := a.Pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT SUM(a.opening_balance) FROM account a
		                  WHERE a.household_id = $1 AND a.id IN (`+ph+`)), 0)
		 + COALESCE((SELECT SUM(t.total_amount) FROM "transaction" t
		              WHERE t.household_id = $1 AND t.record_status <> 'void'
		        AND t.origin_account_id IN (`+ph+`)), 0)
		 - COALESCE((SELECT SUM(t.total_amount) FROM "transaction" t
		              WHERE t.household_id = $1 AND t.record_status <> 'void'
		        AND t.destination_account_id IN (`+ph+`)), 0)
		 - COALESCE((SELECT SUM(p.amount) FROM settlement p
		              WHERE p.household_id = $1 AND p.ledger_transaction_id IS NULL
		        AND p.account_id IN (`+ph+`)), 0)`,
		append([]any{hh}, toAny(fam)...)...).Scan(&bal)
	return bal, err
}

// cardUnpaidLines lists the card family's payable splits with
// outstanding > 0, oldest first.
func (a *App) cardUnpaidLines(ctx context.Context, hh int64, fam []int64) ([]unpaidLine, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT s.id, t.transaction_date,
		       COALESCE(NULLIF(t.description, ''), m.name, '—'),
		       COALESCE(p.name, 'Unknown'),
		       t.record_status, s.amount,
		       COALESCE(al.a, 0)
		FROM transaction_split s
		JOIN "transaction" t ON t.id = s.transaction_id
		LEFT JOIN merchant m ON m.id = t.merchant_id
		LEFT JOIN party p ON p.id = s.party_id
		LEFT JOIN (SELECT split_id, SUM(amount) AS a
			FROM settlement_allocation GROUP BY split_id) al ON al.split_id = s.id
		WHERE `+unpaidWhereFor(fam)+`
		AND s.amount - COALESCE(al.a, 0) > 0
		ORDER BY t.transaction_date, s.id`, append([]any{hh}, toAny(fam)...)...)
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
	return out, rows.Err()
}

// cardByPerson summarises outstanding per participant over the given
// account scope (self-only for a child's detail, the whole family for a
// parent's roll-up).
func (a *App) cardByPerson(ctx context.Context, hh int64, fam []int64) ([]byPerson, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT COALESCE(p.name, 'Unknown'),
		       CASE WHEN p.app_user_id IS NOT NULL
		            THEN p.app_user_id ELSE 0 END,
		       SUM(s.amount),
		       SUM(s.amount - COALESCE(al.a, 0))
		FROM transaction_split s
		JOIN "transaction" t ON t.id = s.transaction_id
		LEFT JOIN party p ON p.id = s.party_id
		LEFT JOIN (SELECT split_id, SUM(amount) AS a
			FROM settlement_allocation GROUP BY split_id) al ON al.split_id = s.id
		WHERE `+unpaidWhereFor(fam)+`
		GROUP BY 1, 2
		HAVING SUM(s.amount - COALESCE(al.a, 0)) > 0
		ORDER BY 4 DESC, 1`, append([]any{hh}, toAny(fam)...)...)
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
	return out, rows.Err()
}

// cardPayments loads the card family's payment history with allocations attached.
func (a *App) cardPayments(ctx context.Context, hh int64, fam []int64) ([]payLine, error) {
	ph := inPh(fam)
	rows, err := a.Pool.Query(ctx, `
		SELECT p.id, p.payment_date, p.amount, p.method, u.display_name,
		       p.notes, COALESCE(p.ledger_transaction_id, 0)
		FROM settlement p
		JOIN app_user u ON u.id = p.paid_by_user_id
		WHERE p.household_id = $1 AND p.account_id IN (`+ph+`)
		ORDER BY p.payment_date DESC, p.id DESC`, append([]any{hh}, toAny(fam)...)...)
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
	ph2 := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, hh)
	for i, id := range ids {
		ph2[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	arows, err := a.Pool.Query(ctx, `
		SELECT pa.payment_id, pa.amount, t.transaction_date,
		       COALESCE(NULLIF(t.description, ''), m.name, '—'),
		       COALESCE(pt.name, '')
		FROM settlement_allocation pa
		JOIN settlement p ON p.id = pa.payment_id
		JOIN transaction_split s ON s.id = pa.split_id
		JOIN "transaction" t ON t.id = s.transaction_id
		LEFT JOIN merchant m ON m.id = t.merchant_id
		LEFT JOIN party pt ON pt.id = s.party_id
		WHERE p.household_id = $1
		  AND p.id IN (`+strings.Join(ph2, ",")+`)
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
	// Unapplied credit per settlement (Gate 4, option (a)): the part of a
	// payment that was not allocated to a split.
	for i := range pays {
		var allocTotal money.Money
		for _, al := range pays[i].Allocs {
			allocTotal = allocTotal.Add(al.Amount)
		}
		if c := pays[i].Amount.Sub(allocTotal); c > 0 {
			pays[i].Credit = c
		}
	}
	return pays, nil
}

// cardFamilyView builds the D19 family display for a card detail page:
// the card itself, its parent link (when a child), and its active
// children (when a parent) with each child's own numbers. The roll-up
// (TotalOut/TotalRec) is computed by the caller over the whole family.
func (a *App) cardFamilyView(ctx context.Context, hh, id int64) (*cardFamily, error) {
	var pid sql.NullInt64
	if err := a.Pool.QueryRow(ctx, `
		SELECT parent_id FROM account
		WHERE id = $1 AND household_id = $2 AND account_type = 'credit_card'`, id, hh).
		Scan(&pid); err != nil {
		return nil, err
	}
	var pname string
	if pid.Valid {
		if err := a.Pool.QueryRow(ctx, `
			SELECT name FROM account WHERE id = $1 AND household_id = $2`,
			pid.Int64, hh).Scan(&pname); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// Broken link: show the card alone.
				return &cardFamily{ID: id}, nil
			}
			return nil, err
		}
	}
	rows, err := a.Pool.Query(ctx, `
		SELECT id, name, COALESCE(card_type, ''), parent_id
		FROM account
		WHERE household_id = $1 AND account_type = 'credit_card' AND is_active
		  AND (id = $2 OR parent_id = $2)
		ORDER BY sort_order, name`, hh, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cf *cardFamily
	var kids []cardFamilyLine
	for rows.Next() {
		var (
			cid   int64
			cname string
			ct    string
			kpid  sql.NullInt64
		)
		if err := rows.Scan(&cid, &cname, &ct, &kpid); err != nil {
			return nil, err
		}
		if cid == id {
			if kpid.Valid {
				cf = &cardFamily{ID: id, Name: cname, ParentID: pid.Int64, ParentName: pname}
			} else {
				cf = &cardFamily{ID: id, Name: cname, IsParent: true}
			}
		} else {
			kids = append(kids, cardFamilyLine{ID: cid, Name: cname, CardType: ct})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if cf == nil {
		return &cardFamily{ID: id}, nil
	}
	cf.Children = kids
	return cf, nil
}

// cardOperatingFamily returns the account ids the detail/pay pages operate
// on for card id: the whole family when id is a parent (D19 roll-up), else
// just id. Also returns the family view for display.
func (a *App) cardOperatingFamily(ctx context.Context, hh, id int64) ([]int64, *cardFamily, error) {
	famView, err := a.cardFamilyView(ctx, hh, id)
	if err != nil {
		return nil, nil, err
	}
	if famView.IsParent {
		fam := []int64{famView.ID}
		for _, k := range famView.Children {
			fam = append(fam, k.ID)
		}
		return fam, famView, nil
	}
	return []int64{id}, famView, nil
}

// cardActiveChildrenIDs returns the active child ids of parent id (D19).
func (a *App) cardActiveChildrenIDs(ctx context.Context, hh, parent int64) ([]int64, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT id FROM account
		WHERE household_id = $1 AND parent_id = $2 AND is_active
		ORDER BY sort_order, name`, hh, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var k int64
		if rows.Scan(&k) != nil {
			return nil, rows.Err()
		}
		ids = append(ids, k)
	}
	return ids, rows.Err()
}

// cardListFamily is the account ids a /cards list row aggregates: the
// family (self + active children) when the row is a parent, else itself.
// nCards counts physical cards including the row itself (a child's
// parent row shows "2 physical cards" for parent + 1 child).
func (a *App) cardListFamily(ctx context.Context, hh, id int64) ([]int64, int, error) {
	var pid sql.NullInt64
	if err := a.Pool.QueryRow(ctx,
		`SELECT parent_id FROM account WHERE id = $1`, id).Scan(&pid); err != nil {
		return nil, 0, err
	}
	if pid.Valid { // child row: no roll-up
		return []int64{id}, 1, nil
	}
	kids, err := a.cardActiveChildrenIDs(ctx, hh, id)
	if err != nil {
		return nil, 0, err
	}
	fam := append([]int64{id}, kids...)
	return fam, len(kids) + 1, nil
}

// --- list + detail -----------------------------------------------------------

func (a *App) handleCards(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household

	// One row per family (D19): cards with parent_id NULL are either
	// standalones (aggregate self) or parents (roll up active children).
	// Children are hidden from the list; they show under the parent page.
	rows, err := a.Pool.Query(ctx, `
		SELECT id, name, institution, COALESCE(card_type, ''), COALESCE(credit_limit, 0)
		FROM account
		WHERE household_id = $1 AND account_type = 'credit_card'
		  AND parent_id IS NULL
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
		fam, nKids, err := a.cardListFamily(ctx, hh, c.ID)
		if err != nil {
			http.Error(w, "load card family: "+err.Error(), 500)
			return
		}
		c.ChildrenCount = nKids
		out, err := a.cardOutstanding(ctx, hh, fam)
		if err != nil {
			http.Error(w, "card outstanding: "+err.Error(), 500)
			return
		}
		bal, err := a.cardRecordedBalance(ctx, hh, fam)
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
	famView, err := a.cardFamilyView(ctx, hh, id)
	if err != nil {
		http.Error(w, "load card family: "+err.Error(), 500)
		return
	}
	var fam []int64
	if famView.IsParent {
		fam = []int64{famView.ID}
		for _, k := range famView.Children {
			fam = append(fam, k.ID)
		}
	} else {
		fam = []int64{id}
	}
	out, err := a.cardOutstanding(ctx, hh, fam)
	if err != nil {
		http.Error(w, "card outstanding: "+err.Error(), 500)
		return
	}
	bal, err := a.cardRecordedBalance(ctx, hh, fam)
	if err != nil {
		http.Error(w, "card balance: "+err.Error(), 500)
		return
	}
	if famView.IsParent {
		famView.TotalOut, famView.TotalRec = out, bal
		for i := range famView.Children {
			// Each child row shows its own self-scope numbers (a parent
			// can't roll up inside another roll-up).
			cout, err := a.cardOutstanding(ctx, hh, []int64{famView.Children[i].ID})
			if err != nil {
				http.Error(w, "child outstanding: "+err.Error(), 500)
				return
			}
			cbal, err := a.cardRecordedBalance(ctx, hh, []int64{famView.Children[i].ID})
			if err != nil {
				http.Error(w, "child balance: "+err.Error(), 500)
				return
			}
			famView.Children[i].Outstanding = cout
			famView.Children[i].Recorded = cbal
		}
	}
	people, err := a.cardByPerson(ctx, hh, fam)
	if err != nil {
		http.Error(w, "card by person: "+err.Error(), 500)
		return
	}
	unpaid, err := a.cardUnpaidLines(ctx, hh, fam)
	if err != nil {
		http.Error(w, "card unpaid: "+err.Error(), 500)
		return
	}
	pays, err := a.cardPayments(ctx, hh, fam)
	if err != nil {
		http.Error(w, "card payments: "+err.Error(), 500)
		return
	}
	// Unapplied credit (Gate 4, option (a)): the part of this family's
	// settlements not yet absorbed by split allocations.
	var unapplied money.Money
	for _, p := range pays {
		unapplied = unapplied.Add(p.Credit)
	}
	a.render(w, r, http.StatusOK, "card.html", map[string]any{
		"Title": name,
		"Flash": flashFrom(r),
		"C": cardDetailData{
			ID: id, Name: name, Institution: inst, CardType: ctype,
			CreditLimit: limit, Outstanding: out, Recorded: bal,
			UnappliedCredit: unapplied,
			People: people, Unpaid: unpaid, Payments: pays,
			Family: famView,
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
	fam, famView, err := a.cardOperatingFamily(ctx, hh, id)
	if err != nil {
		http.Error(w, "load card family: "+err.Error(), 500)
		return
	}
	all, err := a.cardUnpaidLines(ctx, hh, fam)
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
			whargs := append([]any{hh}, toAny(fam)...)
			whargs = append(whargs, uid)
			rows2, qerr := a.Pool.Query(ctx, `
				SELECT s.id FROM transaction_split s
				JOIN "transaction" t ON t.id = s.transaction_id
				WHERE `+unpaidWhereFor(fam)+`
				AND p.app_user_id = $`+strconv.Itoa(len(whargs))+`
				AND s.amount > COALESCE(
				(SELECT SUM(pa.amount) FROM settlement_allocation pa WHERE pa.split_id = s.id), 0)`,
				whargs...)
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
	if famView != nil && famView.ParentID > 0 {
		f.CardName += " (family: " + famView.ParentName + ")"
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
	fam, _, err := a.cardOperatingFamily(ctx, hh, id)
	if err != nil {
		http.Error(w, "load card family: "+err.Error(), 500)
		return
	}
	all, err := a.cardUnpaidLines(ctx, hh, fam)
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
	paidBy, _ := strconv.ParseInt(r.FormValue("paid_by_user_id"), 10, 64)
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
	if u := amount.Sub(allocSum); u > 0 {
		f.Unapplied = u
	}

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
	} else if allocSum.Cmp(amount) > 0 {
		f.Err = fmt.Sprintf("Allocations sum to %s but the payment is only %s — you cannot allocate more than the payment.",
			allocSum.String(), amount.String())
	} else {
		var n int
		if err := a.Pool.QueryRow(ctx,
			`SELECT count(*) FROM app_user WHERE id = $1 AND household_id = $2`,
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
				household_id, recorded_by_user_id, transaction_date, class,
				description, total_amount,
				origin_account_id, destination_account_id,
				record_status, source, notes
			) VALUES ($1, $2, $3, 'transfer', $4, $5,
				$6, $7,
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
		INSERT INTO settlement (household_id, account_id, paid_by_user_id, amount,
		                      payment_date, method, notes, ledger_transaction_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		hh, id, paidBy, amount, date, method, notes, ledgerTxnID).Scan(&paymentID); err != nil {
		http.Error(w, "create payment: "+err.Error(), 500)
		return
	}
	for _, as := range allocs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO settlement_allocation (household_id, payment_id, split_id, amount)
			VALUES ($1, $2, $3, $4)`, hh, paymentID, as.SplitID, as.Amount); err != nil {
			http.Error(w, "allocate payment: "+err.Error(), 500)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if code := pgErrCode(err); code == "23514" {
			http.Error(w, "Another settlement was recorded at the same time: the "+
				"allocations no longer fit within the item's outstanding amount. "+
				"Please check the card's payment history and try again.", http.StatusBadRequest)
			return
		}
		http.Error(w, "could not record payment: "+err.Error(), 500)
		return
	}

	msg := "Payment of " + amount.String() + " recorded."
	http.Redirect(w, r, fmt.Sprintf("/cards/%d?msg=%s", id, url.QueryEscape(msg)),
		http.StatusSeeOther)
}

// --- payment delete ------------------------------------------------------------

func (a *App) handleSettlementDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household

	var cardID int64
	err := a.Pool.QueryRow(ctx,
		`SELECT COALESCE(account_id, 0) FROM settlement
		 WHERE id = $1 AND household_id = $2`, id, hh).Scan(&cardID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "load payment: "+err.Error(), 500)
		return
	}
	if _, err := a.Pool.Exec(ctx, `DELETE FROM settlement WHERE id = $1 AND household_id = $2`,
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
		`SELECT id, display_name FROM app_user WHERE household_id = $1 ORDER BY id`, hh)
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
