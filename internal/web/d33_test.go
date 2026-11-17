// D33 DB-backed tests: cross-household synced records (the spec's
// verification section; ADR-0006). Scenario-dense synthetic seed per the
// doc — households H1 (owner P) and H2 (owner F), user U in both, member M
// in H1 Related-mode only; H2 expense T1 split U/F (syncable), T2 (U
// recorded, no U split — related but not syncable), T3 (U funded a
// settlement leg — related but not syncable). Skipped unless
// EVENTALLY_TEST_DSN is set (harness: recurring_gate2_test.go).
package web

import (
	"context"
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
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// --- fixtures ----------------------------------------------------------------

type d33Seed struct {
	H1, H2 int64
	P, F, U, M int64
	T1, T2, T3 int64
	CatCoffee, CatDrinkH1 int64
	FamilyCard int64
}

// d33App returns an App with templates loaded on a fresh migrated
// throwaway database.
func d33App(t *testing.T) (*App, *pgxpool.Pool) {
	t.Helper()
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, pool
}

// d33Seed builds the spec's scenario in one pass (neutral labels: the seed
// users are labelled U/F/P/M by role, households "Main household" and
// "Family household"). Member party rows are seeded by the
// household_member trigger.
func d33BuildSeed(t *testing.T, pool *pgxpool.Pool) d33Seed {
	t.Helper()
	ctx := context.Background()
	var s d33Seed

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
	partyID := func(hh int64, name string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx,
			`SELECT id FROM party WHERE household_id = $1 AND name = $2`, hh, name).Scan(&id); err != nil {
			t.Fatalf("party %q: %v", name, err)
		}
		return id
	}
	uDisplayName := func(u int64) string {
		t.Helper()
		var n string
		if err := pool.QueryRow(context.Background(),
			`SELECT display_name FROM app_user WHERE id = $1`, u).Scan(&n); err != nil {
			t.Fatalf("display name: %v", err)
		}
		return n
	}
	// A source-side expense with per-user splits (one tx: the deferred
	// split-sum trigger validates at commit).
	expense := func(hh, acc, catID, recordedBy int64, total, date, desc string, shares map[int64]string) int64 {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO "transaction"
				(household_id, recorded_by_user_id, transaction_date, class,
				 category_id, total_amount, description, origin_account_id, record_status)
			VALUES ($1,$2,$3,'expense',$4,$5,$6,$7,'approved') RETURNING id`,
			hh, recordedBy, date, catID, total, desc, acc).Scan(&id); err != nil {
			t.Fatalf("txn %q: %v", desc, err)
		}
		for u, amt := range shares {
			p := partyID(hh, uDisplayName(u))
			if _, err := tx.Exec(ctx,
				`INSERT INTO transaction_split (household_id, transaction_id, party_id, amount)
				 VALUES ($1,$2,$3,$4)`, hh, id, p, amt); err != nil {
				t.Fatalf("split %q: %v", desc, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %q: %v", desc, err)
		}
		return id
	}

	s.H1 = mkHH("Main household")
	s.H2 = mkHH("Family household")
	s.P = mkUser(s.H1, "u_p")
	s.F = mkUser(s.H2, "u_f")
	s.U = mkUser(s.H1, "u_u") // default household = H1
	s.M = mkUser(s.H1, "u_m")
	member(s.P, s.H1, "owner")
	member(s.U, s.H1, "member")
	member(s.M, s.H1, "member")
	member(s.F, s.H2, "owner")
	member(s.U, s.H2, "member")

	// H1 budgeted categories: Food & Drink > Coffee (100.00), Drink (50.00).
	foodH1 := cat(s.H1, "Food & Drink", 0, "")
	s.CatCoffee = cat(s.H1, "Coffee", foodH1, "100.00")
	s.CatDrinkH1 = cat(s.H1, "Drink", foodH1, "50.00")
	foodH2 := cat(s.H2, "Food & Drink", 0, "")
	catDrinkH2 := cat(s.H2, "Drink", foodH2, "")

	var acc int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO account (household_id, name, account_type, balance_type)
		 VALUES ($1,'Main Account','savings','asset') RETURNING id`, s.H1).Scan(&acc); err != nil {
		t.Fatalf("account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO account (household_id, name, account_type, balance_type)
		 VALUES ($1,'Family Card','credit_card','liability') RETURNING id`, s.H2).Scan(&s.FamilyCard); err != nil {
		t.Fatalf("family card: %v", err)
	}

	// Grants: M holds Related-mode read on the H1 account (Related mode
	// only); U holds a normal All-view grant.
	if _, err := pool.Exec(ctx,
		`INSERT INTO account_grant (user_id, household_id, account_id,
		 can_read, can_create, can_edit, can_delete, view_mode, edit_mode, delete_mode)
		 VALUES ($1,$2,$3, true, false, false, false, 'Related', 'Related', 'Related')`,
		s.M, s.H1, acc); err != nil {
		t.Fatalf("M grant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO account_grant (user_id, household_id, account_id,
		 can_read, can_create, can_edit, can_delete, view_mode, edit_mode, delete_mode)
		 VALUES ($1,$2,$3, true, true, true, true, 'All', 'All', 'All')`,
		s.U, s.H1, acc); err != nil {
		t.Fatalf("U grant: %v", err)
	}
	// U holds a Related-mode read grant on the H2 family card — the typical
	// member grant. That read surface is what puts T1 in U's /txns in H2
	// (D27), and therefore in the Live view as syncable.
	if _, err := pool.Exec(ctx,
		`INSERT INTO account_grant (user_id, household_id, account_id,
		 can_read, can_create, can_edit, can_delete, view_mode, edit_mode, delete_mode)
		 VALUES ($1,$2,$3, true, false, false, false, 'Related', 'Related', 'Related')`,
		s.U, s.H2, s.FamilyCard); err != nil {
		t.Fatalf("U grant on H2 card: %v", err)
	}

	// H2 records.
	s.T1 = expense(s.H2, s.FamilyCard, catDrinkH2, s.F,
		"12.00", "2026-11-12", "coffee beverage",
		map[int64]string{s.U: "6.00", s.F: "6.00"})
	s.T2 = expense(s.H2, s.FamilyCard, catDrinkH2, s.U,
		"15.00", "2026-11-08", "lunch set",
		map[int64]string{s.F: "15.00"})
	s.T3 = expense(s.H2, s.FamilyCard, catDrinkH2, s.F,
		"20.00", "2026-11-05", "streaming subscription",
		map[int64]string{s.F: "20.00"})
	// T3 gets a settlement with a U funding leg (related via the leg, not
	// a split — must NOT be syncable).
	var splitT3 int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM transaction_split WHERE transaction_id = $1`, s.T3).Scan(&splitT3); err != nil {
		t.Fatalf("T3 split: %v", err)
	}
	var settleID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO settlement (household_id, account_id, amount, payment_date, notes)
		VALUES ($1,$2,20.00,'2026-11-05','') RETURNING id`, s.H2, s.FamilyCard).Scan(&settleID); err != nil {
		t.Fatalf("settlement: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO settlement_funding (household_id, settlement_id, user_id, method, amount)
		 VALUES ($1,$2,$3,'transfer',20.00)`, s.H2, settleID, s.U); err != nil {
		t.Fatalf("funding leg: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO settlement_allocation (household_id, payment_id, split_id, amount)
		 VALUES ($1,$2,$3,20.00)`, s.H2, settleID, splitT3); err != nil {
		t.Fatalf("allocation: %v", err)
	}
	return s
}

// d33GET builds a GET request with a session in the context (the authed
// middleware is bypassed; the handlers' own gates are under test).
func d33GET(t *testing.T, ctx context.Context, sess *auth.Session, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	return r.WithContext(contextWithSession(ctx, sess))
}

// d33SyncPOST posts one sync/re-sync form to /sync.
func d33SyncPOST(t *testing.T, a *App, ctx context.Context, sess *auth.Session, srcHH, srcTxn, catID, merchID int64) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{
		"source_household_id": {strconv.FormatInt(srcHH, 10)},
		"source_txn_id":       {strconv.FormatInt(srcTxn, 10)},
		"category_id":         {strconv.FormatInt(catID, 10)},
	}
	if merchID > 0 {
		form.Set("merchant_id", strconv.FormatInt(merchID, 10))
	}
	rec := httptest.NewRecorder()
	a.handleSyncPOST(rec, d30Req(t, ctx, sess, "/sync", 0, form))
	return rec
}

