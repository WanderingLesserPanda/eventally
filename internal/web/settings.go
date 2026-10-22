// D28 permission-UI wave: the Settings page (3 tabs) + grant management.
//
// Tabs: Household (every member: default-household picker), Invites and
// Grants (owner + admin only — D27 policy: admins manage every non-owner
// member, never the owner). Non-admin members see only the Household tab.
package web

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// renderTabbed renders a settings tab page inside the shared tab strip.
// tab is "household" | "invites" | "grants".
func (a *App) renderTabbed(w http.ResponseWriter, r *http.Request, status int, page, tab string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Tab"] = tab
	data["CanManage"] = a.canManageGrants(r)
	a.render(w, r, status, page, data)
}

// handleSettings renders the requested tab. Invites/Grants require the
// admin tier; non-admins fall back to the Household tab.
func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "household"
	}
	switch tab {
	case "invites":
		if a.canManageGrants(r) {
			a.handleInvites(w, r)
			return
		}
		tab = "household"
	case "grants":
		if a.canManageGrants(r) {
			a.handleGrants(w, r)
			return
		}
		tab = "household"
	}
	a.handleSettingsHousehold(w, r)
}

// handleSettingsHousehold is the Household tab: the caller's default
// (login-landing) household picker (disabled when they belong to exactly one
// household) plus the Ownership section (owner only, D30).
func (a *App) handleSettingsHousehold(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	hh := sess.Household
	if hh == 0 {
		hh, _ = a.landHousehold(r.Context(), sess.UserID)
	}
	if err := a.renderSettingsHousehold(r.Context(), w, r, hh, sess.UserID,
		r.URL.Query().Get("err"), "", false); err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
	}
}

