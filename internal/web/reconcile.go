package web

// Phase P6: reconciliation (D8) + dashboard cards (D15).
//
// Reconciliation anchors (D8): per card and statement month, the user
// types the balance they actually see in the bank app. The app keeps
// that next to the recorded balance (computed from the ledger) so the
// delta — and the drill-in lists that explain it — survive without
// re-typing.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/WanderingLesserPanda/eventally/internal/money"
)

const reconMonthLayout = "2006-01"

// reconCardRow is one row of the reconcile index.
type reconCardRow struct {
	ID         int64
	Name       string
	CardType   string
	Recorded   money.Money
	Outstanding money.Money
	HasAnchor  bool
	LastMonth  string // YYYY-MM of newest anchor, "" if none
	LastActual money.Money
	LastDelta  money.Money
}

// reconPivotCell is one charges cell (card type x person).
type reconPivotCell struct {
	Person string
	Amount money.Money
}

// reconCreditLine is a money-in row on the card for the month.
type reconCreditLine struct {
	Date   time.Time
	Label  string
	Amount money.Money
}

// reconDrillLine is an unpaid item that appears on the statement.
type reconDrillLine struct {
	Date      time.Time
	Label     string
	Person    string
	Share     money.Money
	Allocated money.Money
}

// reconMonthData is the payload for the monthly reconciliation page.
type reconMonthData struct {
	CardID     int64
	CardName   string
	CardType   string
	Month      string // YYYY-MM
	MonthStart time.Time
	MonthEnd   time.Time

	ChargesTotal money.Money
	CreditsTotal money.Money
	Charges      []reconPivotCell
	Credits      []reconCreditLine
	Drill        []reconDrillLine

	Recorded money.Money
	Anchor   *reconAnchor
}

// reconAnchor is a saved reconciliation row (actual balance for a month).
type reconAnchor struct {
	Actual     money.Money
	Recorded   money.Money
	Delta      money.Money
	Note       string
	RecordedBy string
}

// parseReconMonth converts a YYYY-MM segment to the month's start.
func parseReconMonth(s string) (time.Time, bool) {
	t, err := time.Parse(reconMonthLayout, s)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), true
}

// handleReconcile lists the household's cards with current computed
// balances and each card's newest reconciliation anchor.
func (a *App) handleReconcile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	nowMonth := time.Now().Format(reconMonthLayout)

	var cards []reconCardRow
	rows, err := a.Pool.Query(ctx, `
		SELECT a.id, a.name, COALESCE(a.card_type, ''),
		       (SELECT COALESCE(x.opening_balance, 0)
		        + COALESCE((SELECT SUM(t.total_amount) FROM "transaction" t
		                    WHERE t.household_id = $1 AND t.record_status <> 'void'
		                  AND t.origin_type = 'account' AND t.origin_ref = a.id), 0)
		        - COALESCE((SELECT SUM(t.total_amount) FROM "transaction" t
		                    WHERE t.household_id = $1 AND t.record_status <> 'void'
		                  AND t.destination_type = 'account' AND t.destination_ref = a.id), 0)
		        - COALESCE((SELECT SUM(p.amount) FROM payment p
		                    WHERE p.household_id = $1 AND p.account_id = a.id
		                  AND p.ledger_transaction_id IS NULL), 0)
		        FROM account x WHERE x.id = a.id) AS recorded,
		       (SELECT COALESCE(SUM(s.amount - COALESCE(al.a, 0)), 0)
		        FROM split s
		        JOIN "transaction" t ON t.id = s.transaction_id
		        LEFT JOIN (SELECT split_id, SUM(amount) AS a
		            FROM payment_allocation GROUP BY split_id) al ON al.split_id = s.id
		        WHERE t.household_id = $1 AND t.record_status <> 'void'
		          AND t.class = 'expense' AND (
		            (t.origin_type = 'account' AND t.origin_ref = a.id)
		            OR (t.destination_type = 'account' AND t.destination_ref = a.id)))
		       AS outstanding,
		       rc.statement_month, rc.actual_balance, rc.recorded_balance
		FROM account a
		LEFT JOIN LATERAL (SELECT rc.statement_month, rc.actual_balance, rc.recorded_balance
		                   FROM reconciliation rc
		                   WHERE rc.household_id = a.household_id
		                     AND rc.account_id = a.id
		                   ORDER BY rc.statement_month DESC LIMIT 1) rc ON TRUE
		WHERE a.household_id = $1 AND a.account_type = 'credit_card' AND a.is_active
		ORDER BY a.sort_order, a.name`, hh)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c reconCardRow
		var lastMonth sql.NullTime
		var lastActual, lastRecorded money.Money
		if err := rows.Scan(&c.ID, &c.Name, &c.CardType, &c.Recorded,
			&c.Outstanding, &lastMonth, &lastActual, &lastRecorded); err != nil {
			http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if lastMonth.Valid {
			c.HasAnchor = true
			c.LastMonth = lastMonth.Time.Format(reconMonthLayout)
			c.LastActual = lastActual
			c.LastDelta = lastActual.Sub(lastRecorded)
		}
		cards = append(cards, c)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	a.render(w, r, http.StatusOK, "reconcile.html", map[string]any{
		"Title": "Reconcile",
		"Flash": flashFrom(r),
		"Month": nowMonth,
		"Cards": cards,
	})
}