// d33TxnForm builds a full edit form for a row (the edit-lock cases flip
// one field at a time against this base).
func d33TxnForm(t *testing.T, pool *pgxpool.Pool, hh, id int64, mutate func(url.Values)) url.Values {
	t.Helper()
	ctx := context.Background()
	var (
		class, desc, notes              string
		date                             time.Time
		total                            money.Money
		catID, merchID, originRef, destRef any
	)
	err := pool.QueryRow(ctx, `
		SELECT t.class, t.transaction_date, t.total_amount,
		       COALESCE(t.description,''), COALESCE(t.notes,''),
		       COALESCE(t.category_id,0), COALESCE(t.merchant_id,0),
		       CASE WHEN t.origin_party_id IS NOT NULL THEN 'party:'
		            WHEN t.origin_account_id IS NOT NULL THEN 'account:'
		            ELSE '' END ||
		       CASE WHEN t.origin_party_id IS NOT NULL THEN t.origin_party_id::text
		            WHEN t.origin_account_id IS NOT NULL THEN t.origin_account_id::text
		            ELSE '' END,
		       CASE WHEN t.destination_party_id IS NOT NULL THEN 'party:'
		            WHEN t.destination_account_id IS NOT NULL THEN 'account:'
		            ELSE '' END ||
		       CASE WHEN t.destination_party_id IS NOT NULL THEN t.destination_party_id::text
		            WHEN t.destination_account_id IS NOT NULL THEN t.destination_account_id::text
		            ELSE '' END
		FROM "transaction" t WHERE t.id = $1 AND t.household_id = $2`, id, hh).
		Scan(&class, &date, &total, &desc, &notes, &catID, &merchID, &originRef, &destRef)
	if err != nil {
		t.Fatalf("load row %d: %v", id, err)
	}
	form := url.Values{
		"class":         {class},
		"date":          {date.Format("2006-01-02")},
		"total":         {total.String()},
		"description":   {desc},
		"notes":         {notes},
		"category_id":   {strconv.FormatInt(catID.(int64), 10)},
		"merchant_id":   {strconv.FormatInt(merchID.(int64), 10)},
		"origin_ref":    {originRef.(string)},
		"destination_ref": {destRef.(string)},
		"split_mode":    {"amount"},
	}
	if mutate != nil {
		mutate(form)
	}
	return form
}