// handleDefaultHouseholdPOST sets the login-landing household (and, for
// convenience, switches the session to it).
func (a *App) handleDefaultHouseholdPOST(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	target, _ := strconv.ParseInt(r.FormValue("household_id"), 10, 64)
	ok, err := a.isMember(r.Context(), sess.UserID, target)
	if err != nil || !ok {
		http.Error(w, "not a member of that household", http.StatusBadRequest)
		return
	}
	if err := a.setDefaultHousehold(r.Context(), sess.UserID, target); err != nil {
		http.Error(w, "set default: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.Sessions.SetHousehold(r.Context(), sess.ID, target); err != nil {
		http.Error(w, "switch: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// --- Grants tab (owner + admin only) -----------------------------------------

type grantRow struct {
	UserID     int64
	Username   string
	Display    string
	Role       string
	IsOwner    bool
	// Household grant.
	Read   bool
	Create bool
	Edit   bool
	Delete bool
	EditMode   string
	DeleteMode string
	Admin    bool
}

type acctGrantRow struct {
	AccountID int64
	Name      string
	Members   []memberAcctGrant
}

type memberAcctGrant struct {
	UserID    int64
	Username  string
	IsOwner   bool
	Read      bool
	Create    bool
	Edit      bool
	Delete    bool
	ViewMode  string
	EditMode  string
	DeleteMode string
}

// handleGrants lists household grants (one row per member) and each
// account's per-member grants for the collapsible panels.
func (a *App) handleGrants(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if !a.canManageGrants(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	hh := sess.Household

	// Household-level grants, joined with membership for the role.
	var hRows []grantRow
	rows, err := a.Pool.Query(ctx, `
		SELECT m.user_id, u.username, u.display_name, m.role, m.role = 'owner',
		       COALESCE(g.can_read, FALSE), COALESCE(g.can_create, FALSE),
		       COALESCE(g.can_edit, FALSE), COALESCE(g.can_delete, FALSE),
		       COALESCE(g.edit_mode, 'created'), COALESCE(g.delete_mode, 'created'),
		       COALESCE(g.is_admin, FALSE)
		FROM household_member m
		JOIN app_user u ON u.id = m.user_id
		LEFT JOIN household_grant g
			ON g.user_id = m.user_id AND g.household_id = m.household_id
		WHERE m.household_id = $1
		ORDER BY (m.role = 'owner') DESC, u.display_name`, hh)
	if err != nil {
		http.Error(w, "list grants: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var g grantRow
		if err := rows.Scan(&g.UserID, &g.Username, &g.Display, &g.Role, &g.IsOwner,
			&g.Read, &g.Create, &g.Edit, &g.Delete, &g.EditMode, &g.DeleteMode, &g.Admin); err != nil {
			rows.Close()
			http.Error(w, "scan grants: "+err.Error(), http.StatusInternalServerError)
			return
		}
		hRows = append(hRows, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		http.Error(w, "iter grants: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Per-account grants (only active accounts; each row = one member with a
	// grant on that account).
	var aRows []acctGrantRow
	seen := map[int64]bool{}
	arows, err := a.Pool.Query(ctx, `
		SELECT a.id, a.name
		FROM account a
		WHERE a.household_id = $1 AND a.is_active
		ORDER BY a.sort_order, a.name`, hh)
	if err != nil {
		http.Error(w, "list accounts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for arows.Next() {
		var acc int64
		var name string
		if err := arows.Scan(&acc, &name); err != nil {
			arows.Close()
			http.Error(w, "scan accounts: "+err.Error(), http.StatusInternalServerError)
			return
		}
		var ar acctGrantRow
		ar.AccountID, ar.Name = acc, name
		seen[acc] = true
		// Every member of the household (grant optional) — a member with no
		// row yet still gets an editable panel.
		rows2, err := a.Pool.Query(ctx, `
			SELECT m.user_id, u.username, m.role = 'owner',
			       COALESCE(g.can_read, FALSE), COALESCE(g.can_create, FALSE),
			       COALESCE(g.can_edit, FALSE), COALESCE(g.can_delete, FALSE),
			       COALESCE(g.view_mode, 'Related'), COALESCE(g.edit_mode, 'Related'),
			       COALESCE(g.delete_mode, 'Related')
			FROM household_member m
			JOIN app_user u ON u.id = m.user_id
			LEFT JOIN account_grant g
				ON g.user_id = m.user_id AND g.household_id = m.household_id AND g.account_id = $2
			WHERE m.household_id = $1
			ORDER BY (m.role = 'owner') DESC, u.display_name`, hh, acc)
		if err != nil {
			arows.Close()
			http.Error(w, "list acct grants: "+err.Error(), http.StatusInternalServerError)
			return
		}
		for rows2.Next() {
			var m memberAcctGrant
			if err := rows2.Scan(&m.UserID, &m.Username, &m.IsOwner, &m.Read, &m.Create,
				&m.Edit, &m.Delete, &m.ViewMode, &m.EditMode, &m.DeleteMode); err != nil {
				rows2.Close()
				arows.Close()
				http.Error(w, "scan acct grants: "+err.Error(), http.StatusInternalServerError)
				return
			}
			ar.Members = append(ar.Members, m)
		}
		rows2.Close()
		if err := rows2.Err(); err != nil {
			arows.Close()
			http.Error(w, "iter acct grants: "+err.Error(), http.StatusInternalServerError)
			return
		}
		aRows = append(aRows, ar)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		http.Error(w, "iter accounts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = seen

	a.renderTabbed(w, r, http.StatusOK, "settings_grants.html", "grants", map[string]any{
		"Title": "Settings · Grants",
		"HouseholdGrants": hRows,
		"Accounts":   aRows,
	})
}

// --- grant write handlers (owner + admin; the owner row is untouchable) -----

// manageTarget checks the caller may manage the target member's grants:
// never the owner (D27), only non-owners, and the caller must be owner or
// admin.
func (a *App) manageTarget(r *http.Request, targetUserID int64) (*perms, bool) {
	sess := sessionFromContext(r.Context())
	p, err := a.permFor(r)
	if err != nil || !p.canManageGrants() {
		return nil, false
	}
	// Owner's grants are untouchable by anyone (owner included — they are
	// implicit full, there is nothing to manage).
	var role string
	if err := a.Pool.QueryRow(r.Context(),
		`SELECT role FROM household_member WHERE user_id = $1 AND household_id = $2`,
		targetUserID, sess.Household).Scan(&role); err != nil {
		return nil, false
	}
	if role == "owner" {
		return nil, false
	}
	return p, true
}

// handleHouseholdGrantSave upserts one member's household grant. Unchecking
// everything (with no admin flag) deletes the row — a grant that grants
// nothing is noise.
func (a *App) handleHouseholdGrantSave(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	target, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, ok := a.manageTarget(r, target); !ok {
		http.Error(w, "cannot manage that member's grants", http.StatusForbidden)
		return
	}
	sess := sessionFromContext(r.Context())
	ctx := r.Context()
	read := r.FormValue("can_read") == "1"
	create := r.FormValue("can_create") == "1"
	edit := r.FormValue("can_edit") == "1"
	del := r.FormValue("can_delete") == "1"
	admin := r.FormValue("is_admin") == "1"
	editMode := r.FormValue("edit_mode")
	delMode := r.FormValue("delete_mode")
	if !validCreatedAll(editMode) || !validCreatedAll(delMode) {
		http.Error(w, "bad mode", http.StatusBadRequest)
		return
	}
	// Mirror the DB forcing rules server-side (a clearer error than 23503).
	if err := enforceHouseholdGrant(read, create, edit, del, editMode, delMode); err != "" {
		http.Error(w, err, http.StatusBadRequest)
		return
	}
	if !read && !admin {
		// A row that grants nothing (and isn't an admin) is deleted.
		if _, err := a.Pool.Exec(ctx,
			`DELETE FROM household_grant WHERE user_id = $1 AND household_id = $2`,
			target, sess.Household); err != nil {
			http.Error(w, "save grant: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		if _, err := a.Pool.Exec(ctx, `
			INSERT INTO household_grant (user_id, household_id, can_read, can_create,
			    can_edit, can_delete, edit_mode, delete_mode, is_admin)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (user_id, household_id) DO UPDATE
			SET can_read = EXCLUDED.can_read, can_create = EXCLUDED.can_create,
			    can_edit = EXCLUDED.can_edit, can_delete = EXCLUDED.can_delete,
			    edit_mode = EXCLUDED.edit_mode, delete_mode = EXCLUDED.delete_mode,
			    is_admin = EXCLUDED.is_admin`,
			target, sess.Household, read, create, edit, del, editMode, delMode, admin); err != nil {
			http.Error(w, "save grant: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/settings?tab=grants", http.StatusSeeOther)
}

// handleAccountGrantSave upserts one member's grant on one account; deleting
// the last right removes the row.
func (a *App) handleAccountGrantSave(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	sess := sessionFromContext(r.Context())
	ctx := r.Context()
	target, _ := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	account, _ := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
	if _, ok := a.manageTarget(r, target); !ok {
		http.Error(w, "cannot manage that member's grants", http.StatusForbidden)
		return
	}
	// The account must belong to this household.
	var n int
	if err := a.Pool.QueryRow(ctx, `
		SELECT count(*) FROM account WHERE id = $1 AND household_id = $2`,
		account, sess.Household).Scan(&n); err != nil || n == 0 {
		http.Error(w, "unknown account", http.StatusNotFound)
		return
	}
	read := r.FormValue("can_read") == "1"
	create := r.FormValue("can_create") == "1"
	edit := r.FormValue("can_edit") == "1"
	del := r.FormValue("can_delete") == "1"
	viewMode := r.FormValue("view_mode")
	editMode := r.FormValue("edit_mode")
	delMode := r.FormValue("delete_mode")
	if !validRelatedAll(viewMode) || !validRelatedAll(editMode) || !validRelatedAll(delMode) {
		http.Error(w, "bad mode", http.StatusBadRequest)
		return
	}
	if err := enforceAccountGrant(read, create, edit, del, viewMode, editMode, delMode); err != "" {
		http.Error(w, err, http.StatusBadRequest)
		return
	}
	if !read {
		if _, err := a.Pool.Exec(ctx, `
			DELETE FROM account_grant WHERE user_id = $1 AND household_id = $2 AND account_id = $3`,
			target, sess.Household, account); err != nil {
			http.Error(w, "save grant: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		if _, err := a.Pool.Exec(ctx, `
			INSERT INTO account_grant (user_id, household_id, account_id, can_read,
			    can_create, can_edit, can_delete, view_mode, edit_mode, delete_mode)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (user_id, household_id, account_id) DO UPDATE
			SET can_read = EXCLUDED.can_read, can_create = EXCLUDED.can_create,
			    can_edit = EXCLUDED.can_edit, can_delete = EXCLUDED.can_delete,
			    view_mode = EXCLUDED.view_mode, edit_mode = EXCLUDED.edit_mode,
			    delete_mode = EXCLUDED.delete_mode`,
			target, sess.Household, account, read, create, edit, del,
			viewMode, editMode, delMode); err != nil {
			http.Error(w, "save grant: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/settings?tab=grants", http.StatusSeeOther)
}

func validCreatedAll(m string) bool { return m == "created" || m == "all" }
func validRelatedAll(m string) bool { return m == "Related" || m == "All" }

// enforceHouseholdGrant mirrors the 000012 DB CHECKs for a clearer error.
func enforceHouseholdGrant(read, create, edit, del bool, editMode, delMode string) string {
	if create && !read {
		return "Create requires Read."
	}
	if edit && !create {
		return "Edit requires Create."
	}
	if del && !edit {
		return "Delete requires Edit."
	}
	if delMode == "all" && (editMode != "all" || !read) {
		return "Delete = all forces Edit = all (and Read)."
	}
	return ""
}

// enforceAccountGrant mirrors the 000012 account_grant CHECKs.
func enforceAccountGrant(read, create, edit, del bool, viewMode, editMode, delMode string) string {
	if create && !read {
		return "Create requires Read."
	}
	if edit && !create {
		return "Edit requires Create."
	}
	if del && !edit {
		return "Delete requires Edit."
	}
	if editMode == "All" && viewMode != "All" {
		return "Edit = All forces View = All."
	}
	if delMode == "All" && (editMode != "All" || viewMode != "All") {
		return "Delete = All forces Edit = All and View = All."
	}
	return ""
}
