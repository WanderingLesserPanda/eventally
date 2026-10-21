// DB-backed tests for Gate 5 (000012): the two-tier grant model enforcement.
// Scenario-dense synthetic seeds — no bulk data (goal item 7). Skipped unless
// EVENTALLY_TEST_DSN is set (harness: recurring_gate2_test.go).
//
// Coverage:
//   - mode forcing (DB CHECKs): edit=All⇒view=All, delete=All⇒view+edit All,
//     edit⇒read, delete⇒read+edit, create⇒read (both grant tiers)
//   - related slice (view mode Related vs All) via the txn visibility SQL
//   - anchor rule (settlement surface = own account + card family; the D6
//     source account is NOT required for read/delete)
//   - R5 (recurring delete author-scoped; split parties see + edit, no delete)
//   - strict account delete (zero tied records)
//   - household-level edit/delete modes (created | all) and the admin flag
//   - owner implicit full; member default read-only
package web

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// g5Fixture seeds a household with an owner, two members, and the account
// shapes under test (a plain account, a card family, a source asset account).
// Returns the App wired to the pool plus all the ids.
func g5Fixture(t *testing.T, pool *pgxpool.Pool) (a *App, hh, owner, member, member2, acctA, cardParent, cardChild, sourceAcct int64) {
	t.Helper()
	ctx := context.Background()

	if err := pool.QueryRow(ctx,
		`INSERT INTO household (name) VALUES ('Gate5 Test') RETURNING id`).Scan(&hh); err != nil {
		t.Fatalf("household: %v", err)
	}
	mkUser := func(username, role string) int64 {
		var u int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO app_user (default_household_id, username, password_hash, display_name)
			 VALUES ($1,$2,'x',$2) RETURNING id`, hh, username).Scan(&u); err != nil {
			t.Fatalf("user %s: %v", username, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO household_member (user_id, household_id, role) VALUES ($1,$2,$3)`,
			u, hh, role); err != nil {
			t.Fatalf("membership %s: %v", username, err)
		}
		return u
	}
	owner = mkUser("g5owner", "owner")
	member = mkUser("g5member", "member")
	member2 = mkUser("g5member2", "member")

	mkAcct := func(name, typ, bal string, parent any) int64 {
		var id int64
		err := pool.QueryRow(ctx,
			`INSERT INTO account (household_id, name, account_type, balance_type, parent_id)
			 VALUES ($1,$2,$3,$4,$5) RETURNING id`, hh, name, typ, bal, parent).Scan(&id)
		if err != nil {
			t.Fatalf("account %s: %v", name, err)
		}
		return id
	}
	acctA = mkAcct("G5 Savings", "savings", "asset", nil)
	cardParent = mkAcct("G5 Card", "credit_card", "liability", nil)
	cardChild = mkAcct("G5 Card Child", "credit_card", "liability", cardParent)
	sourceAcct = mkAcct("G5 Checking", "cash", "asset", nil)

	a = &App{Pool: pool}
	return
}

// grantAccount inserts an account-level grant row. Modes are forced by DB
// CHECKs, so only legal combinations may be inserted.
func grantAccount(t *testing.T, pool *pgxpool.Pool, hh, user, acct int64,
	read, create, edit, del bool, viewMode, editMode, delMode string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO account_grant
			(user_id, household_id, account_id, can_read, can_create, can_edit, can_delete,
			 view_mode, edit_mode, delete_mode)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		user, hh, acct, read, create, edit, del, viewMode, editMode, delMode); err != nil {
		t.Fatalf("account grant: %v", err)
	}
}

// grantHousehold inserts the household-level grant (one row per user: PK).
func grantHousehold(t *testing.T, pool *pgxpool.Pool, hh, user int64,
	read, create, edit, del bool, editMode, delMode string, admin bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO household_grant
			(user_id, household_id, can_read, can_create, can_edit, can_delete,
			 edit_mode, delete_mode, is_admin)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		user, hh, read, create, edit, del, editMode, delMode, admin); err != nil {
		t.Fatalf("household grant: %v", err)
	}
}