// d33HistoryRows returns the txn_history (action, summary, actor) rows of a
// transaction.
func d33HistoryRows(t *testing.T, pool *pgxpool.Pool, hh, txn int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT action || ' | ' || summary || ' | ' || actor_id
		 FROM txn_history
		 WHERE household_id = $1 AND entity_type = 'transaction' AND entity_id = $2
		 ORDER BY id`, hh, txn)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatalf("history scan: %v", err)
		}
		out = append(out, l)
	}
	return out
}

// --- tests ---------------------------------------------------------------------

// TestD33LiveViewListsOnlySyncable: the Live view lists, per other
// household, exactly the caller's syncable records — expense, unvoided,
// caller holds a split share > 0. T2 (recorded, no split) and T3 (funded a
// settlement leg) stay invisible.
func TestD33LiveViewListsOnlySyncable(t *testing.T) {
	a, pool := d33App(t)
	s := d33BuildSeed(t, pool)
	ctx := context.Background()

	sections, err := a.syncLiveSections(ctx, s.H1, s.U)
	if err != nil {
		t.Fatalf("syncLiveSections(U, H1): %v", err)
	}
	if len(sections) != 1 || sections[0].HouseholdID != s.H2 {
		t.Fatalf("U in H1: sections = %+v, want exactly H2", sections)
	}
	rows := sections[0].Rows
	if len(rows) != 1 || rows[0].SrcTxnID != s.T1 || rows[0].Share != money.Money(600) {
		t.Fatalf("U in H1: rows = %+v, want exactly T1 at share 6.00", rows)
	}

	// U viewing H2: H1 is the other household — U holds no H1 splits, so
	// the section is empty (but present).
	sections, err = a.syncLiveSections(ctx, s.H2, s.U)
	if err != nil {
		t.Fatalf("syncLiveSections(U, H2): %v", err)
	}
	if len(sections) != 1 || len(sections[0].Rows) != 0 {
		t.Fatalf("U in H2: sections = %+v, want one empty H1 section", sections)
	}

	// M (H1 only) and F (H2 only) hold no other household at all.
	for _, sess := range []struct {
		user, hh int64
	}{
		{s.M, s.H1}, {s.F, s.H2},
	} {
		sections, err = a.syncLiveSections(ctx, sess.hh, sess.user)
		if err != nil {
			t.Fatalf("syncLiveSections: %v", err)
		}
		if len(sections) != 0 {
			t.Fatalf("user in one household: sections = %+v, want none", sections)
		}
	}
	_ = pool
}

// TestD33SyncCreatesExactlyAndDuplicateIsNoOp: a sync creates exactly the
// specified row (share 6.00, source household's party origin, approved, no
// splits, no account endpoints); a duplicate sync re-classifies the one
// row, never doubles it.
func TestD33SyncCreatesExactlyAndDuplicateIsNoOp(t *testing.T) {
	a, pool := d33App(t)
	s := d33BuildSeed(t, pool)
	ctx := context.Background()
	sess := &auth.Session{ID: "u", Household: s.H1, UserID: s.U}

	rec := d33SyncPOST(t, a, ctx, sess, s.H2, s.T1, s.CatCoffee, 0)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sync: status %d body %q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/sync?msg=") {
		t.Fatalf("sync redirect = %q", loc)
	}

	count := d30Int(t, pool, `
		SELECT count(*) FROM "transaction"
		WHERE household_id = $1 AND sync_source_household_id = $2 AND sync_source_txn_id = $3`,
		s.H1, s.H2, s.T1)
	if count != 1 {
		t.Fatalf("synced rows = %d, want 1", count)
	}
	var (
		id, recordedBy int64
		originAcc, destAcc, destParty *int64
		source, class, status, desc string
		total money.Money
		catID int64
		origName, origType string
		origUser *int64
	)
	err := pool.QueryRow(context.Background(), `
		SELECT t.id, t.source, t.class, t.record_status, t.total_amount,
		       t.category_id, t.description, t.recorded_by_user_id,
		       t.origin_account_id, t.destination_account_id, t.destination_party_id,
		       p.name, p.type, p.app_user_id
		FROM "transaction" t JOIN party p ON p.id = t.origin_party_id
		WHERE t.household_id = $1 AND t.id = (
			SELECT id FROM "transaction" WHERE sync_source_household_id = $2 AND sync_source_txn_id = $3)`,
		s.H1, s.H2, s.T1).
		Scan(&id, &source, &class, &status, &total, &catID, &desc, &recordedBy,
			&originAcc, &destAcc, &destParty, &origName, &origType, &origUser)
	if err != nil {
		t.Fatalf("load synced row: %v", err)
	}
	if source != "sync" || class != "expense" || status != "approved" ||
		total != money.Money(600) || catID != s.CatCoffee || desc != "coffee beverage" ||
		recordedBy != s.U {
		t.Fatalf("synced row = (source %q, class %q, status %q, total %v, cat %d, desc %q, by %d)",
			source, class, status, total, catID, desc, recordedBy)
	}
	if originAcc != nil || destAcc != nil || destParty != nil {
		t.Fatalf("synced row has account endpoints (origin %v dest %v/%v)", originAcc, destAcc, destParty)
	}
	if origName != "Family household" || origType != "organisation" || origUser != nil {
		t.Fatalf("origin party = (%q, %q, %v), want (Family household, organisation, nil)", origName, origType, origUser)
	}
	if n := d30Int(t, pool, `SELECT count(*) FROM transaction_split WHERE transaction_id = $1`, id); n != 0 {
		t.Fatalf("synced row splits = %d, want 0", n)
	}

	// Duplicate sync: no second row; the redirect is the re-sync message.
	rec = d33SyncPOST(t, a, ctx, sess, s.H2, s.T1, s.CatCoffee, 0)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("re-sync: status %d body %q", rec.Code, rec.Body.String())
	}
	if count := d30Int(t, pool, `
		SELECT count(*) FROM "transaction"
		WHERE household_id = $1 AND sync_source_household_id = $2 AND sync_source_txn_id = $3`,
		s.H1, s.H2, s.T1); count != 1 {
		t.Fatalf("after duplicate sync: rows = %d, want 1", count)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, url.QueryEscape("Re-synced")) {
		t.Fatalf("duplicate redirect = %q, want the re-sync message", loc)
	}

	// Re-validation failures.
	if rec := d33SyncPOST(t, a, ctx, sess, s.H2, s.T2, s.CatCoffee, 0); rec.Code != http.StatusBadRequest {
		t.Fatalf("sync of T2 (no U split): status %d, want 400", rec.Code)
	}
	if rec := d33SyncPOST(t, a, ctx, sess, s.H2, s.T3, s.CatCoffee, 0); rec.Code != http.StatusBadRequest {
		t.Fatalf("sync of T3 (leg-funded): status %d, want 400", rec.Code)
	}
	// A source household the caller does not belong to.
	otherHH := d30Household(t, pool, "Stranger household")
	if rec := d33SyncPOST(t, a, ctx, sess, otherHH, s.T1, s.CatCoffee, 0); rec.Code != http.StatusBadRequest {
		t.Fatalf("sync from a non-member household: status %d, want 400", rec.Code)
	}
	// A category from the source household (not this one).
	var catH2 int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM category WHERE household_id = $1 AND name = 'Drink'`, s.H2).Scan(&catH2); err != nil {
		t.Fatalf("H2 category: %v", err)
	}
	if rec := d33SyncPOST(t, a, ctx, sess, s.H2, s.T1, catH2, 0); rec.Code != http.StatusBadRequest {
		t.Fatalf("sync with a foreign category: status %d, want 400", rec.Code)
	}
}

