// DB-backed tests for D28 (000013/000014): multi-household identity —
// global usernames, login-landing order, invite lifecycle, smart join, and
// the household switcher's session patching. Scenario-dense synthetic seeds,
// no bulk data (goal item 9). Skipped unless EVENTALLY_TEST_DSN is set
// (harness: recurring_gate2_test.go).
package web

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/WanderingLesserPanda/eventally/internal/auth"
)

// --- fixtures ---------------------------------------------------------------

func d28Household(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO household (name) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatalf("household %q: %v", name, err)
	}
	return id
}

func d28User(t *testing.T, pool *pgxpool.Pool, defaultHH int64, username string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO app_user (default_household_id, username, password_hash, display_name)
		 VALUES ($1,$2,'x',$2) RETURNING id`, defaultHH, username).Scan(&id)
	if err != nil {
		t.Fatalf("user %q: %v", username, err)
	}
	return id
}

func d28Membership(t *testing.T, pool *pgxpool.Pool, user, hh int64, role string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO household_member (user_id, household_id, role) VALUES ($1,$2,$3)`,
		user, hh, role); err != nil {
		t.Fatalf("membership %d→%d (%s): %v", user, hh, role, err)
	}
}

// d28Invite mirrors handleInviteCreate's insert (SHA-256 hash at rest, 4-char
// prefix, expiry now()+days).
func d28Invite(t *testing.T, pool *pgxpool.Pool, hh, by int64, code string, days int) int64 {
	t.Helper()
	sum := sha256.Sum256([]byte(code))
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO invite (household_id, code_hash, code_prefix, created_by_user_id, expires_at)
		 VALUES ($1,$2,$3,$4, now() + ($5 || ' days')::interval) RETURNING id`,
		hh, fmt.Sprintf("%x", sum), code[:4], by, fmt.Sprint(days)).Scan(&id)
	if err != nil {
		t.Fatalf("invite %q: %v", code, err)
	}
	return id
}

// --- tests ------------------------------------------------------------------

// TestD28GlobalUsernameUniqueness: one username = one person, globally and
// case-insensitively, even across households (ADR-0002).
func TestD28GlobalUsernameUniqueness(t *testing.T) {
	pool := testPool(t, testDSN(t))
	ctx := context.Background()
	hh1 := d28Household(t, pool, "Uniq One")
	hh2 := d28Household(t, pool, "Uniq Two")
	d28User(t, pool, hh1, "SamePerson")
	var dummy int64
	err := pool.QueryRow(ctx,
		`INSERT INTO app_user (default_household_id, username, password_hash, display_name)
		 VALUES ($1,$2,'x',$2) RETURNING id`, hh2, "sameperson").Scan(&dummy)
	if err == nil {
		t.Fatal("case-insensitive duplicate username in a second household was accepted")
	}
}

// TestD28LandingOrder: default_household_id wins when still a membership,
// else the first remaining membership (alphabetical by name), else 0.
func TestD28LandingOrder(t *testing.T) {
	pool := testPool(t, testDSN(t))
	ctx := context.Background()
	a := &App{Pool: pool}

	alpha := d28Household(t, pool, "Alpha")
	beta := d28Household(t, pool, "Beta")
	u := d28User(t, pool, beta, "landuser") // default = Beta
	d28Membership(t, pool, u, alpha, "member")
	d28Membership(t, pool, u, beta, "owner")

	land := func() int64 {
		t.Helper()
		got, err := a.landHousehold(ctx, u)
		if err != nil {
			t.Fatalf("landHousehold: %v", err)
		}
		return got
	}

	if got := land(); got != beta {
		t.Fatalf("default membership: landed %d, want default %d", got, beta)
	}

	// Default pointing at a household the user no longer belongs to:
	// fall back to the first remaining membership, alphabetical.
	gamma := d28Household(t, pool, "Gamma")
	if err := a.setDefaultHousehold(ctx, u, gamma); err != nil {
		t.Fatalf("setDefaultHousehold: %v", err)
	}
	if got := land(); got != alpha {
		t.Fatalf("stale default: landed %d, want alphabetical first %d", got, alpha)
	}

	// No memberships at all: 0 (recovery), not an error. The no-orphan
	// rule blocks dropping a household's LAST member, so give each
	// household a keeper first.
	keeper := d28User(t, pool, alpha, "landkeeper01")
	d28Membership(t, pool, keeper, alpha, "member")
	d28Membership(t, pool, keeper, beta, "member")
	if _, err := pool.Exec(ctx, `DELETE FROM household_member WHERE user_id = $1`, u); err != nil {
		t.Fatalf("drop memberships: %v", err)
	}
	if got := land(); got != 0 {
		t.Fatalf("zero memberships: landed %d, want 0", got)
	}
}

// TestD28InviteLifecycle: codes are hashed at rest (prefix only shown),
// single-use, revocable, expiring, with the 5-outstanding cap.
func TestD28InviteLifecycle(t *testing.T) {
	pool := testPool(t, testDSN(t))
	ctx := context.Background()
	a := &App{Pool: pool}

	hh := d28Household(t, pool, "Invite Hh")
	owner := d28User(t, pool, hh, "invowner")
	d28Membership(t, pool, owner, hh, "owner")

	const code = "Ab3dEF9kLM01"
	id := d28Invite(t, pool, hh, owner, code, 7)

	// Hashed at rest: the plaintext code appears nowhere in the row.
	var hash, prefix string
	if err := pool.QueryRow(ctx,
		`SELECT code_hash, code_prefix FROM invite WHERE id = $1`, id).Scan(&hash, &prefix); err != nil {
		t.Fatalf("invite row: %v", err)
	}
	if hash == code || hash == code[:4] {
		t.Fatal("plaintext code stored in code_hash")
	}
	if prefix != code[:4] {
		t.Fatalf("prefix = %q, want %q", prefix, code[:4])
	}

	// Live lookup: right code works, wrong code does not.
	inv, err := a.liveInvite(ctx, code)
	if err != nil || inv.ID != id || inv.HouseholdID != hh {
		t.Fatalf("liveInvite(%q) = %+v, %v", code, inv, err)
	}
	if _, err := a.liveInvite(ctx, "WRONGCODE99999"); err == nil {
		t.Fatal("liveInvite accepted an unknown code")
	}

	// Expiry: a past expires_at is not live (the per-code override shape).
	// Backdate created_at too — CHECK requires expires_at > created_at.
	{
		sum := sha256.Sum256([]byte("ExPiReDc0de01"))
		var expired int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO invite (household_id, code_hash, code_prefix, created_by_user_id,
			                     created_at, expires_at)
			 VALUES ($1,$2,$3,$4, now() - interval '2 days', now() - interval '1 day')
			 RETURNING id`,
			hh, fmt.Sprintf("%x", sum), "ExPi", owner).Scan(&expired); err != nil {
			t.Fatalf("expired invite: %v", err)
		}
		if _, err := a.liveInvite(ctx, "ExPiReDc0de01"); err == nil {
			t.Fatalf("expired invite %d still live", expired)
		}
	}

	// Revoke: revoked_at kills the code.
	revoked := d28Invite(t, pool, hh, owner, "Rev0kEdCoDe01", 7)
	if _, err := pool.Exec(ctx,
		`UPDATE invite SET revoked_at = now() WHERE id = $1`, revoked); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := a.liveInvite(ctx, "Rev0kEdCoDe01"); err == nil {
		t.Fatalf("revoked invite %d still live", revoked)
	}

	// Single-use: completing the join consumes the code.
	joiner := d28User(t, pool, hh, "joiner01")
	if _, err := a.addMemberWithGrants(ctx, joiner, id, hh); err != nil {
		t.Fatalf("addMemberWithGrants: %v", err)
	}
	var member, grant, consumed bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM household_member WHERE user_id=$1 AND household_id=$2)`,
		joiner, hh).Scan(&member); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM household_grant
		                WHERE user_id=$1 AND household_id=$2 AND can_read AND NOT is_admin)`,
		joiner, hh).Scan(&grant); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT consumed_at IS NOT NULL AND consumed_by_user_id = $1 FROM invite WHERE id = $2`,
		joiner, id).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if !member || !grant || !consumed {
		t.Fatalf("join effects: member=%v readGrant=%v consumed=%v", member, grant, consumed)
	}
	if _, err := a.liveInvite(ctx, code); err == nil {
		t.Fatal("consumed code still live")
	}
	late := d28User(t, pool, hh, "joiner02")
	if _, err := a.addMemberWithGrants(ctx, late, id, hh); err == nil {
		t.Fatal("second use of a consumed invite succeeded")
	}

	// Cap: the handler's live-count expression must reflect exactly the
	// outstanding codes (unconsumed, unrevoked, unexpired).
	for i := 0; i < 4; i++ {
		c := fmt.Sprintf("Cap%04dCode01", i)
		d28Invite(t, pool, hh, owner, c, 7)
	}
	var live int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM invite
		 WHERE household_id = $1
		   AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > now()`,
		hh).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 4 {
		t.Fatalf("outstanding live invites = %d, want 4 (cap is %d)", live, maxOutstandingInvites)
	}
}

