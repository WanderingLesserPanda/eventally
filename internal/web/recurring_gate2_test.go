// DB-backed tests for the Gate 2 recurring fix (000007) and the
// split_spec JSONB conversion. Skipped unless EVENTALLY_TEST_DSN points at a
// reachable Postgres (e.g. postgres://hermes@127.0.0.1:5432/postgres). Each
// test creates its own throwaway database, migrates it with goose, and never
// touches the DSN's own tables.
package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func testDSN(t *testing.T) string {
	dsn := os.Getenv("EVENTALLY_TEST_DSN")
	if dsn == "" {
		t.Skip("set EVENTALLY_TEST_DSN to run DB-backed tests")
	}
	if !strings.Contains(dsn, "sslmode") {
		dsn += "?sslmode=disable"
	}
	return dsn
}

// testPool migrates a throwaway database and returns a pool on it.
func testPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	dbname := "eventally_test_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+dbname); err != nil {
		_ = admin.Close()
		t.Fatalf("create test db: %v", err)
	}
	// admin must outlive the test (the drop runs in Cleanup); note the
	// LIFO order — the pool.Close registered below runs before this drop.
	t.Cleanup(func() {
		ctx := context.Background()
		// Kill lingering connections, then drop (a couple of retries).
		for attempt := 0; attempt < 5; attempt++ {
			_, _ = admin.ExecContext(ctx,
				`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
				 WHERE datname = $1 AND pid <> pg_backend_pid()`, dbname)
			if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbname); err == nil {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		_ = admin.Close()
	})
	cc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cc.ConnConfig.Config.Database = dbname
	pool, err := pgxpool.NewWithConfig(context.Background(), cc)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	// Migrate the throwaway DB with the embedded migrations.
	goose.SetBaseFS(db.MigrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	sqlDB := stdlib.OpenDB(*cc.ConnConfig)
	defer sqlDB.Close()
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// seedHousehold creates the minimum rows generation needs and returns the
// household id and a recurring template split between two users.
func seedHousehold(t *testing.T, pool *pgxpool.Pool) (hh, templateID int64) {
	t.Helper()
	ctx := context.Background()
	var user1, user2, accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO household (name, base_currency, language, first_day_of_week)
		 VALUES ('Gate2 Test','MYR','en',1) RETURNING id`).Scan(&hh); err != nil {
		t.Fatalf("household: %v", err)
	}
	for i, nm := range []string{"gate2u1", "gate2u2"} {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
			 VALUES ($1, $2, $2, $2||'@test.local', 'x') RETURNING id`,
			hh, nm).Scan(&id); err != nil {
			t.Fatalf("user %s: %v", nm, err)
		}
		if i == 0 {
			user1 = id
		} else {
			user2 = id
		}
	}
	for _, u := range []int64{user1, user2} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO household_member (user_id, household_id, role) VALUES ($1,$2,'member')`,
			u, hh); err != nil {
			t.Fatalf("membership: %v", err)
		}
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO account (household_id, name, account_type, balance_type)
		 VALUES ($1,'Gate2 Card','credit_card','liability') RETURNING id`, hh).Scan(&accountID); err != nil {
		t.Fatalf("account: %v", err)
	}
	spec, err := splitSpecToJSONB([]string{
		"user:" + strconv.FormatInt(user1, 10),
		"user:" + strconv.FormatInt(user2, 10),
	})
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO recurring_template
			(household_id, name, account_id, amount, day_of_month, split_spec, is_active)
		 VALUES ($1,'Gate2 Rent',$2,1500.00,1,$3,true) RETURNING id`,
		hh, accountID, spec).Scan(&templateID); err != nil {
		t.Fatalf("template: %v", err)
	}
	return
}

func occCount(t *testing.T, pool *pgxpool.Pool, templateID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM "transaction" WHERE recurring_template_id=$1`, templateID).Scan(&n); err != nil {
		t.Fatalf("count occurrences: %v", err)
	}
	return n
}

