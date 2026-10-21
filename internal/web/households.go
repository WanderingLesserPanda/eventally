// D28 permission-UI wave: multi-household identity, invitations, switcher.
//
// Membership questions route through household_member (one username is one
// person, unique app-wide; a person may belong to many households —
// ADR-0002). The user row's default_household_id is only the login-landing
// household.
package web

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// membership is one household the user belongs to.
type membership struct {
	HouseholdID int64
	Name        string
	Role        string // "owner" | "member"
	IsDefault   bool
	IsCurrent   bool // the household the session is viewing (switcher)
}

// memberships lists every household the user belongs to (switcher source),
// sorted by name; the default household is flagged.
func (a *App) memberships(ctx context.Context, userID int64) ([]membership, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT m.household_id, h.name, m.role,
		       (u.default_household_id = m.household_id)
		FROM household_member m
		JOIN household h ON h.id = m.household_id
		JOIN app_user u ON u.id = m.user_id
		WHERE m.user_id = $1
		ORDER BY h.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []membership
	for rows.Next() {
		var m membership
		if err := rows.Scan(&m.HouseholdID, &m.Name, &m.Role, &m.IsDefault); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// isMember reports whether the user belongs to the household.
func (a *App) isMember(ctx context.Context, userID, household int64) (bool, error) {
	var n int
	err := a.Pool.QueryRow(ctx,
		`SELECT count(*) FROM household_member WHERE user_id = $1 AND household_id = $2`,
		userID, household).Scan(&n)
	return n > 0, err
}

// ownerOfHousehold returns the owner's display name (the "owned by" text on
// join prompts); "" when the household has no owner row.
func (a *App) ownerOfHousehold(ctx context.Context, household int64) (string, error) {
	var name string
	err := a.Pool.QueryRow(ctx, `
		SELECT u.display_name
		FROM household_member m
		JOIN app_user u ON u.id = m.user_id
		WHERE m.household_id = $1 AND m.role = 'owner'
		LIMIT 1`, household).Scan(&name)
	if err != nil {
		return "", nil
	}
	return name, nil
}

// landHousehold resolves the household a user lands in at login (D28):
// default_household_id if it is still a membership, else the first remaining
// membership (alphabetical by household name), else 0 (recovery).
func (a *App) landHousehold(ctx context.Context, userID int64) (int64, error) {
	var hh int64
	const landSQL = `
		SELECT COALESCE(
			(SELECT m.household_id FROM household_member m
			 WHERE m.user_id = $1
			        AND m.household_id = (SELECT default_household_id FROM app_user WHERE id = $1)),
			(SELECT m.household_id FROM household_member m
			 JOIN household h ON h.id = m.household_id
			 WHERE m.user_id = $1 ORDER BY h.name LIMIT 1),
			0)`
	err := a.Pool.QueryRow(ctx, landSQL, userID).Scan(&hh)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if hh == 0 {
		return 0, nil
	}
	return hh, err
}

// setDefaultHousehold updates the user's login-landing household (Settings).
func (a *App) setDefaultHousehold(ctx context.Context, userID, household int64) error {
	tag, err := a.Pool.Exec(ctx,
		`UPDATE app_user SET default_household_id = $2 WHERE id = $1`, userID, household)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// --- nav-bar household switch + in-app household creation -------------------

// switchHouseholds loads the caller's memberships for the layout's switcher.
// It always renders for a session with a household (the menu is the entry
// point for "＋ New household" and "Join with code" even when there is
// nothing to switch to); the session's household is flagged IsCurrent.
func (a *App) switchHouseholds(ctx context.Context) []membership {
	sess := sessionFromContext(ctx)
	if sess == nil || !sess.HasHousehold() {
		return nil
	}
	ms, err := a.memberships(ctx, sess.UserID)
	if err != nil {
		return nil
	}
	for i := range ms {
		if ms[i].HouseholdID == sess.Household {
			ms[i].IsCurrent = true
		}
	}
	return ms
}

// handleHouseholdSwitch lands the session in another of the caller's
// households and sends them to its dashboard.
func (a *App) handleHouseholdSwitch(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Sessions.SetHousehold(r.Context(), sess.ID, target); err != nil {
		log.Printf("household switch: %v", err)
		http.Error(w, "could not switch household", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- in-app household creation (creator is the owner) ----------------------

type newHouseholdForm struct {
	Name     string
	Currency string
	FirstDay string
	Err      string
}

// handleNewHouseholdGET renders the creation form (both entry points: the
// nav-bar "＋ New household" and the dashboard recovery path).
func (a *App) handleNewHouseholdGET(w http.ResponseWriter, r *http.Request) {
	a.renderNewHousehold(w, r, newHouseholdForm{Currency: setupCurrencies[0], FirstDay: "1"})
}

func (a *App) renderNewHousehold(w http.ResponseWriter, r *http.Request, f newHouseholdForm) {
	a.render(w, r, http.StatusOK, "new_household.html", map[string]any{
		"Title":       "New household",
		"F":           f,
		"Currencies":  setupCurrencies,
		"FirstDays":   firstDayOptions,
	})
}

// handleNewHouseholdPOST creates the household, makes the caller its owner
// (implicit full in code + an owner grant row matching the 000012 backfill),
// sets it as their login-landing household, and lands them in it.
func (a *App) handleNewHouseholdPOST(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	f := newHouseholdForm{
		Name:     strings.TrimSpace(r.FormValue("name")),
		Currency: r.FormValue("currency"),
		FirstDay: r.FormValue("first_day"),
	}
	// The nav-bar modal submits only a name: currency and first day fall
	// back to the defaults (MYR, Monday).
	if f.Currency == "" {
		f.Currency = setupCurrencies[0]
	}
	if f.FirstDay == "" {
		f.FirstDay = "1"
	}
	if err := validateNewHousehold(&f); err != "" {
		f.Err = err
		a.renderNewHousehold(w, r, f)
		return
	}
	firstDay := firstDayInt(f.FirstDay)

	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	var hh int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO household (name, base_currency, language, first_day_of_week)
		 VALUES ($1, 'MYR', 'en', $2) RETURNING id`, f.Name, firstDay).Scan(&hh); err != nil {
		log.Printf("new household insert: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO household_member (user_id, household_id, role)
		 VALUES ($1, $2, 'owner')`, sess.UserID, hh); err != nil {
		log.Printf("new household membership: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Owner grant row, matching the 000012 backfill's owner shape (implicit
	// full in code; the row exists so the grants UI can render it locked).
	if _, err := tx.Exec(ctx, `
		INSERT INTO household_grant (user_id, household_id, can_read, can_create,
		    can_edit, can_delete, edit_mode, delete_mode, is_admin)
		 VALUES ($1, $2, TRUE, TRUE, TRUE, TRUE, 'all', 'all', TRUE)`,
		sess.UserID, hh); err != nil {
		log.Printf("new household owner grant: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE app_user SET default_household_id = $2 WHERE id = $1`,
		sess.UserID, hh); err != nil {
		log.Printf("new household default: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("new household commit: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := a.Sessions.SetHousehold(ctx, sess.ID, hh); err != nil {
		log.Printf("new household session: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func validateNewHousehold(f *newHouseholdForm) string {
	if m := validateHouseholdName(f.Name); m != "" {
		return m
	}
	if f.Currency != setupCurrencies[0] {
		return "Default currency must be " + setupCurrencies[0] + " (for now)."
	}
	if firstDayInt(f.FirstDay) == 0 {
		return "Choose a first day of the week."
	}
	return ""
}

// firstDayInt maps a "1".."7" option to the ISO day (Monday=1). 0 if unknown.
func firstDayInt(s string) int {
	for _, d := range firstDayOptions {
		if d.Value == s {
			return int(d.Value[0] - '0')
		}
	}
	return 0
}

// refExists checks that a reference (account/category/merchant/party/user)
// exists in the household. For app_user, D28: membership lives in
// household_member, not the user row.
func refExists(ctx context.Context, pool *pgxpool.Pool, table string, id, hh int64) bool {
	if id <= 0 {
		return true
	}
	var n int
	var q string
	if table == `app_user` {
		q = `SELECT count(*) FROM household_member WHERE user_id = $1 AND household_id = $2`
	} else {
		q = fmt.Sprintf(`SELECT count(*) FROM %s WHERE id = $1 AND household_id = $2`, table)
	}
	return pool.QueryRow(ctx, q, id, hh).Scan(&n) == nil && n > 0
}

// pendingJoinCookie carries a smart-join join-token from /join (username
// collision) to the post-login dashboard modal: one at a time, one hour,
// HttpOnly. The value is an opaque random token; the raw code is re-derived
// server-side, so the cookie itself is not the credential.
const pendingJoinCookie = "eventally_pending_join"