// TestD28SmartJoin: a /join collision stores a one-hour token keyed to the
// collided username; the dashboard prompt shows the household/owner and
// goes away once the user is a member; expired and foreign tokens vanish.
func TestD28SmartJoin(t *testing.T) {
	pool := testPool(t, testDSN(t))
	ctx := context.Background()
	a := &App{Pool: pool}

	target := d28Household(t, pool, "Join Target")
	owner := d28User(t, pool, target, "jtargetowner")
	d28Membership(t, pool, owner, target, "owner")
	home := d28Household(t, pool, "Join Home")
	existing := d28User(t, pool, home, "existing01")
	d28Membership(t, pool, existing, home, "owner")

	// Invalid codes never produce a token (the handler gates on liveInvite).
	if _, err := a.liveInvite(ctx, "N0SuchCode99"); err == nil {
		t.Fatal("liveInvite accepted a bogus code")
	}

	const invCode = "Sm4rTJoinC0de1"
	inv := d28Invite(t, pool, target, owner, invCode, 7)

	// Collision: mirror handleJoinPOST's token insert.
	token := randTokenHex(16)
	if token == "" {
		t.Fatal("randTokenHex returned empty")
	}
	var tokID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO invite_join_token (token, invite_id, username, expires_at)
		 VALUES ($1,$2,$3, now() + ($4 || ' seconds')::interval) RETURNING token`,
		token, inv, "existing01", fmt.Sprint(int(pendingJoinTTL.Seconds()))).Scan(&tokID); err != nil {
		t.Fatalf("token insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM invite_join_token WHERE token = $1`, tokID)
	})

	// The token's TTL is the one-hour window.
	var expires time.Time
	if err := pool.QueryRow(ctx,
		`SELECT expires_at FROM invite_join_token WHERE token = $1`, token).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if d := time.Until(expires); d < 55*time.Minute || d > 65*time.Minute {
		t.Fatalf("token TTL = %v, want ~1h", d)
	}

	prompt := func(cookie string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: pendingJoinCookie, Value: cookie})
		sess := &auth.Session{ID: "test", Household: home, UserID: existing}
		return a.pendingJoinPrompt(httptest.NewRecorder(),
			req.WithContext(contextWithSession(ctx, sess)))
	}

	// The collided (not-yet-member) user sees the prompt with household+owner.
	p := prompt(token)
	if p == nil {
		t.Fatal("pending prompt missing for a valid token")
	}
	if p["Household"] != "Join Target" || p["Owner"] != "jtargetowner" || p["HID"] != target {
		t.Fatalf("prompt = %+v", p)
	}

	// After joining, the prompt goes away.
	if _, err := a.addMemberWithGrants(ctx, existing, inv, target); err != nil {
		t.Fatalf("join complete: %v", err)
	}
	if p := prompt(token); p != nil {
		t.Fatalf("prompt still offered to a member: %+v", p)
	}

	// A token stored for someone else never matches.
	const otherCode = "0therPers0nC0d1"
	otherInv := d28Invite(t, pool, target, owner, otherCode, 7)
	otherTok := randTokenHex(16)
	if _, err := pool.Exec(ctx,
		`INSERT INTO invite_join_token (token, invite_id, username, expires_at)
		 VALUES ($1,$2,$3, now() + interval '1 hour')`, otherTok, otherInv, "nobody"); err != nil {
		t.Fatal(err)
	}
	if p := prompt(otherTok); p != nil {
		t.Fatalf("foreign-username token produced a prompt: %+v", p)
	}

	// An expired token vanishes.
	expTok := randTokenHex(16)
	if _, err := pool.Exec(ctx,
		`INSERT INTO invite_join_token (token, invite_id, username, expires_at)
		 VALUES ($1,$2,$3, now() - interval '1 minute')`, expTok, otherInv, "existing01"); err != nil {
		t.Fatal(err)
	}
	if p := prompt(expTok); p != nil {
		t.Fatalf("expired token produced a prompt: %+v", p)
	}
}

