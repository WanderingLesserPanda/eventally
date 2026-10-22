// D30: account archive (soft is_active=false) + restore — the reversible
// counterpart to the strict hard delete (reference_accounts.go).
//
// Archiving an account:
//   - flips is_active to false (history stays intact; the account drops out
//     of every new-activity picklist and into the "Inactive accounts"
//     section),
//   - pauses its active recurring templates (is_active=false — the existing
//     pause mechanism; due-date generation only picks active templates),
//   - optionally cascades to its child accounts (the "Also archive child
//     accounts" checkbox) — each child's own templates pause by the same rule.
//
// Restoring flips is_active back to true and nothing else: paused templates
// STAY paused (the user re-enables them individually; a pause is a decision,
// and restore is not an undo of it). Restore never cascades.
//
// Permission: an account-level Edit grant in ALL mode on the account
// (canAllEditOn — a Related-mode edit grant does not qualify, since
// archiving touches every record on the account). Cascading additionally
// requires Edit-All on every active child.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// accountLifecycle carries the archive/restore/delete facts the account
// details page renders (buttons + modal copy).
type accountLifecycle struct {
	CanArchive        bool // account-level Edit grant, mode All (owner: always)
	CanDelete         bool // household-level delete grant (created|all mode)
	ActiveTemplates   int  // recurring templates on the account, currently active
	TotalTemplates    int  // recurring templates on the account, any state
	ChildCount        int  // active child accounts
	CanCascade        bool // the actor holds Edit-All on every active child
}

// accountLifecycleFor loads the lifecycle facts for one account.
func (a *App) accountLifecycleFor(ctx context.Context, hh, id int64, p *perms) (*accountLifecycle, error) {
	lc := &accountLifecycle{
		CanArchive: p.canAllEditOn(id),
	}
	var createdBy int64
	err := a.Pool.QueryRow(ctx,
		`SELECT COALESCE(created_by_user_id, 0) FROM account
		 WHERE id = $1 AND household_id = $2`, id, hh).Scan(&createdBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("not found")
	}
	if err != nil {
		return nil, err
	}
	lc.CanDelete = p.canDeleteAccountObject(id, createdBy)

	if err := a.Pool.QueryRow(ctx,
		`SELECT count(*) FROM recurring_template
		 WHERE household_id = $1 AND account_id = $2 AND is_active`, hh, id).Scan(&lc.ActiveTemplates); err != nil {
		return nil, err
	}
	if err := a.Pool.QueryRow(ctx,
		`SELECT count(*) FROM recurring_template
		 WHERE household_id = $1 AND account_id = $2`, hh, id).Scan(&lc.TotalTemplates); err != nil {
		return nil, err
	}

	rows, err := a.Pool.Query(ctx,
		`SELECT id FROM account
		 WHERE household_id = $1 AND parent_id = $2 AND is_active
		 ORDER BY id`, hh, id)
	if err != nil {
		return nil, err
	}
	var childIDs []int64
	for rows.Next() {
		var cid int64
		if err := rows.Scan(&cid); err != nil {
			rows.Close()
			return nil, err
		}
		childIDs = append(childIDs, cid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lc.ChildCount = len(childIDs)
	lc.CanCascade = true
	for _, cid := range childIDs {
		if !p.canAllEditOn(cid) {
			lc.CanCascade = false
		}
	}
	return lc, nil
}

// applyAccountArchive flips the account (and, when cascade, its active
// children) to inactive and pauses their active recurring templates.
func (a *App) applyAccountArchive(ctx context.Context, hh, id int64, cascade bool) (paused, cascaded int, err error) {
	targets := []int64{id}
	if cascade {
		rows, qerr := a.Pool.Query(ctx,
			`SELECT id FROM account
			 WHERE household_id = $1 AND parent_id = $2 AND is_active
			 ORDER BY id`, hh, id)
		if qerr != nil {
			return 0, 0, qerr
		}
		for rows.Next() {
			var cid int64
			if err := rows.Scan(&cid); err != nil {
				rows.Close()
				return 0, 0, err
			}
			targets = append(targets, cid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, 0, err
		}
		cascaded = len(targets) - 1
	}

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	ph := inPh(targets)
	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`UPDATE account SET is_active = false
		 WHERE household_id = $1 AND id IN (%s)`, ph),
		append([]any{hh}, toAny(targets)...)...); err != nil {
		return 0, 0, err
	}
	var pr interface {
		RowsAffected() int64
	}
	pr, err = tx.Exec(ctx,
		fmt.Sprintf(`UPDATE recurring_template SET is_active = false
		 WHERE household_id = $1 AND is_active AND account_id IN (%s)`, ph),
		append([]any{hh}, toAny(targets)...)...)
	if err != nil {
		return 0, 0, err
	}
	paused = int(pr.RowsAffected())
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return paused, cascaded, nil
}

// applyAccountRestore flips one account back to active. Deliberately does
// NOT touch recurring templates (paused ones stay paused) and does not
// cascade to children.
func (a *App) applyAccountRestore(ctx context.Context, hh, id int64) error {
	res, err := a.Pool.Exec(ctx,
		`UPDATE account SET is_active = true WHERE id = $1 AND household_id = $2`,
		id, hh)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return errors.New("not found")
	}
	return nil
}

// --- handlers ----------------------------------------------------------------

// handleAccountArchive processes the archive confirmation modal.
func (a *App) handleAccountArchive(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
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
	lc, err := a.accountLifecycleFor(ctx, hh, id, p)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	if !lc.CanArchive {
		http.Error(w, "You do not have permission to archive this account.", http.StatusForbidden)
		return
	}
	cascade := r.FormValue("cascade_children") == "1"
	if cascade && !lc.CanCascade {
		http.Error(w, "You do not have permission to archive all linked accounts.", http.StatusForbidden)
		return
	}
	paused, cascaded, err := a.applyAccountArchive(ctx, hh, id, cascade)
	if err != nil {
		http.Error(w, "archive account: "+err.Error(), 500)
		return
	}
	msg := "Account archived."
	if paused > 0 {
		msg += fmt.Sprintf(" %d recurring template(s) paused.", paused)
	}
	if cascaded > 0 {
		msg += fmt.Sprintf(" %d linked account(s) archived as well.", cascaded)
	}
	a.redirectRef(w, r, "/accounts/"+strconv.FormatInt(id, 10), msg)
}

// handleAccountRestore processes the restore confirmation modal.
func (a *App) handleAccountRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.Error(w, "bad id", 400)
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
	var active bool
	err = a.Pool.QueryRow(ctx,
		`SELECT is_active FROM account WHERE id = $1 AND household_id = $2`,
		id, hh).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, "load account: "+err.Error(), 500)
		return
	}
	if !p.canAllEditOn(id) {
		http.Error(w, "You do not have permission to restore this account.", http.StatusForbidden)
		return
	}
	if active {
		a.redirectRef(w, r, "/accounts/"+strconv.FormatInt(id, 10), "Account is already active.")
		return
	}
	if err := a.applyAccountRestore(ctx, hh, id); err != nil {
		http.Error(w, "restore account: "+err.Error(), 500)
		return
	}
	a.redirectRef(w, r, "/accounts/"+strconv.FormatInt(id, 10),
		"Account restored. Previously paused recurring templates were not resumed — "+
			"re-enable them individually on the Recurring page.")
}
