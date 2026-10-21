// D28 permission-UI wave: invitations + join flows.
//
// Invite codes are one-time, single-use, hashed at rest. Two join paths:
// /join (logged-out new user) and the in-app "Join with code" (existing
// user). The smart-join variant of /join (username already exists) stores a
// short-lived join-token cookie and surfaces a dashboard modal after the
// user signs in.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
)

// inviteExpiryDays are the allowed picklist values (household setting and
// per-invite override); 0 = use the household setting.
var inviteExpiryDays = []int{1, 3, 7, 14, 30}

// maxOutstandingInvites caps live (un-consumed, un-revoked, unexpired)
// codes per household.
const maxOutstandingInvites = 5

// pendingJoinTTL is how long a smart-join join-token survives after the
// /join collision.
const pendingJoinTTL = time.Hour

// --- helpers ---------------------------------------------------------------

func randTokenHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// liveInvite looks up a code (raw) that is usable: un-consumed, un-revoked,
// unexpired. Returns the row for join flows.
type liveInvite struct {
	ID          int64
	HouseholdID int64
	ExpiresAt   time.Time
}

func (a *App) liveInvite(ctx context.Context, rawCode string) (*liveInvite, error) {
	sum := sha256.Sum256([]byte(rawCode))
	var inv liveInvite
	err := a.Pool.QueryRow(ctx, `
		SELECT id, household_id, expires_at
		FROM invite
		WHERE code_hash = $1
		  AND consumed_at IS NULL
		  AND revoked_at IS NULL
		  AND expires_at > now()`,
		fmt.Sprintf("%x", sum)).Scan(&inv.ID, &inv.HouseholdID, &inv.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

// --- invite management (Settings → Invites, owner/admin only) ---------------

// handleInvites lists a household's live invite codes and the household's
// default expiry.
func (a *App) handleInvites(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if !a.canManageGrants(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	type inviteRow struct {
		ID       int64
		Prefix   string
		Created  time.Time
		Expires  time.Time
		CreatedBy string
	}
	var rows []inviteRow
	rs, err := a.Pool.Query(ctx, `
		SELECT i.id, i.code_prefix, i.created_at, i.expires_at, COALESCE(u.display_name, '?')
		FROM invite i
		LEFT JOIN app_user u ON u.id = i.created_by_user_id
		WHERE i.household_id = $1 AND i.consumed_at IS NULL AND i.revoked_at IS NULL
		ORDER BY i.created_at DESC`, sess.Household)
	if err != nil {
		http.Error(w, "list invites: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rs.Close()
	for rs.Next() {
		var row inviteRow
		if err := rs.Scan(&row.ID, &row.Prefix, &row.Created, &row.Expires, &row.CreatedBy); err != nil {
			http.Error(w, "scan invites: "+err.Error(), http.StatusInternalServerError)
			return
		}
		rows = append(rows, row)
	}
	if err := rs.Err(); err != nil {
		http.Error(w, "iter invites: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var defDays int
	if err := a.Pool.QueryRow(ctx, `SELECT invite_expiry_days FROM household WHERE id = $1`,
		sess.Household).Scan(&defDays); err != nil {
		http.Error(w, "read expiry: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Title": "Settings · Invites",
		"Section": "Invites",
		"Invites": rows,
		"ExpiryDays": inviteExpiryDays,
		"DefaultExpiry": defDays,
		"New": map[string]any{"Expiry": defDays},
		"JoinURL": joinURL(r),
	}
	a.renderTabbed(w, r, http.StatusOK, "settings_invites.html", "invites", data)
}

// handleInviteCreate issues a new code (honouring the cap + per-code expiry
// override) and returns to the invites tab with the raw code shown once.
func (a *App) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	sess := sessionFromContext(r.Context())
	if !a.canManageGrants(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Per-code expiry override (blank = household default).
	override := 0
	if v := r.FormValue("expiry_days"); v != "" {
		for _, d := range inviteExpiryDays {
			if fmt.Sprintf("%d", d) == v {
				override = d
			}
		}
	}
	var days int
	if override > 0 {
		days = override
	} else {
		if err := a.Pool.QueryRow(ctx, `SELECT invite_expiry_days FROM household WHERE id = $1`,
			sess.Household).Scan(&days); err != nil {
			http.Error(w, "read expiry: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	var live int
	if err := a.Pool.QueryRow(ctx, `
		SELECT count(*) FROM invite
		WHERE household_id = $1 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > now()`,
		sess.Household).Scan(&live); err != nil {
		http.Error(w, "count invites: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if live >= maxOutstandingInvites {
		a.render(w, r, http.StatusOK, "settings_invites.html", map[string]any{
			"Title": "Settings · Invites", "Section": "Invites",
			"ExpiryDays": inviteExpiryDays, "DefaultExpiry": days,
			"Error": fmt.Sprintf("Up to %d outstanding invites at a time.", maxOutstandingInvites),
		})
		return
	}

	raw := randTokenHex(16) // 32 base64url chars
	sum := sha256.Sum256([]byte(raw))
	var id int64
	err := a.Pool.QueryRow(ctx, `
		INSERT INTO invite (household_id, code_hash, code_prefix, created_by_user_id, expires_at)
		 VALUES ($1, $2, $3, $4, now() + ($5 || ' days')::interval)
		 RETURNING id`,
		sess.Household, fmt.Sprintf("%x", sum), raw[:4], sess.UserID, fmt.Sprint(days)).Scan(&id)
	if err != nil {
		http.Error(w, "create invite: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.render(w, r, http.StatusOK, "settings_invites.html", map[string]any{
		"Title": "Settings · Invites", "Section": "Invites",
		"ExpiryDays": inviteExpiryDays, "DefaultExpiry": days,
		"JoinURL": joinURL(r),
		"Created": map[string]any{"Code": raw, "Days": days},
	})
}

// handleInviteRevoke marks a code revoked (a live code can be revoked; the
// DB CHECK keeps a consumed one immutable).
func (a *App) handleInviteRevoke(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	sess := sessionFromContext(r.Context())
	if !a.canManageGrants(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var id int64
	if _, err := fmt.Sscanf(r.PathValue("id"), "%d", &id); err != nil {
		http.Error(w, "bad invite id", http.StatusBadRequest)
		return
	}
	tag, err := a.Pool.Exec(ctx, `
		UPDATE invite SET revoked_at = now()
		WHERE id = $1 AND household_id = $2 AND revoked_at IS NULL AND consumed_at IS NULL`,
		id, sess.Household)
	if err != nil {
		http.Error(w, "revoke invite: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "invite not found or already gone", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/settings?tab=invites", http.StatusSeeOther)
}

// setDefaultExpiry writes the household's default invite expiry.
func (a *App) handleSetDefaultExpiry(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	sess := sessionFromContext(r.Context())
	if !a.canManageGrants(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var days int
	if _, err := fmt.Sscanf(r.FormValue("expiry_days"), "%d", &days); err != nil {
		http.Error(w, "bad expiry", http.StatusBadRequest)
		return
	}
	valid := false
	for _, d := range inviteExpiryDays {
		if d == days {
			valid = true
		}
	}
	if !valid {
		http.Error(w, "expiry must be one of 1/3/7/14/30 days", http.StatusBadRequest)
		return
	}
	if _, err := a.Pool.Exec(r.Context(), `
		UPDATE household SET invite_expiry_days = $2 WHERE id = $1`,
		sess.Household, days); err != nil {
		http.Error(w, "set expiry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?tab=invites", http.StatusSeeOther)
}

// --- join flows -------------------------------------------------------------

// joinForm is the /join (new user) or in-app join shape.
type joinForm struct {
	Code        string
	Username    string
	DisplayName string
	Password    string
	Err         string
	Pending     bool // smart-join collision: sign in first
}

// handleJoinGET renders the public join page.
func (a *App) handleJoinGET(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "join.html", map[string]any{
		"Title": "Join a household",
		"F":     joinForm{},
	})
}

// handleJoinPOST processes a join code from the public /join page.
//
// Order matters (D28 smart-join): the CODE is validated first, before the
// username is looked up. A valid code + an already-taken username is the
// collision case: the raw code is never shown again, so a short-lived
// join-token is stored (HttpOnly cookie + a join_token row) and the user is
// told to sign in; after login the dashboard shows the join modal. An
// invalid/unknown code is rejected outright and nothing is stored.
func (a *App) handleJoinPOST(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	f := joinForm{
		// Codes are case-sensitive (base64url) — normalize whitespace only.
		Code:        strings.TrimSpace(r.FormValue("code")),
		Username:    strings.TrimSpace(r.FormValue("username")),
		DisplayName: strings.TrimSpace(r.FormValue("display_name")),
		Password:    r.FormValue("password"),
	}

	// 1) Code first.
	inv, err := a.liveInvite(ctx, f.Code)
	if err != nil {
		f.Err = "That invite is not valid (it may have expired or already been used). Ask the admin for a new code."
		a.renderJoin(w, r, f)
		return
	}

	// 2) Username: does this person already exist?
	var existing int64
	uerr := a.Pool.QueryRow(ctx, `SELECT id FROM app_user WHERE username = $1`,
		f.Username).Scan(&existing)
	if uerr == nil {
		// Collision: smart-join. Store a join-token for the logged-in flow.
		token := randTokenHex(24)
		if _, err := a.Pool.Exec(ctx, `
			INSERT INTO invite_join_token (token, invite_id, username, expires_at)
			 VALUES ($1, $2, $3, now() + interval '1 hour')`,
			token, inv.ID, f.Username); err != nil {
			http.Error(w, "could not hold your place", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: pendingJoinCookie, Value: token, Path: "/",
			MaxAge: int(pendingJoinTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		f.Pending = true
		f.Err = "This username already exists — please sign in first to join from the app."
		a.renderJoin(w, r, f)
		return
	}
	if uerr != nil && !errors.Is(uerr, pgx.ErrNoRows) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 3) New person: validate, then create user + membership + default grants.
	var errs []string
	if m := validateUsername(f.Username); m != "" {
		errs = append(errs, m)
	}
	if m := validatePersonalName(f.DisplayName, 2); m != "" {
		errs = append(errs, "Display name: "+m)
	}
	if m := validateSetupPassword(f.Password); m != "" {
		errs = append(errs, m)
	}
	if len(errs) > 0 {
		f.Password = ""
		f.Err = strings.Join(errs, " ")
		a.renderJoin(w, r, f)
		return
	}

	hash, err := auth.HashPassword(f.Password)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	var userID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
		 VALUES ($1, $2, $3, '', $4) RETURNING id`,
		inv.HouseholdID, f.Username, f.DisplayName, hash).Scan(&userID); err != nil {
		if isUniqueViolation(err) {
			f.Password = ""
			f.Err = "That username is already taken — pick another."
			a.renderJoin(w, r, f)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO household_member (user_id, household_id, role)
		 VALUES ($1, $2, 'member')`, userID, inv.HouseholdID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// D27 member default: read-only household grant + read-only All-view
	// grant on every existing account (the 000012 backfill's member shape).
	if _, err := tx.Exec(ctx, `
		INSERT INTO household_grant (user_id, household_id, can_read, can_create,
		    can_edit, can_delete, edit_mode, delete_mode, is_admin)
		 VALUES ($1, $2, TRUE, FALSE, FALSE, FALSE, 'created', 'created', FALSE)`,
		userID, inv.HouseholdID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_grant (user_id, household_id, account_id, can_read, view_mode)
		SELECT $1, $2, a.id, TRUE, 'All' FROM account a WHERE a.household_id = $2`,
		userID, inv.HouseholdID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Consume the code (single-use) — fail if another join beat us to it.
	tag, err := tx.Exec(ctx, `
		UPDATE invite SET consumed_at = now(), consumed_by_user_id = $2
		WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`,
		inv.ID, userID)
	if err != nil || tag.RowsAffected() == 0 {
		f.Err = "This invite was just used or revoked — ask the admin for a new code."
		f.Password = ""
		a.renderJoin(w, r, f)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// A fresh account's login-landing household is the one just joined (set
	// at insert). Start a session and land them there.
	sess, err := a.Sessions.Create(ctx, inv.HouseholdID, userID, r.UserAgent(), r.RemoteAddr)
	if err != nil {
		http.Error(w, "could not start session", http.StatusInternalServerError)
		return
	}
	auth.SetSessionCookie(w, sess.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) renderJoin(w http.ResponseWriter, r *http.Request, f joinForm) {
	a.render(w, r, http.StatusOK, "join.html", map[string]any{
		"Title": "Join a household",
		"F":     f,
	})
}

// joinWithCodeForm is the in-app "Join with code" (existing user).
type joinWithCodeForm struct {
	Code string
	Err  string
}

// handleJoinComplete finalises a smart-join: the dashboard modal's "Join"
// button POSTs the token; membership + default grants are added and the
// session lands in the joined household.
func (a *App) handleJoinComplete(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	token := r.FormValue("token")
	var inviteID int64
	var username string
	err := a.Pool.QueryRow(ctx, `
		SELECT invite_id, username FROM invite_join_token
		WHERE token = $1 AND expires_at > now()`, token).
		Scan(&inviteID, &username)
	if err != nil {
		clearPendingJoin(w)
		http.Redirect(w, r, "/?join=expired", http.StatusSeeOther)
		return
	}
	var sessName string
	if err := a.Pool.QueryRow(ctx, `SELECT username FROM app_user WHERE id = $1`,
		sess.UserID).Scan(&sessName); err != nil || sessName != username {
		clearPendingJoin(w)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	var hhID int64
	if err := a.Pool.QueryRow(ctx, `SELECT household_id FROM invite WHERE id = $1`,
		inviteID).Scan(&hhID); err != nil {
		clearPendingJoin(w)
		http.Redirect(w, r, "/?join=expired", http.StatusSeeOther)
		return
	}
	if ok, _ := a.isMember(ctx, sess.UserID, hhID); ok {
		a.clearJoinToken(ctx, token)
		clearPendingJoin(w)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if _, err := a.addMemberWithGrants(ctx, sess.UserID, inviteID, hhID); err != nil {
		clearPendingJoin(w)
		http.Redirect(w, r, "/?join=failed", http.StatusSeeOther)
		return
	}
	a.clearJoinToken(ctx, token)
	clearPendingJoin(w)
	if err := a.Sessions.SetHousehold(ctx, sess.ID, hhID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleJoinDismiss is the modal's "Don't Join": drop the token + cookie.
func (a *App) handleJoinDismiss(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	a.clearJoinToken(r.Context(), r.FormValue("token"))
	clearPendingJoin(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// clearJoinToken deletes the smart-join token row (one-shot).
func (a *App) clearJoinToken(ctx context.Context, token string) {
	if token == "" {
		return
	}
	_, _ = a.Pool.Exec(ctx, `DELETE FROM invite_join_token WHERE token = $1`, token)
}

// joinURL builds the host-agnostic join-page hint from the request's Host
// header (correct over LAN or a future public address; scheme is http — the
// home-LAN default).
func joinURL(r *http.Request) string {
	return "http://" + r.Host + "/join"
}

// joinFlash is the dashboard's flash line: a one-shot join outcome (from the
// ?join= query set by /join/complete) falls back to the generic ?msg= flash.
func (a *App) joinFlash(r *http.Request) string {
	switch r.URL.Query().Get("join") {
	case "expired":
		return "That invite is no longer valid — ask the admin for a new code."
	case "failed":
		return "Could not join that household; the invite may have expired or been revoked."
	}
	return flashFrom(r)
}

// handleJoinWithCodeGET renders the in-app join page (switcher menu link).
func (a *App) handleJoinWithCodeGET(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "join_code.html", map[string]any{
		"Title": "Join with code", "F": joinWithCodeForm{},
	})
}

// handleJoinWithCodePOST validates a code for an already-logged-in user and
// adds the membership + default grants, then lands them in it.
func (a *App) handleJoinWithCodePOST(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	code := strings.TrimSpace(r.FormValue("code"))
	inv, err := a.liveInvite(ctx, code)
	if err != nil {
		a.render(w, r, http.StatusOK, "join_code.html", map[string]any{
			"Title": "Join with code",
			"F":     joinWithCodeForm{Err: "That invite is not valid (expired, used, or revoked)."},
		})
		return
	}
	if ok, _ := a.isMember(ctx, sess.UserID, inv.HouseholdID); ok {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if _, err := a.addMemberWithGrants(ctx, sess.UserID, inv.ID, inv.HouseholdID); err != nil {
		http.Error(w, "join failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.Sessions.SetHousehold(ctx, sess.ID, inv.HouseholdID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// addMemberWithGrants adds a user to a household as a member with the D27
// default grants and consumes the invite. Returns error if the code was
// already consumed/revoked.
func (a *App) addMemberWithGrants(ctx context.Context, userID, inviteID, householdID int64) (int64, error) {
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO household_member (user_id, household_id, role)
		 VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`, userID, householdID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO household_grant (user_id, household_id, can_read, can_create,
		    can_edit, can_delete, edit_mode, delete_mode, is_admin)
		 VALUES ($1, $2, TRUE, FALSE, FALSE, FALSE, 'created', 'created', FALSE)
		 ON CONFLICT (user_id, household_id) DO NOTHING`,
		userID, householdID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_grant (user_id, household_id, account_id, can_read, view_mode)
		SELECT $1, $2, a.id, TRUE, 'All' FROM account a
		WHERE a.household_id = $2
		  AND NOT EXISTS (SELECT 1 FROM account_grant ag
		              WHERE ag.user_id = $1 AND ag.household_id = $2 AND ag.account_id = a.id)`,
		userID, householdID); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE invite SET consumed_at = now(), consumed_by_user_id = $2
		WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`,
		inviteID, userID)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, fmt.Errorf("invite already used or revoked")
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return userID, nil
}

// pendingJoinPrompt reads the caller's smart-join cookie and returns the
// household/owner to offer (or nil when none pending or already a member).
// It does NOT clear the cookie — the dashboard clears it on the outcome.
func (a *App) pendingJoinPrompt(w http.ResponseWriter, r *http.Request) map[string]any {
	sess := sessionFromContext(r.Context())
	if sess == nil {
		return nil
	}
	c, err := r.Cookie(pendingJoinCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	ctx := r.Context()
	var inviteID, hhID int64
	var username string
	err = a.Pool.QueryRow(ctx, `
		SELECT ij.invite_id, i.household_id, ij.username
		FROM invite_join_token ij
		JOIN invite i ON i.id = ij.invite_id
		WHERE ij.token = $1 AND ij.expires_at > now()`, c.Value).
		Scan(&inviteID, &hhID, &username)
	if err != nil {
		clearPendingJoin(w)
		return nil
	}
	// The token names the username it was stored for; the signed-in user
	// must be that same person.
	var sessName string
	if err := a.Pool.QueryRow(ctx, `SELECT username FROM app_user WHERE id = $1`,
		sess.UserID).Scan(&sessName); err != nil {
		clearPendingJoin(w)
		return nil
	}
	if sessName != username {
		clearPendingJoin(w)
		return nil
	}
	if ok, _ := a.isMember(ctx, sess.UserID, hhID); ok {
		clearPendingJoin(w)
		return nil
	}
	name, _ := householdName(ctx, a.Pool, hhID)
	owner, _ := a.ownerOfHousehold(ctx, hhID)
	return map[string]any{
		"Token":     c.Value,
		"Household": name,
		"Owner":     owner,
		"HID":       hhID,
	}
}

func clearPendingJoin(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: pendingJoinCookie, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// householdName is a small shared lookup for prompts ("Join household X").
func householdName(ctx context.Context, pool *pgxpool.Pool, hh int64) (string, error) {
	var name string
	err := pool.QueryRow(ctx, `SELECT name FROM household WHERE id = $1`, hh).Scan(&name)
	if err != nil {
		return "", err
	}
	return name, nil
}
