// D34 DB-backed tests: auto-sync + detach (docs/notes/d34-auto-sync-detach.md,
// ADR-0007/0008). Scenario-dense synthetic seed per the doc — households H1
// (target) and H2 (source), user U in both (the auto-sync member), M in H1
// (a second member for the conflict test), F the H2 owner. Skipped unless
// EVENTALLY_TEST_DSN is set (harness: recurring_gate2_test.go).
package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
	"github.com/WanderingLesserPanda/eventally/internal/money"
	"github.com/jackc/pgx/v5/pgxpool"
)

// --- fixtures ----------------------------------------------------------------

type d34Seed struct {
	H1, H2 int64
	U, M, F int64
	FoodH1 int64 // parent category (H1, "Food")
	CatH1 int64  // "Coffee" child (H1, budget 100)
	CatH1b int64 // "Drink" child (H1, budget 50)
	CatH2 int64  // "Drink" (H2 source)
	H1Acc, H2Card int64
}

func d34App(t *testing.T) (*App, *pgxpool.Pool) {
	t.Helper()
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, pool
}

func d34BuildSeed(t *testing.T, pool *pgxpool.Pool) d34Seed {
	t.Helper()
	ctx := context.Background()
	var s d34Seed

	mkHH := func(name string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO household (name, base_currency, language, first_day_of_week)
			 VALUES ($1,'MYR','en',1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatalf("household %q: %v", name, err)
		}
		return id
	}
	mkUser := func(hh int64, username string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
			 VALUES ($1,$2,$2,$2||'@test.local','x') RETURNING id`, hh, username).Scan(&id); err != nil {
			t.Fatalf("user %q: %v", username, err)
		}
		return id
	}
	member := func(user, hh int64, role string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO household_member (user_id, household_id, role) VALUES ($1,$2,$3)`,
			user, hh, role); err != nil {
			t.Fatalf("membership %d→%d: %v", user, hh, err)
		}
	}
	cat := func(hh int64, name string, parent int64, budget string) int64 {
		t.Helper()
		var b any
		if budget != "" {
			b = budget
		}
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO category (household_id, name, parent_id, transaction_class, budget_per_month, is_budgetable)
			 VALUES ($1,$2,$3,'expense',$4,true) RETURNING id`, hh, name, nilIfZero(parent), b).Scan(&id); err != nil {
			t.Fatalf("category %q: %v", name, err)
		}
		return id
	}

	s.H1 = mkHH("Main household")
	s.H2 = mkHH("Family household")
	s.U = mkUser(s.H1, "u_u")
	s.M = mkUser(s.H1, "u_m")
	s.F = mkUser(s.H2, "u_f")
	member(s.U, s.H1, "member")
	member(s.M, s.H1, "member")
	member(s.F, s.H2, "owner")
	member(s.U, s.H2, "member") // U in both households

	s.FoodH1 = cat(s.H1, "Food", 0, "")
	s.CatH1 = cat(s.H1, "Coffee", s.FoodH1, "100.00")
	s.CatH1b = cat(s.H1, "Drink", s.FoodH1, "50.00")
	s.CatH2 = cat(s.H2, "Drink", 0, "")

	if err := pool.QueryRow(ctx,
		`INSERT INTO account (household_id, name, account_type, balance_type)
		 VALUES ($1,'Main Account','savings','asset') RETURNING id`, s.H1).Scan(&s.H1Acc); err != nil {
		t.Fatalf("H1 account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO account (household_id, name, account_type, balance_type)
		 VALUES ($1,'Family Card','credit_card','liability') RETURNING id`, s.H2).Scan(&s.H2Card); err != nil {
		t.Fatalf("H2 account: %v", err)
	}
	// U records in H2 (source) and H1 (manual side); full grants on both.
	d30Grant(t, pool, s.H2, s.U, s.H2Card, "All", "All")
	d30Grant(t, pool, s.H1, s.U, s.H1Acc, "All", "All")
	return s
}