// cardRecordedAt computes the recorded card balance as of endOfMonth
// (inclusive date): opening + charges - credits - out-of-band payments,
// each restricted to rows dated on or before that day.
func (a *App) cardRecordedAt(ctx context.Context, hh, card int64, endOfMonth time.Time) (money.Money, error) {
	var bal money.Money
	err := a.Pool.QueryRow(ctx, `
		SELECT COALESCE(a.opening_balance, 0)
		 + COALESCE((SELECT SUM(t.total_amount) FROM "transaction" t
		              WHERE t.household_id = $1 AND t.record_status <> 'void'
		        AND t.origin_type = 'account' AND t.origin_ref = $2
		        AND t.transaction_date <= $3), 0)
		 - COALESCE((SELECT SUM(t.total_amount) FROM "transaction" t
		              WHERE t.household_id = $1 AND t.record_status <> 'void'
		        AND t.destination_type = 'account' AND t.destination_ref = $2
		        AND t.transaction_date <= $3), 0)
		 - COALESCE((SELECT SUM(p.amount) FROM payment p
		              WHERE p.household_id = $1 AND p.account_id = $2
		        AND p.ledger_transaction_id IS NULL
		        AND p.payment_date <= $3), 0)
		FROM account a WHERE a.id = $2 AND a.household_id = $1`, hh, card, endOfMonth).Scan(&bal)
	return bal, err
}