// TestD28HouseholdSwitch: the switcher's session patch (SetHousehold) moves
// the viewed household, 0 clears it to NULL, and membership gates the
// switch (isMember).
func TestD28HouseholdSwitch(t *testing.T) {
	pool := testPool(t, testDSN(t))
	ctx := context.Background()
	a := &App{Pool: pool}
	store := auth.NewStore(pool)

	hh1 := d28Household(t, pool, "Switch One")
	hh2 := d28Household(t, pool, "Switch Two")
	hh3 := d28Household(t, pool, "Switch Three")
	u := d28User(t, pool, hh1, "switchuser")
	d28Membership(t, pool, u, hh1, "owner")
	d28Membership(t, pool, u, hh2, "member")

	sess, err := store.Create(ctx, hh1, u, "", "")
	if err != nil {
		t.Fatalf("session create: %v", err)
	}
	if sess.Household != hh1 {
		t.Fatalf("fresh session household = %d, want %d", sess.Household, hh1)
	}
	if err := store.SetHousehold(ctx, sess.ID, hh2); err != nil {
		t.Fatalf("switch: %v", err)
	}
	got, err := store.Get(ctx, sess.ID)
	if err != nil || got.Household != hh2 {
		t.Fatalf("after switch: %+v, %v", got, err)
	}
	if err := store.SetHousehold(ctx, sess.ID, 0); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err = store.Get(ctx, sess.ID)
	if err != nil || got.Household != 0 {
		t.Fatalf("after clear: %+v, %v (want household 0/NULL)", got, err)
	}

	// Membership gate: the user may switch into hh2, not hh3.
	for hh, want := range map[int64]bool{hh1: true, hh2: true, hh3: false} {
		ok, err := a.isMember(ctx, u, hh)
		if err != nil || ok != want {
			t.Fatalf("isMember(%d) = %v, %v (want %v)", hh, ok, err, want)
		}
	}
}
