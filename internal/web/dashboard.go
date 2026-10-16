package web

// Phase P6: light dashboard, card-based layout (D15) + budget-vs-actual
// display (D10). The MVP shows a fixed set of composable cards; the
// layout is built from independent card sections so future
// customisation is configuration, not redesign.

import (
	"net/http"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/money"
)

// dashCatRow is one top-level category in the spend and budget cards.
type dashCatRow struct {
	Name   string
	Actual money.Money
	Budget money.Money
}

// dashCardBal is one credit card in the balances card.
type dashCardBal struct {
	Name        string
	Recorded    money.Money
	Outstanding money.Money
}

// dashPersonOut is one person in the outstanding card.
type dashPersonOut struct {
	Person string
	Due    money.Money
}

// handleDashboard renders the home page: current-month spend by
// top-level category, card balances, per-person outstanding,
// pending-review count, and budget-vs-actual (D10/D15).
func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hh := sessionFromContext(ctx).Household
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	monthEnd := monthStart.AddDate(0, 1, 0)

	// Card 1+5: top-level expense categories, budget roll-up (parent
	// collects its subtree) and current-month actuals.
	var cats []dashCatRow
	rows, err := a.Pool.Query(ctx, `
		WITH RECURSIVE tree AS (
			SELECT id, id AS root_id FROM category
			WHERE household_id = $1 AND is_active AND parent_id IS NULL
			UNION ALL
			SELECT c.id, t.root_id FROM category c
			JOIN tree t ON c.parent_id = t.id
			WHERE c.household_id = $1 AND c.is_active
		)
		SELECT tops.id, tops.name,
		       (SELECT COALESCE(SUM(COALESCE(c.budget_per_month, 0)), 0)
		        FROM category c JOIN tree t ON t.id = c.id
		        WHERE c.household_id = $1 AND c.is_active
		          AND t.root_id = tops.id),
		       (SELECT COALESCE(SUM(a.total_amount), 0) FROM "transaction" a
		        WHERE a.household_id = $1 AND a.record_status <> 'void'
		          AND a.class = 'expense'
		          AND a.transaction_date >= $2 AND a.transaction_date < $3
		          AND a.category_id IN (SELECT t.id FROM tree t
		                               WHERE t.root_id = tops.id))
		FROM category tops
		WHERE tops.household_id = $1 AND tops.is_active
		  AND tops.parent_id IS NULL AND tops.transaction_class = 'expense'
		ORDER BY tops.sort_order, tops.name`, hh, monthStart, monthEnd)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var c dashCatRow
		var id int64
		if err := rows.Scan(&id, &c.Name, &c.Budget, &c.Actual); err != nil {
			rows.Close()
			http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		cats = append(cats, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var spendTotal, budgetTotal, actualOnBudget money.Money
	for _, c := range cats {
		spendTotal = spendTotal.Add(c.Actual)
		if c.Budget > 0 {
			budgetTotal = budgetTotal.Add(c.Budget)
			actualOnBudget = actualOnBudget.Add(c.Actual)
		}
	}

	// Card 2: credit card balances.
	var cardBals []dashCardBal
	rows, err = a.Pool.Query(ctx, `
		SELECT a.name,
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
		        FROM account x WHERE x.id = a.id),
		       (SELECT COALESCE(SUM(s.amount - COALESCE(al.a, 0)), 0)
		        FROM split s
		        JOIN "transaction" t ON t.id = s.transaction_id
		        LEFT JOIN (SELECT split_id, SUM(amount) AS a
		            FROM payment_allocation GROUP BY split_id) al ON al.split_id = s.id
		        WHERE t.household_id = $1 AND t.record_status <> 'void'
		          AND t.class = 'expense' AND (
		            (t.origin_type = 'account' AND t.origin_ref = a.id)
		            OR (t.destination_type = 'account' AND t.destination_ref = a.id)))
		FROM account a
		WHERE a.household_id = $1 AND a.account_type = 'credit_card' AND a.is_active
		ORDER BY a.sort_order, a.name`, hh)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var c dashCardBal
		if err := rows.Scan(&c.Name, &c.Recorded, &c.Outstanding); err != nil {
			rows.Close()
			http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		cardBals = append(cardBals, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Card 3: outstanding per person across all credit cards.
	var personOuts []dashPersonOut
	rows, err = a.Pool.Query(ctx, `
		SELECT COALESCE(CASE s.participant_type
		                  WHEN 'household_user' THEN u.display_name
		                  ELSE cp.name END, 'Unknown'),
		       SUM(s.amount - COALESCE(al.a, 0))
		FROM split s
		JOIN "transaction" t ON t.id = s.transaction_id
		JOIN account a ON a.account_type = 'credit_card' AND a.is_active
		  AND a.household_id = $1 AND (
			(t.origin_type = 'account' AND t.origin_ref = a.id)
			OR (t.destination_type = 'account' AND t.destination_ref = a.id))
		LEFT JOIN "user" u ON s.participant_type = 'household_user' AND u.id = s.participant_ref
		LEFT JOIN counterparty cp ON s.participant_type = 'counterparty' AND s.participant_ref = cp.id
		LEFT JOIN (SELECT split_id, SUM(amount) AS a
			FROM payment_allocation GROUP BY split_id) al ON al.split_id = s.id
		WHERE t.household_id = $1 AND t.record_status <> 'void' AND t.class = 'expense'
		GROUP BY 1
		HAVING SUM(s.amount - COALESCE(al.a, 0)) > 0
		ORDER BY 2 DESC`, hh)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var p dashPersonOut
		if err := rows.Scan(&p.Person, &p.Due); err != nil {
			rows.Close()
			http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		personOuts = append(personOuts, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Card 4: pending review.
	var pending int
	if err := a.Pool.QueryRow(ctx, `
		SELECT count(*) FROM "transaction"
		WHERE household_id = $1 AND record_status = 'pending'`, hh).Scan(&pending); err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	a.render(w, r, http.StatusOK, "dashboard.html", map[string]any{
		"Title":        "Dashboard",
		"Flash":        flashFrom(r),
		"MonthLabel":   now.Format("January 2006"),
		"Cats":         cats,
		"SpendTotal":   spendTotal,
		"BudgetTotal":   budgetTotal,
		"ActualOnBudget": actualOnBudget,
		"BudgetRemaining": budgetTotal.Sub(actualOnBudget),
		"CardBals":     cardBals,
		"PersonOuts":   personOuts,
		"Pending":      pending,
	})
}