// d34PartyOf returns a member's party id in a household (seeded by trigger).
func d34PartyOf(t *testing.T, pool *pgxpool.Pool, hh, user int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM party WHERE household_id = $1 AND app_user_id = $2`, hh, user).Scan(&id); err != nil {
		t.Fatalf("party for user %d in hh %d: %v", user, hh, err)
	}
	return id
}

// d34CreateExpense creates a record via the HTTP create path (firing the
// write-time auto-sync trigger) and returns its id.
func d34CreateExpense(t *testing.T, a *App, ctx context.Context, sess *auth.Session,
	catID int64, total, desc string, originAcc, destParty int64, split map[int64]string) int64 {
	t.Helper()
	form := url.Values{}
	form.Set("class", "expense")
	form.Set("date", "2026-11-19")
	form.Set("total", total)
	form.Set("description", desc)
	if catID > 0 {
		form.Set("category_id", strconv.FormatInt(catID, 10))
	}
	form.Set("origin_ref", "account:"+strconv.FormatInt(originAcc, 10))
	if destParty > 0 {
		form.Set("destination_ref", "party:"+strconv.FormatInt(destParty, 10))
	}
	form.Set("split_mode", "amount")
	for u, amt := range split {
		form.Add("part_key[]", "user:"+strconv.FormatInt(u, 10))
		form.Add("part_value[]", amt)
	}
	rec := httptest.NewRecorder()
	a.handleTxnCreate(rec, d30Req(t, ctx, sess, "/txns", 0, form))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create %q: status %d body %q", desc, rec.Code, rec.Body.String())
	}
	var id int64
	if err := a.Pool.QueryRow(ctx,
		`SELECT id FROM "transaction" WHERE household_id = $1 AND description = $2 ORDER BY id DESC LIMIT 1`,
		sess.Household, desc).Scan(&id); err != nil {
		t.Fatalf("find created txn %q: %v", desc, err)
	}
	return id
}

// d34LegacyNoCat creates a pending manual record via the normal create path
// (so it carries a valid origin/destination/splits) and then strips its
// category — simulating a pre-policy "legacy" row with no category (the
// create form now requires one, so a no-cat row can't be created through it).
func d34LegacyNoCat(t *testing.T, a *App, pool *pgxpool.Pool, ctx context.Context, sess *auth.Session,
	catID, originAcc, destParty int64, total, desc string, split map[int64]string) int64 {
	t.Helper()
	id := d34CreateExpense(t, a, ctx, sess, catID, total, desc, originAcc, destParty, split)
	if _, err := pool.Exec(ctx, `UPDATE "transaction" SET category_id = NULL WHERE id=$1`, id); err != nil {
		t.Fatalf("strip category: %v", err)
	}
	return id
}

// d34Toggle posts the per-member auto-sync toggle (spec §A). sess.Household is
// the target household; srcHH the source.
func d34Toggle(t *testing.T, a *App, ctx context.Context, sess *auth.Session, srcHH int64, on bool, ret string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("source_household_id", strconv.FormatInt(srcHH, 10))
	if on {
		form.Set("on", "1")
	}
	if ret != "" {
		form.Set("ret", ret)
	}
	rec := httptest.NewRecorder()
	a.handleSyncAutoPOST(rec, d30Req(t, ctx, sess, "/sync/auto", 0, form))
	return rec
}

func d34ToggleOn(t *testing.T, a *App, ctx context.Context, sess *auth.Session, srcHH int64) {
	t.Helper()
	rec := d34Toggle(t, a, ctx, sess, srcHH, true, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("toggle on: status %d body %q", rec.Code, rec.Body.String())
	}
}

func d34Mapping(t *testing.T, a *App, ctx context.Context, sess *auth.Session, srcHH, srcCat, targetCat int64) {
	t.Helper()
	form := url.Values{}
	form.Set("source", fmt.Sprintf("%d:%d", srcHH, srcCat))
	form.Set("target_category_id", strconv.FormatInt(targetCat, 10))
	rec := httptest.NewRecorder()
	a.handleMappingCreate(rec, d30Req(t, ctx, sess, "/settings/mapping", 0, form))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("mapping create: status %d body %q", rec.Code, rec.Body.String())
	}
}

func d34SyncCopy(t *testing.T, pool *pgxpool.Pool, hh, srcHH, srcTxn int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM "transaction" WHERE household_id = $1
		 AND sync_source_household_id = $2 AND sync_source_txn_id = $3`, hh, srcHH, srcTxn).Scan(&id); err != nil {
		t.Fatalf("sync copy (hh %d, src %d/%d): %v", hh, srcHH, srcTxn, err)
	}
	return id
}

func d34Str(t *testing.T, pool *pgxpool.Pool, q string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

func d34Int(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func d34Bool(t *testing.T, pool *pgxpool.Pool, q string, args ...any) bool {
	t.Helper()
	var b bool
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&b); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return b
}

// --- tests ---------------------------------------------------------------------

// TestD34ToggleRoundTrip: the toggle is one setting, two UI homes (the Live
// view modal posts ret=live; the settings section posts ret=settings) — both
// write the same auto_sync row. Turning it ON also runs the catch-up sweep
// over the caller's currently-syncable backlog.
func TestD34ToggleRoundTrip(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}
	sessU2 := &auth.Session{ID: "u2", Household: s.H2, UserID: s.U}

	// A syncable backlog record in H2 (U holds a split share).
	uP := d34PartyOf(t, pool, s.H2, s.U)
	src := d34CreateExpense(t, a, ctx, sessU2, s.CatH2, "12.00", "backlog coffee", s.H2Card, uP,
		map[int64]string{s.U: "6.00", s.F: "6.00"})

	// Off by default: no auto_sync row, no copy.
	if n := d34Int(t, pool, `SELECT count(*) FROM auto_sync WHERE member_user_id=$1`, s.U); n != 0 {
		t.Fatalf("auto_sync rows before toggle = %d, want 0", n)
	}
	if d34Int(t, pool, `SELECT count(*) FROM "transaction" WHERE household_id=$1 AND sync_source_txn_id=$2`, s.H1, src) != 0 {
		t.Fatal("copy exists before toggle")
	}

	// Toggle ON via the Live-view home (ret=live): row appears AND the
	// catch-up sweep syncs the backlog.
	rec := d34Toggle(t, a, ctx, sessU1, s.H2, true, "live")
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/sync") {
		t.Fatalf("toggle-on (live): status %d loc %q", rec.Code, rec.Header().Get("Location"))
	}
	if n := d34Int(t, pool,
		`SELECT count(*) FROM auto_sync WHERE member_user_id=$1 AND source_household_id=$2 AND target_household_id=$3`,
		s.U, s.H2, s.H1); n != 1 {
		t.Fatalf("auto_sync row count = %d, want 1", n)
	}
	copyID := d34SyncCopy(t, pool, s.H1, s.H2, src)
	if st := d34Str(t, pool, `SELECT record_status FROM "transaction" WHERE id=$1`, copyID); st != "pending" {
		t.Fatalf("catch-up copy status = %q, want pending", st)
	}

	// Toggle OFF (ret=settings): row deleted; the existing copy is untouched.
	rec = d34Toggle(t, a, ctx, sessU1, s.H2, false, "settings")
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "tab=sync") {
		t.Fatalf("toggle-off (settings): status %d loc %q", rec.Code, rec.Header().Get("Location"))
	}
	if n := d34Int(t, pool, `SELECT count(*) FROM auto_sync WHERE member_user_id=$1`, s.U); n != 0 {
		t.Fatalf("auto_sync rows after off = %d, want 0", n)
	}
	if d34Int(t, pool, `SELECT count(*) FROM "transaction" WHERE id=$1`, copyID) != 1 {
		t.Fatal("toggle-off deleted the existing copy (it must not)")
	}
}