// loadPerms is permFor without an HTTP request.
func loadPerms(t *testing.T, a *App, hh, user int64) *perms {
	t.Helper()
	p, err := a.permLoad(context.Background(), hh, user)
	if err != nil {
		t.Fatalf("permLoad: %v", err)
	}
	return p
}

// mkTxn inserts an expense on the given card recorded by recorder, with an
// equal split across the given party ids (empty = no splits, which the
// deferred split-sum constraint allows). The txn + split rows are inserted in
// ONE transaction because the split-sum check is a deferred constraint
// trigger that evaluates at commit.
func mkTxn(t *testing.T, pool *pgxpool.Pool, hh, recorder, card int64, parties []int64, total string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var txnID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO "transaction"
			(household_id, recorded_by_user_id, transaction_date, class, total_amount,
			 origin_account_id)
		 VALUES ($1, $2, '2026-10-15', 'expense', $3, $4)
		RETURNING id`, hh, recorder, total, card).Scan(&txnID)
	if err != nil {
		tx.Rollback(ctx)
		t.Fatalf("txn: %v", err)
	}
	if len(parties) > 0 {
		var cents int64
		if i := dotIndex(total); i >= 0 {
			whole, frac := total[:i], total[i+1:]
			for len(frac) < 2 {
				frac += "0"
			}
			cents = int64(atoiSafe(whole))*100 + int64(atoiSafe(frac[:2]))
		} else {
			cents = int64(atoiSafe(total)) * 100
		}
		if cents%int64(len(parties)) != 0 {
			tx.Rollback(ctx)
			t.Fatalf("amount %s not divisible by %d parties", total, len(parties))
		}
		per := cents / int64(len(parties))
		share := fmt.Sprintf("%d.%02d", per/100, per%100)
		for _, party := range parties {
			if _, err := tx.Exec(ctx,
				`INSERT INTO transaction_split (household_id, transaction_id, party_id, amount)
				 VALUES ($1,$2,$3,$4)`, hh, txnID, party, share); err != nil {
				tx.Rollback(ctx)
				t.Fatalf("split: %v", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return txnID
}

func dotIndex(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

// partyIDOf returns the seeded member party row for a user (trigger-seeded).
func partyIDOf(t *testing.T, pool *pgxpool.Pool, hh, user int64) int64 {
	t.Helper()
	var p int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM party WHERE household_id = $1 AND app_user_id = $2`, hh, user).Scan(&p); err != nil {
		t.Fatalf("member party for user %d: %v", user, err)
	}
	return p
}