// TestD33ReSyncRefreshesMoneyFactsOnly: after the source's amount/date
// change, the Live view derives "source changed"; a re-sync refreshes
// amount and date only — the copy keeps its classification.
func TestD33ReSyncRefreshesMoneyFactsOnly(t *testing.T) {
	a, pool := d33App(t)
	s := d33BuildSeed(t, pool)
	ctx := context.Background()
	sess := &auth.Session{ID: "u", Household: s.H1, UserID: s.U}

	d33SyncPOST(t, a, ctx, sess, s.H2, s.T1, s.CatCoffee, 0)
	// A partner re-classifies the copy to Drink (the freely-editable
	// surface) before the source moves.
	var catID int64
	if err := pool.QueryRow(ctx, `
		UPDATE "transaction"
		SET category_id = $2
		WHERE household_id = $1 AND sync_source_household_id = $3 AND sync_source_txn_id = $4
		RETURNING id`, s.H1, s.CatDrinkH1, s.H2, s.T1).Scan(&catID); err != nil {
		t.Fatalf("reclassify: %v", err)
	}

	// The source record changes: total 12.00 → 18.00, date → 11-15, splits
	// 9/9 (one tx: the deferred split-sum trigger checks at commit).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "transaction"
		SET total_amount = 18.00, transaction_date = '2026-11-15'
		WHERE id = $1`, s.T1); err != nil {
		t.Fatalf("source change: %v", err)
	}
	for u, amt := range map[int64]string{s.U: "9.00", s.F: "9.00"} {
		name := userDisplay(t, pool, u)
		if _, err := tx.Exec(ctx, `
			UPDATE transaction_split SET amount = $2
			WHERE transaction_id = $1 AND party_id = (
				SELECT id FROM party WHERE household_id = $3 AND name = $4)`,
			s.T1, amt, s.H2, name); err != nil {
			t.Fatalf("source split: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	sections, err := a.syncLiveSections(ctx, s.H1, s.U)
	if err != nil {
		t.Fatalf("syncLiveSections: %v", err)
	}
	if len(sections) != 1 || len(sections[0].Rows) != 1 {
		t.Fatalf("sections = %+v", sections)
	}
	row := sections[0].Rows[0]
	if !row.Synced || !row.SourceChanged {
		t.Fatalf("row = %+v, want synced + source-changed", row)
	}

	// Re-sync: money facts refresh, the Drink classification is kept.
	rec := d33SyncPOST(t, a, ctx, sess, s.H2, s.T1, s.CatCoffee, 0)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("re-sync: status %d body %q", rec.Code, rec.Body.String())
	}
	var (
		total money.Money
		date  string
	)
	if err := pool.QueryRow(ctx, `
		SELECT total_amount, to_char(transaction_date,'YYYY-MM-DD')
		FROM "transaction" WHERE id = $1`, catID).Scan(&total, &date); err != nil {
		t.Fatalf("load: %v", err)
	}
	if total != money.Money(900) || date != "2026-11-15" {
		t.Fatalf("re-synced row = (%v, %s), want (9.00, 2026-11-15)", total, date)
	}
	if got := d30Str(t, pool, `SELECT c.name FROM "transaction" t JOIN category c ON c.id = t.category_id WHERE t.id = $1`, catID); got != "Drink" {
		t.Fatalf("re-sync changed the category to %q", got)
	}

	sections, _ = a.syncLiveSections(ctx, s.H1, s.U)
	if row := sections[0].Rows[0]; !row.Synced || row.SourceChanged {
		t.Fatalf("after re-sync: row = %+v, want synced and not source-changed", row)
	}

	// The re-sync wrote an 'edited' history event with the diff.
	var found bool
	for _, line := range d33HistoryRows(t, pool, s.H1, catID) {
		if strings.HasPrefix(line, "edited |") && strings.Contains(line, "amount") && strings.Contains(line, "date") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no re-sync history diff among %v", d33HistoryRows(t, pool, s.H1, catID))
	}
}

// TestD33DashboardActualsIncludesSyncedShare: the dashboard's
// category-actuals query (unchanged) picks up the synced row — U and P see
// the 6.00 share in Food & Drink, M (Related mode, not in the slice) sees
// nothing.
func TestD33DashboardActualsIncludesSyncedShare(t *testing.T) {
	a, pool := d33App(t)
	s := d33BuildSeed(t, pool)
	ctx := context.Background()
	sess := &auth.Session{ID: "u", Household: s.H1, UserID: s.U}
	d33SyncPOST(t, a, ctx, sess, s.H2, s.T1, s.CatCoffee, 0)

	monthStart, _ := parseDate("2026-11-01")
	monthEnd, _ := parseDate("2026-12-01")

	actualFor := func(user int64) money.Money {
		t.Helper()
		p, err := a.permLoad(ctx, s.H1, user)
		if err != nil {
			t.Fatalf("permLoad: %v", err)
		}
		cats, err := a.dashCategoryRows(ctx, p, s.H1, user, monthStart, monthEnd)
		if err != nil {
			t.Fatalf("dashCategoryRows: %v", err)
		}
		for _, c := range cats {
			if c.Name == "Food & Drink" {
				if c.Budget != money.Money(15000) {
					t.Fatalf("budget roll-up = %v, want 150.00", c.Budget)
				}
				return c.Actual
			}
		}
		t.Fatalf("no Food & Drink row in %+v", cats)
		return 0
	}
	if got := actualFor(s.U); got != money.Money(600) {
		t.Fatalf("U actual = %v, want 6.00", got)
	}
	if got := actualFor(s.P); got != money.Money(600) {
		t.Fatalf("P actual = %v, want 6.00", got)
	}
	if got := actualFor(s.M); got != 0 {
		t.Fatalf("M actual = %v, want 0 (Related mode, not in the slice)", got)
	}
}

// TestD33EditLock: on a synced row, changes to amount/date/class/endpoints
// are 400 "Locked to source…"; category stays freely editable.
func TestD33EditLock(t *testing.T) {
	a, pool := d33App(t)
	s := d33BuildSeed(t, pool)
	ctx := context.Background()
	sess := &auth.Session{ID: "u", Household: s.H1, UserID: s.U}
	d33SyncPOST(t, a, ctx, sess, s.H2, s.T1, s.CatCoffee, 0)

	var id int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM "transaction"
		WHERE household_id = $1 AND sync_source_household_id = $2 AND sync_source_txn_id = $3`,
		s.H1, s.H2, s.T1).Scan(&id); err != nil {
		t.Fatalf("synced row: %v", err)
	}

	post := func(mutate func(url.Values)) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleTxnEditPost(rec, d30Req(t, ctx, sess, "/txns/"+strconv.FormatInt(id, 10), id, d33TxnForm(t, pool, s.H1, id, mutate)))
		return rec
	}

	lock := func(name string, mutate func(url.Values)) {
		t.Helper()
		rec := post(mutate)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", name, rec.Code)
			return
		}
		if !strings.Contains(rec.Body.String(), "Locked to source") {
			t.Fatalf("%s: body %q, want the locked-to-source message", name, rec.Body.String())
		}
	}
	lock("amount", func(f url.Values) { f.Set("total", "7.00") })
	lock("date", func(f url.Values) { f.Set("date", "2026-11-20") })
	lock("class", func(f url.Values) { f.Set("class", "income") })
	originParty := d30Str(t, pool, `
		SELECT 'party:' || id FROM party
		WHERE household_id = $1 AND name = 'u_u' AND app_user_id IS NOT NULL`, s.H1)
	lock("origin", func(f url.Values) { f.Set("origin_ref", originParty) })
	lock("destination", func(f url.Values) { f.Set("destination_ref", "account:1") })

	// Category re-classification is the freely-editable surface.
	rec := post(func(f url.Values) { f.Set("category_id", strconv.FormatInt(s.CatDrinkH1, 10)) })
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("category edit: status %d body %q, want 303", rec.Code, rec.Body.String())
	}
	if got := d30Int(t, pool, `SELECT category_id FROM "transaction" WHERE id = $1`, id); got != s.CatDrinkH1 {
		t.Fatalf("category = %d, want %d", got, s.CatDrinkH1)
	}
}