// TestGate2GenerationIsIdempotent drives the real generateDueRecurring path:
// split_spec lands as JSONB, generation creates exactly one occurrence, and
// a second generation pass (with the pointer cleared so the SELECT passes
// again) is refused by the void-aware unique index.
func TestGate2GenerationIsIdempotent(t *testing.T) {
	pool := testPool(t, testDSN(t))
	hh, templateID := seedHousehold(t, pool)
	app := &App{Pool: pool}
	ctx := context.Background()
	now := time.Now()
	app.generateDueRecurring(ctx, hh, now)
	if n := occCount(t, pool, templateID); n != 1 {
		t.Fatalf("after first generation: got %d occurrences, want 1", n)
	}
	var specJSON []byte
	if err := pool.QueryRow(ctx,
		`SELECT split_spec FROM recurring_template WHERE id=$1`, templateID).Scan(&specJSON); err != nil {
		t.Fatalf("read spec: %v", err)
	}
	if keys := splitSpecKeys(specJSON); len(keys) != 2 {
		t.Fatalf("split_spec not a 2-key JSONB array: %s", specJSON)
	}
	// Force the race condition the unique index guards: clear the pointer so
	// the dues SELECT passes again; the INSERT must then be the loser.
	if _, err := pool.Exec(ctx,
		`UPDATE recurring_template SET last_generated_date=NULL WHERE id=$1`, templateID); err != nil {
		t.Fatalf("clear last_generated: %v", err)
	}
	app.generateDueRecurring(ctx, hh, now)
	if n := occCount(t, pool, templateID); n != 1 {
		t.Fatalf("after forced second generation: got %d occurrences, want 1", n)
	}
	// The occurrence is attributed to its template (back-reference).
	var backRef int64
	if err := pool.QueryRow(ctx,
		`SELECT recurring_template_id FROM "transaction" WHERE recurring_template_id IS NOT NULL`).
		Scan(&backRef); err != nil {
		t.Fatalf("back-ref: %v", err)
	}
	if backRef != templateID {
		t.Fatalf("back-ref %d, want template %d", backRef, templateID)
	}
}