// visibleTxnIDs runs the exact visibility SQL listTxns uses: the household
// filter + the fragment, with $1 = household, $2 = user (an unused $2 is
// harmless for owners, whose fragment is the constant TRUE).
func visibleTxnIDs(t *testing.T, pool *pgxpool.Pool, p *perms, hh, user int64) []int64 {
	t.Helper()
	ctx := context.Background()
	where := txnVisibleWhere(p, 2, 1)
	rows, err := pool.Query(ctx,
		`SELECT t.id FROM "transaction" t
		 WHERE t.household_id = $1 AND `+where+`
		 ORDER BY t.id`, hh, user)
	if err != nil {
		t.Fatalf("visibility query: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return ids
}

// mkSettlement records a settlement on the card (optionally with a D6 ledger
// transfer from sourceID) allocating the full amount to the given split row.
func mkSettlement(t *testing.T, pool *pgxpool.Pool, hh, payer, card, sourceID, split int64, amount string) int64 {
	t.Helper()
	ctx := context.Background()
	var ledger int64
	if sourceID > 0 {
		// D6: source -> card transfer (origin = source account).
		if err := pool.QueryRow(ctx, `
			INSERT INTO "transaction"
				(household_id, recorded_by_user_id, transaction_date, class, description,
				 total_amount, origin_account_id, destination_account_id, record_status)
			 VALUES ($1,$2,'2026-10-20','transfer','Card payment',$3,$4,$5,'approved')
			RETURNING id`, hh, payer, amount, sourceID, card).Scan(&ledger); err != nil {
			t.Fatalf("d6 transfer: %v", err)
		}
	}
	var payID int64
	if sourceID > 0 {
		if err := pool.QueryRow(ctx, `
			INSERT INTO settlement
				(household_id, account_id, paid_by_user_id, amount, payment_date, method, ledger_transaction_id)
			 VALUES ($1,$2,$3,$4,'2026-10-20','transfer',$5) RETURNING id`,
			hh, card, payer, amount, ledger).Scan(&payID); err != nil {
			t.Fatalf("settlement: %v", err)
		}
	} else {
		if err := pool.QueryRow(ctx, `
			INSERT INTO settlement
				(household_id, account_id, paid_by_user_id, amount, payment_date, method)
			 VALUES ($1,$2,$3,$4,'2026-10-20','transfer') RETURNING id`,
			hh, card, payer, amount).Scan(&payID); err != nil {
			t.Fatalf("settlement: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO settlement_allocation (household_id, payment_id, split_id, amount)
		 VALUES ($1,$2,$3,$4)`, hh, payID, split, amount); err != nil {
		t.Fatalf("allocation: %v", err)
	}
	return payID
}

func splitIDOf(t *testing.T, pool *pgxpool.Pool, txn, party int64) int64 {
	t.Helper()
	var s int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM transaction_split WHERE transaction_id = $1 AND party_id = $2`,
		txn, party).Scan(&s); err != nil {
		t.Fatalf("find split (txn %d, party %d): %v", txn, party, err)
	}
	return s
}

func hasID(ids []int64, x int64) bool {
	for _, v := range ids {
		if v == x {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestG5_ModeForcingChecks(t *testing.T) {
	dsn := testDSN(t)
	pool := testPool(t, dsn)
	ctx := context.Background()
	_, hh, owner, member, _, acct, _, _, _ := g5Fixture(t, pool)

	acctBad := func(desc string, read, create, edit, del bool, vm, em, dm string) {
		t.Helper()
		_, err := pool.Exec(ctx,
			`INSERT INTO account_grant
				(user_id, household_id, account_id, can_read, can_create, can_edit, can_delete,
				 view_mode, edit_mode, delete_mode)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			owner, hh, acct, read, create, edit, del, vm, em, dm)
		if err == nil {
			t.Fatalf("%s: expected CHECK violation, got none", desc)
		}
		if code := errCode(err); code != "23514" {
			t.Fatalf("%s: expected 23514 (check_violation), got %s: %v", desc, code, err)
		}
	}
	// edit=All forces view=All.
	acctBad("edit=All with view=Related", true, false, true, false, "Related", "All", "Related")
	// edit without read.
	acctBad("edit without read", false, false, true, false, "Related", "Related", "Related")
	// create without read.
	acctBad("create without read", false, true, false, false, "Related", "Related", "Related")
	// delete without read.
	acctBad("delete without read", false, false, false, true, "Related", "Related", "Related")
	// delete without edit.
	acctBad("delete without edit", true, false, false, true, "Related", "Related", "Related")
	// delete=All forces view=All (and edit=All).
	acctBad("delete=All with view=Related", true, false, true, true, "Related", "All", "All")
	// A legal full row lands.
	if _, err := pool.Exec(ctx,
		`INSERT INTO account_grant
			(user_id, household_id, account_id, can_read, can_create, can_edit, can_delete,
			 view_mode, edit_mode, delete_mode)
		 VALUES ($1,$2,$3,true,true,true,true,'All','All','All')`,
		owner, hh, acct); err != nil {
		t.Fatalf("legal full grant rejected: %v", err)
	}

	hhBad := func(desc string, read, create, edit, del bool, em, dm string, admin bool) {
		t.Helper()
		_, err := pool.Exec(ctx,
			`INSERT INTO household_grant
				(user_id, household_id, can_read, can_create, can_edit, can_delete,
				 edit_mode, delete_mode, is_admin)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			member, hh, read, create, edit, del, em, dm, admin)
		if err == nil {
			t.Fatalf("%s: expected CHECK violation, got none", desc)
		}
		if code := errCode(err); code != "23514" {
			t.Fatalf("%s: expected 23514, got %s: %v", desc, code, err)
		}
	}
	// delete=all forces edit=all.
	hhBad("h-del=all with edit=created", true, true, true, true, "created", "all", false)
	// delete without edit.
	hhBad("h-del without h-edit", true, true, false, true, "created", "created", false)
	// empty row with no rights and no admin.
	hhBad("h-empty non-admin row", false, false, false, false, "created", "created", false)
}

func TestG5_RelatedSlice(t *testing.T) {
	dsn := testDSN(t)
	pool := testPool(t, dsn)
	a, hh, owner, member, member2, _, cardParent, _, _ := g5Fixture(t, pool)

	pOwner := partyIDOf(t, pool, hh, owner)
	pMember := partyIDOf(t, pool, hh, member)

	// txn1: recorded by owner, split owner-only (member NOT related).
	txn1 := mkTxn(t, pool, hh, owner, cardParent, []int64{pOwner}, "100.00")
	// txn2: recorded by owner, split includes the member (member IS related).
	txn2 := mkTxn(t, pool, hh, owner, cardParent, []int64{pOwner, pMember}, "60.00")

	// Member: read + Related view on the card.
	grantAccount(t, pool, hh, member, cardParent, true, false, false, false, "Related", "Related", "Related")
	pMem := loadPerms(t, a, hh, member)
	seen := visibleTxnIDs(t, pool, pMem, hh, member)
	if len(seen) != 1 || seen[0] != txn2 {
		t.Fatalf("member Related view should see only the related txn; got %v (want [%d])", seen, txn2)
	}
	_ = txn1

	// Member: All view sees both.
	if _, err := pool.Exec(context.Background(),
		`UPDATE account_grant SET view_mode = 'All'
		 WHERE user_id = $1 AND household_id = $2 AND account_id = $3`,
		member, hh, cardParent); err != nil {
		t.Fatalf("update grant: %v", err)
	}
	pMemAll := loadPerms(t, a, hh, member)
	if got := visibleTxnIDs(t, pool, pMemAll, hh, member); len(got) != 2 {
		t.Fatalf("member All view should see both txns; got %v", got)
	}

	// Owner sees everything without any grant rows.
	pOwn := loadPerms(t, a, hh, owner)
	if !pOwn.isOwner() {
		t.Fatalf("owner must be implicit full")
	}
	if got := visibleTxnIDs(t, pool, pOwn, hh, owner); len(got) != 2 {
		t.Fatalf("owner should see both; got %v", got)
	}

	// Member2: no grants at all -> sees nothing (default read-only).
	pM2 := loadPerms(t, a, hh, member2)
	if got := visibleTxnIDs(t, pool, pM2, hh, member2); len(got) != 0 {
		t.Fatalf("member without grants sees nothing; got %v", got)
	}
}

func TestG5_AnchorRule(t *testing.T) {
	dsn := testDSN(t)
	pool := testPool(t, dsn)
	a, hh, owner, member, member2, _, cardParent, cardChild, sourceAcct := g5Fixture(t, pool)
	ctx := context.Background()

	pOwner := partyIDOf(t, pool, hh, owner)
	pMember2 := partyIDOf(t, pool, hh, member2)

	// Expense on the CHILD card, total 40.00 split owner+member2 (20/20).
	txn1 := mkTxn(t, pool, hh, owner, cardChild, []int64{pOwner, pMember2}, "40.00")
	splitM2 := splitIDOf(t, pool, txn1, pMember2)
	// MEMBER (the payer) records a 40.00 settlement on the PARENT (the family
	// anchor) with a D6 transfer from the source asset account, allocating
	// member2's full 20.00 split (the cap allows up to the split amount).
	pay1 := mkSettlement(t, pool, hh, member, cardParent, sourceAcct, splitM2, "20.00")

	// The payer has family read grants but NO grant on the D6 source: the
	// anchor rule says the source is not required for read/delete.
	grantAccount(t, pool, hh, member, cardParent, true, false, false, false, "Related", "Related", "Related")
	grantAccount(t, pool, hh, member, cardChild, true, false, false, false, "Related", "Related", "Related")
	pMem := loadPerms(t, a, hh, member)
	see, err := a.canSeeSettlement(ctx, pMem, pay1)
	if err != nil {
		t.Fatalf("canSeeSettlement(payer): %v", err)
	}
	if !see {
		t.Fatalf("payer with family read grants must see the settlement (D6 source not required)")
	}

	// Surface sanity: both family members in, D6 source out.
	surf, err := a.settlementSurface(ctx, hh, pay1)
	if err != nil {
		t.Fatalf("settlementSurface: %v", err)
	}
	if !hasID(surf, cardParent) || !hasID(surf, cardChild) {
		t.Fatalf("surface must include the whole card family: %v", surf)
	}
	if hasID(surf, sourceAcct) {
		t.Fatalf("surface must NOT include the D6 source account: %v", surf)
	}

	// member2 is a split party of the allocated split -> related -> sees it
	// with a family read grant.
	grantAccount(t, pool, hh, member2, cardParent, true, false, false, false, "Related", "Related", "Related")
	grantAccount(t, pool, hh, member2, cardChild, true, false, false, false, "Related", "Related", "Related")
	pM2 := loadPerms(t, a, hh, member2)
	see2, err := a.canSeeSettlement(ctx, pM2, pay1)
	if err != nil {
		t.Fatalf("canSeeSettlement(split party): %v", err)
	}
	if !see2 {
		t.Fatalf("split party of an allocated split is in the settlement slice")
	}

	// Unrelated: a second settlement (no D6) by the OWNER allocating to an
	// owner-only split. member2 has the read grant but is unrelated -> no.
	txn2 := mkTxn(t, pool, hh, owner, cardChild, []int64{pOwner}, "30.00")
	splitOwner := splitIDOf(t, pool, txn2, pOwner)
	pay2 := mkSettlement(t, pool, hh, owner, cardParent, 0, splitOwner, "30.00")
	see3, err := a.canSeeSettlement(ctx, pM2, pay2)
	if err != nil {
		t.Fatalf("canSeeSettlement(unrelated): %v", err)
	}
	if see3 {
		t.Fatalf("unrelated member must NOT see the settlement (Related view, not payer, not split party)")
	}

	// Owner sees both without grants.
	pOwn := loadPerms(t, a, hh, owner)
	if seeO, err := a.canSeeSettlement(ctx, pOwn, pay2); err != nil || !seeO {
		t.Fatalf("owner must see all settlements: %v", err)
	}
}

func TestG5_R5RecurringAuthorScoped(t *testing.T) {
	dsn := testDSN(t)
	pool := testPool(t, dsn)
	a, hh, owner, member, member2, _, cardParent, _, _ := g5Fixture(t, pool)
	ctx := context.Background()

	// Owner creates a template on the card; split_spec names owner + member.
	spec := fmt.Sprintf(`["user:%d","user:%d"]`, owner, member)
	var tmplID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO recurring_template
			(household_id, name, account_id, amount, day_of_month, split_spec, created_by_user_id)
		 VALUES ($1,'G5 Rent',$2,1000.00,1,$3::jsonb,$4) RETURNING id`,
		hh, cardParent, spec, owner).Scan(&tmplID); err != nil {
		t.Fatalf("template: %v", err)
	}
	_ = tmplID

	// member: read+create+edit+delete All on the card (legal: edit=All forces
	// view=All, delete=All forces edit+view All).
	grantAccount(t, pool, hh, member, cardParent, true, true, true, true, "All", "All", "All")
	// member2: read+create+edit (Related) — the DB forces edit⇒create⇒read.
	grantAccount(t, pool, hh, member2, cardParent, true, true, true, false, "Related", "Related", "Related")
	pMem := loadPerms(t, a, hh, member)
	pM2 := loadPerms(t, a, hh, member2)
	pOwn := loadPerms(t, a, hh, owner)

	// R5: delete is author-scoped — the creator (or owner) only.
	if !pOwn.canDeleteRecurring(owner) {
		t.Fatalf("owner may delete any template")
	}
	if pMem.canDeleteRecurring(owner) {
		t.Fatalf("split party must NOT delete another's template (R5)")
	}
	if !pMem.canDeleteRecurring(member) {
		t.Fatalf("creator may delete their own template")
	}

	// Edit: member is a split party (All edit grant) -> can edit; member2 is
	// NOT named in the spec -> Related edit refuses.
	if !pMem.canEditRecurring(cardParent, true) {
		t.Fatalf("split party with an edit grant can edit")
	}
	if !pMem.canEditRecurring(cardParent, false) {
		t.Fatalf("All-mode edit grant does not require the slice")
	}
	if pM2.canEditRecurring(cardParent, false) {
		t.Fatalf("member2 (Related edit, not in spec) must not edit")
	}
	if !pM2.canEditRecurring(cardParent, true) {
		t.Fatalf("Related-mode edit allows in-spec parties")
	}

	// Visibility (SQL predicate, as handleRecurring uses): the creator sees
	// their own; a split party with an All-read grant sees it too.
	if _, err := pool.Exec(ctx, `
		INSERT INTO recurring_template
			(household_id, name, account_id, amount, day_of_month, split_spec, created_by_user_id)
		 VALUES ($1,'G5 Rent2',$2,500.00,2,'[]'::jsonb,$3)`, hh, cardParent, owner); err != nil {
		t.Fatalf("template2: %v", err)
	}
	seeMem := visibleRecurringIDs(t, pool, pMem, hh, member)
	if !hasID(seeMem, tmplID) {
		t.Fatalf("split party with All-read grant sees the template; got %v", seeMem)
	}
	seeM2 := visibleRecurringIDs(t, pool, pM2, hh, member2)
	if hasID(seeM2, tmplID) {
		t.Fatalf("member2 (Related view, not in spec) must NOT see the template; got %v", seeM2)
	}

	// Split-party visibility through a Related-view grant only (the branch
	// that a bare All-read grant never exercises): a new account + a
	// Related read grant for member, and a template whose spec names member.
	var relAcc int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO account (household_id, name, account_type, balance_type, created_by_user_id)
		 VALUES ($1,'G5 RelAcct','cash','asset',$2) RETURNING id`, hh, member).Scan(&relAcc); err != nil {
		t.Fatalf("rel account: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE account_grant SET view_mode = 'Related', edit_mode = 'Related', delete_mode = 'Related'
		 WHERE user_id = $1 AND household_id = $2 AND account_id = $3`, member, hh, relAcc); err != nil {
		t.Fatalf("related grant: %v", err)
	}
	// Perms are loaded per request — re-load after the grant change.
	pMem = loadPerms(t, a, hh, member)
	var relTmpl int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO recurring_template
			(household_id, name, account_id, amount, day_of_month, split_spec, created_by_user_id)
		 VALUES ($1,'G5 RelRent',$2,100.00,3,$3::jsonb,$4) RETURNING id`,
		hh, relAcc, spec, owner).Scan(&relTmpl); err != nil {
		t.Fatalf("related template: %v", err)
	}
	seeMem = visibleRecurringIDs(t, pool, pMem, hh, member)
	if !hasID(seeMem, relTmpl) {
		t.Fatalf("split party with Related-view grant sees the template; got %v", seeMem)
	}
	seeM2 = visibleRecurringIDs(t, pool, pM2, hh, member2)
	if hasID(seeM2, relTmpl) {
		t.Fatalf("member2 (no grant on this account) must NOT see it; got %v", seeM2)
	}
}

// visibleRecurringIDs runs the recurring visibility predicate (same SQL the
// list handler uses). args: $1 = household, $uN = user (uN passed to
// recurringVisibleWhere; the template id is matched per row).
func visibleRecurringIDs(t *testing.T, pool *pgxpool.Pool, p *perms, hh, user int64) []int64 {
	t.Helper()
	where := recurringVisibleWhere(p, 2) // $2 = user, $1 = household
	rows, err := pool.Query(context.Background(),
		`SELECT t.id FROM recurring_template t
		 WHERE t.household_id = $1 `+where, hh, user)
	if err != nil {
		t.Fatalf("recurring visibility query: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return ids
}

func TestG5_StrictDelete(t *testing.T) {
	dsn := testDSN(t)
	pool := testPool(t, dsn)
	a, hh, owner, _, _, acctA, cardParent, _, sourceAcct := g5Fixture(t, pool)
	ctx := context.Background()

	// Clean account: no tied records.
	tied, err := a.strictAccountHasTiedRecords(ctx, hh, acctA)
	if err != nil {
		t.Fatalf("strict check: %v", err)
	}
	if tied {
		t.Fatalf("empty account must be strictly deletable")
	}

	// Parent with a child card: tied.
	tied, err = a.strictAccountHasTiedRecords(ctx, hh, cardParent)
	if err != nil {
		t.Fatalf("strict check (child): %v", err)
	}
	if !tied {
		t.Fatalf("parent with a child card is tied (cannot strict-delete)")
	}

	// Account with a non-void transaction: tied.
	txnID := mkTxn(t, pool, hh, owner, sourceAcct, nil, "10.00")
	tied, err = a.strictAccountHasTiedRecords(ctx, hh, sourceAcct)
	if err != nil {
		t.Fatalf("strict check (txn): %v", err)
	}
	if !tied {
		t.Fatalf("account with a non-void transaction is tied")
	}
	_ = txnID

	// Void transactions do NOT count: void the txn, account becomes clean.
	if _, err := pool.Exec(ctx,
		`UPDATE "transaction" SET record_status = 'void' WHERE id = $1`, txnID); err != nil {
		t.Fatalf("void txn: %v", err)
	}
	tied, err = a.strictAccountHasTiedRecords(ctx, hh, sourceAcct)
	if err != nil {
		t.Fatalf("strict check (voided): %v", err)
	}
	if tied {
		t.Fatalf("voided-only transactions do not tie an account")
	}
}

func TestG5_HouseholdModesAndAdmin(t *testing.T) {
	dsn := testDSN(t)
	pool := testPool(t, dsn)
	a, hh, owner, member, member2, _, cardParent, _, _ := g5Fixture(t, pool)
	ctx := context.Background()

	mkAcct := func(creator int64, name string) int64 {
		var id int64
		err := pool.QueryRow(ctx,
			`INSERT INTO account (household_id, name, account_type, balance_type, created_by_user_id)
			 VALUES ($1,$2,'savings','asset',$3) RETURNING id`, hh, name, creator).Scan(&id)
		if err != nil {
			t.Fatalf("account: %v", err)
		}
		return id
	}
	ownAcct := mkAcct(owner, "Owner Acct")
	memAcct := mkAcct(member, "Member Acct")

	// Member with household edit/delete in created mode.
	grantHousehold(t, pool, hh, member, true, true, true, true, "created", "created", false)
	pMem := loadPerms(t, a, hh, member)
	if !pMem.canDeleteAccountObject(memAcct, member) {
		t.Fatalf("created mode: member deletes their own account")
	}
	if pMem.canDeleteAccountObject(ownAcct, owner) {
		t.Fatalf("created mode: member must NOT delete another's account")
	}
	if !pMem.canEditAccountObject(memAcct, member) {
		t.Fatalf("created mode: member edits their own account")
	}
	if pMem.canEditAccountObject(ownAcct, owner) {
		t.Fatalf("created mode: member must NOT edit another's account")
	}

	// Admin with all-mode household grants: full object reach, but NOT
	// record-level rights (those come only from account grants) and NOT the
	// owner.
	grantHousehold(t, pool, hh, member2, true, true, true, true, "all", "all", true)
	pAdmin := loadPerms(t, a, hh, member2)
	if pAdmin.isOwner() {
		t.Fatalf("an admin is never the owner")
	}
	if !pAdmin.canDeleteAccountObject(ownAcct, owner) {
		t.Fatalf("admin all-mode deletes any account object")
	}
	if !pAdmin.canEditAccountObject(ownAcct, owner) {
		t.Fatalf("admin all-mode edits any account object")
	}
	if pAdmin.canReadRecords(cardParent) {
		t.Fatalf("admin without account grants has no record read")
	}
	if pAdmin.canCreateOn(cardParent) {
		t.Fatalf("admin without account grants cannot create records")
	}

	// Owner implicit: everything works with zero grant rows of their own.
	pOwn := loadPerms(t, a, hh, owner)
	if !pOwn.isOwner() {
		t.Fatalf("owner is implicit full")
	}
	if !pOwn.canDeleteAccountObject(ownAcct, owner) || !pOwn.canEditAccountObject(memAcct, member) {
		t.Fatalf("owner deletes/edits any account object")
	}
	if !pOwn.canReadRecords(cardParent) {
		t.Fatalf("owner reads any record")
	}
}