// TestD33DetailVisibility: a synced row opens on its detail page for the
// caller and the owner (party-only surface — no account grant to hold), and
// 403s for a member who is not in its related slice.
func TestD33DetailVisibility(t *testing.T) {
	a, pool := d33App(t)
	s := d33BuildSeed(t, pool)
	ctx := context.Background()
	sessU := &auth.Session{ID: "u", Household: s.H1, UserID: s.U}
	sessM := &auth.Session{ID: "m", Household: s.H1, UserID: s.M}
	sessP := &auth.Session{ID: "p", Household: s.H1, UserID: s.P}
	d33SyncPOST(t, a, ctx, sessU, s.H2, s.T1, s.CatCoffee, 0)

	var id int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM "transaction"
		WHERE household_id = $1 AND sync_source_household_id = $2 AND sync_source_txn_id = $3`,
		s.H1, s.H2, s.T1).Scan(&id); err != nil {
		t.Fatalf("synced row: %v", err)
	}

	get := func(sess *auth.Session) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", "/txns/"+strconv.FormatInt(id, 10), nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", strconv.FormatInt(id, 10))
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		r = r.WithContext(contextWithSession(r.Context(), sess))
		rec := httptest.NewRecorder()
		a.handleTxnDetail(rec, r)
		return rec
	}

	if rec := get(sessU); rec.Code != http.StatusOK {
		t.Fatalf("U detail: status %d, want 200", rec.Code)
	} else if !strings.Contains(rec.Body.String(), "synced ✓") {
		t.Fatal("U detail: missing the synced badge")
	}
	if rec := get(sessP); rec.Code != http.StatusOK {
		t.Fatalf("P detail: status %d, want 200", rec.Code)
	}
	if rec := get(sessM); rec.Code != http.StatusForbidden {
		t.Fatalf("M detail: status %d, want 403 (not in the related slice)", rec.Code)
	}
}

// TestD33VoidCascade: voiding the source in H2 voids the H1 copy
// immediately, each with a History row (actor = whoever voided the
// source).
func TestD33VoidCascade(t *testing.T) {
	a, pool := d33App(t)
	s := d33BuildSeed(t, pool)
	ctx := context.Background()
	sessU := &auth.Session{ID: "u", Household: s.H1, UserID: s.U}
	sessF := &auth.Session{ID: "f", Household: s.H2, UserID: s.F}
	d33SyncPOST(t, a, ctx, sessU, s.H2, s.T1, s.CatCoffee, 0)

	var copyID int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM "transaction"
		WHERE household_id = $1 AND sync_source_household_id = $2 AND sync_source_txn_id = $3`,
		s.H1, s.H2, s.T1).Scan(&copyID); err != nil {
		t.Fatalf("synced row: %v", err)
	}

	// F voids T1 in H2 (single-void path).
	rec := httptest.NewRecorder()
	a.handleTxnStatus(rec, d30Req(t, ctx, sessF, "/txns/"+strconv.FormatInt(s.T1, 10), s.T1, url.Values{"status": {"void"}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("void: status %d body %q", rec.Code, rec.Body.String())
	}
	if d30Status(t, pool, s.T1) != "void" {
		t.Fatal("source not voided")
	}
	if d30Status(t, pool, copyID) != "void" {
		t.Fatal("the H1 copy was not voided immediately")
	}

	// Both History rows: the source's (in H2) and the copy's (in H1), each
	// with F as actor.
	srcLines := d33HistoryRows(t, pool, s.H2, s.T1)
	copyLines := d33HistoryRows(t, pool, s.H1, copyID)
	if !containsActorVoid(srcLines, s.F) {
		t.Fatalf("source history = %v, want a voided row by F", srcLines)
	}
	if !containsActorVoid(copyLines, s.F) {
		t.Fatalf("copy history = %v, want a voided row by F", copyLines)
	}

	// The copy no longer counts in the budgets.
	p, err := a.permLoad(ctx, s.H1, s.U)
	if err != nil {
		t.Fatalf("permLoad: %v", err)
	}
	monthStart, _ := parseDate("2026-11-01")
	monthEnd, _ := parseDate("2026-12-01")
	cats, err := a.dashCategoryRows(ctx, p, s.H1, s.U, monthStart, monthEnd)
	if err != nil {
		t.Fatalf("dashCategoryRows: %v", err)
	}
	for _, c := range cats {
		if c.Name == "Food & Drink" && c.Actual != 0 {
			t.Fatalf("Food & Drink actual after void = %v, want 0", c.Actual)
		}
	}
}

// TestD33UnsyncFreesPointer: un-syncing is the ordinary delete path — a
// deletion_log row as usual, and the source pointers free up (re-syncing
// the same source record creates a fresh row, not a resurrection).
func TestD33UnsyncFreesPointer(t *testing.T) {
	a, pool := d33App(t)
	s := d33BuildSeed(t, pool)
	ctx := context.Background()
	sessU := &auth.Session{ID: "u", Household: s.H1, UserID: s.U}
	d33SyncPOST(t, a, ctx, sessU, s.H2, s.T1, s.CatCoffee, 0)

	var id int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM "transaction"
		WHERE household_id = $1 AND sync_source_household_id = $2 AND sync_source_txn_id = $3`,
		s.H1, s.H2, s.T1).Scan(&id); err != nil {
		t.Fatalf("synced row: %v", err)
	}

	rec := httptest.NewRecorder()
	a.handleBulkDelete(rec, d30Req(t, ctx, sessU, "/txns/bulk-delete", 0,
		url.Values{"ids[]": {strconv.FormatInt(id, 10)}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("bulk delete: status %d body %q", rec.Code, rec.Body.String())
	}
	t.Logf("delete location: %s", rec.Header().Get("Location"))
	outcome := d30Str(t, pool,
		`SELECT outcome FROM deletion_log WHERE household_id = $1 AND txn_id = $2`, s.H1, id)
	if outcome != "deleted" {
		t.Fatalf("deletion_log outcome = %q, want deleted", outcome)
	}
	if d30Int(t, pool, `SELECT count(*) FROM "transaction" WHERE id = $1`, id) != 0 {
		t.Fatal("row still present after un-sync")
	}

	// The pointer is free: re-syncing creates a fresh row.
	d33SyncPOST(t, a, ctx, sessU, s.H2, s.T1, s.CatCoffee, 0)
	var newID int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM "transaction"
		WHERE household_id = $1 AND sync_source_household_id = $2 AND sync_source_txn_id = $3`,
		s.H1, s.H2, s.T1).Scan(&newID); err != nil {
		t.Fatalf("re-sync row: %v", err)
	}
	if newID == id {
		t.Fatal("re-sync reused the deleted row id")
	}
	if n := d30Int(t, pool, `
		SELECT count(*) FROM "transaction"
		WHERE household_id = $1 AND sync_source_household_id = $2 AND sync_source_txn_id = $3`,
		s.H1, s.H2, s.T1); n != 1 {
		t.Fatalf("rows after un-sync + re-sync = %d, want 1", n)
	}
}

