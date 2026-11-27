// D33: cross-household synced records (docs/notes/d33-..., ADR-0006).
//
// This file owns the wave's three named cross-household surfaces — and by
// the extended isolation probe (d33_test.go), nothing else may cross
// households:
//
//  1. The Live view (GET /sync): the only cross-household READ — the
//     caller's Related slice in each other household, filtered to the
//     syncable rule (expense, not voided, caller holds a split share > 0).
//  2. The sync action (POST /sync): server-side re-validation (the caller
//     still belongs to the source household; the record is still syncable),
//     then the ONE centralized upsert below — the seam the parked
//     auto-sync wave must call, never reimplement.
//  3. The void-path cascade (voidTransaction): the only cross-household
//     WRITE — when a record is voided, every synced copy in any household
//     pointing at it is voided in the same transaction, each with a
//     standard History row (actor = whoever voided the source; no reason
//     field — D31 convention).
//  4. The A-side badge read (aSyncedBy, D33 follow-up signed 2026-11-17):
//     for records rendered in household A, WHO synced a copy of each into
//     another household. Render-time only; the badge names the syncing
//     member only and never the target household's name.
//
// D34 added three more (docs/notes/d34-auto-sync-detach.md):
//
//  5. The write-time auto-sync trigger (ADR-0008): after a source record's
//     own commit, the centralized upsert for every toggle-on member in the
//     record's syncable slice — reads the source household's record state +
//     the member's shares. Best-effort; never fails the member's save.
//  6. The source-category list (the settings mapping form): the source
//     household's active category names — the only place a household's
//     CATEGORIES cross.
//  7. The household detail (Live-view header modal + settings section):
//     the source household's name + member names, for the auto-sync
//     toggle's UI.
//
// A synced record is an ordinary "transaction" row in the current
// household on the source household's party endpoint (type='organisation',
// name = the source household's name, upserted per source household):
// source='sync', plain source pointers, amount = the caller's split share
// (never the whole amount), no split rows and no account endpoints — so it
// counts in the category budgets with zero budget-query changes (ADR-0004
// allocation is split-keyed) and is settlement-ineligible by data.

package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/WanderingLesserPanda/eventally/internal/money"
)

// errAlreadyVoid: the centralized void path found the record already void
// (both callers redirect with a friendly message instead of erroring).
var errAlreadyVoid = errors.New("already void")

// errSyncNameClash: the source household's name is taken by a MEMBER party
// row in the target household (same (household_id, name), app_user_id set),
// so the organisation endpoint cannot be upserted.
var errSyncNameClash = errors.New("source household name collides with a member party row")

// syncSource is a re-validated, syncable source record (spec §A).
type syncSource struct {
	HouseholdID int64
	TxnID       int64
	Name        string // the source household's name (the party endpoint name)
	Date        time.Time
	Desc        string
	CategoryID  int64 // the source record's category (mapping key, may be 0)
	Share       money.Money // the caller's split share in the source
}

// syncSourceRow is one Live-view row with its sync state derived against
// the current household (rendered state, never stored).
type syncSourceRow struct {
	SrcHouseholdID int64
	SrcTxnID       int64
	Date           time.Time
	Description    string
	CategoryName   string // the source category (reference), "Parent › child"
	Share          money.Money

	Synced        bool
	LocalTxnID    int64 // the current household's copy (badge link target)
	SourceChanged bool  // source money facts drifted since the last sync
	LocalCat      int64 // preselect on re-sync (classification is local)
	LocalMerch    int64
	Detached      bool // the copy exists but is detached — the Sync button re-links it
}

// syncSection is one other household and its syncable records.
type syncSection struct {
	HouseholdID int64
	Name        string
	Members     []string // D34: the household-detail read (surface 7)
	AutoSync    bool     // D34: the caller's auto-sync toggle state for this household
	Rows        []syncSourceRow
}

// --- Live view (GET /sync) ---------------------------------------------------

// syncLiveSections loads the Live view: for each of the caller's other
// households, the caller's syncable records there, with each row's sync
// state derived against the current household. Surface 1 of 3.
func (a *App) syncLiveSections(ctx context.Context, curHH, user int64) ([]syncSection, error) {
	ms, err := a.memberships(ctx, user)
	if err != nil {
		return nil, err
	}
	var sections []syncSection
	for _, m := range ms {
		if m.HouseholdID == curHH {
			continue
		}
		rows, err := a.syncRowsInHousehold(ctx, curHH, m.HouseholdID, user)
		if err != nil {
			return nil, err
		}
		// D34: the household-detail read (surface 7) — the section's
		// members — and the caller's auto-sync toggle state for it.
		members, err := a.syncHouseholdMembers(ctx, m.HouseholdID)
		if err != nil {
			return nil, err
		}
		auto, err := a.syncToggleState(ctx, user, m.HouseholdID, curHH)
		if err != nil {
			return nil, err
		}
		sections = append(sections, syncSection{
			HouseholdID: m.HouseholdID, Name: m.Name,
			Members: members, AutoSync: auto, Rows: rows,
		})
	}
	return sections, nil
}

