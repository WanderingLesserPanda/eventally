// D31 §B/§C: the History trail (txn_history) — write path, per-row
// visibility, and the /history page. This file owns the audit WRITE side
// (snapshots + row inserts inside the action's own DB transaction) and the
// household HISTORY read side. The D30 deletion log keeps its own file
// (bulk.go); the two share only the per-row visibility rule (perm.go,
// ADR-0005). Keeping write and read here means the expected audit-UI
// redesign touches one place.
package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/WanderingLesserPanda/eventally/internal/money"
)

// auditQuery is the DB surface the audit helpers need. Both *pgxpool.Pool
// and pgx.Tx satisfy it; handlers pass the action's open transaction so the
// snapshot is computed against the action's own (not-yet-committed) writes
// — the row commits with the action (design doc §B5).
type auditQuery interface {
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// historyEvent is one txn_history row, written in the same DB transaction
// as the action that caused it.
type historyEvent struct {
	EntityType string // "transaction" | "settlement"
	EntityID   int64
	Action     string // created | edited | voided | deleted | cleared
	Summary    string // rendered text (diff for edits, leg list for creates)
	Name       string // display snapshot
	EventDate  *time.Time
	Amount     *money.Money // display snapshot (nil = unknown)
	AccountIDs []int64 // access snapshot: the record's surface accounts
	RelatedIDs []int64 // access snapshot: its Related-slice members
	ActorID    int64
}

// writeHistoryTx inserts the event through q (the action's transaction).
func writeHistoryTx(ctx context.Context, q auditQuery, hh int64, ev historyEvent) error {
	if ev.AccountIDs == nil {
		ev.AccountIDs = []int64{}
	}
	if ev.RelatedIDs == nil {
		ev.RelatedIDs = []int64{}
	}
	_, err := q.Exec(ctx, `
		INSERT INTO txn_history (household_id, entity_type, entity_id, action,
		                          summary, name, event_date, amount,
		                          account_ids, related_user_ids, actor_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		hh, ev.EntityType, ev.EntityID, ev.Action, ev.Summary,
		ev.Name, ev.EventDate, ev.Amount,
		ev.AccountIDs, ev.RelatedIDs, ev.ActorID)
	return err
}

// --- write-time snapshots ---------------------------------------------------

// txnHistorySnapshot loads a transaction's display snapshot (name/date/
// amount) plus its WRITE-TIME access snapshot: the account surface (the two
// endpoint accounts) and the people in its Related slice — recorder, split
// parties, and payers of any settlement allocated to its splits.
func (a *App) txnHistorySnapshot(ctx context.Context, q auditQuery, hh, txnID int64) (string, *time.Time, *money.Money, []int64, []int64, error) {
	var (
		name   string
		date   *time.Time
		amount *money.Money
		oAcc   int64
		dAcc   int64
	)
	err := q.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(t.description, ''), COALESCE(m.name, ''), '—'),
		       t.transaction_date, t.total_amount,
		       COALESCE(t.origin_account_id, 0), COALESCE(t.destination_account_id, 0)
		FROM "transaction" t
		LEFT JOIN merchant m ON m.id = t.merchant_id
		WHERE t.id = $1 AND t.household_id = $2`, txnID, hh).
		Scan(&name, &date, &amount, &oAcc, &dAcc)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	var accIDs []int64
	if oAcc > 0 {
		accIDs = append(accIDs, oAcc)
	}
	if dAcc > 0 {
		accIDs = append(accIDs, dAcc)
	}
	relIDs, err := a.relatedUsersOn(ctx, q, hh, txnRelatedSliceSQL, txnID, hh)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	return name, date, amount, accIDs, relIDs, nil
}

// settlementHistorySnapshot loads a settlement's display snapshot (card
// name/date/amount) plus its WRITE-TIME access snapshot: the settlement
// surface (its own account + family, plus the families behind its allocated
// splits — mirroring settlementSurface) and its Related slice (payers of any
// leg + split parties in the allocated splits).
func (a *App) settlementHistorySnapshot(ctx context.Context, q auditQuery, hh, settlementID int64) (string, *time.Time, *money.Money, []int64, []int64, error) {
	var (
		name   string
		date   *time.Time
		amount *money.Money
	)
	err := q.QueryRow(ctx, `
		SELECT COALESCE(a.name, '—'), p.payment_date, p.amount
		FROM settlement p
		LEFT JOIN account a ON a.id = p.account_id
		WHERE p.id = $1 AND p.household_id = $2`, settlementID, hh).
		Scan(&name, &date, &amount)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	accRows, err := q.Query(ctx, `
		SELECT id FROM (
			SELECT st.account_id AS id
			FROM settlement st WHERE st.id = $1
			UNION
			SELECT c.id
			FROM settlement st
			JOIN account c ON c.household_id = $2 AND c.account_type = 'credit_card'
			              AND (c.id = st.account_id OR c.parent_id = st.account_id)
			WHERE st.id = $1 AND st.account_id IS NOT NULL
			UNION
			SELECT t.origin_account_id
			FROM settlement_allocation pa
			JOIN settlement st ON st.id = pa.payment_id
			JOIN transaction_split s ON s.id = pa.split_id
			JOIN "transaction" t ON t.id = s.transaction_id
			WHERE st.id = $1
			UNION
			SELECT c.id
			FROM settlement_allocation pa
			JOIN settlement st ON st.id = pa.payment_id
			JOIN transaction_split s ON s.id = pa.split_id
			JOIN "transaction" t ON t.id = s.transaction_id
			JOIN account c ON c.household_id = $2 AND c.account_type = 'credit_card'
			                AND (c.id = t.origin_account_id OR c.parent_id = t.origin_account_id)
			WHERE st.id = $1 AND t.origin_account_id IS NOT NULL
		) u WHERE id IS NOT NULL`, settlementID, hh)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	var accIDs []int64
	for accRows.Next() {
		var x int64
		if err := accRows.Scan(&x); err != nil {
			accRows.Close()
			return "", nil, nil, nil, nil, err
		}
		accIDs = append(accIDs, x)
	}
	accRows.Close()
	if err := accRows.Err(); err != nil {
		return "", nil, nil, nil, nil, err
	}
	relIDs, err := a.relatedUsersOn(ctx, q, hh, `
		SELECT id FROM (
			SELECT sf.user_id AS id
			FROM settlement_funding sf
			WHERE sf.settlement_id = $1
			UNION
			SELECT pt.app_user_id
			FROM settlement_allocation pa
			JOIN settlement st ON st.id = pa.payment_id
			JOIN transaction_split s ON s.id = pa.split_id
			JOIN party pt ON pt.id = s.party_id
			WHERE st.id = $1
		) u WHERE id IS NOT NULL`,
		settlementID)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	return name, date, amount, accIDs, relIDs, nil
}

// txnRelatedSliceSQL lists the people in a transaction's Related slice
// (recorder, split parties, payers of settlements allocated to its splits)
// as a UNION — parameterised $1 = txn id, $2 = household id. Used by both
// the History snapshot (inside the action tx) and the deletion-log snapshot
// (against the pool, before the delete).
const txnRelatedSliceSQL = `
	SELECT id FROM (
		SELECT t.recorded_by_user_id AS id
		FROM "transaction" t
		WHERE t.id = $1 AND t.household_id = $2
		UNION
		SELECT pt.app_user_id
		FROM transaction_split s
		JOIN party pt ON pt.id = s.party_id
		WHERE s.transaction_id = $1
		UNION
		SELECT sf.user_id
		FROM settlement_funding sf
		JOIN settlement st ON st.id = sf.settlement_id
		WHERE EXISTS (SELECT 1 FROM settlement_allocation pa
		              JOIN transaction_split s2 ON s2.id = pa.split_id
		              WHERE pa.payment_id = st.id AND s2.transaction_id = $1)
	) u WHERE id IS NOT NULL`

// txnRelatedIDs loads the user ids in a transaction's Related slice (D31
// §C2: the deletion-log access snapshot, taken while the record exists).
func (a *App) txnRelatedIDs(ctx context.Context, hh, txnID int64) ([]int64, error) {
	return a.relatedUsersOn(ctx, a.Pool, hh, txnRelatedSliceSQL, txnID, hh)
}

// txnSurfaceIDs loads a transaction's account surface (its endpoint
// accounts) for the deletion-log access snapshot.
func (a *App) txnSurfaceIDs(ctx context.Context, hh, txnID int64) ([]int64, error) {
	var oAcc, dAcc int64
	err := a.Pool.QueryRow(ctx,
		`SELECT COALESCE(origin_account_id, 0), COALESCE(destination_account_id, 0)
		 FROM "transaction" WHERE id = $1 AND household_id = $2`, txnID, hh).Scan(&oAcc, &dAcc)
	if err != nil {
		return nil, err
	}
	var out []int64
	if oAcc > 0 {
		out = append(out, oAcc)
	}
	if dAcc > 0 {
		out = append(out, dAcc)
	}
	return out, nil
}

// relatedUsersOn runs a UNION query (two $1/$2-bound variants of the same
// shape are inlined at the call site) and collects distinct user ids.
func (a *App) relatedUsersOn(ctx context.Context, q auditQuery, hh int64, sql string, args ...any) ([]int64, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- summaries ---------------------------------------------------------------

// nameIDs loads the names of the given ids from a simple id/name reference
// table (category, merchant, account or party).
func nameIDs(ctx context.Context, q auditQuery, table string, ids []int64) map[int64]string {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args = append(args, id)
	}
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT id, name FROM %s WHERE id IN (%s)`, table, strings.Join(ph, ",")), args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			break
		}
		out[id] = name
	}
	return out
}

// txnOldRow is the pre-edit shape of a transaction, loaded inside the save
// transaction for the edited-summary diff (design doc §B2).
type txnOldRow struct {
	Date        time.Time
	Class       string
	Description string
	Total       money.Money
	Notes       string
	CategoryID  *int64
	MerchantID  *int64
	OriginAcc   *int64
	DestAcc     *int64
	OriginParty *int64
	DestParty   *int64
}

// loadTxnOldRow reads the transaction's current field values (the "before"
// side of an edit).
func loadTxnOldRow(ctx context.Context, q auditQuery, hh, txnID int64) (*txnOldRow, error) {
	var row txnOldRow
	err := q.QueryRow(ctx, `
		SELECT transaction_date, class, COALESCE(description, ''), total_amount,
		       COALESCE(notes, ''), category_id, merchant_id,
		       origin_account_id, destination_account_id,
		       origin_party_id, destination_party_id
		FROM "transaction" WHERE id = $1 AND household_id = $2`, txnID, hh).
		Scan(&row.Date, &row.Class, &row.Description, &row.Total, &row.Notes,
			&row.CategoryID, &row.MerchantID,
			&row.OriginAcc, &row.DestAcc, &row.OriginParty, &row.DestParty)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func refName(m map[int64]string, id *int64) string {
	if id == nil || *id == 0 {
		return "—"
	}
	if n, ok := m[*id]; ok && n != "" {
		return n
	}
	return "—"
}

func orDashStr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// txnEditSummary renders the before→after diff of the fields the save form
// can change (design doc §B2: "amount 100.00 → 120.00 · status pending →
// approved"). Names come from the action's own transaction.
func (a *App) txnEditSummary(ctx context.Context, q auditQuery, hh int64, old *txnOldRow, new *txnspec) (string, error) {
	// Collect the ids whose names we need to resolve.
	var catIDs, merchIDs, accIDs, partyIDs []int64
	addID := func(list *[]int64, id *int64) {
		if id != nil && *id > 0 {
			*list = append(*list, *id)
		}
	}
	addID(&catIDs, old.CategoryID)
	addID(&catIDs, nilIfZero(new.categoryID))
	addID(&merchIDs, old.MerchantID)
	addID(&merchIDs, nilIfZero(new.merchantID))
	addID(&accIDs, old.OriginAcc)
	addID(&accIDs, old.DestAcc)
	addID(&partyIDs, old.OriginParty)
	addID(&partyIDs, old.DestParty)
	if new.originType == "account" {
		addID(&accIDs, nilIfZero(new.originRef))
	} else {
		addID(&partyIDs, nilIfZero(new.originRef))
	}
	if new.destType == "account" {
		addID(&accIDs, nilIfZero(new.destRef))
	} else {
		addID(&partyIDs, nilIfZero(new.destRef))
	}
	cats := nameIDs(ctx, q, "category", catIDs)
	merchs := nameIDs(ctx, q, "merchant", merchIDs)
	accs := nameIDs(ctx, q, "account", accIDs)
	parties := nameIDs(ctx, q, "party", partyIDs)

	var parts []string
	diff := func(label, before, after string) {
		if before != after {
			parts = append(parts, label+" "+orDashStr(before)+" → "+orDashStr(after))
		}
	}
	diff("date", old.Date.Format("2006-01-02"), new.date)
	diff("class", old.Class, new.class)
	diff("description", old.Description, new.description)
	diff("amount", old.Total.String(), new.total.String())
	diff("category", refName(cats, old.CategoryID), refName(cats, nilIfZero(new.categoryID)))
	diff("merchant", refName(merchs, old.MerchantID), refName(merchs, nilIfZero(new.merchantID)))
	diff("from", endpointFromOld(old, accs, parties, true),
		endpointName(new.originType, accs, parties, new.originRef))
	diff("to", endpointFromOld(old, accs, parties, false),
		endpointName(new.destType, accs, parties, new.destRef))
	diff("notes", old.Notes, new.notes)
	if len(parts) == 0 {
		return "no changes", nil
	}
	return strings.Join(parts, " · "), nil
}

// endpointName renders one endpoint (account or party) from a resolved name
// map.
func endpointName(typ string, accs, parties map[int64]string, id int64) string {
	if id == 0 {
		return "—"
	}
	if typ == "account" {
		return refName(accs, nilIfZero(id))
	}
	return refName(parties, nilIfZero(id))
}

// endpointFromOld renders the OLD side of an endpoint change: the old row
// stores both endpoint kinds as pointers; whichever is set is the endpoint.
func endpointFromOld(old *txnOldRow, accs, parties map[int64]string, origin bool) string {
	if origin {
		if old.OriginAcc != nil && *old.OriginAcc > 0 {
			return refName(accs, old.OriginAcc)
		}
		return refName(parties, old.OriginParty)
	}
	if old.DestAcc != nil && *old.DestAcc > 0 {
		return refName(accs, old.DestAcc)
	}
	return refName(parties, old.DestParty)
}

// nilIfZero boxes a non-zero id (0/absent → nil = "no endpoint").
func nilIfZero(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

// settlementLegsSummary renders the settlement's funding legs for its
// created-summary: "60.00 from <source> by <payer> + 40.00 by <payer>" (an
// out-of-band leg has no "from" — design doc §B2).
func (a *App) settlementLegsSummary(ctx context.Context, q auditQuery, hh, settlementID int64) (string, error) {
	rows, err := q.Query(ctx, `
		SELECT COALESCE(u.display_name, '—'), COALESCE(ac.name, ''), sf.amount
		FROM settlement_funding sf
		LEFT JOIN app_user u ON u.id = sf.user_id
		LEFT JOIN account ac ON ac.id = sf.source_account_id
		WHERE sf.settlement_id = $1
		ORDER BY sf.id`, settlementID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var payer, source string
		var amount money.Money
		if err := rows.Scan(&payer, &source, &amount); err != nil {
			return "", err
		}
		part := amount.String()
		if source != "" {
			part += " from " + source
		}
		part += " by " + payer
		parts = append(parts, part)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(parts, " + "), nil
}

// --- /history page (household-wide, ADR-0005 per-row visibility) -------------

// historyLine is one row of the /history page (or a record's History
// section) — a tombstone renders with an empty record link.
type historyLine struct {
	At         time.Time
	Actor      string
	EntityType string
	EntityID   int64
	Name       string
	EventDateStr string
	AmountStr  string
	Action     string
	Summary    string
	Link       string // "" for tombstones / deleted entities
}

// historyLinesFor loads the household's History rows, newest first, and
// applies the per-row visibility rule (ADR-0005): a member sees a row only
// if their write-time snapshot would have let them see the record. Owners
// and admins see every row; empty snapshots (the clear tombstone) are
// visible to all members, mirroring the deletion log.
func (a *App) historyLinesFor(ctx context.Context, p *perms, hh int64) ([]historyLine, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT h.created_at, COALESCE(u.display_name, '—'),
		       h.entity_type, h.entity_id, h.action, h.summary,
		       COALESCE(h.name, ''), h.event_date, h.amount,
		       h.account_ids, h.related_user_ids,
		       (SELECT s.account_id FROM settlement s
		         WHERE s.id = h.entity_id AND h.entity_type = 'settlement')
		FROM txn_history h
		LEFT JOIN app_user u ON u.id = h.actor_id
		WHERE h.household_id = $1
		ORDER BY h.created_at DESC, h.id DESC
		LIMIT 500`, hh)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []historyLine
	for rows.Next() {
		var (
			l      historyLine
			date   *time.Time
			amount *money.Money
			accIDs []int64
			relIDs []int64
			cardID *int64
		)
		if err := rows.Scan(&l.At, &l.Actor, &l.EntityType, &l.EntityID,
			&l.Action, &l.Summary, &l.Name, &date, &amount, &accIDs, &relIDs, &cardID); err != nil {
			return nil, err
		}
		if !p.auditRowVisible(accIDs, relIDs) {
			continue
		}
		if date != nil {
			l.EventDateStr = dateFmt(*date)
		}
		if amount != nil {
			l.AmountStr = moneyFmt(*amount)
		}
		if l.EntityType == "transaction" && l.EntityID > 0 {
			l.Link = fmt.Sprintf("/txns/%d", l.EntityID)
		} else if l.EntityType == "settlement" && cardID != nil && *cardID > 0 {
			l.Link = fmt.Sprintf("/cards/%d", *cardID)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// txnHistoryLines loads one record's own events (the collapsible History
// section on /txns/{id}). No extra per-row filter: reaching the record's
// page already implies visibility of the record (design doc §B4).
func (a *App) txnHistoryLines(ctx context.Context, hh, txnID int64) ([]historyLine, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT h.created_at, COALESCE(u.display_name, '—'),
		       h.entity_type, h.entity_id, h.action, h.summary,
		       COALESCE(h.name, ''), h.event_date, h.amount,
		       '{}'::bigint[], '{}'::bigint[], NULL
		FROM txn_history h
		LEFT JOIN app_user u ON u.id = h.actor_id
		WHERE h.household_id = $1 AND h.entity_type = 'transaction' AND h.entity_id = $2
		ORDER BY h.created_at DESC, h.id DESC`, hh, txnID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []historyLine
	for rows.Next() {
		var (
			l      historyLine
			date   *time.Time
			amount *money.Money
			accIDs []int64
			relIDs []int64
			cardID *int64
		)
		if err := rows.Scan(&l.At, &l.Actor, &l.EntityType, &l.EntityID,
			&l.Action, &l.Summary, &l.Name, &date, &amount, &accIDs, &relIDs, &cardID); err != nil {
			return nil, err
		}
		l.Link = fmt.Sprintf("/txns/%d", txnID)
		if date != nil {
			l.EventDateStr = dateFmt(*date)
		}
		if amount != nil {
			l.AmountStr = moneyFmt(*amount)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// handleHistory is GET /history: the household's History trail, newest
// first, per-row visibility applied.
func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, err := a.permFor(r)
	if err != nil {
		http.Error(w, "permission load: "+err.Error(), 500)
		return
	}
	lines, err := a.historyLinesFor(ctx, p, hh)
	if err != nil {
		http.Error(w, "load history: "+err.Error(), 500)
		return
	}
	a.render(w, r, http.StatusOK, "history.html", map[string]any{
		"Title":    "History",
		"Section":  "txns",
		"Flash":    flashFrom(r),
		"Rows":     lines,
		"CanClear": p.canManageGrants(),
	})
}

// handleHistoryClear is POST /history/clear. Owner and admins only. A clear
// removes every row and leaves one tombstone ("History cleared by X on
// <date>") so the trail records that a clear happened. No automatic expiry
// (D30 house pattern).
func (a *App) handleHistoryClear(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, err := a.permFor(r)
	if err != nil {
		http.Error(w, "permission load: "+err.Error(), 500)
		return
	}
	if !p.canManageGrants() {
		http.Error(w, "Only the owner or an admin can clear the history.", http.StatusForbidden)
		return
	}
	var name string
	if err := a.Pool.QueryRow(ctx,
		`SELECT COALESCE(display_name, username) FROM app_user WHERE id = $1`,
		sess.UserID).Scan(&name); err != nil {
		name = strconv.FormatInt(sess.UserID, 10)
	}
	if _, err := a.Pool.Exec(ctx,
		`DELETE FROM txn_history WHERE household_id = $1 AND action <> 'cleared'`, hh); err != nil {
		http.Error(w, "clear history: "+err.Error(), 500)
		return
	}
	tombstone := fmt.Sprintf("History cleared by %s on %s", name, time.Now().Format("2 Jan 2006"))
	if _, err := a.Pool.Exec(ctx, `
		INSERT INTO txn_history (household_id, entity_type, entity_id, action, summary,
		                          name, actor_id)
		VALUES ($1, 'transaction', 0, 'cleared', $2, '—', $3)`,
		hh, tombstone, sess.UserID); err != nil {
		http.Error(w, "tombstone: "+err.Error(), 500)
		return
	}
	a.redirectRef(w, r, "/history", "History cleared.")
}