// TestGate2VoidedOccurrenceRegenerates: a voided row does not block the next
// month's generation (void-aware unique index).
func TestGate2VoidedOccurrenceRegenerates(t *testing.T) {
	pool := testPool(t, testDSN(t))
	hh, templateID := seedHousehold(t, pool)
	app := &App{Pool: pool}
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	app.generateDueRecurring(ctx, hh, now)
	if n := occCount(t, pool, templateID); n != 1 {
		t.Fatalf("setup: got %d occurrences, want 1", n)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE "transaction" SET record_status='void' WHERE recurring_template_id=$1`, templateID); err != nil {
		t.Fatalf("void: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE recurring_template SET last_generated_date=$1 WHERE id=$2`,
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), templateID); err != nil {
		t.Fatalf("advance pointer: %v", err)
	}
	app.generateDueRecurring(ctx, hh, now.AddDate(0, 1, 0))
	var live, total int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE record_status<>'void'), count(*)
		FROM "transaction" WHERE recurring_template_id=$1`, templateID).Scan(&live, &total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 1 || total != 2 {
		t.Fatalf("after void+regen: live=%d total=%d, want live=1 total=2", live, total)
	}
}

// TestGate2ConcurrentGenerationExactlyOne: N workers call the real
// generateRecurringEntry simultaneously; the row lock + void-aware unique
// index must yield exactly one occurrence.
func TestGate2ConcurrentGenerationExactlyOne(t *testing.T) {
	pool := testPool(t, testDSN(t))
	hh, templateID := seedHousehold(t, pool)
	app := &App{Pool: pool}
	ctx := context.Background()
	var acctID int64
	if err := pool.QueryRow(ctx,
		`SELECT account_id FROM recurring_template WHERE id=$1`, templateID).Scan(&acctID); err != nil {
		t.Fatalf("account id: %v", err)
	}
	var specJSON []byte
	if err := pool.QueryRow(ctx,
		`SELECT split_spec FROM recurring_template WHERE id=$1`, templateID).Scan(&specJSON); err != nil {
		t.Fatalf("spec: %v", err)
	}
	d := dueTemplate{id: templateID, account: acctID, amount: 1500, day: 1, specJSON: specJSON}
	const workers = 5
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := app.generateRecurringEntry(ctx, hh, d, "2026-10-01")
			if err != nil && !errors.Is(err, errSiblingGenerated) {
				errs <- fmt.Errorf("worker: %w", err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent generation failed: %v", err)
	}
	if n := occCount(t, pool, templateID); n != 1 {
		t.Fatalf("after %d concurrent workers: got %d occurrences, want 1", workers, n)
	}
}

// TestGate2SettlementLedgerUnique: two settlements on one ledger transaction
// are rejected by uq_payment_ledger_txn.
func TestGate2SettlementLedgerUnique(t *testing.T) {
	pool := testPool(t, testDSN(t))
	ctx := context.Background()
	var hh, user1, accountID, txnID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO household (name, base_currency, language, first_day_of_week)
		 VALUES ('Gate2 Uniq','MYR','en',1) RETURNING id`).Scan(&hh); err != nil {
		t.Fatalf("household: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
		 VALUES ($1,'gate2uniq','Gate2 Uniq','u@test.local','x') RETURNING id`, hh).Scan(&user1); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO account (household_id, name, account_type, balance_type)
		 VALUES ($1,'Gate2 Card2','credit_card','liability') RETURNING id`, hh).Scan(&accountID); err != nil {
		t.Fatalf("account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO "transaction"
			(household_id, transaction_date, class, total_amount,
			 origin_account_id, record_status)
		 VALUES ($1, '2026-10-01', 'expense', 100.00, $2, 'approved')
		 RETURNING id`, hh, accountID).Scan(&txnID); err != nil {
		t.Fatalf("txn: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO settlement (household_id, account_id, paid_by_user_id, amount, payment_date, ledger_transaction_id)
		 VALUES ($1,$2,$3,50.00,'2026-10-02',$4)`, hh, accountID, user1, txnID); err != nil {
		t.Fatalf("first settlement: %v", err)
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO settlement (household_id, account_id, paid_by_user_id, amount, payment_date, ledger_transaction_id)
		 VALUES ($1,$2,$3,60.00,'2026-10-03',$4)`, hh, accountID, user1, txnID)
	if err == nil {
		t.Fatalf("second settlement on same ledger txn: expected 23505, but the INSERT succeeded")
	}
	if !isUniqueViolation(err) {
		t.Fatalf("second settlement on same ledger txn: want 23505, got %v", err)
	}
}

// TestGate2ExternalSourceUnique: duplicate external_source_id is rejected.
func TestGate2ExternalSourceUnique(t *testing.T) {
	pool := testPool(t, testDSN(t))
	ctx := context.Background()
	var hh, accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO household (name, base_currency, language, first_day_of_week)
		 VALUES ('Gate2 Ext','MYR','en',1) RETURNING id`).Scan(&hh); err != nil {
		t.Fatalf("household: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO account (household_id, name, account_type, balance_type)
		 VALUES ($1,'Gate2 Card3','savings','asset') RETURNING id`, hh).Scan(&accountID); err != nil {
		t.Fatalf("account: %v", err)
	}
	insert := func(esc string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO "transaction"
				(household_id, transaction_date, class, total_amount,
				 origin_account_id, record_status, external_source_id)
			 VALUES ($1,'2026-10-01','expense',10.00,$2,'approved',$3)`,
			hh, accountID, esc)
		return err
	}
	if err := insert("imp-42"); err != nil {
		t.Fatalf("first import row: %v", err)
	}
	err := insert("imp-42")
	if err == nil || !isUniqueViolation(err) {
		t.Fatalf("duplicate external_source_id: want 23505, got %v", err)
	}
}