// syncRowsInHousehold lists one source household's syncable records — the
// caller's visibility rule there (the exact /txns fragment) filtered by the
// syncability rule — and derives each row's sync state from the current
// household's synced rows.
func (a *App) syncRowsInHousehold(ctx context.Context, curHH, srcHH, user int64) ([]syncSourceRow, error) {
	p, err := a.permLoad(ctx, srcHH, user)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`
		SELECT t.id, t.transaction_date,
		       COALESCE(NULLIF(t.description, ''), COALESCE(p.name, '')),
		       COALESCE(c.name, ''), COALESCE(pc.name, ''),
		       s.amount,
		       loc.id, loc.total_amount, loc.transaction_date,
		       COALESCE(loc.category_id, 0), COALESCE(loc.merchant_id, 0),
		       COALESCE(loc.is_synced, false)
		FROM "transaction" t
		LEFT JOIN category c ON c.id = t.category_id
		LEFT JOIN category pc ON pc.id = c.parent_id
		LEFT JOIN party p ON p.id = t.origin_party_id
		JOIN transaction_split s ON s.transaction_id = t.id AND s.household_id = $1
		JOIN party pt ON pt.id = s.party_id AND pt.app_user_id = $2
		LEFT JOIN "transaction" loc
		       ON loc.household_id = $3
		      AND loc.sync_source_household_id = $1
		      AND loc.sync_source_txn_id = t.id
		WHERE t.household_id = $1
		  AND t.class = 'expense'
		  AND t.record_status <> 'void'
		  AND s.amount > 0
		  AND %s
		ORDER BY t.transaction_date DESC, t.id DESC
		LIMIT 200`, txnVisibleWhereAliased(p, 2, 1, "t"))

	rows, err := a.Pool.Query(ctx, q, srcHH, user, curHH)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []syncSourceRow
	for rows.Next() {
		var (
			r          syncSourceRow
			catName    string
			catParent  string
			locID      sql.NullInt64
			locTotal   *money.Money
			locDate    *time.Time
			locCat     int64
			locMerch   int64
			locSynced  bool
		)
		r.SrcHouseholdID = srcHH
		if err := rows.Scan(&r.SrcTxnID, &r.Date, &r.Description, &catName, &catParent,
			&r.Share, &locID, &locTotal, &locDate, &locCat, &locMerch, &locSynced); err != nil {
			return nil, err
		}
		if catParent != "" {
			r.CategoryName = catParent + " › " + catName
		} else {
			r.CategoryName = catName
		}
		if locID.Valid {
			r.LocalTxnID = locID.Int64
			r.Detached = !locSynced // D34: the copy exists but is detached
			if locSynced {
				r.Synced = true
				r.LocalCat, r.LocalMerch = locCat, locMerch
				// "source changed": the source's money facts no longer match
				// the copy (derived at render time — never stored).
				if locTotal == nil || *locTotal != r.Share || locDate == nil ||
					locDate.Format("2006-01-02") != r.Date.Format("2006-01-02") {
					r.SourceChanged = true
				}
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// handleSyncGET renders the Live view (sync.html). The nav item is hidden
// for members with no other household; reaching the page directly renders
// the empty state.
func (a *App) handleSyncGET(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil || !sess.HasHousehold() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	sections, err := a.syncLiveSections(ctx, sess.Household, sess.UserID)
	if err != nil {
		http.Error(w, "could not load the sync view.", http.StatusInternalServerError)
		return
	}
	a.render(w, r, http.StatusOK, "sync.html", map[string]any{
		"Title":     "From other households",
		"Flash":     flashFrom(r),
		"Sections":  sections,
		"HasOther":  len(sections) > 0,
		"Cats":      a.syncCategoryOptions(ctx, sess.Household),
		"Merchants": a.syncMerchantOptions(ctx, sess.Household),
	})
}

// syncCategoryOptions: this household's active expense categories (the
// classification pick at sync time is the review — §A).
func (a *App) syncCategoryOptions(ctx context.Context, hh int64) []opt {
	var out []opt
	rows, err := a.Pool.Query(ctx, `
		SELECT c.id, c.name, COALESCE(p.name, '')
		FROM category c LEFT JOIN category p ON p.id = c.parent_id
		WHERE c.household_id = $1 AND c.is_active AND c.transaction_class = 'expense'
		ORDER BY c.sort_order, c.name`, hh)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c opt
		if err := rows.Scan(&c.ID, &c.Name, &c.Parent); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// syncMerchantOptions: this household's merchants (optional preselect).
func (a *App) syncMerchantOptions(ctx context.Context, hh int64) []opt {
	var out []opt
	rows, err := a.Pool.Query(ctx, `
		SELECT id, name FROM merchant
		WHERE household_id = $1 AND (is_active
			OR id IN (SELECT merchant_id FROM "transaction"
			             WHERE household_id = $1 AND merchant_id IS NOT NULL))
		ORDER BY name`, hh)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c opt
		if err := rows.Scan(&c.ID, &c.Name); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// --- sync action (POST /sync) --------------------------------------------------

// handleSyncPOST validates and performs a sync / re-sync. Surface 2 of 3.
func (a *App) handleSyncPOST(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil || !sess.HasHousehold() {
		http.Error(w, "sign in first.", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	hh, user := sess.Household, sess.UserID

	srcHH, err := formInt64(r, "source_household_id")
	if err != nil || srcHH <= 0 {
		http.Error(w, "missing source household.", http.StatusBadRequest)
		return
	}
	srcTxn, err := formInt64(r, "source_txn_id")
	if err != nil || srcTxn <= 0 {
		http.Error(w, "missing source record.", http.StatusBadRequest)
		return
	}
	catID, _ := formInt64(r, "category_id") // D34: required only for a first sync
	merchID, _ := formInt64(r, "merchant_id") // optional

	p, err := a.permFor(r)
	if err != nil {
		http.Error(w, "could not load your access.", http.StatusInternalServerError)
		return
	}
	if !p.canCreateTxn(nil) {
		http.Error(w, "you may not create records in this household.", http.StatusForbidden)
		return
	}

	// Server-side re-validation (§B): the caller still belongs to the
	// source household and the record is still syncable there — never
	// trust the GET-time list.
	member, err := a.isMember(ctx, user, srcHH)
	if err != nil {
		http.Error(w, "could not check membership.", http.StatusInternalServerError)
		return
	}
	if !member {
		http.Error(w, "you no longer belong to that household.", http.StatusBadRequest)
		return
	}
	src, err := a.resolveSyncSource(ctx, srcHH, srcTxn, user)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "that record is no longer syncable (it was edited or voided).", http.StatusBadRequest)
			return
		}
		http.Error(w, "could not load that record.", http.StatusInternalServerError)
		return
	}
	// D34: is this household's copy already here? A missing category is
	// fine for a re-sync or a re-link (the row keeps its own); a first
	// sync must pick one.
	var localID int64
	localErr := a.Pool.QueryRow(ctx, `
		SELECT id FROM "transaction"
		WHERE household_id = $1 AND sync_source_household_id = $2 AND sync_source_txn_id = $3`,
		hh, srcHH, srcTxn).Scan(&localID)
	firstSync := errors.Is(localErr, pgx.ErrNoRows)
	if localErr != nil && !errors.Is(localErr, pgx.ErrNoRows) {
		http.Error(w, "could not check for an existing copy.", http.StatusInternalServerError)
		return
	}
	if firstSync && catID <= 0 {
		http.Error(w, "pick the category it counts against.", http.StatusBadRequest)
		return
	}
	if catID > 0 && !a.syncCatFits(ctx, hh, catID) {
		http.Error(w, "choose one of this household's expense categories.", http.StatusBadRequest)
		return
	}
	if merchID > 0 && !a.syncMerchFits(ctx, hh, merchID) {
		http.Error(w, "choose a merchant from this household.", http.StatusBadRequest)
		return
	}

	id, kind, err := a.syncUpsert(ctx, hh, user, src, catID, merchID, "approved")
	if err != nil {
		switch {
		case errors.Is(err, errSyncNameClash):
			http.Error(w, "this household's name collides with a member's name — fix the member row first.", http.StatusBadRequest)
		default:
			http.Error(w, "could not sync the record.", http.StatusInternalServerError)
		}
		return
	}
	switch kind {
	case "relinked":
		a.redirectRef(w, r, "/sync", "Re-linked from "+src.Name+" — amount and date reset to the source's values; the edit lock returns.")
	case "resynced":
		a.redirectRef(w, r, "/sync", "Re-synced from "+src.Name+" — money facts refreshed, your classification kept.")
	default:
		a.redirectRef(w, r, "/sync", "Synced from "+src.Name+" — it now counts in this household's budgets at your share.")
	}
	_ = id
}

// resolveSyncSource re-validates a POST /sync source reference (§B): the
// record exists in that household, is an expense that is not voided, and
// the caller holds a split share > 0 there. pgx.ErrNoRows = not syncable.
func (a *App) resolveSyncSource(ctx context.Context, srcHH, srcTxn, user int64) (syncSource, error) {
	var s syncSource
	err := a.Pool.QueryRow(ctx, `
		SELECT t.id, t.transaction_date,
		       COALESCE(NULLIF(t.description, ''), ''),
		       COALESCE(t.category_id, 0),
		       s.amount, h.name
		FROM "transaction" t
		JOIN household h ON h.id = t.household_id
		JOIN transaction_split s ON s.transaction_id = t.id AND s.household_id = t.household_id
		JOIN party pt ON pt.id = s.party_id AND pt.app_user_id = $3
		WHERE t.id = $2
		  AND t.household_id = $1
		  AND t.class = 'expense'
		  AND t.record_status <> 'void'
		  AND s.amount > 0
		LIMIT 1`, srcHH, srcTxn, user).Scan(&s.TxnID, &s.Date, &s.Desc, &s.CategoryID, &s.Share, &s.Name)
	s.HouseholdID = srcHH
	return s, err
}

// syncCatFits: the category exists in this household, is active, and is an
// expense class (a synced row is always an expense).
func (a *App) syncCatFits(ctx context.Context, hh, catID int64) bool {
	var ok bool
	err := a.Pool.QueryRow(ctx, `
		SELECT TRUE FROM category
		WHERE id = $1 AND household_id = $2 AND is_active AND transaction_class = 'expense'`,
		catID, hh).Scan(&ok)
	return err == nil && ok
}

// syncMerchFits: the merchant exists in this household and is active.
func (a *App) syncMerchFits(ctx context.Context, hh, merchID int64) bool {
	var ok bool
	err := a.Pool.QueryRow(ctx, `
		SELECT TRUE FROM merchant
		WHERE id = $1 AND household_id = $2 AND is_active`, merchID, hh).Scan(&ok)
	return err == nil && ok
}

// syncUpsert is the ONE centralized sync write (spec §B; ADR-0008's
// constraint): manual sync, auto-sync and re-link ALL write through here —
// there is no second write path. It upserts the source-household party
// endpoint, then upserts the synced record keyed by (household_id, source
// pointers) — the UNIQUE constraint makes a duplicate or re-link never
// create a second row.
//
// status: 'approved' (manual sync — the category pick at sync time IS the
// review) or 'pending' (auto-sync — the copy lands in the review queue with
// the mapped category pre-filled). The flag is_synced is set true on every
// write (ADR-0007): a first sync creates the synced row; a re-sync
// refreshes amount/date only and keeps the row's classification; a
// re-link (the row exists but is_synced = false) flips the flag back on,
// clears detached_at, resets amount/date from the source, and KEEPS the
// classification/status/merchant/notes.
//
// kind: "created" | "resynced" | "relinked".
func (a *App) syncUpsert(ctx context.Context, hh, actor int64, src syncSource, categoryID, merchantID int64, status string) (int64, string, error) {
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback(ctx)

	// 1. The source household's party endpoint: one organisation party per
	//    source household, upserted on (household_id, name).
	var partyID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO party (household_id, name, type)
		VALUES ($1, $2, 'organisation')
		ON CONFLICT (household_id, name) DO UPDATE
			SET type = 'organisation'
		WHERE party.app_user_id IS NULL
		RETURNING id`, hh, src.Name).Scan(&partyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", errSyncNameClash
	}
	if err != nil {
		return 0, "", err
	}

	// 2. The synced record: first sync (INSERT), re-sync, or re-link.
	var (
		id            int64
		existingDate  time.Time
		existingTotal money.Money
		existingSynced bool
	)
	err = tx.QueryRow(ctx, `
		SELECT id, total_amount, transaction_date, is_synced
		FROM "transaction"
		WHERE household_id = $1
		  AND sync_source_household_id = $2
		  AND sync_source_txn_id = $3`, hh, src.HouseholdID, src.TxnID).
		Scan(&id, &existingTotal, &existingDate, &existingSynced)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = tx.QueryRow(ctx, `
			INSERT INTO "transaction" (
				household_id, recorded_by_user_id, transaction_date, class,
				category_id, merchant_id, description,
				total_amount, origin_party_id,
				record_status, source, is_synced,
				sync_source_household_id, sync_source_txn_id)
			VALUES ($1, $2, $3, 'expense', $4, $5, $6, $7, $8,
			        $9, 'sync', true, $10, $11)
			RETURNING id`,
			hh, actor, src.Date, nilIfZero(categoryID), nilIfZero(merchantID), src.Desc,
			src.Share, partyID, status, src.HouseholdID, src.TxnID).Scan(&id)
		if err != nil {
			return 0, "", err
		}
		if err := a.writeTxnHistoryEvent(ctx, tx, hh, id, "created", "synced from "+src.Name, actor); err != nil {
			return 0, "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, "", err
		}
		return id, "created", nil
	case err != nil:
		return 0, "", err
	}

	if !existingSynced {
		// Re-link (spec §D): the row exists but is detached. The flag
		// flips back on, the money facts reset to the source, and the
		// classification/status/merchant/notes the member chose are kept.
		if _, err := tx.Exec(ctx, `
			UPDATE "transaction"
			SET is_synced = true, detached_at = NULL,
			    total_amount = $3, transaction_date = $4
			WHERE id = $1 AND household_id = $2`, id, hh, src.Share, src.Date); err != nil {
			return 0, "", err
		}
		if err := a.writeTxnHistoryEvent(ctx, tx, hh, id, "edited",
			"re-linked from "+src.Name+" — amount and date reset to the source", actor); err != nil {
			return 0, "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, "", err
		}
		return id, "relinked", nil
	}

	// Re-sync: refresh the money facts from the source ONLY — the
	// copy keeps its category/merchant/description/notes/status.
	if _, err := tx.Exec(ctx, `
		UPDATE "transaction"
		SET total_amount = $3, transaction_date = $4
		WHERE id = $1 AND household_id = $2`, id, hh, src.Share, src.Date); err != nil {
		return 0, "", err
	}
	var parts []string
	if existingTotal != src.Share {
		parts = append(parts, "amount "+existingTotal.String()+" → "+src.Share.String())
	}
	if existingDate.Format("2006-01-02") != src.Date.Format("2006-01-02") {
		parts = append(parts, "date "+existingDate.Format("2006-01-02")+" → "+src.Date.Format("2006-01-02"))
	}
	summary := strings.Join(parts, " · ")
	if summary == "" {
		summary = "no changes"
	}
	if err := a.writeTxnHistoryEvent(ctx, tx, hh, id, "edited", summary, actor); err != nil {
		return 0, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", err
	}
	return id, "resynced", nil
}

// writeTxnHistoryEvent writes a standard History row (display + access
// snapshot, same shape as the regular edit/void path) inside the caller's
// transaction.
func (a *App) writeTxnHistoryEvent(ctx context.Context, tx pgx.Tx, hh, txnID int64, action, summary string, actor int64) error {
	name, date, amount, accIDs, relIDs, err := a.txnHistorySnapshot(ctx, tx, hh, txnID)
	if err != nil {
		return err
	}
	return writeHistoryTx(ctx, tx, hh, historyEvent{
		EntityType: "transaction", EntityID: txnID,
		Action: action, Summary: summary,
		Name: name, EventDate: date, Amount: amount,
		AccountIDs: accIDs, RelatedIDs: relIDs, ActorID: actor,
	})
}

// --- void-path cascade ----------------------------------------------------------

// voidTransaction is the centralized void path shared by single void and
// bulk void (spec §C): the record is set to void, its History row is
// written, and every synced copy in any household pointing at it is voided
// in the same transaction — a void is a fact, the cascade is immediate, and
// each cascaded copy gets its own History row (actor = whoever voided the
// source; no reason field — D31 convention). App-level, never a DB trigger
// (ADR-0005: every lifecycle event must land a History row with an actor).
// Surface 3 of 3 — the only cross-household WRITE.
func (a *App) voidTransaction(ctx context.Context, actor, hh, txnID int64) error {
	var cur string
	err := a.Pool.QueryRow(ctx, `
		SELECT record_status FROM "transaction"
		WHERE id = $1 AND household_id = $2`, txnID, hh).Scan(&cur)
	if err != nil {
		return err
	}
	if cur == "void" {
		return errAlreadyVoid
	}

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE "transaction" SET record_status = 'void'
		WHERE id = $1 AND household_id = $2`, txnID, hh); err != nil {
		return err
	}
	summary := "voided"
	if cur != "pending" {
		summary = "status " + cur + " → void"
	}
	if err := a.writeTxnHistoryEvent(ctx, tx, hh, txnID, "voided", summary, actor); err != nil {
		return err
	}

	// The cascade: the CURRENTLY-synced copies of this record, in any
	// household. D34: a detached copy (is_synced = false) is an ordinary
	// local record — it survives a source void.
	rows, err := tx.Query(ctx, `
		SELECT id, household_id, record_status
		FROM "transaction"
		WHERE sync_source_household_id = $1
		  AND sync_source_txn_id = $2
		  AND is_synced = true
		  AND record_status <> 'void'`, hh, txnID)
	if err != nil {
		return err
	}
	var copies []struct {
		id     int64
		hh     int64
		status string
	}
	for rows.Next() {
		var c struct {
			id     int64
			hh     int64
			status string
		}
		if err := rows.Scan(&c.id, &c.hh, &c.status); err != nil {
			rows.Close()
			return err
		}
		copies = append(copies, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, c := range copies {
		csum := "voided"
		if c.status != "pending" {
			csum = "status " + c.status + " → void"
		}
		if _, err := tx.Exec(ctx, `
			UPDATE "transaction" SET record_status = 'void'
			WHERE id = $1 AND household_id = $2`, c.id, c.hh); err != nil {
			return err
		}
		if err := a.writeTxnHistoryEvent(ctx, tx, c.hh, c.id, "voided", csum, actor); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// endpointOf reads an old record's origin or destination endpoint
// ("account"/"party" + id; ""/0 when unset) — the D33 edit-lock compares
// the posted form against these.
func endpointOf(old *txnOldRow, origin bool) (string, int64) {
	if origin {
		if old.OriginAcc != nil && *old.OriginAcc > 0 {
			return "account", *old.OriginAcc
		}
		if old.OriginParty != nil && *old.OriginParty > 0 {
			return "party", *old.OriginParty
		}
	} else {
		if old.DestAcc != nil && *old.DestAcc > 0 {
			return "account", *old.DestAcc
		}
		if old.DestParty != nil && *old.DestParty > 0 {
			return "party", *old.DestParty
		}
	}
	return "", 0
}

// formInt64 parses an optional int64 form field ("" → 0).
func formInt64(r *http.Request, name string) (int64, error) {
	s := strings.TrimSpace(r.FormValue(name))
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// aSyncedBy is the fourth named cross-household surface (D33 follow-up,
// signed 2026-11-17): for the records being rendered in household A, the
// members who synced a copy of each into another household — the source
// side's "synced by [member]" chip.
//
// Render-time only: nothing is stored on the A side, so un-syncing a copy
// removes the annotation with no tombstone (the un-sync trace lives in the
// target household's deletion log).
//
// Leak rule: the badge names the syncing MEMBER only. The query reads just
// the copy rows' sync pointers + recorded_by_user_id; it must never SELECT
// the target household's name or any target-household party name (an
// A-only member must not learn where the copy went).
//
// D34: the badge follows is_synced, not source — a detached copy stops
// showing as synced by anyone (the flag returns on re-link).
func (a *App) aSyncedBy(ctx context.Context, hh int64, ids []int64) map[int64][]string {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out
	}
	rows, err := a.Pool.Query(ctx, `
		SELECT c.sync_source_txn_id, c.recorded_by_user_id
		FROM "transaction" c
		WHERE c.sync_source_household_id = $1
		  AND c.sync_source_txn_id = ANY($2)
		  AND c.is_synced = true`, hh, ids)
	if err != nil {
		return out
	}
	defer rows.Close()
	perSrc := map[int64][]int64{}
	actors := map[int64]struct{}{}
	for rows.Next() {
		var src, actor int64
		if err := rows.Scan(&src, &actor); err != nil {
			continue
		}
		perSrc[src] = append(perSrc[src], actor)
		actors[actor] = struct{}{}
	}
	if len(actors) == 0 {
		return out
	}
	var actorIDs []int64
	for id := range actors {
		actorIDs = append(actorIDs, id)
	}
	sort.Slice(actorIDs, func(i, j int) bool { return actorIDs[i] < actorIDs[j] })
	names := map[int64]string{}
	rows2, err := a.Pool.Query(ctx, `
		SELECT id, COALESCE(NULLIF(display_name, ''), username)
		FROM app_user WHERE id = ANY($1)`, actorIDs)
	if err != nil {
		return out
	}
	defer rows2.Close()
	for rows2.Next() {
		var id int64
		var n string
		if err := rows2.Scan(&id, &n); err != nil {
			continue
		}
		names[id] = n
	}
	for src, acts := range perSrc {
		sort.Slice(acts, func(i, j int) bool { return acts[i] < acts[j] })
		seen := map[int64]struct{}{}
		var chip []string
		for _, act := range acts {
			if _, dup := seen[act]; dup {
				continue
			}
			seen[act] = struct{}{}
			if n, ok := names[act]; ok && n != "" {
				chip = append(chip, n)
			}
		}
		if len(chip) > 0 {
			out[src] = chip
		}
	}
	return out
}

// --- D34: auto-sync (ADR-0008) ---------------------------------------------------

// syncHouseholdMembers is the household-detail read (surface 7): the
// source household's member display names, user.id order. The Live view's
// household header modal and the settings section both show them.
func (a *App) syncHouseholdMembers(ctx context.Context, srcHH int64) ([]string, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT COALESCE(NULLIF(u.display_name, ''), u.username)
		FROM household_member m
		JOIN app_user u ON u.id = m.user_id
		WHERE m.household_id = $1
		ORDER BY u.id`, srcHH)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// syncToggleState reports whether the member's auto-sync toggle is ON for
// (source household → target household). Household-local read.
func (a *App) syncToggleState(ctx context.Context, member, srcHH, targetHH int64) (bool, error) {
	var on bool
	err := a.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM auto_sync
			WHERE member_user_id = $1 AND source_household_id = $2 AND target_household_id = $3)`,
		member, srcHH, targetHH).Scan(&on)
	return on, err
}

// mappingCategory resolves the target category for an auto-synced copy from
// the household's category mapping (spec §B), or 0 — no rule, the copy
// arrives without a category and the review pop-up forces one.
// Household-local read (the rule belongs to the target household).
func (a *App) mappingCategory(ctx context.Context, targetHH, srcHH, srcCat int64) int64 {
	if srcCat == 0 {
		return 0
	}
	var id int64
	err := a.Pool.QueryRow(ctx, `
		SELECT target_category_id FROM category_mapping
		WHERE household_id = $1 AND source_household_id = $2 AND source_category_id = $3`,
		targetHH, srcHH, srcCat).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// autoSyncSweep runs the centralized upsert over the member's currently-
// syncable backlog in the source household — the catch-up sweep that is
// part of turning the toggle ON (spec Q18). Every write goes through
// syncUpsert (surface 2); the copy lands PENDING with the mapped category
// pre-filled (or none).
func (a *App) autoSyncSweep(ctx context.Context, srcHH, targetHH, member int64) error {
	srcs, err := a.autoSyncable(ctx, srcHH, member)
	if err != nil {
		return err
	}
	for _, src := range srcs {
		if _, _, err := a.syncUpsert(ctx, targetHH, member, src, a.mappingCategory(ctx, targetHH, srcHH, src.CategoryID), 0, "pending"); err != nil {
			return err
		}
	}
	return nil
}

// autoSyncable lists the member's syncable records in the source household
// (expense, not voided, split share > 0), id order. The trigger's and the
// catch-up's read — run in the source household's context (surface 5).
func (a *App) autoSyncable(ctx context.Context, srcHH, member int64) ([]syncSource, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT t.id, t.transaction_date,
		       COALESCE(NULLIF(t.description, ''), ''),
		       COALESCE(t.category_id, 0),
		       MAX(s.amount), h.name
		FROM "transaction" t
		JOIN household h ON h.id = t.household_id
		JOIN transaction_split s ON s.transaction_id = t.id AND s.household_id = t.household_id
		JOIN party pt ON pt.id = s.party_id AND pt.app_user_id = $2
		WHERE t.household_id = $1
		  AND t.class = 'expense'
		  AND t.record_status <> 'void'
		  AND s.amount > 0
		GROUP BY t.id, h.id
		ORDER BY t.id`, srcHH, member)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []syncSource
	for rows.Next() {
		var s syncSource
		if err := rows.Scan(&s.TxnID, &s.Date, &s.Desc, &s.CategoryID, &s.Share, &s.Name); err != nil {
			return nil, err
		}
		s.HouseholdID = srcHH
		out = append(out, s)
	}
	return out, rows.Err()
}

// autoSyncTrigger is the write-time auto-sync trigger (ADR-0008): after a
// source record's own commit (the saveTxn path — create and edit), it runs
// the centralized upsert for every toggle-on member in the record's
// syncable slice, into each of their target households. Members are
// processed in user.id order, so a two-member conflict resolves
// deterministically (the later upsert wins). A trigger failure NEVER fails
// the member's own save — errors are logged and swallowed. Voids do not
// trigger: the one-way cascade already voids the linked copies.
func (a *App) autoSyncTrigger(ctx context.Context, srcHH, srcTxn int64) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Every toggle-on member with a share in this record, user.id order.
	rows, err := a.Pool.Query(ctx, `
		SELECT as_.member_user_id, as_.target_household_id
		FROM auto_sync as_
		JOIN household_member m ON m.user_id = as_.member_user_id AND m.household_id = $1
		JOIN transaction_split s ON s.transaction_id = $2 AND s.household_id = $1
		JOIN party pt ON pt.id = s.party_id AND pt.app_user_id = m.user_id
		WHERE as_.source_household_id = $1
		  AND s.amount > 0
		ORDER BY as_.member_user_id`, srcHH, srcTxn)
	if err != nil {
		log.Printf("auto-sync trigger: %v", err)
		return
	}
	type target struct{ member, household int64 }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.member, &t.household); err != nil {
			rows.Close()
			log.Printf("auto-sync trigger: %v", err)
			return
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		log.Printf("auto-sync trigger: %v", err)
		return
	}
	rows.Close()
	for _, t := range targets {
		src, err := a.resolveSyncSource(ctx, srcHH, srcTxn, t.member)
		if err != nil {
			continue // no longer syncable (voided / non-expense / lost share)
		}
		if _, _, err := a.syncUpsert(ctx, t.household, t.member, src, a.mappingCategory(ctx, t.household, srcHH, src.CategoryID), 0, "pending"); err != nil {
			log.Printf("auto-sync trigger: member %d → household %d: %v", t.member, t.household, err)
		}
	}
}

// handleSyncAutoPOST is POST /sync/auto — the per-member auto-sync toggle
// (spec §A). Row existence in auto_sync = toggle ON. Turning it ON also
// runs the catch-up sweep over the caller's currently-syncable backlog in
// the source household (spec Q18). `ret` picks the redirect home: the Live
// view modal or the settings section — one setting, two views.
func (a *App) handleSyncAutoPOST(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil || !sess.HasHousehold() {
		http.Error(w, "sign in first.", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	hh, user := sess.Household, sess.UserID

	srcHH, err := formInt64(r, "source_household_id")
	if err != nil || srcHH <= 0 || srcHH == hh {
		http.Error(w, "missing source household.", http.StatusBadRequest)
		return
	}
	on := r.FormValue("on") == "1"
	home := "/sync"
	if r.FormValue("ret") == "settings" {
		home = "/settings/sync"
	}

	member, err := a.isMember(ctx, user, srcHH)
	if err != nil {
		http.Error(w, "could not check membership.", http.StatusInternalServerError)
		return
	}
	if !member {
		http.Error(w, "you no longer belong to that household.", http.StatusBadRequest)
		return
	}

	if on {
		if _, err := a.Pool.Exec(ctx, `
			INSERT INTO auto_sync (member_user_id, source_household_id, target_household_id)
			VALUES ($1, $2, $3)
			ON CONFLICT (member_user_id, source_household_id, target_household_id) DO NOTHING`,
			user, srcHH, hh); err != nil {
			http.Error(w, "could not turn auto-sync on.", http.StatusInternalServerError)
			return
		}
		// The catch-up sweep is part of turning the setting on; it is
		// best-effort — the toggle state is already saved.
		if err := a.autoSyncSweep(ctx, srcHH, hh, user); err != nil {
			log.Printf("auto-sync catch-up: %v", err)
		}
		name, _ := householdName(ctx, a.Pool, srcHH)
		a.redirectRef(w, r, home, "Auto-sync on — new records from "+name+" arrive here as pending, and the backlog was synced now.")
		return
	}

	if _, err := a.Pool.Exec(ctx, `
		DELETE FROM auto_sync
		WHERE member_user_id = $1 AND source_household_id = $2 AND target_household_id = $3`,
		user, srcHH, hh); err != nil {
		http.Error(w, "could not turn auto-sync off.", http.StatusInternalServerError)
		return
	}
	name, _ := householdName(ctx, a.Pool, srcHH)
	a.redirectRef(w, r, home, "Auto-sync off — "+name+" no longer syncs to this household.")
}

// handleTxnDetach is POST /txns/{id}/detach (spec §D) — the B-side copy's
// "Detach from source" action on the detail page, Edit-gated in the target
// household (a modification, not a deletion). One UPDATE: the flag flips
// off, detached_at is set; the source and the pointers are UNCHANGED. A
// History row is written (it may name the source household — it is the B
// side's own log). The record becomes fully ordinary: every field editable,
// the edit lock released, the badge gone, void cascades no longer reach it.
func (a *App) handleTxnDetach(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil || !sess.HasHousehold() {
		http.Error(w, "sign in first.", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	hh := sess.Household

	var (
		srcHH    int64
		srcTxn   int64
		isSynced bool
	)
	err := a.Pool.QueryRow(ctx, `
		SELECT COALESCE(sync_source_household_id, 0),
		       COALESCE(sync_source_txn_id, 0),
		       is_synced
		FROM "transaction"
		WHERE id = $1 AND household_id = $2`, id, hh).Scan(&srcHH, &srcTxn, &isSynced)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not load the record.", http.StatusInternalServerError)
		return
	}
	if !isSynced {
		http.Error(w, "this record is not linked to a source.", http.StatusBadRequest)
		return
	}

	// Edit-gated in the target household (D27: a status/structural move on
	// someone else's record needs the Edit grant).
	p, err := a.permFor(r)
	if err != nil {
		http.Error(w, "could not load your access.", http.StatusInternalServerError)
		return
	}
	surface, inRelated, err := a.txnAccess(ctx, hh, sess.UserID, id)
	if err != nil {
		http.Error(w, "could not load the record.", http.StatusInternalServerError)
		return
	}
	if !p.canEditTxn(surface, inRelated) {
		http.Error(w, "you do not have permission to detach this record.", http.StatusForbidden)
		return
	}

	// The source household's name for the B-side log line — the allowed
	// direction (a B member may see A's name; the leak rule is one-way).
	srcName, _ := householdName(ctx, a.Pool, srcHH)
	if srcName == "" {
		srcName = "its source"
	}

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "could not detach the record.", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE "transaction"
		SET is_synced = false, detached_at = now()
		WHERE id = $1 AND household_id = $2 AND is_synced = true`, id, hh)
	if err != nil {
		http.Error(w, "could not detach the record.", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "this record is already detached.", http.StatusBadRequest)
		return
	}
	if err := a.writeTxnHistoryEvent(ctx, tx, hh, id, "detached", "detached from "+srcName, sess.UserID); err != nil {
		http.Error(w, "could not detach the record.", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "could not detach the record.", http.StatusInternalServerError)
		return
	}
	a.redirectRef(w, r, "/txns/"+strconv.FormatInt(id, 10),
		"Detached — the record is now fully yours: every field is editable, and the source no longer controls it.")
}

// rememberMapping saves a "remember this mapping" rule from the
// single-record approval pop-up (spec §B, Q12): the copy's source category
// — read through the copy's OWN pointers, the only way the source key is
// known — to the picked target category, in the caller's household.
// Upsert on the unique triple: re-remembering retargets the existing rule.
func (a *App) rememberMapping(ctx context.Context, hh, txnID, targetCat int64) error {
	var (
		srcHH  int64
		srcCat int64
	)
	err := a.Pool.QueryRow(ctx, `
		SELECT COALESCE(t.sync_source_household_id, 0),
		       COALESCE(sc.category_id, 0)
		FROM "transaction" t
		LEFT JOIN "transaction" sc
		       ON sc.id = t.sync_source_txn_id
		      AND sc.household_id = t.sync_source_household_id
		WHERE t.id = $1 AND t.household_id = $2`, txnID, hh).Scan(&srcHH, &srcCat)
	if err != nil {
		return err
	}
	if srcHH == 0 || srcCat == 0 || targetCat == 0 {
		return errors.New("this record has no source category to map")
	}
	_, err = a.Pool.Exec(ctx, `
		INSERT INTO category_mapping (household_id, source_household_id, source_category_id, target_category_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (household_id, source_household_id, source_category_id)
		DO UPDATE SET target_category_id = EXCLUDED.target_category_id`,
		hh, srcHH, srcCat, targetCat)
	return err
}

