// D30 §D + ADR-0003: household ownership transfer.
//
// The current owner only may initiate; the recipient is any current
// non-owner member of the same household. The owner's password is
// re-verified server-side BEFORE anything changes (a wrong password
// re-shows the modal with an error). Effect: the household_member roles
// swap — the old owner becomes a member with a full admin grant row, the
// recipient becomes the owner with the owner-shaped grant row. The old
// owner keeps full access but is no longer untouchable.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
)

// transferRecipient is one non-owner member who may receive the ownership.
type transferRecipient struct {
	UserID int64
	Name   string
}

// ownershipSection loads the Household-tab Ownership section facts: whether
// the user is the household's owner, and the non-owner members (in name
// order) who could receive the transfer.
func (a *App) ownershipSection(ctx context.Context, hh, userID int64) (bool, []transferRecipient, error) {
	var role string
	err := a.Pool.QueryRow(ctx,
		`SELECT role FROM household_member WHERE user_id = $1 AND household_id = $2`,
		userID, hh).Scan(&role)
	if err != nil {
		return false, nil, err
	}
	isOwner := role == "owner"
	rows, err := a.Pool.Query(ctx,
		`SELECT m.user_id, COALESCE(NULLIF(u.display_name, ''), u.username)
		 FROM household_member m
		 JOIN app_user u ON u.id = m.user_id
		 WHERE m.household_id = $1 AND m.role <> 'owner'
		 ORDER BY COALESCE(NULLIF(u.display_name, ''), u.username)`, hh)
	if err != nil {
		return isOwner, nil, err
	}
	defer rows.Close()
	var recips []transferRecipient
	for rows.Next() {
		var tr transferRecipient
		if err := rows.Scan(&tr.UserID, &tr.Name); err != nil {
			return isOwner, nil, err
		}
		recips = append(recips, tr)
	}
	return isOwner, recips, rows.Err()
}

// ownerGrantSQL writes the full-access household grant row (the owner/admin
// shape). The old owner receives it after being demoted to member, so they
// keep full access as an admin.
const ownerGrantSQL = `
	INSERT INTO household_grant
	   (user_id, household_id, can_read, can_create, can_edit, can_delete,
	    edit_mode, delete_mode, is_admin)
	VALUES ($1, $2, true, true, true, true, 'all', 'all', true)
	ON CONFLICT (user_id, household_id) DO UPDATE
	SET can_read = true, can_create = true, can_edit = true, can_delete = true,
	    edit_mode = 'all', delete_mode = 'all', is_admin = true`

// handleTransferOwnership is POST /household/transfer-ownership.
func (a *App) handleTransferOwnership(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	sess := sessionFromContext(ctx)
	if sess == nil || sess.Household == 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	hh, user := sess.Household, sess.UserID

	isOwner, _, err := a.ownershipSection(ctx, hh, user)
	if err != nil {
		http.Error(w, "load membership: "+err.Error(), 500)
		return
	}
	if !isOwner {
		http.Error(w, "Only the owner can transfer ownership.", http.StatusForbidden)
		return
	}
	fail := func(msg string) {
		if err := a.renderSettingsHousehold(ctx, w, r, hh, user, "", msg, true); err != nil {
			http.Error(w, "render: "+err.Error(), 500)
		}
	}
	recipID, perr := strconv.ParseInt(r.FormValue("recipient"), 10, 64)
	if perr != nil || recipID <= 0 {
		fail("Pick who will become the owner.")
		return
	}
	var rrole string
	err = a.Pool.QueryRow(ctx,
		`SELECT role FROM household_member WHERE user_id = $1 AND household_id = $2`,
		recipID, hh).Scan(&rrole)
	if errors.Is(err, pgx.ErrNoRows) {
		fail("Pick a current member of this household.")
		return
	}
	if err != nil {
		http.Error(w, "load member: "+err.Error(), 500)
		return
	}
	if rrole == "owner" || recipID == user {
		fail("Pick a member who is not the owner.")
		return
	}
	// Password re-entry — verified before anything changes (ADR-0003).
	var hash string
	if err := a.Pool.QueryRow(ctx,
		`SELECT password_hash FROM app_user WHERE id = $1`, user).Scan(&hash); err != nil {
		http.Error(w, "load account: "+err.Error(), 500)
		return
	}
	if err := auth.CheckPassword(hash, r.FormValue("password")); err != nil {
		fail("The password was incorrect — nothing was changed.")
		return
	}

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "begin: "+err.Error(), 500)
		return
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE household_member SET role = 'member' WHERE user_id = $1 AND household_id = $2`,
		user, hh); err != nil {
		http.Error(w, "demote owner: "+err.Error(), 500)
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE household_member SET role = 'owner' WHERE user_id = $1 AND household_id = $2`,
		recipID, hh); err != nil {
		http.Error(w, "promote recipient: "+err.Error(), 500)
		return
	}
	for _, u := range []int64{user, recipID} {
		if _, err := tx.Exec(ctx, ownerGrantSQL, u, hh); err != nil {
			http.Error(w, "grant: "+err.Error(), 500)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "commit: "+err.Error(), 500)
		return
	}
	var recipName string
	_ = a.Pool.QueryRow(ctx,
		`SELECT COALESCE(NULLIF(display_name, ''), username) FROM app_user WHERE id = $1`,
		recipID).Scan(&recipName)
	a.redirectRef(w, r, "/settings",
		fmt.Sprintf("Ownership transferred: %s is now the owner. You are an admin.", recipName))
}

// renderSettingsHousehold renders the Household settings tab, including the
// Ownership section facts. cardErr is the Default-household card error (GET
// ?err=); transferErr (with modalOpen) re-shows the transfer modal with the
// error after a failed POST.
func (a *App) renderSettingsHousehold(ctx context.Context, w http.ResponseWriter, r *http.Request, hh, user int64, cardErr, transferErr string, modalOpen bool) error {
	ms, err := a.memberships(ctx, user)
	if err != nil {
		return err
	}
	var current int64
	sess := sessionFromContext(r.Context())
	if sess != nil && sess.Household != 0 {
		current = sess.Household
	} else {
		current, _ = a.landHousehold(ctx, user)
	}
	for i := range ms {
		if ms[i].HouseholdID == current {
			ms[i].IsDefault = true
		}
	}
	var isOwner bool
	var recips []transferRecipient
	var first int64
	if hh != 0 {
		isOwner, recips, err = a.ownershipSection(ctx, hh, user)
		if err != nil {
			return err
		}
		if len(recips) > 0 {
			first = recips[0].UserID
		}
	}
	a.renderTabbed(w, r, http.StatusOK, "settings_household.html", "household", map[string]any{
		"Title":             "Settings · Household",
		"Memberships":       ms,
		"Single":            len(ms) == 1,
		"Err":               cardErr,
		"Flash":             flashFrom(r),
		"IsOwner":           isOwner,
		"Recipients":        recips,
		"FirstRecipient":    first,
		"TransferErr":       transferErr,
		"TransferModalOpen": modalOpen,
	})
	return nil
}
