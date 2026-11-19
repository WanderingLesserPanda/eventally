// D28 permission-UI wave: the Settings page (3 tabs) + grant management.
//
// Tabs: Household (every member: default-household picker), Invites and
// Grants (owner + admin only — D27 policy: admins manage every non-owner
// member, never the owner). Non-admin members see only the Household tab.
package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
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
// admin tier; non-admins fall back to the Household tab. Sync (D34) is for
// every member: the auto-sync toggles + the category mappings.
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
	case "sync":
		a.handleSettingsSync(w, r)
		return
	}
	a.handleSettingsHousehold(w, r)
}

// handleSettingsSync is the Settings "Sync" tab (D34) — the second home of
// the auto-sync setting (the Live view's household modal is the first: one
// setting, two views on the same row) and the category-mapping section
// (spec §B): the caller's own rules, grouped by source household, with the
// create form (the source category list is the named cross-household read,
// surface 6).
func (a *App) handleSettingsSync(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	hh := sess.Household
	if hh == 0 {
		hh, _ = a.landHousehold(ctx, sess.UserID)
	}
	if hh == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	user := sess.UserID

	// The auto-sync toggles: the caller's other households, each with its
	// members (the household-detail read, surface 7) and the toggle state.
	type toggleRow struct {
		HouseholdID int64
		Name        string
		Members     []string
		On          bool
	}
	ms, err := a.memberships(ctx, user)
	if err != nil {
		http.Error(w, "load memberships: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var toggles []toggleRow
	for _, m := range ms {
		if m.HouseholdID == hh {
			continue
		}
		members, err := a.syncHouseholdMembers(ctx, m.HouseholdID)
		if err != nil {
			http.Error(w, "load household members: "+err.Error(), http.StatusInternalServerError)
			return
		}
		on, err := a.syncToggleState(ctx, user, m.HouseholdID, hh)
		if err != nil {
			http.Error(w, "load auto-sync state: "+err.Error(), http.StatusInternalServerError)
			return
		}
		toggles = append(toggles, toggleRow{m.HouseholdID, m.Name, members, on})
	}

	// The caller's category-mapping rules, grouped by source household.
	type mappingLine struct {
		ID        int64
		SourceCat string
		TargetCat string
	}
	type mappingGroup struct {
		HH   int64
		Name string
		Rows []mappingLine
	}
	groups := map[int64]*mappingGroup{}
	var groupOrder []int64
	mrows, err := a.Pool.Query(ctx, `
		SELECT cm.id, cm.source_household_id, sh.name,
		       COALESCE(sc.name, '—'),
		       CASE WHEN pc.name IS NOT NULL AND pc.name <> ''
		            THEN pc.name || ' › ' || tc.name ELSE tc.name END
		FROM category_mapping cm
		JOIN household sh ON sh.id = cm.source_household_id
		LEFT JOIN category sc ON sc.id = cm.source_category_id
		LEFT JOIN category tc ON tc.id = cm.target_category_id
		LEFT JOIN category pc ON pc.id = tc.parent_id
		WHERE cm.household_id = $1
		ORDER BY sh.name, sc.name, tc.name`, hh)
	if err != nil {
		http.Error(w, "load mappings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for mrows.Next() {
		var l mappingLine
		var srcHH int64
		var srcHHName, srcCat string
		var targetCat string
		if err := mrows.Scan(&l.ID, &srcHH, &srcHHName, &srcCat, &targetCat); err != nil {
			mrows.Close()
			http.Error(w, "load mappings: "+err.Error(), http.StatusInternalServerError)
			return
		}
		l.SourceCat, l.TargetCat = srcCat, targetCat
		g, ok := groups[srcHH]
		if !ok {
			g = &mappingGroup{HH: srcHH, Name: srcHHName}
			groups[srcHH] = g
			groupOrder = append(groupOrder, srcHH)
		}
		g.Rows = append(g.Rows, l)
	}
	if err := mrows.Err(); err != nil {
		mrows.Close()
		http.Error(w, "load mappings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	mrows.Close()
	var mappingGroups []mappingGroup
	for _, hhID := range groupOrder {
		mappingGroups = append(mappingGroups, *groups[hhID])
	}

	// The create form: the source household's active categories (the named
	// cross-household read, surface 6), each labelled with its household;
	// the target picker is this household's active expense categories.
	type srcChoice struct {
		HHID  int64
		CatID int64
		Label string
	}
	var srcChoices []srcChoice
	for _, m := range ms {
		if m.HouseholdID == hh {
			continue
		}
		cats := a.sourceCategoryList(ctx, m.HouseholdID)
		for _, c := range cats {
			name := c.Name
			if c.Parent != "" {
				name = c.Parent + " › " + c.Name
			}
			srcChoices = append(srcChoices, srcChoice{m.HouseholdID, c.ID, m.Name + " › " + name})
		}
	}

	a.renderTabbed(w, r, 200, "settings_sync.html", "sync", map[string]any{
		"Title":         "Settings · Sync",
		"Flash":         flashFrom(r),
		"Toggles":       toggles,
		"MappingGroups": mappingGroups,
		"SrcChoices":    srcChoices,
		"TargetCats":    a.syncCategoryOptions(ctx, hh),
		"Err":           r.URL.Query().Get("err"),
	})
}

// sourceCategoryList is the source-household category read for the mapping
// create form (surface 6): the source household's active category names —
// the only place a household's CATEGORIES cross.
func (a *App) sourceCategoryList(ctx context.Context, srcHH int64) []opt {
	var out []opt
	rows, err := a.Pool.Query(ctx, `
		SELECT c.id, c.name, COALESCE(p.name, '')
		FROM category c
		LEFT JOIN category p ON p.id = c.parent_id
		WHERE c.household_id = $1 AND c.is_active
		ORDER BY c.transaction_class, c.sort_order, c.name`, srcHH)
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

// mappingTargetFits: the target category exists in the caller's household,
// is active, and is an expense class (a mapped copy is an expense).
func (a *App) mappingTargetFits(ctx context.Context, hh, catID int64) bool {
	return a.syncCatFits(ctx, hh, catID)
}

// handleMappingCreate is POST /settings/mapping — the category-mapping
// create (spec §B): one rule per (source household, source category) in the
// caller's household. The source pair is app-validated (the isolation
// convention — no cross-household FKs): the caller still belongs to the
// source household and the category is still active there.
func (a *App) handleMappingCreate(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	hh := sess.Household
	if hh == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	const home = "/settings?tab=sync"
	srcHH, srcCat, err := parseMappingChoice(r.FormValue("source"))
	if err != nil {
		http.Error(w, "pick a source category.", http.StatusBadRequest)
		return
	}
	target, _ := formInt64(r, "target_category_id")
	member, merr := a.isMember(ctx, sess.UserID, srcHH)
	if merr != nil {
		http.Error(w, "could not check membership.", http.StatusInternalServerError)
		return
	}
	if !member {
		http.Error(w, "you no longer belong to that household.", http.StatusBadRequest)
		return
	}
	// Surface 6: the source category must still be active there.
	if !a.sourceCatActive(ctx, srcHH, srcCat) {
		a.redirectRef(w, r, home+"?err=1", "That source category is no longer active.")
		return
	}
	if !a.mappingTargetFits(ctx, hh, target) {
		http.Error(w, "choose one of this household's expense categories.", http.StatusBadRequest)
		return
	}
	_, err = a.Pool.Exec(ctx, `
		INSERT INTO category_mapping (household_id, source_household_id, source_category_id, target_category_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (household_id, source_household_id, source_category_id) DO NOTHING`,
		hh, srcHH, srcCat, target)
	if err != nil {
		http.Error(w, "save mapping: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.redirectRef(w, r, home, "Category mapping saved.")
}

// parseMappingChoice splits the create form's "srcHH:srcCat" value.
func parseMappingChoice(v string) (srcHH, srcCat int64, err error) {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return 0, 0, errors.New("bad choice")
	}
	srcHH, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || srcHH <= 0 {
		return 0, 0, errors.New("bad source household")
	}
	srcCat, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || srcCat <= 0 {
		return 0, 0, errors.New("bad source category")
	}
	return srcHH, srcCat, nil
}

// sourceCatActive: the source category exists and is active in the source
// household (surface 6 read).
func (a *App) sourceCatActive(ctx context.Context, srcHH, catID int64) bool {
	var ok bool
	err := a.Pool.QueryRow(ctx, `
		SELECT TRUE FROM category
		WHERE id = $1 AND household_id = $2 AND is_active`, catID, srcHH).Scan(&ok)
	return err == nil && ok
}

// handleMappingUpdate is POST /settings/mapping/{id} — retargets an existing
// rule (the source pair is immutable; the rule belongs to the caller's
// household).
func (a *App) handleMappingUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	hh := sess.Household
	if hh == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	target, _ := formInt64(r, "target_category_id")
	if !a.mappingTargetFits(ctx, hh, target) {
		http.Error(w, "choose one of this household's expense categories.", http.StatusBadRequest)
		return
	}
	tag, err := a.Pool.Exec(ctx, `
		UPDATE category_mapping SET target_category_id = $1
		WHERE id = $2 AND household_id = $3`, target, id, hh)
	if err != nil {
		http.Error(w, "update mapping: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	a.redirectRef(w, r, "/settings?tab=sync", "Category mapping updated.")
}

// handleMappingDelete is POST /settings/mapping/{id}/delete — removes one of
// the caller's rules (household-local).
func (a *App) handleMappingDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	hh := sess.Household
	if hh == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	tag, err := a.Pool.Exec(ctx, `
		DELETE FROM category_mapping WHERE id = $1 AND household_id = $2`, id, hh)
	if err != nil {
		http.Error(w, "delete mapping: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	a.redirectRef(w, r, "/settings?tab=sync", "Category mapping removed.")
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