// handleReconcileMonth renders the monthly page for one card: charges
// by person, credits, drill-in items, recorded balance, and the
// actual-balance form (prefilled from a saved anchor, if any).
func (a *App) handleReconcileMonth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household

	cardID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	monthStart, ok := parseReconMonth(chi.URLParam(r, "month"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	monthEnd := monthStart.AddDate(0, 1, -1)

	_, name, _, cardType, _, err := a.cardAccountRow(ctx, hh, cardID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Charges by person: expense splits on the card in the month.
	var (
		charges    []reconPivotCell
		chargesTot money.Money
	)
	rows, err := a.Pool.Query(ctx, `
		SELECT COALESCE(CASE s.participant_type
		                  WHEN 'household_user' THEN u.display_name
		                  ELSE cp.name END, 'Unknown'),
		       SUM(s.amount)
		FROM split s
		JOIN "transaction" t ON t.id = s.transaction_id
		LEFT JOIN "user" u ON s.participant_type = 'household_user' AND u.id = s.participant_ref
		LEFT JOIN counterparty cp ON s.participant_type = 'counterparty' AND s.participant_ref = cp.id
		WHERE `+cardUnpaidWhere+`
		  AND t.transaction_date >= $3 AND t.transaction_date <= $4
		GROUP BY 1
		ORDER BY 2 DESC, 1`, hh, cardID, monthStart, monthEnd)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var c reconPivotCell
		if err := rows.Scan(&c.Person, &c.Amount); err != nil {
			rows.Close()
			http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		charges = append(charges, c)
		chargesTot = chargesTot.Add(c.Amount)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Credits: ledger rows where the card is the destination, plus
	// out-of-band payments (no ledger link) that reduce the balance.
	var (
		credits    []reconCreditLine
		creditsTot money.Money
	)
	rows, err = a.Pool.Query(ctx, `
		SELECT t.transaction_date,
		       CASE t.class WHEN 'income' THEN 'credit: '
		                    ELSE 'transfer in: ' END ||
		       COALESCE(NULLIF(t.description, ''), m.name, '—'),
		       t.total_amount
		FROM "transaction" t
		LEFT JOIN merchant m ON m.id = t.merchant_id
		WHERE t.household_id = $1 AND t.record_status <> 'void'
		  AND t.destination_type = 'account' AND t.destination_ref = $2
		  AND t.transaction_date >= $3 AND t.transaction_date <= $4
		ORDER BY t.transaction_date, t.id`, hh, cardID, monthStart, monthEnd)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var l reconCreditLine
		if err := rows.Scan(&l.Date, &l.Label, &l.Amount); err != nil {
			rows.Close()
			http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		credits = append(credits, l)
		creditsTot = creditsTot.Add(l.Amount)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rows, err = a.Pool.Query(ctx, `
		SELECT p.payment_date, 'payment: ' || u.display_name, p.amount
		FROM payment p
		JOIN "user" u ON u.id = p.paid_by
		WHERE p.household_id = $1 AND p.account_id = $2
		  AND p.ledger_transaction_id IS NULL
		  AND p.payment_date >= $3 AND p.payment_date <= $4
		ORDER BY p.payment_date, p.id`, hh, cardID, monthStart, monthEnd)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var l reconCreditLine
		if err := rows.Scan(&l.Date, &l.Label, &l.Amount); err != nil {
			rows.Close()
			http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		credits = append(credits, l)
		creditsTot = creditsTot.Add(l.Amount)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Drill-in: the month's statement items (unpaid expense splits).
	var drill []reconDrillLine
	rows, err = a.Pool.Query(ctx, `
		SELECT t.transaction_date,
		       COALESCE(NULLIF(t.description, ''), m.name, '—'),
		       COALESCE(CASE s.participant_type
		                  WHEN 'household_user' THEN u.display_name
		                  ELSE cp.name END, 'Unknown'),
		       s.amount, COALESCE(al.a, 0)
		FROM split s
		JOIN "transaction" t ON t.id = s.transaction_id
		LEFT JOIN merchant m ON m.id = t.merchant_id
		LEFT JOIN "user" u ON s.participant_type = 'household_user' AND u.id = s.participant_ref
		LEFT JOIN counterparty cp ON s.participant_type = 'counterparty' AND s.participant_ref = cp.id
		LEFT JOIN (SELECT split_id, SUM(amount) AS a
			FROM payment_allocation GROUP BY split_id) al ON al.split_id = s.id
		WHERE `+cardUnpaidWhere+`
		  AND t.transaction_date >= $3 AND t.transaction_date <= $4
		ORDER BY t.transaction_date, s.id`, hh, cardID, monthStart, monthEnd)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var l reconDrillLine
		if err := rows.Scan(&l.Date, &l.Label, &l.Person, &l.Share, &l.Allocated); err != nil {
			rows.Close()
			http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		drill = append(drill, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	recorded, err := a.cardRecordedAt(ctx, hh, cardID, monthEnd)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var anchor *reconAnchor
	var actual, recordedBal money.Money
	var note, byName sql.NullString
	err = a.Pool.QueryRow(ctx, `
		SELECT rc.actual_balance, rc.recorded_balance, rc.note, u.display_name
		FROM reconciliation rc
		LEFT JOIN "user" u ON u.id = rc.recorded_by
		WHERE rc.household_id = $1 AND rc.account_id = $2
		  AND rc.statement_month = $3`, hh, cardID, monthStart).
		Scan(&actual, &recordedBal, &note, &byName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err == nil {
		anchor = &reconAnchor{
			Actual:     actual,
			Recorded:   recordedBal,
			Delta:      actual.Sub(recordedBal),
			Note:       note.String,
			RecordedBy: byName.String,
		}
	}

	a.render(w, r, http.StatusOK, "reconcile_month.html", map[string]any{
		"Title": "Reconcile " + name,
		"Flash": flashFrom(r),
		"Data": &reconMonthData{
			CardID:       cardID,
			CardName:     name,
			CardType:     cardType,
			Month:        monthStart.Format(reconMonthLayout),
			MonthStart:   monthStart,
			MonthEnd:     monthEnd,
			ChargesTotal: chargesTot,
			CreditsTotal: creditsTot,
			Charges:      charges,
			Credits:      credits,
			Drill:        drill,
			Recorded:     recorded,
			Anchor:       anchor,
		},
	})
}

// handleReconcileMonthPost saves (or updates) the typed actual balance
// for the card's statement month — the D8 anchor.
func (a *App) handleReconcileMonthPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	monthParam := chi.URLParam(r, "month")

	cardID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	monthStart, ok := parseReconMonth(monthParam)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, _, _, _, _, err := a.cardAccountRow(ctx, hh, cardID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	actual, perr := money.Parse(r.FormValue("actual"))
	if perr != nil || actual.IsNegative() {
		http.Redirect(w, r, fmt.Sprintf("/reconcile/%d/%s?msg=%s",
			cardID, monthParam, url.QueryEscape("Actual balance must be a non-negative amount.")),
			http.StatusSeeOther)
		return
	}
	note := r.FormValue("note")

	recorded, err := a.cardRecordedAt(ctx, hh, cardID, monthStart.AddDate(0, 1, -1))
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := a.Pool.Exec(ctx, `
		INSERT INTO reconciliation (household_id, account_id, statement_month,
		                             actual_balance, recorded_balance, note, recorded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (household_id, account_id, statement_month)
		DO UPDATE SET actual_balance = EXCLUDED.actual_balance,
		              recorded_balance = EXCLUDED.recorded_balance,
		              note = EXCLUDED.note,
		              recorded_by = EXCLUDED.recorded_by`,
		hh, cardID, monthStart, actual, recorded, note, sess.UserID); err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/reconcile/%d/%s?msg=%s",
		cardID, monthParam, url.QueryEscape("Reconciliation saved for "+monthParam+".")),
		http.StatusSeeOther)
}