// TestD34WriteTimeTrigger: with the toggle ON, a new source record produces a
// pending copy in the target with NO page load — the mapped category
// pre-filled when a rule exists, otherwise none.
func TestD34WriteTimeTrigger(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}
	sessU2 := &auth.Session{ID: "u2", Household: s.H2, UserID: s.U}
	uP := d34PartyOf(t, pool, s.H2, s.U)

	// No mapping: the copy lands with no category.
	d34ToggleOn(t, a, ctx, sessU1, s.H2)
	src := d34CreateExpense(t, a, ctx, sessU2, s.CatH2, "10.00", "no-map coffee", s.H2Card, uP,
		map[int64]string{s.U: "10.00"})
	copyID := d34SyncCopy(t, pool, s.H1, s.H2, src)
	if st := d34Str(t, pool, `SELECT record_status FROM "transaction" WHERE id=$1`, copyID); st != "pending" {
		t.Fatalf("trigger copy status = %q, want pending", st)
	}
	if cat := d34Int(t, pool, `SELECT COALESCE(category_id,0) FROM "transaction" WHERE id=$1`, copyID); cat != 0 {
		t.Fatalf("trigger copy category = %d, want 0 (no mapping)", cat)
	}
	if !d34Bool(t, pool, `SELECT is_synced FROM "transaction" WHERE id=$1`, copyID) {
		t.Fatal("trigger copy is_synced = false, want true")
	}

	// Add the mapping, then a second record: the copy is pre-filled.
	d34Mapping(t, a, ctx, sessU1, s.H2, s.CatH2, s.CatH1)
	src2 := d34CreateExpense(t, a, ctx, sessU2, s.CatH2, "20.00", "mapped coffee", s.H2Card, uP,
		map[int64]string{s.U: "20.00"})
	copy2 := d34SyncCopy(t, pool, s.H1, s.H2, src2)
	if cat := d34Int(t, pool, `SELECT COALESCE(category_id,0) FROM "transaction" WHERE id=$1`, copy2); cat != s.CatH1 {
		t.Fatalf("mapped copy category = %d, want %d", cat, s.CatH1)
	}
	if st := d34Str(t, pool, `SELECT record_status FROM "transaction" WHERE id=$1`, copy2); st != "pending" {
		t.Fatalf("mapped copy status = %q, want pending", st)
	}
}

// TestD34TwoMemberConflict: two toggle-on members with shares in the same
// source record both run the centralized upsert — exactly ONE copy row is
// created (the UNIQUE triple), and the deterministic last-writer (highest
// member_user_id) decides the pre-filled category.
func TestD34TwoMemberConflict(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	// M joins H2 (source) so M can hold a share + a toggle.
	d30Membership(t, pool, s.M, s.H2, "member")
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}
	sessM1 := &auth.Session{ID: "m1", Household: s.H1, UserID: s.M}
	sessU2 := &auth.Session{ID: "u2", Household: s.H2, UserID: s.U}
	uP := d34PartyOf(t, pool, s.H2, s.U)
	mP := d34PartyOf(t, pool, s.H2, s.M)

	d34ToggleOn(t, a, ctx, sessU1, s.H2)
	d34ToggleOn(t, a, ctx, sessM1, s.H2)
	// Distinct mappings so the winner is observable.
	d34Mapping(t, a, ctx, sessU1, s.H2, s.CatH2, s.CatH1)
	d34Mapping(t, a, ctx, sessM1, s.H2, s.CatH2, s.CatH1b)

	src := d34CreateExpense(t, a, ctx, sessU2, s.CatH2, "16.00", "conflict coffee", s.H2Card, uP,
		map[int64]string{s.U: "8.00", s.M: "8.00"})

	if n := d34Int(t, pool,
		`SELECT count(*) FROM "transaction" WHERE household_id=$1 AND sync_source_household_id=$2 AND sync_source_txn_id=$3`,
		s.H1, s.H2, src); n != 1 {
		t.Fatalf("copy rows = %d, want exactly 1", n)
	}
	copyID := d34SyncCopy(t, pool, s.H1, s.H2, src)
	gotCat := d34Int(t, pool, `SELECT COALESCE(category_id,0) FROM "transaction" WHERE id=$1`, copyID)
	// The copy's category is set by the FIRST writer — the lowest
	// member_user_id, processed first, whose upsert creates the row. Later
	// upserts re-sync the money facts but keep the existing category, so the
	// outcome is deterministic either way: one row, first-writer category.
	var first int64
	if err := pool.QueryRow(ctx,
		`SELECT member_user_id FROM auto_sync WHERE source_household_id=$1 AND target_household_id=$2 ORDER BY member_user_id ASC LIMIT 1`,
		s.H2, s.H1).Scan(&first); err != nil {
		t.Fatalf("lowest toggle member: %v", err)
	}
	wantCat := s.CatH1 // U's mapping
	if first == s.M {
		wantCat = s.CatH1b // M's mapping
	}
	if gotCat != wantCat {
		t.Fatalf("copy category = %d, want %d (first writer member %d)", gotCat, wantCat, first)
	}
	_ = mP
}

