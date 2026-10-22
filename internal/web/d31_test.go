// DB-backed tests for D31 (000016): multi-payer settlement funding legs,
// the txn_history audit trail, and the deletion-log access snapshots.
// Skipped unless EVENTALLY_TEST_DSN is set (see recurring_gate2_test.go).
package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
	"github.com/WanderingLesserPanda/eventally/internal/db"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// --- shared fixtures --------------------------------------------------------

// d31Card creates a credit-card (liability) account.
func d31Card(t *testing.T, pool *pgxpool.Pool, hh int64, name string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO account (household_id, name, account_type, balance_type)
		 VALUES ($1,$2,'credit_card','liability') RETURNING id`, hh, name).Scan(&id)
	if err != nil {
		t.Fatalf("card %q: %v", name, err)
	}
	return id
}

// d31Charge posts a charge of `amount` on the card split to party and returns
// the split id and the charge txn id (the unpaid line a payment allocates to).
func d31Charge(t *testing.T, pool *pgxpool.Pool, hh, card, party int64, desc, amount string) (splitID, txnID int64) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`INSERT INTO "transaction" (household_id, transaction_date, class, total_amount,
                                    origin_account_id, description)
		 VALUES ($1,'2026-10-01','expense',$2,$3,$4) RETURNING id`,
		hh, amount, card, desc).Scan(&txnID)
	if err != nil {
		t.Fatalf("charge %q: %v", desc, err)
	}
	err = pool.QueryRow(context.Background(),
		`INSERT INTO transaction_split (household_id, transaction_id, party_id, amount)
		 VALUES ($1,$2,$3,$4) RETURNING id`, hh, txnID, party, amount).Scan(&splitID)
	if err != nil {
		t.Fatalf("charge split: %v", err)
	}
	return
}

// d31Int coerces an untyped numeric any (int or int64) to int64.
func d31Int(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

// d31PayForm builds a /cards/{id}/pay POST form. legs are (user, source,
// method, amount, ledger); an empty amount row models a removed leg.
func d31PayForm(t *testing.T, card int64, amount, date string, allocs map[int64]string,
	legs [][5]any) url.Values {
	t.Helper()
	form := url.Values{}
	form.Set("amount", amount)
	form.Set("payment_date", date)
	for sid, v := range allocs {
		form.Add("alloc_split[]", strconv.FormatInt(sid, 10))
		form.Add("alloc[]", v)
	}
	for _, lg := range legs {
		form.Add("leg_user[]", strconv.FormatInt(d31Int(lg[0]), 10))
		form.Add("leg_source[]", strconv.FormatInt(d31Int(lg[1]), 10))
		form.Add("leg_method[]", lg[2].(string))
		form.Add("leg_amount[]", lg[3].(string))
		if lg[4].(bool) {
			form.Add("leg_ledger[]", "on")
		} else {
			form.Add("leg_ledger[]", "")
		}
	}
	return form
}

// --- 1. pay flow: legs, transfers, validation --------------------------------

// TestD31PayFlow covers the funding-legs pay flow end to end: the
// single-leg default (== today's behaviour), a two-leg settlement (Σ
// invariant, one transfer per funded leg, out-of-band leg subtracts from
// the recorded balance), the validation matrix, and the 403 naming the
// untouchable leg source.
func TestD31PayFlow(t *testing.T) {
	a, pool := d30App(t)
	ctx := context.Background()
	hh := d30Household(t, pool, "D31 Pay")
	owner := d30User(t, pool, hh, "d31payowner", "")
	member := d30User(t, pool, hh, "d31paymember", "")
	d30Membership(t, pool, owner, hh, "owner")
	d30Membership(t, pool, member, hh, "member")

	card := d31Card(t, pool, hh, "D31 Card")
	accA := d30Account(t, pool, hh, "D31 Savings A", 0)
	accB := d30Account(t, pool, hh, "D31 Savings B", 0) // owner-only source

	// Member: read on the card (see the pay page), create on the card and
	// on Savings A (fund a leg from it). Savings B stays owner-only.
	d30Grant(t, pool, hh, member, card, "All", "")
	d30Grant(t, pool, hh, member, accA, "All", "")

	memberParty := d30Int(t, pool,
		`SELECT id FROM party WHERE household_id = $1 AND app_user_id = $2`, hh, member)
	split, _ := d31Charge(t, pool, hh, card, memberParty, "D31 Charge", "300.00")

	sess := &auth.Session{ID: "d31pay", Household: hh, UserID: member}
	ownerSess := &auth.Session{ID: "d31payo", Household: hh, UserID: owner}
	pay := func(s *auth.Session, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleCardPayPost(rec, d30Req(t, ctx, s, "/cards/pay", card, form))
		return rec
	}

	// Single leg, ledger ON: the old default flow, one for one.
	rec := pay(sess, d31PayForm(t, card, "40.00", "2026-10-05",
		map[int64]string{split: "40.00"},
		[][5]any{{member, accA, "transfer", "40.00", true}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("single-leg pay: status %d body %q", rec.Code, rec.Body.String())
	}
	payID := d30Int(t, pool, `SELECT id FROM settlement WHERE household_id = $1`, hh)
	if n := d30Int(t, pool,
		`SELECT count(*) FROM settlement_funding WHERE settlement_id = $1`, payID); n != 1 {
		t.Fatalf("single-leg settlement: legs = %d, want 1", n)
	}
	d30Str(t, pool,
		`SELECT user_id::text FROM settlement_funding WHERE settlement_id = $1`, payID)
	// The D6 transfer: source -> card, linked to the settlement.
	txnID := d30Int(t, pool,
		`SELECT ledger_transaction_id FROM settlement WHERE id = $1`, payID)
	if got := d30Str(t, pool,
		`SELECT description FROM "transaction" WHERE id = $1`, txnID); !strings.Contains(got, "leg 1 of 1") {
		t.Fatalf("transfer description = %q, want the leg 1 of 1 form", got)
	}
	if n := d30Int(t, pool,
		`SELECT count(*) FROM "transaction"
		 WHERE household_id = $1 AND class = 'transfer' AND destination_account_id = $2`,
		hh, card); n != 1 {
		t.Fatalf("transfers on card = %d, want 1", n)
	}

	// Two legs: 60 funded (Savings A) + 40 out-of-band by the other member.
	// Σ must equal the payment amount; exactly one transfer appears.
	before, err := a.cardRecordedAtFamily(ctx, hh, []int64{card}, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("recorded before: %v", err)
	}
	rec = pay(sess, d31PayForm(t, card, "100.00", "2026-10-06",
		map[int64]string{split: "100.00"},
		[][5]any{{owner, accA, "transfer", "60.00", true}, {member, 0, "cash", "40.00", false}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("two-leg pay: status %d body %q", rec.Code, rec.Body.String())
	}
	payID = d30Int(t, pool,
		`SELECT id FROM settlement WHERE household_id = $1 ORDER BY id DESC LIMIT 1`, hh)
	if n := d30Int(t, pool,
		`SELECT count(*) FROM settlement_funding WHERE settlement_id = $1`, payID); n != 2 {
		t.Fatalf("two-leg settlement: legs = %d, want 2", n)
	}
	if s := d30Int(t, pool,
		`SELECT coalesce(sum(amount),0) FROM settlement_funding WHERE settlement_id = $1`, payID); s != 100 {
		t.Fatalf("Σ legs = %d, want 100 (the Σ invariant)", s)
	}
	// One funded leg -> exactly one new transfer (the out-of-band leg
	// writes nothing).
	if n := d30Int(t, pool,
		`SELECT count(*) FROM "transaction"
		 WHERE household_id = $1 AND class = 'transfer' AND destination_account_id = $2`,
		hh, card); n != 2 {
		t.Fatalf("transfers on card = %d, want 2 (the out-of-band leg writes nothing)", n)
	}
	if got := d30Str(t, pool,
		`SELECT description FROM "transaction"
		 WHERE household_id = $1 AND class = 'transfer' AND destination_account_id = $2
		 ORDER BY id DESC LIMIT 1`, hh, card); !strings.Contains(got, "leg 1 of 2") {
		t.Fatalf("funded-leg description = %q, want the leg 1 of 2 form", got)
	}
	// The funded 60 lands as a D6 credit on the card (ledger); the 40
	// out-of-band leg is subtracted explicitly. Together the recorded
	// balance moves by the full 100.
	after, err := a.cardRecordedAtFamily(ctx, hh, []int64{card}, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("recorded after: %v", err)
	}
	if diff := before.Sub(after); diff.AsCents() != 10000 {
		t.Fatalf("recorded balance moved by %v, want the full two-leg payment (100)", diff)
	}

	// Validation failures: re-rendered form, no settlement written. Each
	// carries a valid allocation (10.00 of the 300 charge) so the checks
	// reach the funding-leg rules instead of stopping at the allocation gate.
	bad := func(label string, legs [][5]any, wantSub string) {
		t.Helper()
		rec := pay(sess, d31PayForm(t, card, "10.00", "2026-10-07",
			map[int64]string{split: "10.00"}, legs))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want a re-render", label, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), wantSub) {
			t.Fatalf("%s: body lacks %q", label, wantSub)
		}
	}
	bad("sum mismatch", [][5]any{{member, 0, "cash", "10.00", false}, {owner, 0, "cash", "20.00", false}},
		"must add up exactly")
	bad("non-positive leg", [][5]any{{member, 0, "cash", "0", false}}, "positive numbers")
	bad("ledger without source", [][5]any{{member, 0, "transfer", "10.00", true}}, "needs a source account")
	bad("source without ledger", [][5]any{{member, accA, "cash", "10.00", false}}, "check the ledger box")
	bad("non-member payer", [][5]any{{9999, 0, "cash", "10.00", false}}, "choose a payer")
	if n := d30Int(t, pool,
		`SELECT count(*) FROM settlement WHERE household_id = $1`, hh); n != 2 {
		t.Fatalf("failed pays wrote settlements: count = %d, want 2", n)
	}

	// 403 naming the untouchable leg source (Savings B is owner-only).
	// A valid allocation is supplied so the request reaches the permission
	// gate (which runs after the allocation validation).
	rec = pay(sess, d31PayForm(t, card, "10.00", "2026-10-08",
		map[int64]string{split: "10.00"},
		[][5]any{{member, accB, "transfer", "10.00", true}}))
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "fund this settlement from D31 Savings B") {
		t.Fatalf("unowned source: status %d, want the named 403 message; body %q",
			rec.Code, rec.Body.String())
	}
	if n := d30Int(t, pool,
		`SELECT count(*) FROM settlement WHERE household_id = $1`, hh); n != 2 {
		t.Fatalf("403 pay wrote a settlement")
	}

	// GET defaults to one pre-filled leg (the actor, full amount, ledger ON).
	{
		r := httptest.NewRequest("GET", "/cards/"+strconv.FormatInt(card, 10)+"/pay", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", strconv.FormatInt(card, 10))
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		a.handleCardPayGet(rec, r.WithContext(contextWithSession(r.Context(), sess)))
		if rec.Code != http.StatusOK {
			t.Fatalf("pay GET: status %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "leg-table") {
			t.Fatalf("pay GET body lacks the funding leg table")
		}
	}
	_ = ownerSess
}

// --- 2. migration backfill ----------------------------------------------------

// TestD31BackfillEquivalence runs the real 000016 migration over a legacy
// v15 database and checks the backfill: one leg per settlement, payer from
// paid_by_user_id, source from the linked D6 transfer's origin account
// (NULL when no transfer), method/amount carried over.
func TestD31BackfillEquivalence(t *testing.T) {
	dsn := testDSN(t)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	dbname := "eventally_test_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+dbname); err != nil {
		_ = admin.Close()
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
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
	goose.SetBaseFS(db.MigrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	sqlDB := stdlib.OpenDB(*cc.ConnConfig)
	defer sqlDB.Close()
	// Migrate to v15 only, seed the legacy shape, then apply 000016.
	if err := goose.UpTo(sqlDB, "migrations", 15); err != nil {
		t.Fatalf("migrate to 15: %v", err)
	}
	q := func(query string, args ...any) int64 {
		t.Helper()
		var v int64
		if err := sqlDB.QueryRow(query, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return v
	}
	qs := func(query string, args ...any) string {
		t.Helper()
		var v string
		if err := sqlDB.QueryRow(query, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return v
	}
	x := func(query string, args ...any) {
		t.Helper()
		if _, err := sqlDB.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	hh := q(`INSERT INTO household (name) VALUES ('D31 Backfill') RETURNING id`)
	x(`INSERT INTO app_user (default_household_id, username, password_hash, display_name)
	     VALUES ($1,'bfu1','x','bfu1')`, hh)
	x(`INSERT INTO app_user (default_household_id, username, password_hash, display_name)
	     VALUES ($1,'bfu2','x','bfu2')`, hh)
	u1 := q(`SELECT id FROM app_user WHERE username = 'bfu1'`)
	u2 := q(`SELECT id FROM app_user WHERE username = 'bfu2'`)
	card := q(`INSERT INTO account (household_id, name, account_type, balance_type)
	           VALUES ($1,'BF Card','credit_card','liability') RETURNING id`, hh)
	src := q(`INSERT INTO account (household_id, name, account_type, balance_type)
	          VALUES ($1,'BF Savings','savings','asset') RETURNING id`, hh)

	// Legacy settlement WITH a linked D6 transfer (source = its origin).
	transfer := q(`INSERT INTO "transaction" (household_id, transaction_date, class, total_amount,
	                   origin_account_id, destination_account_id)
	                 VALUES ($1,'2026-09-01','transfer',80.00,$2,$3) RETURNING id`, hh, src, card)
	pay1 := q(`INSERT INTO settlement (household_id, account_id, paid_by_user_id, amount,
	                                   payment_date, method, ledger_transaction_id)
	           VALUES ($1,$2,$3,80.00,'2026-09-02','transfer',$4) RETURNING id`, hh, card, u1, transfer)
	// Legacy settlement with no linked transfer (source must backfill NULL).
	pay2 := q(`INSERT INTO settlement (household_id, account_id, paid_by_user_id, amount,
	                                   payment_date, method)
	           VALUES ($1,$2,$3,25.00,'2026-09-03','cash') RETURNING id`, hh, card, u2)

	if err := goose.Up(sqlDB, "migrations"); err != nil {
		t.Fatalf("apply 000016: %v", err)
	}
	if n := q(`SELECT max(version_id) FROM goose_db_version`); n != 16 {
		t.Fatalf("version = %d, want 16", n)
	}
	// One leg per settlement, exactly.
	if n := q(`SELECT count(*) FROM settlement_funding`); n != 2 {
		t.Fatalf("backfilled legs = %d, want 2 (1:1)", n)
	}
	if got := q(`SELECT user_id FROM settlement_funding WHERE settlement_id = $1`, pay1); got != u1 {
		t.Fatalf("pay1 leg payer = %d, want %d", got, u1)
	}
	if got := q(`SELECT coalesce(source_account_id,0) FROM settlement_funding WHERE settlement_id = $1`, pay1); got != src {
		t.Fatalf("pay1 leg source = %d, want the transfer origin %d", got, src)
	}
	if got := qs(`SELECT method FROM settlement_funding WHERE settlement_id = $1`, pay1); got != "transfer" {
		t.Fatalf("pay1 leg method = %q, want transfer", got)
	}
	if got := q(`SELECT coalesce(source_account_id,0) FROM settlement_funding WHERE settlement_id = $1`, pay2); got != 0 {
		t.Fatalf("pay2 leg source = %d, want NULL (no linked transfer)", got)
	}
	if got := q(`SELECT amount::int8 FROM settlement_funding WHERE settlement_id = $1`, pay2); got != 25 {
		t.Fatalf("pay2 leg amount = %d, want 25", got)
	}
	// The dropped columns are gone.
	if n := q(`SELECT count(*) FROM information_schema.columns
	            WHERE table_name = 'settlement' AND column_name IN ('paid_by_user_id','method')`); n != 0 {
		t.Fatalf("settlement still carries dropped columns: %d", n)
	}
}

// --- 3. permission matrix Δ ---------------------------------------------------

// TestD31PermissionMatrix covers the Δ rows of the v1 matrix: a leg-2 payer
// can VIEW and DELETE (Related mode) a settlement they have no card grant on,
// and they are in the allocated txn's Related slice — while a member who
// only holds a source grant sees nothing (sources stay off the read/delete
// surface).
func TestD31PermissionMatrix(t *testing.T) {
	a, pool := d30App(t)
	ctx := context.Background()
	hh := d30Household(t, pool, "D31 Perms")
	owner := d30User(t, pool, hh, "d31powner", "")
	m1 := d30User(t, pool, hh, "d31p1", "")
	m2 := d30User(t, pool, hh, "d31p2", "")
	m3 := d30User(t, pool, hh, "d31p3", "")
	d30Membership(t, pool, owner, hh, "owner")
	for _, u := range []int64{m1, m2, m3} {
		d30Membership(t, pool, u, hh, "member")
	}
	card := d31Card(t, pool, hh, "D31P Card")
	accA := d30Account(t, pool, hh, "D31P A", 0)
	accB := d30Account(t, pool, hh, "D31P B", 0)

	// m2: delete-Related on the card (the D2 delete path). m3: a READ grant
	// on the leg source only (the V2 negative).
	d30Grant(t, pool, hh, m2, card, "Related", "Related")
	d30Grant(t, pool, hh, m3, accB, "Related", "")

	m1Party := d30Int(t, pool,
		`SELECT id FROM party WHERE household_id = $1 AND app_user_id = $2`, hh, m1)
	split, chargeTxn := d31Charge(t, pool, hh, card, m1Party, "D31P Charge", "300.00")

	// Owner records the two-leg settlement: leg 1 funded by m1 (from A,
	// allocated to the charge), leg 2 out-of-band by m2.
	ownerSess := &auth.Session{ID: "d31po", Household: hh, UserID: owner}
	rec := httptest.NewRecorder()
	a.handleCardPayPost(rec, d30Req(t, ctx, ownerSess, "/cards/pay", card,
		d31PayForm(t, card, "100.00", "2026-10-09",
			map[int64]string{split: "60.00"},
			[][5]any{{m1, accA, "transfer", "60.00", true}, {m2, 0, "cash", "40.00", false}})))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("owner two-leg pay: status %d body %q", rec.Code, rec.Body.String())
	}
	payID := d30Int(t, pool,
		`SELECT id FROM settlement WHERE household_id = $1`, hh)

	permsOf := func(u int64) *perms {
		t.Helper()
		r := d30Req(t, ctx, &auth.Session{ID: "perm", Household: hh, UserID: u}, "/txns", 0, nil)
		p, err := a.permFor(r)
		if err != nil {
			t.Fatalf("permFor(%d): %v", u, err)
		}
		return p
	}
	see := func(u int64) bool {
		t.Helper()
		ok, err := a.canSeeSettlement(ctx, permsOf(u), payID)
		if err != nil {
			t.Fatalf("canSeeSettlement(%d): %v", u, err)
		}
		return ok
	}

	// V3: m2 (leg-2 payer) sees the settlement despite NO card read grant —
	// the related slice (any-leg payer) is the Δ.
	if !see(m2) {
		t.Fatalf("V3: the leg-2 payer must see the settlement (related slice)")
	}
	// V2 negative: m3 holds only a read grant on the leg SOURCE — sources
	// stay off the read/delete surface.
	if see(m3) {
		t.Fatalf("V2: a source-only grant must NOT open the settlement")
	}
	// R3: m2 (a leg payer of a settlement allocated to the charge's split)
	// is in the CHARGE txn's Related slice; m3 (source-only) is not.
	if rel, err := a.isRelatedToTxn(ctx, hh, m2, chargeTxn); err != nil || !rel {
		t.Fatalf("R3: leg-2 payer must be related to the charge txn (err %v)", err)
	}
	if rel, err := a.isRelatedToTxn(ctx, hh, m3, chargeTxn); err != nil || rel {
		t.Fatalf("R3: source-only member must NOT be related (err %v)", err)
	}

	// D2: m2 deletes through the handler (delete-Related grant + related
	// slice). The legs cascade away.
	rec = httptest.NewRecorder()
	a.handleSettlementDelete(rec, d30Req(t, ctx,
		&auth.Session{ID: "d31p2del", Household: hh, UserID: m2},
		"/settlements/delete", payID, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("D2: leg-2 payer delete: status %d body %q", rec.Code, rec.Body.String())
	}
	if n := d30Int(t, pool,
		`SELECT count(*) FROM settlement WHERE id = $1`, payID); n != 0 {
		t.Fatalf("settlement survived the delete")
	}
	if n := d30Int(t, pool,
		`SELECT count(*) FROM settlement_funding WHERE settlement_id = $1`, payID); n != 0 {
		t.Fatalf("legs did not cascade on delete")
	}
}

// --- 4. history trail ---------------------------------------------------------

// d31TxnForm builds a minimal /txns create/edit form: an expense from an
// account to the recorder's party (destParty), one equal split to `user`.
func d31TxnForm(t *testing.T, user int64, total, desc string, origin, destParty int64) url.Values {
	t.Helper()
	form := url.Values{}
	form.Set("class", "expense")
	form.Set("date", "2026-10-04")
	form.Set("total", total)
	form.Set("description", desc)
	form.Set("split_mode", "equal")
	form.Set("origin_ref", fmt.Sprintf("account:%d", origin))
	form.Set("destination_ref", fmt.Sprintf("party:%d", destParty))
	form.Set("part_key[]", fmt.Sprintf("user:%d", user))
	form.Set("part_value[]", "")
	return form
}

// TestD31HistoryTrail covers the audit write path (created / edited /
// voided rows with snapshots), per-row visibility, and the clear tombstone.
func TestD31HistoryTrail(t *testing.T) {
	a, pool := d30App(t)
	ctx := context.Background()
	hh := d30Household(t, pool, "D31 Hist")
	owner := d30User(t, pool, hh, "d31howner", "")
	uA := d30User(t, pool, hh, "d31ha", "")
	uB := d30User(t, pool, hh, "d31hb", "")
	d30Membership(t, pool, owner, hh, "owner")
	d30Membership(t, pool, uA, hh, "member")
	d30Membership(t, pool, uB, hh, "member")

	accA := d30Account(t, pool, hh, "D31H AccA", 0)
	accB := d30Account(t, pool, hh, "D31H AccB", 0)
	// uA works on AccA, uB on AccB; no cross-grants (per-row visibility).
	d30Grant(t, pool, hh, uA, accA, "All", "All")
	d30Grant(t, pool, hh, uB, accB, "All", "All")

	histOf := func(txn int64) int64 {
		t.Helper()
		return d30Int(t, pool,
			`SELECT count(*) FROM txn_history WHERE entity_type = 'transaction' AND entity_id = $1`, txn)
	}
	sumOf := func(txn int64, action string) string {
		t.Helper()
		return d30Str(t, pool,
			`SELECT summary FROM txn_history
			 WHERE entity_type = 'transaction' AND entity_id = $1 AND action = $2
			 ORDER BY created_at DESC, id DESC LIMIT 1`, txn, action)
	}

	// uA creates a record -> a 'created' row with the access snapshot.
	uAParty := d30Int(t, pool,
		`SELECT id FROM party WHERE household_id = $1 AND app_user_id = $2`, hh, uA)
	recA := &auth.Session{ID: "d31ha", Household: hh, UserID: uA}
	rec := httptest.NewRecorder()
	a.handleTxnCreate(rec, d30Req(t, ctx, recA, "/txns/new", 0,
		d31TxnForm(t, uA, "100.00", "D31H A", accA, uAParty)))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("txn create: status %d body %q", rec.Code, rec.Body.String())
	}
	txnA := d30Int(t, pool,
		`SELECT id FROM "transaction" WHERE household_id = $1 ORDER BY id DESC LIMIT 1`, hh)
	if n := histOf(txnA); n != 1 {
		t.Fatalf("created history rows = %d, want 1", n)
	}
	if s := sumOf(txnA, "created"); s == "" {
		t.Fatalf("created summary is empty")
	}
	// Snapshot: the record's account surface + its Related-slice members.
	var accIDs, relIDs []int64
	if err := pool.QueryRow(ctx,
		`SELECT account_ids, related_user_ids FROM txn_history WHERE entity_id = $1`,
		txnA).Scan(&accIDs, &relIDs); err != nil {
		t.Fatalf("snapshot scan: %v", err)
	}
	if len(accIDs) != 1 || accIDs[0] != accA {
		t.Fatalf("snapshot account_ids = %v, want [%d]", accIDs, accA)
	}
	if len(relIDs) == 0 || relIDs[0] != uA {
		t.Fatalf("snapshot related_user_ids = %v, want the recorder %d", relIDs, uA)
	}

	// uA edits the amount -> an 'edited' row with a before→after diff.
	{
		form := d31TxnForm(t, uA, "120.00", "D31H A edited", accA, uAParty)
		rec = httptest.NewRecorder()
		a.handleTxnEditPost(rec, d30Req(t, ctx, recA, "/txns/edit", txnA, form))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("txn edit: status %d body %q", rec.Code, rec.Body.String())
		}
		if n := histOf(txnA); n != 2 {
			t.Fatalf("history rows after edit = %d, want 2", n)
		}
		if s := sumOf(txnA, "edited"); !strings.Contains(s, "amount") {
			t.Fatalf("edited summary = %q, want the amount diff", s)
		}
	}

	// uA voids -> a 'voided' row.
	{
		rec = httptest.NewRecorder()
		form := url.Values{}
		form.Set("status", "void")
		a.handleTxnStatus(rec, d30Req(t, ctx, recA, "/txns/status", txnA, form))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("txn void: status %d body %q", rec.Code, rec.Body.String())
		}
		if n := histOf(txnA); n != 3 {
			t.Fatalf("history rows after void = %d, want 3", n)
		}
	}

	// Per-row visibility on /history: uB sees uA's rows NOT (uB has no read
	// on AccA and is not related); the owner sees them.
	{
		fetch := func(s *auth.Session) string {
			t.Helper()
			r := httptest.NewRequest("GET", "/history", nil)
			rec := httptest.NewRecorder()
			a.handleHistory(rec, r.WithContext(contextWithSession(ctx, s)))
			if rec.Code != http.StatusOK {
				t.Fatalf("/history: status %d", rec.Code)
			}
			return rec.Body.String()
		}
		if b := fetch(&auth.Session{ID: "d31hb", Household: hh, UserID: uB}); strings.Contains(b, "D31H A") {
			t.Fatalf("visibility: uB (no grant, not related) must not see uA's record rows")
		}
		if b := fetch(&auth.Session{ID: "d31ho", Household: hh, UserID: owner}); !strings.Contains(b, "D31H A") {
			t.Fatalf("visibility: the owner must see every row")
		}
	}

	// Clear (owner): removes the rows, leaves one tombstone.
	{
		rec = httptest.NewRecorder()
		a.handleHistoryClear(rec, d30Req(t, ctx,
			&auth.Session{ID: "d31hoc", Household: hh, UserID: owner}, "/history/clear", 0, nil))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("history clear: status %d", rec.Code)
		}
		if n := d30Int(t, pool,
			`SELECT count(*) FROM txn_history WHERE household_id = $1 AND action <> 'cleared'`, hh); n != 0 {
			t.Fatalf("rows after clear = %d, want 0", n)
		}
		if n := d30Int(t, pool,
			`SELECT count(*) FROM txn_history WHERE household_id = $1 AND action = 'cleared'`, hh); n != 1 {
			t.Fatalf("tombstones after clear = %d, want 1", n)
		}
	}
}

// --- 5. deletion log: snapshots + ?op filter ----------------------------------

// TestD31DeletionLogOp covers the deletion-log extension: new rows carry the
// write-time access snapshot, ?op=N renders one operation's rows only, and
// legacy (empty-snapshot) rows stay visible to every member.
func TestD31DeletionLogOp(t *testing.T) {
	a, pool := d30App(t)
	ctx := context.Background()
	hh := d30Household(t, pool, "D31 DL")
	owner := d30User(t, pool, hh, "d31dlo", "")
	uA := d30User(t, pool, hh, "d31dla", "")
	uB := d30User(t, pool, hh, "d31dib", "")
	d30Membership(t, pool, owner, hh, "owner")
	d30Membership(t, pool, uA, hh, "member")
	d30Membership(t, pool, uB, hh, "member")

	accA := d30Account(t, pool, hh, "D31DL A", 0)
	accB := d30Account(t, pool, hh, "D31DL B", 0)
	// uA may delete-All on AccA only; uB sees nothing (the visibility Δ).
	d30Grant(t, pool, hh, uA, accA, "All", "All")
	d30Grant(t, pool, hh, uB, accB, "Related", "")

	txnA := d30Txn(t, pool, hh, accA, uA, "D31DL record A")
	txnB := d30Txn(t, pool, hh, accB, uB, "D31DL record B")

	// uA bulk-deletes their record (delete-All on AccA).
	sessA := &auth.Session{ID: "d31dla", Household: hh, UserID: uA}
	rec := httptest.NewRecorder()
	form := url.Values{}
	form.Add("ids[]", strconv.FormatInt(txnA, 10))
	a.handleBulkDelete(rec, d30Req(t, ctx, sessA, "/txns/bulk-delete", 0, form))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("bulk delete: status %d body %q", rec.Code, rec.Body.String())
	}
	// New rows carry the write-time access snapshot.
	var opID int64
	var accIDs, relIDs []int64
	if err := pool.QueryRow(ctx,
		`SELECT operation_id, account_ids, related_user_ids FROM deletion_log
		 WHERE household_id = $1 AND txn_id = $2 AND outcome = 'deleted'`, hh, txnA).
		Scan(&opID, &accIDs, &relIDs); err != nil {
		t.Fatalf("deletion_log row: %v", err)
	}
	if len(accIDs) == 0 || accIDs[0] != accA {
		t.Fatalf("new-row snapshot account_ids = %v, want [%d]", accIDs, accA)
	}
	if len(relIDs) == 0 || relIDs[0] != uA {
		t.Fatalf("new-row snapshot related_user_ids = %v, want the recorder %d", relIDs, uA)
	}

	// Legacy row (pre-D31 shape: empty snapshot) stays visible to all.
	_, err := pool.Exec(ctx,
		`INSERT INTO deletion_log (household_id, operation_id, txn_id, txn_name, outcome, actor_id)
		 VALUES ($1, 999, $2, 'legacy row', 'deleted', $3)`, hh, txnB, uB)
	if err != nil {
		t.Fatalf("legacy row: %v", err)
	}

	fetch := func(s *auth.Session, op int64) string {
		t.Helper()
		path := "/deletion-log"
		if op > 0 {
			path += "?op=" + strconv.FormatInt(op, 10)
		}
		r := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		a.handleDeletionLog(rec, r.WithContext(contextWithSession(ctx, s)))
		if rec.Code != http.StatusOK {
			t.Fatalf("/deletion-log: status %d", rec.Code)
		}
		return rec.Body.String()
	}

	// ?op filter: only this operation's row, with its reason column.
	if b := fetch(sessA, opID); !strings.Contains(b, "D31DL record A") || strings.Contains(b, "legacy row") {
		t.Fatalf("?op=%d: must render the operation's row only", opID)
	}
	// uB (no read on AccA, not related) does NOT see the new row...
	if b := fetch(&auth.Session{ID: "d31dib", Household: hh, UserID: uB}, 0); strings.Contains(b, "D31DL record A") {
		t.Fatalf("per-row visibility: uB must not see the snapshot'd row")
	}
	// ...but the legacy row is still visible (empty snapshot = all members).
	if b := fetch(&auth.Session{ID: "d31dib", Household: hh, UserID: uB}, 0); !strings.Contains(b, "legacy row") {
		t.Fatalf("legacy row must stay visible to every member")
	}
}
