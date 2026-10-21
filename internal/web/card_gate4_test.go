// DB-backed tests for Gate 4 (000011): the deferred per-split allocation cap
// and the base-currency lock. Skipped unless EVENTALLY_TEST_DSN is set (see
// recurring_gate2_test.go for the harness docs).
package web

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errCode extracts the Postgres SQLSTATE from an error.
func errCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// capFixture seeds a household + user + member party + card + a single
// 100.00 expense split (the cap unit) and returns the ids the tests need.
func capFixture(t *testing.T, pool *pgxpool.Pool) (hh, userID, partyID, accountID, splitID int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO household (name, base_currency, language, first_day_of_week)
		 VALUES ('Gate4 Test','MYR','en',1) RETURNING id`).Scan(&hh); err != nil {
		t.Fatalf("household: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
		 VALUES ($1,'gate4u','Gate4 U','gate4@test.local','x') RETURNING id`, hh).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO household_member (user_id, household_id, role) VALUES ($1,$2,'member')`,
		userID, hh); err != nil {
		t.Fatalf("membership: %v", err)
	}
	// The membership trigger seeds the member's party row.
	if err := pool.QueryRow(ctx,
		`SELECT id FROM party WHERE household_id = $1 AND app_user_id = $2`,
		hh, userID).Scan(&partyID); err != nil {
		t.Fatalf("member party missing (seed trigger?): %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO account (household_id, name, account_type, balance_type)
		 VALUES ($1,'Gate4 Card','credit_card','liability') RETURNING id`,
		hh).Scan(&accountID); err != nil {
		t.Fatalf("account: %v", err)
	}
	var txnID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "transaction"
			(household_id, transaction_date, class, total_amount,
			 origin_account_id, record_status)
		 VALUES ($1, '2026-10-01', 'expense', 100.00, $2, 'pending')
		 RETURNING id`, hh, accountID).Scan(&txnID); err != nil {
		t.Fatalf("txn: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO transaction_split (household_id, transaction_id, party_id, amount)
		 VALUES ($1, $2, $3, 100.00) RETURNING id`,
		hh, txnID, partyID).Scan(&splitID); err != nil {
		t.Fatalf("split: %v", err)
	}
	return
}

// settle inserts one settlement + its allocations inside the given txn.
func settle(t *testing.T, tx pgx.Tx, hh, userID, splitID int64, amount, alloc float64) int64 {
	t.Helper()
	var payID int64
	err := tx.QueryRow(context.Background(),
		`INSERT INTO settlement (household_id, account_id, paid_by_user_id, amount, payment_date, method)
		 VALUES ($1, NULL, $2, $3, '2026-10-02', 'transfer') RETURNING id`,
		hh, userID, amount).Scan(&payID)
	if err != nil {
		t.Fatalf("settlement insert: %v", err)
	}
	if alloc > 0 {
		if _, err := tx.Exec(context.Background(),
			`INSERT INTO settlement_allocation (household_id, payment_id, split_id, amount)
			 VALUES ($1, $2, $3, $4)`,
			hh, payID, splitID, alloc); err != nil {
			t.Fatalf("allocation insert: %v", err)
		}
	}
	return payID
}

// TestGate4AllocationCapCommit: allocations summing to (or under) the share
// commit cleanly.
func TestGate4AllocationCapCommit(t *testing.T) {
	pool := testPool(t, testDSN(t))
	hh, userID, _, _, splitID := capFixture(t, pool)

	// Sum == share (100 + 0? use two settlements of 40 and 60).
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	settle(t, tx, hh, userID, splitID, 40, 40)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("settlement at 40/100 should commit: %v", err)
	}
	tx, err = pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	settle(t, tx, hh, userID, splitID, 60, 60) // 40+60 = 100 = share
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("allocations summing to exactly the share must commit: %v", err)
	}
}