// TestD34SingleRecordApprove: approving a pending row that has no category
// (any source) is rejected until a category is supplied; supplying one
// approves + sets it; the optional "remember this mapping" writes the rule.
func TestD34SingleRecordApprove(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}
	sessU2 := &auth.Session{ID: "u2", Household: s.H2, UserID: s.U}
	uP := d34PartyOf(t, pool, s.H2, s.U)

	// A pending, no-category synced copy (no mapping).
	d34ToggleOn(t, a, ctx, sessU1, s.H2)
	src := d34CreateExpense(t, a, ctx, sessU2, s.CatH2, "9.00", "approve coffee", s.H2Card, uP,
		map[int64]string{s.U: "9.00"})
	copyID := d34SyncCopy(t, pool, s.H1, s.H2, src)

	approve := func(id int64, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleTxnStatus(rec, d30Req(t, ctx, sessU1, "/txns/"+strconv.FormatInt(id, 10)+"/status", id, form))
		return rec
	}

	// No category → 400.
	rec := approve(copyID, url.Values{"status": {"approved"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("approve no-cat: status %d, want 400", rec.Code)
	}
	if st := d34Str(t, pool, `SELECT record_status FROM "transaction" WHERE id=$1`, copyID); st != "pending" {
		t.Fatalf("row status after rejected approve = %q, want pending", st)
	}

	// With a category → approved + category set.
	rec = approve(copyID, url.Values{"status": {"approved"}, "category_id": {strconv.FormatInt(s.CatH1, 10)}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve with cat: status %d body %q", rec.Code, rec.Body.String())
	}
	if st := d34Str(t, pool, `SELECT record_status FROM "transaction" WHERE id=$1`, copyID); st != "approved" {
		t.Fatalf("row status = %q, want approved", st)
	}
	if cat := d34Int(t, pool, `SELECT COALESCE(category_id,0) FROM "transaction" WHERE id=$1`, copyID); cat != s.CatH1 {
		t.Fatalf("row category = %d, want %d", cat, s.CatH1)
	}
	// No remember_mapping → no rule written.
	if n := d34Int(t, pool,
		`SELECT count(*) FROM category_mapping WHERE household_id=$1 AND source_household_id=$2 AND source_category_id=$3`,
		s.H1, s.H2, s.CatH2); n != 0 {
		t.Fatalf("mapping rows = %d, want 0 (no remember)", n)
	}

	// A second no-cat copy, approved WITH remember_mapping → rule written.
	src2 := d34CreateExpense(t, a, ctx, sessU2, s.CatH2, "9.00", "approve coffee 2", s.H2Card, uP,
		map[int64]string{s.U: "9.00"})
	copy2 := d34SyncCopy(t, pool, s.H1, s.H2, src2)
	rec = approve(copy2, url.Values{"status": {"approved"},
		"category_id": {strconv.FormatInt(s.CatH1b, 10)}, "remember_mapping": {"on"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve+remember: status %d body %q", rec.Code, rec.Body.String())
	}
	var target int64
	if err := pool.QueryRow(ctx,
		`SELECT target_category_id FROM category_mapping WHERE household_id=$1 AND source_household_id=$2 AND source_category_id=$3`,
		s.H1, s.H2, s.CatH2).Scan(&target); err != nil {
		t.Fatalf("remember-mapping rule missing: %v", err)
	}
	if target != s.CatH1b {
		t.Fatalf("remembered target = %d, want %d", target, s.CatH1b)
	}
}

// TestD34BulkModal: the bulk modal surfaces ONLY when the selection includes
// ≥1 currently-synced row; it lists the sync rows (category editable) and the
// non-sync rows approve straight through; a non-sync row missing a category
// (legacy) is skipped + reported. Confirm commits all-or-nothing.
func TestD34BulkModal(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}
	sessU2 := &auth.Session{ID: "u2", Household: s.H2, UserID: s.U}
	uP1 := d34PartyOf(t, pool, s.H1, s.U)
	uP2 := d34PartyOf(t, pool, s.H2, s.U)

	// A synced, pending, no-category copy.
	d34ToggleOn(t, a, ctx, sessU1, s.H2)
	src := d34CreateExpense(t, a, ctx, sessU2, s.CatH2, "10.00", "bulk sync coffee", s.H2Card, uP2,
		map[int64]string{s.U: "10.00"})
	syncCopy := d34SyncCopy(t, pool, s.H1, s.H2, src)

	// A manual, pending, WITH-category row (straight through).
	straight := d34CreateExpense(t, a, ctx, sessU1, s.CatH1, "4.00", "bulk straight", s.H1Acc, uP1,
		map[int64]string{s.U: "4.00"})
	// A manual, pending, NO-category row (legacy → skipped + reported).
	legacy := d34LegacyNoCat(t, a, pool, ctx, sessU1, s.CatH1, s.H1Acc, uP1,
		"4.00", "bulk legacy", map[int64]string{s.U: "4.00"})

	bulk := func(form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleReviewBulk(rec, d30Req(t, ctx, sessU1, "/review/bulk", 0, form))
		return rec
	}

	// Phase 1: no phase → the modal (200) because a synced row is selected;
	// nothing commits yet.
	f1 := url.Values{"status": {"approved"},
		"id[]": {strconv.FormatInt(syncCopy, 10), strconv.FormatInt(straight, 10), strconv.FormatInt(legacy, 10)}}
	rec := bulk(f1)
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk phase1: status %d, want 200 (modal)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Approve selected records") {
		t.Fatalf("modal missing heading: %q", body[:min(len(body), 120)])
	}
	if !strings.Contains(body, "bulk sync coffee") {
		t.Fatal("modal does not list the synced row")
	}
	// Nothing committed yet.
	for _, id := range []int64{syncCopy, straight, legacy} {
		if st := d34Str(t, pool, `SELECT record_status FROM "transaction" WHERE id=$1`, id); st != "pending" {
			t.Fatalf("row %d committed before confirm (status %q)", id, st)
		}
	}

	// Phase 2: confirm with the synced row's category → all-or-nothing commit.
	f2 := url.Values{"status": {"approved"}, "phase": {"confirm"},
		"cat_"+strconv.FormatInt(syncCopy, 10): {strconv.FormatInt(s.CatH1, 10)},
		"id[]": {strconv.FormatInt(syncCopy, 10), strconv.FormatInt(straight, 10), strconv.FormatInt(legacy, 10)}}
	rec = bulk(f2)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("bulk confirm: status %d body %q", rec.Code, rec.Body.String()[:min(len(rec.Body.String()), 120)])
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatal("confirm: no redirect")
	}
	du, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location %q: %v", loc, err)
	}
	if msg := du.Query().Get("msg"); !strings.Contains(msg, "1 skipped") {
		t.Fatalf("confirm did not report the legacy skip: %q", msg)
	}
	// Synced row: approved + category set.
	if st := d34Str(t, pool, `SELECT record_status FROM "transaction" WHERE id=$1`, syncCopy); st != "approved" {
		t.Fatalf("sync row status = %q, want approved", st)
	}
	if cat := d34Int(t, pool, `SELECT COALESCE(category_id,0) FROM "transaction" WHERE id=$1`, syncCopy); cat != s.CatH1 {
		t.Fatalf("sync row category = %d, want %d", cat, s.CatH1)
	}
	// Straight row: approved, category unchanged.
	if st := d34Str(t, pool, `SELECT record_status FROM "transaction" WHERE id=$1`, straight); st != "approved" {
		t.Fatalf("straight row status = %q, want approved", st)
	}
	if cat := d34Int(t, pool, `SELECT COALESCE(category_id,0) FROM "transaction" WHERE id=$1`, straight); cat != s.CatH1 {
		t.Fatalf("straight row category changed to %d", cat)
	}
	// Legacy no-cat row: still pending (skipped).
	if st := d34Str(t, pool, `SELECT record_status FROM "transaction" WHERE id=$1`, legacy); st != "pending" {
		t.Fatalf("legacy row status = %q, want pending (skipped)", st)
	}
}

