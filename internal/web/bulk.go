// D30 §C: bulk void + bulk delete on the /txns list, and the persistent
// deletion log (deletes only — void auditing belongs to the parked
// transaction-history enhancement).
//
// Both bulk actions are partial-success, never all-or-nothing: every record
// is enforced individually against its grants. The flash reports counts only
// ("N record(s) successfully …" plus, only on failure, "M record(s) failed
// to …") — no per-record names.
//
// Bulk DELETE additionally writes one deletion_log row per attempted record
// (snapshot name/date/amount, outcome, reason, actor, operation id). Bulk
// VOID outcomes are NOT logged.
package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/WanderingLesserPanda/eventally/internal/money"
)

// int64sFromForm reads a repeated form field (e.g. ids[]), keeping order and
// deduping; non-positive / unparsable values are dropped.
func int64sFromForm(r *http.Request, key string) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, s := range r.Form[key] {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v <= 0 || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// handleBulkVoid is POST /txns/bulk-void. Per record: the status move follows
// the Edit grant (same rule as the single-record status handler) — an Edit
// grant on the record's account surface, Related mode requiring the actor to
// be in the related slice. Already-voided records fail (void is one-way).
func (a *App) handleBulkVoid(w http.ResponseWriter, r *http.Request) {
	ids := int64sFromForm(r, "ids[]")
	if len(ids) == 0 {
		a.redirectRef(w, r, "/txns", "No records selected.")
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
	oks, fails := 0, 0
	for _, id := range ids {
		surface, inRel, aerr := a.txnAccess(ctx, hh, sess.UserID, id)
		if aerr != nil {
			fails++
			continue
		}
		var cur string
		if err := a.Pool.QueryRow(ctx,
			`SELECT record_status FROM "transaction" WHERE id = $1 AND household_id = $2`,
			id, hh).Scan(&cur); err != nil {
			fails++
			continue
		}
		if cur == "void" {
			fails++
			continue
		}
		if !p.canEditTxn(surface, inRel) {
			fails++
			continue
		}
		if _, err := a.Pool.Exec(ctx,
			`UPDATE "transaction" SET record_status = 'void' WHERE id = $1 AND household_id = $2`,
			id, hh); err != nil {
			fails++
			continue
		}
		oks++
	}
	msg := fmt.Sprintf("%d record(s) successfully voided.", oks)
	if fails > 0 {
		msg += fmt.Sprintf(" %d record(s) failed to void.", fails)
	}
	a.redirectRef(w, r, "/txns", msg)
}

// deleteFailure reasons shown in the deletion log's reason column.
const (
	reasonNotFound  = "record not found"
	reasonNoGrant   = "delete grant not held"
	reasonNotParty  = "Related mode — not a party to this record"
	reasonSettled   = "has settlements"
	reasonHasChild  = "has child transactions"
)

// handleBulkDelete is POST /txns/bulk-delete. Per record: the Delete grant
// check (Related/All mode), then safety blocks — a settlement that references
// the record (its ledger link would be silently orphaned) or child
// transactions (they would lose their parent). Every attempted record gets a
// deletion_log row grouped under one operation id.
func (a *App) handleBulkDelete(w http.ResponseWriter, r *http.Request) {
	ids := int64sFromForm(r, "ids[]")
	if len(ids) == 0 {
		a.redirectRef(w, r, "/txns", "No records selected.")
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
	var opID int64
	if err := a.Pool.QueryRow(ctx, `SELECT nextval('deletion_operation_seq')`).Scan(&opID); err != nil {
		http.Error(w, "operation id: "+err.Error(), 500)
		return
	}

	oks, fails := 0, 0
	for _, id := range ids {
		// Snapshot for the log row (the row disappears on success). Amount is
		// scanned as int64 exactly like the /txns list (money.Money) so the log
		// formats identically.
		var name string
		var date *time.Time
		var amount *int64
		err := a.Pool.QueryRow(ctx,
			`SELECT COALESCE(NULLIF(t.description, ''), COALESCE(m.name, ''), '—'),
			       t.transaction_date, t.total_amount
			 FROM "transaction" t
			 LEFT JOIN merchant m ON m.id = t.merchant_id
			 WHERE t.id = $1 AND t.household_id = $2`, id, hh).
			Scan(&name, &date, &amount)
		logRow := func(outcome, reason string) {
			_, lerr := a.Pool.Exec(ctx,
				`INSERT INTO deletion_log
				   (household_id, operation_id, txn_id, txn_name, txn_date, amount, outcome, reason, actor_id)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				hh, opID, id, name, date, amount, outcome, reason, sess.UserID)
			if lerr != nil {
				// Best-effort audit row; the action's own outcome still
				// reports.
				_ = lerr
			}
		}
		if errors.Is(err, pgx.ErrNoRows) {
			logRow("failed", reasonNotFound)
			fails++
			continue
		}
		if err != nil {
			fails++
			continue
		}
		surface, inRel, aerr := a.txnAccess(ctx, hh, sess.UserID, id)
		if aerr != nil {
			logRow("failed", reasonNotFound)
			fails++
			continue
		}
		if !p.canDeleteTxn(surface, inRel) {
			reason := reasonNoGrant
			for _, sid := range surface {
				if g, gok := p.acct[sid]; gok && g.del && g.delMode == "Related" && !inRel {
					reason = reasonNotParty
				}
			}
			logRow("failed", reason)
			fails++
			continue
		}
		var n int
		if err := a.Pool.QueryRow(ctx,
			`SELECT count(*) FROM settlement WHERE ledger_transaction_id = $1`, id).Scan(&n); err != nil || n > 0 {
			logRow("failed", reasonSettled)
			fails++
			continue
		}
		if err := a.Pool.QueryRow(ctx,
			`SELECT count(*) FROM "transaction"
			 WHERE parent_transaction_id = $1 AND household_id = $2`, id, hh).Scan(&n); err != nil || n > 0 {
			logRow("failed", reasonHasChild)
			fails++
			continue
		}
		if _, err := a.Pool.Exec(ctx,
			`DELETE FROM "transaction" WHERE id = $1 AND household_id = $2`, id, hh); err != nil {
			logRow("failed", err.Error())
			fails++
			continue
		}
		logRow("deleted", "")
		oks++
	}
	msg := fmt.Sprintf("%d record(s) successfully deleted.", oks)
	if fails > 0 {
		msg += fmt.Sprintf(" %d record(s) failed to delete. See the Deletion log for details.", fails)
	}
	a.redirectRef(w, r, "/txns", msg)
}

// --- deletion log ----------------------------------------------------------

// deletionLogGroup is one bulk operation and its per-record rows (newest
// operation first).
type deletionLogGroup struct {
	OperationID int64
	At          time.Time
	Actor       string
	Deleted     int
	Failed      int
	Rows        []deletionLogRow
}

type deletionLogRow struct {
	Name      string
	DateStr   string // "" when the row has no date (tombstones)
	AmountStr string // "" when the row has no amount
	Outcome   string
	Reason    string
}

// handleDeletionLog is GET /deletion-log: newest first, grouped by operation.
func (a *App) handleDeletionLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, err := a.permFor(r)
	if err != nil {
		http.Error(w, "permission load: "+err.Error(), 500)
		return
	}
	rows, err := a.Pool.Query(ctx,
		`SELECT dl.operation_id, dl.created_at, COALESCE(u.display_name, '—'),
		        dl.txn_id, dl.txn_name, dl.txn_date, dl.amount, dl.outcome, dl.reason
		 FROM deletion_log dl
		 LEFT JOIN app_user u ON u.id = dl.actor_id
		 WHERE dl.household_id = $1
		 ORDER BY dl.created_at DESC, dl.id DESC`, hh)
	if err != nil {
		http.Error(w, "load log: "+err.Error(), 500)
		return
	}
	defer rows.Close()
	var groups []deletionLogGroup
	byOp := map[int64]int{}
	for rows.Next() {
		var op int64
		var at time.Time
		var actor string
		var txnID *int64
		var name string
		var date *time.Time
		var amount *int64
		var outcome, reason string
		if err := rows.Scan(&op, &at, &actor, &txnID, &name, &date, &amount, &outcome, &reason); err != nil {
			http.Error(w, "scan log: "+err.Error(), 500)
			return
		}
		i, ok := byOp[op]
		if !ok {
			groups = append(groups, deletionLogGroup{OperationID: op, At: at, Actor: actor})
			i = len(groups) - 1
			byOp[op] = i
		}
		g := &groups[i]
		row := deletionLogRow{Name: name, Outcome: outcome, Reason: reason}
		if date != nil {
			row.DateStr = dateFmt(*date)
		}
		if amount != nil {
			row.AmountStr = moneyFmt(money.Money(*amount))
		}
		g.Rows = append(g.Rows, row)
		if outcome == "deleted" {
			g.Deleted++
		} else if outcome == "failed" {
			g.Failed++
		}
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "read log: "+err.Error(), 500)
		return
	}
	a.render(w, r, http.StatusOK, "deletion_log.html", map[string]any{
		"Title":    "Deletion log",
		"Section":  "txns",
		"Flash":    flashFrom(r),
		"Groups":   groups,
		"CanClear": p.canManageGrants(), // owner or admin
	})
}

// handleDeletionLogClear is POST /deletion-log/clear. Owner and admins only.
// A clear removes every row except the tombstones and leaves one new
// tombstone row ("Log cleared by X on <date>") so the trail records that a
// clear happened. No automatic expiry — retention is manual only.
func (a *App) handleDeletionLogClear(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	hh := sess.Household
	p, err := a.permFor(r)
	if err != nil {
		http.Error(w, "permission load: "+err.Error(), 500)
		return
	}
	if !p.canManageGrants() {
		http.Error(w, "Only the owner or an admin can clear the deletion log.", http.StatusForbidden)
		return
	}
	var name string
	if err := a.Pool.QueryRow(ctx,
		`SELECT COALESCE(display_name, username) FROM app_user WHERE id = $1`,
		sess.UserID).Scan(&name); err != nil {
		name = strconv.FormatInt(sess.UserID, 10)
	}
	var opID int64
	if err := a.Pool.QueryRow(ctx, `SELECT nextval('deletion_operation_seq')`).Scan(&opID); err != nil {
		http.Error(w, "operation id: "+err.Error(), 500)
		return
	}
	if _, err := a.Pool.Exec(ctx,
		`DELETE FROM deletion_log WHERE household_id = $1 AND outcome <> 'cleared'`, hh); err != nil {
		http.Error(w, "clear log: "+err.Error(), 500)
		return
	}
	tombstone := fmt.Sprintf("Log cleared by %s on %s", name, time.Now().Format("2 Jan 2006"))
	if _, err := a.Pool.Exec(ctx,
		`INSERT INTO deletion_log
		   (household_id, operation_id, txn_id, txn_name, txn_date, amount, outcome, reason, actor_id)
		 VALUES ($1, $2, NULL, $3, NULL, NULL, 'cleared', '', $4)`,
		hh, opID, tombstone, sess.UserID); err != nil {
		http.Error(w, "tombstone: "+err.Error(), 500)
		return
	}
	a.redirectRef(w, r, "/deletion-log", "Deletion log cleared.")
}