// TestGate4AllocationCapExceeds: allocations over the share in one
// transaction fail at commit with check_violation and leave no rows.
func TestGate4AllocationCapExceeds(t *testing.T) {
	pool := testPool(t, testDSN(t))
	hh, userID, _, _, splitID := capFixture(t, pool)

	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	settle(t, tx, hh, userID, splitID, 120, 120) // 120 > 100 share
	err = tx.Commit(context.Background())
	if err == nil {
		t.Fatal("allocations over the share must fail at commit")
	}
	if code := errCode(err); code != "23514" {
		t.Fatalf("want check_violation (23514), got %s: %v", code, err)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM settlement_allocation WHERE split_id = $1`, splitID).
		Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("rolled-back allocations must be gone, got %d rows", n)
	}
}

// TestGate4AllocationCapConcurrency: two SEPARATE transactions, each within
// the cap individually, but over together → the second commit fails.
// This is the concurrency guarantee the deferred trigger provides.
func TestGate4AllocationCapConcurrency(t *testing.T) {
	pool := testPool(t, testDSN(t))
	hh, userID, _, _, splitID := capFixture(t, pool)

	// First: 60 of the 100 share.
	tx1, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin 1: %v", err)
	}
	settle(t, tx1, hh, userID, splitID, 60, 60)
	if err := tx1.Commit(context.Background()); err != nil {
		t.Fatalf("first settlement (60/100) should commit: %v", err)
	}
	// Second: another 60 — 60 alone is under the cap, but 60+60 = 120 over.
	tx2, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	settle(t, tx2, hh, userID, splitID, 60, 60)
	err = tx2.Commit(context.Background())
	if err == nil {
		t.Fatal("second settlement taking the share over must fail at commit")
	}
	if code := errCode(err); code != "23514" {
		t.Fatalf("want check_violation (23514), got %s: %v", code, err)
	}
	var total float64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount),0) FROM settlement_allocation WHERE split_id = $1`,
		splitID).Scan(&total); err != nil {
		t.Fatalf("sum: %v", err)
	}
	if total != 60 {
		t.Fatalf("cap must leave only the first 60.00 allocated, got %v", total)
	}
}

// TestGate4UnappliedCredit: a settlement larger than its allocations commits
// (option (a)) — the excess is the unapplied credit shown on the card page.
func TestGate4UnappliedCredit(t *testing.T) {
	pool := testPool(t, testDSN(t))
	hh, userID, _, _, splitID := capFixture(t, pool)

	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	payID := settle(t, tx, hh, userID, splitID, 120, 100) // pay 120, allocate 100
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("payment over-allocating nothing (120 paid / 100 allocated) must commit: %v", err)
	}
	ctx := context.Background()
	var paid, alloc float64
	if err := pool.QueryRow(ctx, `SELECT amount FROM settlement WHERE id = $1`, payID).
		Scan(&paid); err != nil {
		t.Fatalf("settlement amount: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount),0) FROM settlement_allocation WHERE payment_id = $1`,
		payID).Scan(&alloc); err != nil {
		t.Fatalf("allocations: %v", err)
	}
	if paid != 120 || alloc != 100 {
		t.Fatalf("want paid=120 alloc=100, got paid=%v alloc=%v", paid, alloc)
	}
}

// TestGate4CurrencyLock: base_currency changes are allowed before the first
// transaction and rejected after.
func TestGate4CurrencyLock(t *testing.T) {
	pool := testPool(t, testDSN(t))
	ctx := context.Background()
	var hh, userID, accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO household (name, base_currency, language, first_day_of_week)
		 VALUES ('Gate4 Cur','MYR','en',1) RETURNING id`).Scan(&hh); err != nil {
		t.Fatalf("household: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
		 VALUES ($1,'gate4c','Gate4 C','gate4c@test.local','x') RETURNING id`,
		hh).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO household_member (user_id, household_id, role) VALUES ($1,$2,'member')`,
		userID, hh); err != nil {
		t.Fatalf("membership: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO account (household_id, name, account_type, balance_type)
		 VALUES ($1,'Gate4 Savings','savings','asset') RETURNING id`, hh).Scan(&accountID); err != nil {
		t.Fatalf("account: %v", err)
	}

	// No transactions yet: the change is legal.
	if _, err := pool.Exec(ctx,
		`UPDATE household SET base_currency = 'USD' WHERE id = $1`, hh); err != nil {
		t.Fatalf("currency change with zero transactions must succeed: %v", err)
	}
	var cur string
	if err := pool.QueryRow(ctx, `SELECT base_currency FROM household WHERE id = $1`, hh).
		Scan(&cur); err != nil {
		t.Fatalf("read currency: %v", err)
	}
	if cur != "USD" {
		t.Fatalf("currency should be USD, got %s", cur)
	}

	// First transaction lands: currency is now frozen.
	if _, err := pool.Exec(ctx,
		`INSERT INTO "transaction"
			(household_id, transaction_date, class, total_amount,
			 origin_account_id, record_status)
		 VALUES ($1, '2026-10-01', 'expense', 10.00, $2, 'pending')`,
		hh, accountID); err != nil {
		t.Log("txn err: " + err.Error())
		t.Fatalf("txn: %v", err)
	}
	_, err := pool.Exec(ctx,
		`UPDATE household SET base_currency = 'EUR' WHERE id = $1`, hh)
	if err == nil {
		t.Fatal("currency change after transactions exist must be rejected")
	}
	if want := fmt.Sprintf("%v", err); !hasMsg(want, "base currency cannot change") {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func hasMsg(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