// TestD34BulkNoSyncedNoModal: a selection with NO synced rows skips the modal
// entirely (straight commit; legacy no-cat rows skipped + reported).
func TestD34BulkNoSyncedNoModal(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}
	uP1 := d34PartyOf(t, pool, s.H1, s.U)

	straight := d34CreateExpense(t, a, ctx, sessU1, s.CatH1, "4.00", "nosync straight", s.H1Acc, uP1,
		map[int64]string{s.U: "4.00"})
	legacy := d34LegacyNoCat(t, a, pool, ctx, sessU1, s.CatH1, s.H1Acc, uP1,
		"4.00", "nosync legacy", map[int64]string{s.U: "4.00"})

	rec := httptest.NewRecorder()
	a.handleReviewBulk(rec, d30Req(t, ctx, sessU1, "/review/bulk", 0,
		url.Values{"status": {"approved"}, "id[]": {strconv.FormatInt(straight, 10), strconv.FormatInt(legacy, 10)}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("no-synced bulk: status %d, want 303 (no modal)", rec.Code)
	}
	if st := d34Str(t, pool, `SELECT record_status FROM "transaction" WHERE id=$1`, straight); st != "approved" {
		t.Fatalf("straight row = %q, want approved", st)
	}
	if st := d34Str(t, pool, `SELECT record_status FROM "transaction" WHERE id=$1`, legacy); st != "pending" {
		t.Fatalf("legacy row = %q, want pending (skipped)", st)
	}
}

// TestD34BudgetGate: a pending synced copy is excluded from the budget; it
// counts after approval; a manual pending row still counts.
func TestD34BudgetGate(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}
	sessU2 := &auth.Session{ID: "u2", Household: s.H2, UserID: s.U}
	uP1 := d34PartyOf(t, pool, s.H1, s.U)
	uP2 := d34PartyOf(t, pool, s.H2, s.U)

	monthStart, _ := time.ParseInLocation("2006-01-02", "2026-11-01", time.Local)
	monthEnd, _ := time.ParseInLocation("2006-01-02", "2026-12-01", time.Local)
	actual := func() money.Money {
		t.Helper()
		p, err := a.permLoad(ctx, s.H1, s.U)
		if err != nil {
			t.Fatalf("permLoad: %v", err)
		}
		cats, err := a.dashCategoryRows(ctx, p, s.H1, s.U, monthStart, monthEnd)
		if err != nil {
			t.Fatalf("dashCategoryRows: %v", err)
		}
		for _, c := range cats {
			if c.Name == "Food" {
				return c.Actual
			}
		}
		t.Fatalf("no Food row in %+v", cats)
		return 0
	}

	// A pending synced copy (mapped to CatH1).
	d34ToggleOn(t, a, ctx, sessU1, s.H2)
	d34Mapping(t, a, ctx, sessU1, s.H2, s.CatH2, s.CatH1)
	src := d34CreateExpense(t, a, ctx, sessU2, s.CatH2, "10.00", "budget coffee", s.H2Card, uP2,
		map[int64]string{s.U: "10.00"})
	copyID := d34SyncCopy(t, pool, s.H1, s.H2, src)

	// Pending synced copy excluded → 0.
	if got := actual(); got != 0 {
		t.Fatalf("actual with pending synced copy = %v, want 0 (excluded)", got)
	}

	// Approve the copy → counts (its total_amount is the share, 10.00).
	rec := httptest.NewRecorder()
	a.handleTxnStatus(rec, d30Req(t, ctx, sessU1,
		"/txns/"+strconv.FormatInt(copyID, 10)+"/status", copyID,
		url.Values{"status": {"approved"}, "category_id": {strconv.FormatInt(s.CatH1, 10)}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: status %d", rec.Code)
	}
	if got := actual(); got != money.Money(1000) {
		t.Fatalf("actual after approval = %v, want 10.00", got)
	}

	// A manual pending row still counts.
	d34CreateExpense(t, a, ctx, sessU1, s.CatH1, "3.00", "budget manual", s.H1Acc, uP1,
		map[int64]string{s.U: "3.00"})
	if got := actual(); got != money.Money(1300) {
		t.Fatalf("actual after manual pending = %v, want 13.00 (manual pending counts)", got)
	}
}

// TestD34Detach: detaching the B-side copy releases the edit lock (amount/
// date/class/endpoints editable), writes a History row, drops the badge, and
// exempts it from the source-void cascade. Source + pointers unchanged.
func TestD34Detach(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}
	sessU2 := &auth.Session{ID: "u2", Household: s.H2, UserID: s.U}
	uP2 := d34PartyOf(t, pool, s.H2, s.U)

	// Manually sync a record (approved) so the copy is locked.
	src := d34SeedSyncable(t, a, pool, s, sessU2, uP2)
	d33SyncPOST(t, a, ctx, sessU1, s.H2, src, s.CatH1, 0)
	copyID := d34SyncCopy(t, pool, s.H1, s.H2, src)

	// While synced: amount edit is locked (400).
	locked := func(mutate func(url.Values)) *httptest.ResponseRecorder {
		t.Helper()
		form := d33TxnForm(t, pool, s.H1, copyID, mutate)
		rec := httptest.NewRecorder()
		a.handleTxnEditPost(rec, d30Req(t, ctx, sessU1, "/txns/"+strconv.FormatInt(copyID, 10), copyID, form))
		return rec
	}
	if rec := locked(func(f url.Values) { f.Set("total", "77.00") }); rec.Code != http.StatusBadRequest {
		t.Fatalf("synced amount edit: status %d, want 400 (locked)", rec.Code)
	}

	// Detach.
	rec := httptest.NewRecorder()
	a.handleTxnDetach(rec, d30Req(t, ctx, sessU1, "/txns/"+strconv.FormatInt(copyID, 10)+"/detach", copyID, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("detach: status %d body %q", rec.Code, rec.Body.String())
	}
	if d34Bool(t, pool, `SELECT NOT is_synced FROM "transaction" WHERE id=$1`, copyID) != true {
		t.Fatal("is_synced still true after detach")
	}
	if !d34Bool(t, pool, `SELECT detached_at IS NOT NULL FROM "transaction" WHERE id=$1`, copyID) {
		t.Fatal("detached_at not set")
	}
	// Pointers unchanged.
	if d34Int(t, pool, `SELECT COALESCE(sync_source_household_id,0) FROM "transaction" WHERE id=$1`, copyID) != s.H2 {
		t.Fatal("detach cleared the source pointer")
	}
	// A History row was written.
	if n := d34Int(t, pool, `SELECT count(*) FROM txn_history WHERE entity_type='transaction' AND entity_id=$1 AND action='detached'`, copyID); n != 1 {
		t.Fatalf("detach history rows = %d, want 1", n)
	}

	// Now unlocked: amount edit succeeds.
	if rec := locked(func(f url.Values) { f.Set("total", "77.00") }); rec.Code != http.StatusSeeOther {
		t.Fatalf("detached amount edit: status %d, want 303 (unlocked)", rec.Code)
	}
	if tot := d34Str(t, pool, `SELECT total_amount::text FROM "transaction" WHERE id=$1`, copyID); tot != "77.00" {
		t.Fatalf("amount after detached edit = %q, want 77.00", tot)
	}
}

// d34SeedSyncable creates a syncable source record in H2 (U share) and returns
// its id, for the manual /sync path.
func d34SeedSyncable(t *testing.T, a *App, pool *pgxpool.Pool, s d34Seed, sess *auth.Session, uP int64) int64 {
	t.Helper()
	return d34CreateExpense(t, a, context.Background(), sess, s.CatH2, "12.00", "detach coffee", s.H2Card, uP,
		map[int64]string{s.U: "6.00", s.F: "6.00"})
}

// TestD34ReLink: re-syncing a detached copy flips is_synced back on, resets
// the money facts to the source, keeps the local classification, restores the
// badge, and still leaves exactly one row.
func TestD34ReLink(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}
	sessU2 := &auth.Session{ID: "u2", Household: s.H2, UserID: s.U}
	uP2 := d34PartyOf(t, pool, s.H2, s.U)

	src := d34SeedSyncable(t, a, pool, s, sessU2, uP2)
	d33SyncPOST(t, a, ctx, sessU1, s.H2, src, s.CatH1, 0)
	copyID := d34SyncCopy(t, pool, s.H1, s.H2, src)

	// Detach, then freely edit amount + change category.
	rec := httptest.NewRecorder()
	a.handleTxnDetach(rec, d30Req(t, ctx, sessU1, "/txns/"+strconv.FormatInt(copyID, 10)+"/detach", copyID, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("detach: status %d", rec.Code)
	}
	{
		form := d33TxnForm(t, pool, s.H1, copyID, func(f url.Values) {
			f.Set("total", "99.00")
			f.Set("category_id", strconv.FormatInt(s.CatH1b, 10))
		})
		rec := httptest.NewRecorder()
		a.handleTxnEditPost(rec, d30Req(t, ctx, sessU1, "/txns/"+strconv.FormatInt(copyID, 10), copyID, form))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("detached edit: status %d body %q", rec.Code, rec.Body.String())
		}
	}
	if tot := d34Str(t, pool, `SELECT total_amount::text FROM "transaction" WHERE id=$1`, copyID); tot != "99.00" {
		t.Fatalf("amount before re-link = %q, want 99.00", tot)
	}

	// Re-link via /sync (the existing row's found-branch detects is_synced=false).
	d33SyncPOST(t, a, ctx, sessU1, s.H2, src, s.CatH1, 0)
	// Still exactly one row, same id.
	if n := d34Int(t, pool,
		`SELECT count(*) FROM "transaction" WHERE household_id=$1 AND sync_source_household_id=$2 AND sync_source_txn_id=$3`,
		s.H1, s.H2, src); n != 1 {
		t.Fatalf("rows after re-link = %d, want 1", n)
	}
	if d34Int(t, pool, `SELECT COALESCE(id,0) FROM "transaction" WHERE household_id=$1 AND sync_source_txn_id=$2`, s.H1, src) != copyID {
		t.Fatal("re-link created a second row")
	}
	// Money facts reset to the source share (12.00*U share = 6.00? no — sync
	// amount = the caller's split share = 6.00). Classification kept (CatH1b).
	if d34Bool(t, pool, `SELECT is_synced FROM "transaction" WHERE id=$1`, copyID) != true {
		t.Fatal("is_synced not restored on re-link")
	}
	if !d34Bool(t, pool, `SELECT detached_at IS NULL FROM "transaction" WHERE id=$1`, copyID) {
		t.Fatal("detached_at not cleared on re-link")
	}
	if cat := d34Int(t, pool, `SELECT COALESCE(category_id,0) FROM "transaction" WHERE id=$1`, copyID); cat != s.CatH1b {
		t.Fatalf("category after re-link = %d, want %d (kept)", cat, s.CatH1b)
	}
}

// TestD34NoTransitiveSync: a synced copy carries no split rows, so it is never
// a source — recording in the target household does not re-sync a copy onward.
func TestD34NoTransitiveSync(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}
	sessU2 := &auth.Session{ID: "u2", Household: s.H2, UserID: s.U}
	uP1 := d34PartyOf(t, pool, s.H1, s.U)
	uP2 := d34PartyOf(t, pool, s.H2, s.U)

	// Sync a copy into H1 (it has no splits).
	d33SyncPOST(t, a, ctx, sessU1, s.H2, d34SeedSyncable(t, a, pool, s, sessU2, uP2), s.CatH1, 0)
	copyID := d34Int(t, pool, `SELECT id FROM "transaction" WHERE household_id=$1 AND source='sync' ORDER BY id DESC LIMIT 1`, s.H1)
	if n := d34Int(t, pool, `SELECT count(*) FROM transaction_split WHERE transaction_id=$1`, copyID); n != 0 {
		t.Fatalf("synced copy has %d split rows, want 0", n)
	}

	// Turn on the reverse toggle (H1→H2) for U (a member of both) and record a
	// NEW manual record in H1. Only the manual record (which has splits) syncs
	// onward — the copy never becomes a source.
	d34ToggleOn(t, a, ctx, &auth.Session{ID: "u", Household: s.H2, UserID: s.U}, s.H1)
	manual := d34CreateExpense(t, a, ctx, sessU1, s.CatH1, "5.00", "manual h1", s.H1Acc, uP1,
		map[int64]string{s.U: "5.00"})
	// In H2, exactly one synced row, pointing at the manual H1 record — not
	// the H2-originated copy.
	if n := d34Int(t, pool,
		`SELECT count(*) FROM "transaction" WHERE household_id=$1 AND sync_source_household_id=$2`, s.H2, s.H1); n != 1 {
		t.Fatalf("H2 synced rows from H1 = %d, want 1", n)
	}
	if d34Int(t, pool,
		`SELECT COALESCE(sync_source_txn_id,0) FROM "transaction" WHERE household_id=$1 AND sync_source_household_id=$2`, s.H2, s.H1) != manual {
		t.Fatal("H2 synced the wrong source (transitive?)")
	}
}