// TestD33ASideBadge: the source-side "synced by [member]" annotation
// (D33 follow-up, signed 2026-11-17). A-side list + detail only; visible to
// every member of A who can see the record; multiple destinations →
// multiple chips; un-sync removes it (render-time, nothing stored on the A
// side); the target household's name never appears in A's rendering.
func TestD33ASideBadge(t *testing.T) {
	a, pool := d33App(t)
	s := d33BuildSeed(t, pool)
	ctx := context.Background()
	sessU := &auth.Session{ID: "u", Household: s.H1, UserID: s.U}
	sessF := &auth.Session{ID: "f", Household: s.H2, UserID: s.F}

	// A third household, so the same source record can be synced into two
	// of the members' other households by two different members.
	var h3 int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO household (name, base_currency, language, first_day_of_week)
		 VALUES ('Third household','MYR','en',1) RETURNING id`).Scan(&h3); err != nil {
		t.Fatalf("h3: %v", err)
	}
	member3 := func(u int64, role string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO household_member (user_id, household_id, role) VALUES ($1,$2,$3)`,
			u, h3, role); err != nil {
			t.Fatalf("membership %d→h3: %v", u, err)
		}
	}
	member3(s.F, "owner")
	member3(s.U, "member")
	var cat3 int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO category (household_id, name, parent_id, transaction_class, is_budgetable)
		 VALUES ($1,'Coffee',NULL,'expense',true) RETURNING id`, h3).Scan(&cat3); err != nil {
		t.Fatalf("h3 category: %v", err)
	}

	d33SyncPOST(t, a, ctx, sessU, s.H2, s.T1, s.CatCoffee, 0) // T1 → H1 by U
	// The sync target is the session's current household: F acts in H3,
	// source H2.
	sessF3 := &auth.Session{ID: "f3", Household: h3, UserID: s.F}
	d33SyncPOST(t, a, ctx, sessF3, s.H2, s.T1, cat3, 0)        // T1 → H3 by F

	get := func(sess *auth.Session) string {
		t.Helper()
		r := httptest.NewRequest("GET", "/txns", nil)
		r = r.WithContext(contextWithSession(r.Context(), sess))
		rec := httptest.NewRecorder()
		a.listTxns(rec, r)
		if rec.Code != http.StatusOK {
			body := rec.Body.String()
			t.Fatalf("GET /txns: status %d, body %q", rec.Code, body[:min(len(body), 200)])
		}
		return rec.Body.String()
	}

	// A-side list (H2, viewed by F): both chips; the household name of the
	// H1 sync target must NOT appear (F holds no H1 membership, so
	// 'Main household' appears nowhere else on the page). 'Third
	// household' does appear — in F's own nav switcher — so it is not
	// asserted.
	bodyF := get(sessF)
	if !strings.Contains(bodyF, "synced by u_u") || !strings.Contains(bodyF, "synced by u_f") {
		t.Fatalf("F H2 /txns chips missing:\n%s", bodyF)
	}
	if strings.Contains(bodyF, "Main household") {
		t.Fatalf("F H2 /txns leaked the target household name:\n%s", bodyF)
	}

	// A-side list (H2, viewed by U — the other member of A). U belongs to
	// both sync targets, so both household names appear in U's own nav
	// switcher anyway; only the chips are asserted. The badge belongs on
	// the source row — the H1 copy row carries the "synced ✓" tag instead
	// (asserted in TestD33DetailVisibility / the smoke), never the chip.
	sessU2 := &auth.Session{ID: "u2", Household: s.H2, UserID: s.U}
	if body := get(sessU2);
		!strings.Contains(body, "synced by u_u") || !strings.Contains(body, "synced by u_f") {
		t.Fatalf("U H2 /txns missing the chips:\n%s", body)
	}

	// Detail pages: the source row (H2) carries the chips; the copy (H1)
	// keeps its "synced ✓" tag and shows no A-side chip.
	var copyID int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM "transaction"
		WHERE household_id = $1 AND sync_source_household_id = $2 AND sync_source_txn_id = $3`,
		s.H1, s.H2, s.T1).Scan(&copyID); err != nil {
		t.Fatalf("copy row: %v", err)
	}
	getDetail := func(sess *auth.Session, id int64) string {
		t.Helper()
		r := httptest.NewRequest("GET", "/txns/"+strconv.FormatInt(id, 10), nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", strconv.FormatInt(id, 10))
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		r = r.WithContext(contextWithSession(r.Context(), sess))
		rec := httptest.NewRecorder()
		a.handleTxnDetail(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("detail: status %d", rec.Code)
		}
		return rec.Body.String()
	}
	if body := getDetail(sessF, s.T1);
		!strings.Contains(body, "synced by u_u") || !strings.Contains(body, "synced by u_f") {
		t.Fatalf("F T1 detail missing the chips:\n%s", body)
	}
	if body := getDetail(sessU, copyID);
		!strings.Contains(body, "synced ✓") || strings.Contains(body, "synced by") {
		t.Fatalf("U copy detail: want the synced tag only, got:\n%s", body)
	}

	// Un-sync (the existing bulk-delete path) removes the chip — the H1
	// copy goes, the H3 copy stays, so exactly one chip remains.
	rec := httptest.NewRecorder()
	a.handleBulkDelete(rec, d30Req(t, ctx, sessU, "/txns/bulk-delete", 0,
		url.Values{"ids[]": {strconv.FormatInt(copyID, 10)}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("bulk delete: status %d body %q", rec.Code, rec.Body.String())
	}
	bodyF2 := get(sessF)
	if strings.Contains(bodyF2, "synced by u_u") {
		t.Fatalf("chip survived the un-sync:\n%s", bodyF2)
	}
	if !strings.Contains(bodyF2, "synced by u_f") {
		t.Fatalf("the H3 chip must remain:\n%s", bodyF2)
	}
}

// TestD33IsolationProbe is the extended D30-style probe: (static) the
// source pointers are the only cross-household bridge and only sync.go
// touches them; (behavioral) no query crosses households outside the four
// named surfaces — the source household's side shows the A-side badge
// only, and non-syncable involvement never appears.
func TestD33IsolationProbe(t *testing.T) {
	a, pool := d33App(t)
	s := d33BuildSeed(t, pool)
	ctx := context.Background()
	sessU := &auth.Session{ID: "u", Household: s.H1, UserID: s.U}
	sessM := &auth.Session{ID: "m", Household: s.H1, UserID: s.M}
	sessF := &auth.Session{ID: "f", Household: s.H2, UserID: s.F}

	// Static seam: the pointer columns appear only in sync.go (tests
	// excluded).
	files, _ := filepath.Glob("*.go")
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
	if len(touching) != 1 || touching[0] != "sync.go" {
		t.Fatalf("cross-household seam moved: files touching the sync_source_ pointers = %v, want [sync.go]", touching)
	}

	d33SyncPOST(t, a, ctx, sessU, s.H2, s.T1, s.CatCoffee, 0)

	page := func(sess *auth.Session, path string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h := map[string]http.HandlerFunc{
			"/txns":       a.listTxns,
			"/sync":       a.handleSyncGET,
			"/history":    a.handleHistory,
			"/deletion-log": a.handleDeletionLog,
		}[path]
		h(rec, d33GET(t, ctx, sess, path))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, rec.Code)
		}
		return rec.Body.String()
	}

	// M (H1, Related mode, not in the slice): the synced row is invisible
	// on /txns, in the history trail, and in the deletion log.
	for _, path := range []string{"/txns", "/history", "/deletion-log"} {
		if body := page(sessM, path); strings.Contains(body, "coffee beverage") {
			t.Fatalf("M sees the synced row on %s", path)
		}
	}
	// M has no other household: /sync renders the empty state.
	if body := page(sessM, "/sync"); !strings.Contains(body, "No other households") {
		t.Fatalf("M /sync empty state missing: %q", body[:min(len(body), 200)])
	}
	// M's nav does not carry the sync item (rendered via .SyncNav).
	if body := page(sessM, "/txns"); strings.Contains(body, "From other households") {
		t.Fatal("M (one household) sees the From-other-households nav item")
	}

	// F's source-side /txns shows the A-side badge (the fourth named
	// surface) but no B-side tag — and the other household's name never
	// leaks into H2's rendering.
	if body := page(sessF, "/txns");
		strings.Contains(body, "synced ✓") || strings.Contains(body, "Main household") {
		t.Fatalf("F's H2 /txns leaked cross-household state:\n%s", body)
	}
	if body := page(sessF, "/txns"); !strings.Contains(body, "synced by u_u") {
		t.Fatalf("F's H2 /txns missing the A-side badge:\n%s", body)
	}

	// U's Live view lists T1 only — T2 (recorded, no split) and T3
	// (leg-funded) do not appear.
	if body := page(sessU, "/sync");
		!strings.Contains(body, "coffee beverage") ||
		strings.Contains(body, "lunch set") ||
		strings.Contains(body, "streaming subscription") ||
		!strings.Contains(body, "Family household") {
		t.Fatalf("U /sync wrong rows: %q", body)
	}
	// U's nav carries the item.
	if body := page(sessU, "/txns"); !strings.Contains(body, "From other households") {
		t.Fatal("U's nav is missing the From-other-households item")
	}
}

// --- small helpers ---------------------------------------------------------

func userDisplay(t *testing.T, pool *pgxpool.Pool, u int64) string {
	t.Helper()
	var n string
	if err := pool.QueryRow(context.Background(),
		`SELECT display_name FROM app_user WHERE id = $1`, u).Scan(&n); err != nil {
		t.Fatalf("display name: %v", err)
	}
	return n
}

func containsActorVoid(lines []string, actor int64) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, "voided |") && strings.HasSuffix(l, "| "+strconv.FormatInt(actor, 10)) {
			return true
		}
	}
	return false
}

func parseDate(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, time.Local)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