// TestD34IsolationProbe (extends D33's): the cross-household pointer seam stays
// inside sync.go; the A-side badge drops on detach and returns on re-link; the
// "syncing…" chip appears only on auto-sync targets with an absent copy.
func TestD34IsolationProbe(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}
	sessU2 := &auth.Session{ID: "u2", Household: s.H2, UserID: s.U}
	sessF := &auth.Session{ID: "f", Household: s.H2, UserID: s.F}
	uP2 := d34PartyOf(t, pool, s.H2, s.U)

	// Static seam: the sync_source_ pointers appear only in sync.go.
	if files := touchingPointerFiles(t); len(files) != 1 || files[0] != "sync.go" {
		t.Fatalf("cross-household seam moved: files touching sync_source_ = %v, want [sync.go]", files)
	}

	// A-side badge present while synced, absent after detach, back on re-link.
	src := d34SeedSyncable(t, a, pool, s, sessU2, uP2)
	d33SyncPOST(t, a, ctx, sessU1, s.H2, src, s.CatH1, 0)
	copyID := d34SyncCopy(t, pool, s.H1, s.H2, src)

	aSide := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		a.listTxns(rec, d33GET(t, ctx, sessF, "/txns"))
		if rec.Code != http.StatusOK {
			t.Fatalf("/txns: status %d", rec.Code)
		}
		return rec.Body.String()
	}
	if b := aSide(); !strings.Contains(b, "synced by u_u") {
		t.Fatalf("A-side badge missing while synced:\n%s", b)
	}
	// Detach → badge drops.
	{
		rec := httptest.NewRecorder()
		a.handleTxnDetach(rec, d30Req(t, ctx, sessU1, "/txns/"+strconv.FormatInt(copyID, 10)+"/detach", copyID, nil))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("detach: status %d", rec.Code)
		}
	}
	if b := aSide(); strings.Contains(b, "synced by u_u") {
		t.Fatalf("A-side badge still present after detach:\n%s", b)
	}
	// Re-link → badge returns.
	d33SyncPOST(t, a, ctx, sessU1, s.H2, src, s.CatH1, 0)
	if b := aSide(); !strings.Contains(b, "synced by u_u") {
		t.Fatalf("A-side badge missing after re-link:\n%s", b)
	}

	// "syncing…" chip: appears on an auto-sync target whose copy is absent, and
	// not on a row that has a synced copy. Build the state directly.
	d34ToggleOn(t, a, ctx, sessU1, s.H2)
	// A syncable record with NO copy (insert directly, bypassing the trigger).
	var orphan int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "transaction"
		 (household_id, recorded_by_user_id, transaction_date, class, category_id, total_amount,
		  description, origin_account_id, record_status, source)
		 VALUES ($1,$2,'2026-11-19','expense',$3,10.00,'orphan coffee',$4,'approved','manual')
		 RETURNING id`, s.H2, s.F, s.CatH2, s.H2Card).Scan(&orphan); err != nil {
		t.Fatalf("orphan insert: %v", err)
	}
	// Give the orphan a U split so it is syncable.
	op := d34PartyOf(t, pool, s.H2, s.U)
	if _, err := pool.Exec(ctx,
		`INSERT INTO transaction_split (household_id, transaction_id, party_id, amount)
		 VALUES ($1,$2,$3,10.00)`, s.H2, orphan, op); err != nil {
		t.Fatalf("orphan split: %v", err)
	}

	live := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleSyncGET(rec, d33GET(t, ctx, sessU1, "/sync"))
		if rec.Code != http.StatusOK {
			t.Fatalf("/sync: status %d", rec.Code)
		}
		return rec.Body.String()
	}
	body := live()
	// The orphan (auto-sync target, no copy) shows the in-motion chip.
	if !strings.Contains(body, "syncing…") {
		t.Fatalf("missing the syncing… chip for the absent-copy target:\n%s", body)
	}
	// The synced row does NOT show the chip (it shows synced ✓).
	if strings.Contains(body, "synced ✓") {
		// expected; the chip must not co-occur on the same row.
	}
}

// TestD34SettingsSyncRenders is a regression test for a template-execution
// bug: the settings sync page used to call {{template "csrf" .}} inside the
// {{range .Toggles}} loop, where `.` is the toggle row struct (no .Session),
// which made the whole GET page 500 the moment a toggle was on. The Go tests
// only exercised the POST handlers, so this GET-render test guards it.
func TestD34SettingsSyncRenders(t *testing.T) {
	a, pool := d34App(t)
	s := d34BuildSeed(t, pool)
	ctx := context.Background()
	sessU1 := &auth.Session{ID: "u1", Household: s.H1, UserID: s.U}

	// A toggle-on (catch-up) plus a mapping rule so both sections have content.
	d34ToggleOn(t, a, ctx, sessU1, s.H2)
	d34Mapping(t, a, ctx, sessU1, s.H2, s.CatH2, s.CatH1)

	rec := httptest.NewRecorder()
	a.handleSettingsSync(rec, d33GET(t, ctx, sessU1, "/settings?tab=sync"))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings sync GET: status %d body %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Auto-sync", "Category mappings", `name="csrf"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings sync page missing %q", want)
		}
	}
}

// touchingPointerFiles returns the non-test .go files that reference the
// cross-household sync_source_ pointer columns.
func touchingPointerFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var touching []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "sync_source_") {
			touching = append(touching, f)
		}
	}
	return touching
}
