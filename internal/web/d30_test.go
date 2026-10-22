// D30 DB-backed tests: the account archive/restore state machine and its
// permission gates (Edit-All required; cascade only with Edit-All on every
// child), strict-delete's template cascade (migration 000015) with the
// transaction/settlement/child blocks, the bulk void/delete per-record
// matrix feeding the persistent deletion log, and the ownership transfer
// (ADR-0003). Scenario-dense synthetic seeds, no bulk data (goal item).
// Skipped unless EVENTALLY_TEST_DSN is set (harness: recurring_gate2_test.go).
package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// --- fixtures ----------------------------------------------------------------

// d30App returns an App with templates loaded (the transfer's error path
// re-renders the settings page, so a bare &App{} would panic) on a fresh
// migrated throwaway database.
func d30App(t *testing.T) (*App, *pgxpool.Pool) {
	t.Helper()
	pool := testPool(t, testDSN(t))
	a, err := New(pool, "templates", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, pool
}

func d30Household(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO household (name) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatalf("household %q: %v", name, err)
	}
	return id
}

// d30User creates a user; pw "" stores a dummy hash (password never checked).
func d30User(t *testing.T, pool *pgxpool.Pool, hh int64, username, pw string) int64 {
	t.Helper()
	hash := "x"
	if pw != "" {
		h, err := auth.HashPassword(pw)
		if err != nil {
			t.Fatalf("HashPassword: %v", err)
		}
		hash = h
	}
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO app_user (default_household_id, username, password_hash, display_name)
		 VALUES ($1,$2,$3,$2) RETURNING id`, hh, username, hash).Scan(&id)
	if err != nil {
		t.Fatalf("user %q: %v", username, err)
	}
	return id
}

func d30Membership(t *testing.T, pool *pgxpool.Pool, user, hh int64, role string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO household_member (user_id, household_id, role) VALUES ($1,$2,$3)`,
		user, hh, role); err != nil {
		t.Fatalf("membership %d→%d (%s): %v", user, hh, role, err)
	}
}

func d30Account(t *testing.T, pool *pgxpool.Pool, hh int64, name string, parent int64) int64 {
	t.Helper()
	var pid any
	if parent > 0 {
		pid = parent
	}
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO account (household_id, name, account_type, balance_type, parent_id)
		 VALUES ($1,$2,'savings','asset',$3) RETURNING id`, hh, name, pid).Scan(&id)
	if err != nil {
		t.Fatalf("account %q: %v", name, err)
	}
	return id
}

func d30Template(t *testing.T, pool *pgxpool.Pool, hh, acc int64, name string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO recurring_template (household_id, account_id, name, amount, day_of_month)
		 VALUES ($1,$2,$3,100,1) RETURNING id`, hh, acc, name).Scan(&id)
	if err != nil {
		t.Fatalf("template %q: %v", name, err)
	}
	return id
}

func d30Party(t *testing.T, pool *pgxpool.Pool, hh int64, name string, appUser int64) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO party (household_id, name, app_user_id) VALUES ($1,$2,$3) RETURNING id`,
		hh, name, appUser).Scan(&id)
	if err != nil {
		t.Fatalf("party %q: %v", name, err)
	}
	return id
}

// d30Txn creates a pending expense from acc recorded by recordedBy.
func d30Txn(t *testing.T, pool *pgxpool.Pool, hh, acc, recordedBy int64, desc string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO "transaction" (household_id, recorded_by_user_id, transaction_date, class,
                                    total_amount, description, origin_account_id)
		 VALUES ($1,$2, now()::date, 'expense', 42, $3, $4) RETURNING id`,
		hh, recordedBy, desc, acc).Scan(&id)
	if err != nil {
		t.Fatalf("txn %q: %v", desc, err)
	}
	return id
}

func d30ChildTxn(t *testing.T, pool *pgxpool.Pool, hh, parentTxn, acc, recordedBy int64, desc string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO "transaction" (household_id, recorded_by_user_id, transaction_date, class,
                                    total_amount, description, origin_account_id, parent_transaction_id)
		 VALUES ($1,$2, now()::date, 'expense', 7, $3, $4, $5) RETURNING id`,
		hh, recordedBy, desc, acc, parentTxn).Scan(&id)
	if err != nil {
		t.Fatalf("child txn %q: %v", desc, err)
	}
	return id
}

func d30Split(t *testing.T, pool *pgxpool.Pool, hh, txn, party int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO transaction_split (transaction_id, amount, household_id, party_id)
		 VALUES ($1,42,$2,$3)`, txn, hh, party); err != nil {
		t.Fatalf("split %d: %v", txn, err)
	}
}

// d30Grant seeds the smallest consistent account_grant for the matrix
// modes: view_mode is always All (an edit-All grant requires it), the edit
// right is always present, and can_delete follows delMode ("" = not
// granted). An empty (editMode, delMode) pair seeds no row at all.
func d30Grant(t *testing.T, pool *pgxpool.Pool, hh, user, acc int64, editMode, delMode string) {
	t.Helper()
	if editMode == "" && delMode == "" {
		return
	}
	// An empty mode means the column default ('Related'), not no value;
	// an empty delMode means the delete right is not granted at all.
	if editMode == "" {
		editMode = "Related"
	}
	canDelete := delMode != ""
	if delMode == "" {
		delMode = "Related"
	}
	canEdit := true
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO account_grant (user_id, household_id, account_id,
		 can_read, can_create, can_edit, can_delete, view_mode, edit_mode, delete_mode)
		 VALUES ($1,$2,$3, true, true, $4, $5, 'All', $6, $7)`,
		user, hh, acc, canEdit, canDelete, editMode, delMode); err != nil {
		t.Fatalf("grant %d→%d (%s/%s): %v", user, acc, editMode, delMode, err)
	}
}

// d30HHGrant seeds a household_grant row for matrix/admin gating.
func d30HHGrant(t *testing.T, pool *pgxpool.Pool, hh, user int64, canRead, isAdmin bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO household_grant (user_id, household_id, can_read, is_admin)
		 VALUES ($1,$2,$3,$4)`, user, hh, canRead, isAdmin); err != nil {
		t.Fatalf("household grant %d→%d: %v", user, hh, err)
	}
}

// d30Req builds a POST with a session in the context (the authed middleware
// is bypassed on purpose: the tests exercise the handlers' own gates). When
// id > 0 the chi route context is seeded so pathID works without the router.
func d30Req(t *testing.T, ctx context.Context, sess *auth.Session, path string, id int64, form url.Values) *http.Request {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest("POST", path, body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if id > 0 {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", strconv.FormatInt(id, 10))
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	return r.WithContext(contextWithSession(ctx, sess))
}

func d30Bool(t *testing.T, pool *pgxpool.Pool, q string, args ...any) bool {
	t.Helper()
	var b bool
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&b); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return b
}

func d30Int(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func d30Str(t *testing.T, pool *pgxpool.Pool, q string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

func d30Status(t *testing.T, pool *pgxpool.Pool, txnID int64) string {
	t.Helper()
	return d30Str(t, pool, `SELECT record_status FROM "transaction" WHERE id = $1`, txnID)
}

func d30Active(t *testing.T, pool *pgxpool.Pool, id int64, table string) bool {
	t.Helper()
	return d30Bool(t, pool, `SELECT is_active FROM `+table+` WHERE id = $1`, id)
}

func d30Role(t *testing.T, pool *pgxpool.Pool, user, hh int64) string {
	t.Helper()
	return d30Str(t, pool,
		`SELECT role FROM household_member WHERE user_id = $1 AND household_id = $2`, user, hh)
}

// --- tests --------------------------------------------------------------------

// TestD30ArchiveState: archive flips is_active=false and pauses the account's
// active templates; the cascade checkbox takes the active children along;
// restore flips the account back but leaves paused templates paused and
// never cascades.
func TestD30ArchiveState(t *testing.T) {
	a, pool := d30App(t)
	ctx := context.Background()

	hh := d30Household(t, pool, "Arc State")
	owner := d30User(t, pool, hh, "arcowner", "")
	d30Membership(t, pool, owner, hh, "owner")
	parent := d30Account(t, pool, hh, "Arc Parent", 0)
	child := d30Account(t, pool, hh, "Arc Child", parent)
	tp := d30Template(t, pool, hh, parent, "Arc Parent Tpl")
	tc := d30Template(t, pool, hh, child, "Arc Child Tpl")
	sess := &auth.Session{ID: "arc", Household: hh, UserID: owner}

	archive := func(form url.Values) string {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleAccountArchive(rec, d30Req(t, ctx, sess, "/accounts/"+strconv.FormatInt(parent, 10)+"/archive", parent, form))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("archive: status %d body %q", rec.Code, rec.Body.String())
		}
		return rec.Header().Get("Location")
	}
	restore := func() {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleAccountRestore(rec, d30Req(t, ctx, sess, "/accounts/"+strconv.FormatInt(parent, 10)+"/restore", parent, nil))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("restore: status %d body %q", rec.Code, rec.Body.String())
		}
	}

	// Archive, no cascade: the parent and its template only.
	loc := archive(nil)
	if !strings.Contains(loc, url.QueryEscape("Account archived. 1 recurring template(s) paused.")) {
		t.Fatalf("archive flash = %q", loc)
	}
	if d30Active(t, pool, parent, "account") {
		t.Fatal("parent still active after archive")
	}
	if !d30Active(t, pool, child, "account") {
		t.Fatal("child archived without the cascade checkbox")
	}
	if d30Active(t, pool, tp, "recurring_template") {
		t.Fatal("parent template still active after archive")
	}
	if !d30Active(t, pool, tc, "recurring_template") {
		t.Fatal("child template paused without the cascade checkbox")
	}

	// Restore: the account comes back, the paused template STAYS paused.
	restore()
	if !d30Active(t, pool, parent, "account") {
		t.Fatal("parent not active after restore")
	}
	if d30Active(t, pool, tp, "recurring_template") {
		t.Fatal("restore resumed a paused template (it must stay paused)")
	}

	// Archive with cascade: children and their templates go too. The
	// parent's template was already paused by the first archive (restore
	// does not resume it), so only the child's template pauses here — the
	// "1 linked account(s)" part is the cascade signal.
	loc = archive(url.Values{"cascade_children": {"1"}})
	if !strings.Contains(loc, url.QueryEscape("1 linked account(s) archived as well.")) {
		t.Fatalf("cascade flash = %q", loc)
	}
	if d30Active(t, pool, parent, "account") || d30Active(t, pool, child, "account") {
		t.Fatal("cascade left an account active")
	}
	if d30Active(t, pool, tp, "recurring_template") || d30Active(t, pool, tc, "recurring_template") {
		t.Fatal("cascade left a template active")
	}

	// Restore never cascades: the child stays archived.
	restore()
	if !d30Active(t, pool, parent, "account") {
		t.Fatal("parent not active after restore")
	}
	if d30Active(t, pool, child, "account") {
		t.Fatal("restore cascaded to the child (it must not)")
	}
}

// TestD30ArchivePermissions: archiving needs an Edit grant in ALL mode on
// the account (Related does not qualify); the cascade checkbox is offered
// only when the actor holds Edit-All on every active child, and requesting
// a cascade without it is a 403.
func TestD30ArchivePermissions(t *testing.T) {
	a, pool := d30App(t)
	ctx := context.Background()

	hh := d30Household(t, pool, "Arc Perm")
	owner := d30User(t, pool, hh, "arcpermown", "")
	d30Membership(t, pool, owner, hh, "owner")

	// Pair 1: member with Edit-All on parent AND child → full cascade.
	p1 := d30Account(t, pool, hh, "Perm Parent 1", 0)
	c1 := d30Account(t, pool, hh, "Perm Child 1", p1)
	mFull := d30User(t, pool, hh, "arcpermfull", "")
	d30Membership(t, pool, mFull, hh, "member")
	d30Grant(t, pool, hh, mFull, p1, "All", "")
	d30Grant(t, pool, hh, mFull, c1, "All", "")

	// Pair 2: member with Edit-All on the parent but only Edit-Related on
	// the child → can archive, cannot cascade.
	p2 := d30Account(t, pool, hh, "Perm Parent 2", 0)
	c2 := d30Account(t, pool, hh, "Perm Child 2", p2)
	mPartial := d30User(t, pool, hh, "arcpermpartial", "")
	d30Membership(t, pool, mPartial, hh, "member")
	d30Grant(t, pool, hh, mPartial, p2, "All", "")
	d30Grant(t, pool, hh, mPartial, c2, "Related", "")

	// p3: member with Edit-Related only → cannot archive at all.
	p3 := d30Account(t, pool, hh, "Perm Parent 3", 0)
	mRelated := d30User(t, pool, hh, "arcpermrelated", "")
	d30Membership(t, pool, mRelated, hh, "member")
	d30Grant(t, pool, hh, mRelated, p3, "Related", "")

	lc := func(user, acc int64) *accountLifecycle {
		t.Helper()
		sess := &auth.Session{ID: "perm", Household: hh, UserID: user}
		p, err := a.permFor(d30Req(t, ctx, sess, "/accounts", 0, nil))
		if err != nil {
			t.Fatalf("permFor: %v", err)
		}
		l, err := a.accountLifecycleFor(ctx, hh, acc, p)
		if err != nil {
			t.Fatalf("accountLifecycleFor: %v", err)
		}
		return l
	}

	if l := lc(owner, p1); !l.CanArchive || !l.CanCascade {
		t.Fatalf("owner lifecycle = %+v (want both true)", l)
	}
	if l := lc(mFull, p1); !l.CanArchive || !l.CanCascade {
		t.Fatalf("full member lifecycle = %+v (want both true)", l)
	}
	if l := lc(mPartial, p2); !l.CanArchive || l.CanCascade {
		t.Fatalf("partial member lifecycle = %+v (want CanArchive true, CanCascade false)", l)
	}
	if l := lc(mRelated, p3); l.CanArchive {
		t.Fatalf("related-only member lifecycle = %+v (want CanArchive false)", l)
	}

	post := func(user, accID int64, path string, form url.Values) int {
		t.Helper()
		sess := &auth.Session{ID: "perm", Household: hh, UserID: user}
		rec := httptest.NewRecorder()
		var h func(http.ResponseWriter, *http.Request)
		if strings.HasSuffix(path, "/archive") {
			h = a.handleAccountArchive
		} else {
			h = a.handleAccountRestore
		}
		h(rec, d30Req(t, ctx, sess, path, accID, form))
		return rec.Code
	}
	p1s := "/accounts/" + strconv.FormatInt(p1, 10)
	p2s := "/accounts/" + strconv.FormatInt(p2, 10)
	p3s := "/accounts/" + strconv.FormatInt(p3, 10)

	if code := post(mFull, p1, p1s+"/archive", url.Values{"cascade_children": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("full member cascade archive = %d, want 303", code)
	}
	if !d30Bool(t, pool, `SELECT NOT is_active FROM account WHERE id = $1`, p1) ||
		!d30Bool(t, pool, `SELECT NOT is_active FROM account WHERE id = $1`, c1) {
		t.Fatal("full member cascade did not archive both accounts")
	}

	// Related-only: 403, account untouched.
	if code := post(mRelated, p3, p3s+"/archive", nil); code != http.StatusForbidden {
		t.Fatalf("related-only archive = %d, want 403", code)
	}
	if !d30Active(t, pool, p3, "account") {
		t.Fatal("related-only 403 still mutated the account")
	}

	// Partial: cascade requested without Edit-All on the child → 403;
	// the plain archive (no cascade) succeeds.
	if code := post(mPartial, p2, p2s+"/archive", url.Values{"cascade_children": {"1"}}); code != http.StatusForbidden {
		t.Fatalf("partial cascade archive = %d, want 403", code)
	}
	if code := post(mPartial, p2, p2s+"/archive", nil); code != http.StatusSeeOther {
		t.Fatalf("partial plain archive = %d, want 303", code)
	}
	if d30Active(t, pool, p2, "account") || !d30Active(t, pool, c2, "account") {
		t.Fatal("plain archive should have archived only the parent")
	}
}

// TestD30StrictDeleteCascade: deleting an account cascades its recurring
// templates (migration 000015) — templates are no longer a blocking tied
// record — while live transactions, settlements, and child accounts still
// block. A voided transaction does not tie an account.
func TestD30StrictDeleteCascade(t *testing.T) {
	a, pool := d30App(t)
	ctx := context.Background()

	hh := d30Household(t, pool, "Strict Del")
	owner := d30User(t, pool, hh, "strdelowner", "")
	d30Membership(t, pool, owner, hh, "owner")
	sess := &auth.Session{ID: "del", Household: hh, UserID: owner}

	del := func(acc int64) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleAccountDelete(rec, d30Req(t, ctx, sess,
			"/accounts/"+strconv.FormatInt(acc, 10)+"/delete", acc, nil))
		return rec
	}

	// Templates alone: the delete succeeds and the template cascades.
	accA := d30Account(t, pool, hh, "Del A", 0)
	tplA := d30Template(t, pool, hh, accA, "Del A Tpl")
	rec := del(accA)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete with templates only: status %d body %q", rec.Code, rec.Body.String())
	}
	if d30Int(t, pool, `SELECT count(*) FROM account WHERE id = $1`, accA) != 0 {
		t.Fatal("account A still exists")
	}
	if d30Int(t, pool, `SELECT count(*) FROM recurring_template WHERE id = $1`, tplA) != 0 {
		t.Fatal("template did not cascade with its account")
	}

	// A live transaction blocks; the message names the tied records.
	accB := d30Account(t, pool, hh, "Del B", 0)
	txB := d30Txn(t, pool, hh, accB, owner, "Del B Txn")
	rec = del(accB)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "records tied to it") {
		t.Fatalf("delete with a live txn: status %d body %q", rec.Code, rec.Body.String())
	}
	if d30Int(t, pool, `SELECT count(*) FROM account WHERE id = $1`, accB) == 0 ||
		d30Int(t, pool, `SELECT count(*) FROM "transaction" WHERE id = $1`, txB) == 0 {
		t.Fatal("blocked delete mutated its data")
	}

	// A child card blocks the parent.
	accC := d30Account(t, pool, hh, "Del C", 0)
	accD := d30Account(t, pool, hh, "Del D", accC)
	rec = del(accC)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete with a child: status %d body %q", rec.Code, rec.Body.String())
	}
	if d30Int(t, pool, `SELECT count(*) FROM account WHERE id IN ($1,$2)`, accC, accD) != 2 {
		t.Fatal("blocked delete mutated its data")
	}

	// A voided transaction STILL ties the account: the FKs are SET NULL and
	// the class-endpoint invariant demands ≥1 surviving endpoint, so the
	// refusal (not a 500) is the correct behavior; deleting the record
	// first unblocks the account.
	if _, err := pool.Exec(ctx,
		`UPDATE "transaction" SET record_status = 'void' WHERE id = $1`, txB); err != nil {
		t.Fatalf("void txn: %v", err)
	}
	if rec := del(accB); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete with a voided txn: status %d body %q (want the 400 refusal, not a 500)", rec.Code, rec.Body.String())
	}
	if _, err := pool.Exec(ctx, `DELETE FROM "transaction" WHERE id = $1`, txB); err != nil {
		t.Fatalf("delete voided txn: %v", err)
	}
	if rec := del(accB); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete after clearing the txn: status %d body %q", rec.Code, rec.Body.String())
	}
	if d30Int(t, pool, `SELECT count(*) FROM account WHERE id = $1`, accB) != 0 {
		t.Fatal("account B still exists after clearing its records")
	}
}

// TestD30BulkMatrix: bulk void enforces the Edit grant per record (Related
// mode requires being in the related slice; void is one-way), bulk delete
// enforces the Delete grant with the same mode rule plus the settlement and
// child-transaction safety blocks, and every attempted delete lands a
// deletion_log row under one operation id. Clearing is owner/admin-only and
// leaves a tombstone.
func TestD30BulkMatrix(t *testing.T) {
	a, pool := d30App(t)
	ctx := context.Background()

	hh := d30Household(t, pool, "Bulk Matrix")
	owner := d30User(t, pool, hh, "bulkowner", "")
	d30Membership(t, pool, owner, hh, "owner")
	member := d30User(t, pool, hh, "bulkmember", "")
	d30Membership(t, pool, member, hh, "member")
	d30HHGrant(t, pool, hh, member, true, false) // can read, not an admin

	// A: edit All / delete Related. B: edit All / delete All.
	// C: edit Related / delete Related. D: no grants.
	accA := d30Account(t, pool, hh, "Bulk A", 0)
	accB := d30Account(t, pool, hh, "Bulk B", 0)
	accC := d30Account(t, pool, hh, "Bulk C", 0)
	accD := d30Account(t, pool, hh, "Bulk D", 0)
	d30Grant(t, pool, hh, member, accA, "All", "Related")
	d30Grant(t, pool, hh, member, accB, "All", "All")
	d30Grant(t, pool, hh, member, accC, "Related", "Related")

	// Party ties member to T3 via a split (the related slice counts split
	// parties, not origin_party_id).
	party := d30Party(t, pool, hh, "Bulk Party", member)
	t1 := d30Txn(t, pool, hh, accA, member, "Bulk T1") // related: recorder
	t2 := d30Txn(t, pool, hh, accA, owner, "Bulk T2")  // not related
	t3 := d30Txn(t, pool, hh, accB, owner, "Bulk T3")  // related: split party
	d30Split(t, pool, hh, t3, party)
	t4 := d30Txn(t, pool, hh, accC, owner, "Bulk T4") // not related
	t5 := d30Txn(t, pool, hh, accD, member, "Bulk T5")
	t6 := d30Txn(t, pool, hh, accB, member, "Bulk T6")
	t7 := d30ChildTxn(t, pool, hh, t6, accB, member, "Bulk T7") // child of T6

	memberSess := &auth.Session{ID: "bulk", Household: hh, UserID: member}
	ownerSess := &auth.Session{ID: "bulkown", Household: hh, UserID: owner}

	voidURLs := func(ids ...int64) string {
		t.Helper()
		form := url.Values{}
		for _, id := range ids {
			form.Add("ids[]", strconv.FormatInt(id, 10))
		}
		rec := httptest.NewRecorder()
		a.handleBulkVoid(rec, d30Req(t, ctx, memberSess, "/txns/bulk-void", 0, form))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("bulk-void: status %d body %q", rec.Code, rec.Body.String())
		}
		return rec.Header().Get("Location")
	}

	// Void: T1/T2 (edit-All on A), T3 (edit-All on B) succeed; T4
	// (edit-Related on C, not related) and T5 (no edit on D) fail.
	loc := voidURLs(t1, t2, t3, t4, t5)
	if !strings.Contains(loc, url.QueryEscape("3 record(s) successfully voided.")) ||
		!strings.Contains(loc, url.QueryEscape("2 record(s) failed to void.")) {
		t.Fatalf("void flash = %q", loc)
	}
	wantStatus := map[int64]string{t1: "void", t2: "void", t3: "void", t4: "pending", t5: "pending"}
	for id, want := range wantStatus {
		if got := d30Status(t, pool, id); got != want {
			t.Fatalf("txn %d status = %q, want %q", id, got, want)
		}
	}

	// Void is one-way: re-voiding an already-voided record fails.
	loc = voidURLs(t1)
	if !strings.Contains(loc, url.QueryEscape("1 record(s) failed to void.")) {
		t.Fatalf("re-void flash = %q", loc)
	}

	deleteURLs := func(ids ...int64) string {
		t.Helper()
		form := url.Values{}
		for _, id := range ids {
			form.Add("ids[]", strconv.FormatInt(id, 10))
		}
		rec := httptest.NewRecorder()
		a.handleBulkDelete(rec, d30Req(t, ctx, memberSess, "/txns/bulk-delete", 0, form))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("bulk-delete: status %d body %q", rec.Code, rec.Body.String())
		}
		return rec.Header().Get("Location")
	}

	// Delete: T1 (delete-Related, related) goes; T2 (delete-Related, not
	// related) fails "not a party"; T3 (delete-All on B, already voided)
	// goes; T4 fails "not a party"; T5 fails "delete grant not held";
	// T6 is blocked by its child T7; T7 (the child itself) goes.
	loc = deleteURLs(t1, t2, t3, t4, t5, t6, t7)
	if !strings.Contains(loc, url.QueryEscape("3 record(s) successfully deleted.")) ||
		!strings.Contains(loc, url.QueryEscape("4 record(s) failed to delete. See the Deletion log for details.")) {
		t.Fatalf("delete flash = %q", loc)
	}
	gone := d30Int(t, pool,
		`SELECT count(*) FROM "transaction" WHERE household_id = $1 AND id IN ($2,$3,$4)`, hh, t1, t3, t7)
	if gone != 0 {
		t.Fatalf("txns t1/t3/t7 still exist after delete: %d survivors, want 0", gone)
	}
	if left := d30Int(t, pool,
		`SELECT count(*) FROM "transaction" WHERE household_id = $1 AND id IN ($2,$3,$4,$5)`, hh, t2, t4, t5, t6); left != 4 {
		t.Fatalf("surviving txns = %d, want 4 (t2 t4 t5 t6)", left)
	}

	// Deletion log: one operation, one row per attempted record, with the
	// exact failure reasons and the snapshot name.
	rows := []struct {
		txn     int64
		outcome string
		reason  string
	}{
		{t1, "deleted", ""},
		{t2, "failed", reasonNotParty},
		{t3, "deleted", ""},
		{t4, "failed", reasonNotParty},
		{t5, "failed", reasonNoGrant},
		{t6, "failed", reasonHasChild},
		{t7, "deleted", ""},
	}
	total := d30Int(t, pool, `SELECT count(*) FROM deletion_log WHERE household_id = $1`, hh)
	if total != int64(len(rows)) {
		t.Fatalf("deletion_log rows = %d, want %d", total, len(rows))
	}
	distinctOps := d30Int(t, pool,
		`SELECT count(DISTINCT operation_id) FROM deletion_log WHERE household_id = $1`, hh)
	if distinctOps != 1 {
		t.Fatalf("distinct operations = %d, want 1", distinctOps)
	}
	for _, r := range rows {
		got := d30Str(t, pool,
			`SELECT outcome || '|' || reason FROM deletion_log
			 WHERE household_id = $1 AND txn_id = $2`, hh, r.txn)
		if want := r.outcome + "|" + r.reason; got != want {
			t.Fatalf("log for txn %d = %q, want %q", r.txn, got, want)
		}
	}
	if name := d30Str(t, pool,
		`SELECT txn_name FROM deletion_log WHERE household_id = $1 AND txn_id = $2`, hh, t5); name != "Bulk T5" {
		t.Fatalf("snapshot name = %q, want %q", name, "Bulk T5")
	}
	if actor := d30Int(t, pool,
		`SELECT actor_id FROM deletion_log WHERE household_id = $1 AND txn_id = $2`, hh, t1); actor != member {
		t.Fatalf("log actor = %d, want member %d", actor, member)
	}

	// Clear: a non-admin member is refused; the owner clears down to a
	// single tombstone row.
	rec := httptest.NewRecorder()
	a.handleDeletionLogClear(rec, d30Req(t, ctx, memberSess, "/deletion-log/clear", 0, nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("clear by member = %d, want 403", rec.Code)
	}
	if n := d30Int(t, pool, `SELECT count(*) FROM deletion_log WHERE household_id = $1`, hh); n != int64(len(rows)) {
		t.Fatalf("rows after refused clear = %d, want %d", n, len(rows))
	}
	rec = httptest.NewRecorder()
	a.handleDeletionLogClear(rec, d30Req(t, ctx, ownerSess, "/deletion-log/clear", 0, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("clear by owner = %d body %q, want 303", rec.Code, rec.Body.String())
	}
	rowsAfterClear := d30Int(t, pool, `SELECT count(*) FROM deletion_log WHERE household_id = $1`, hh)
	if rowsAfterClear != 1 {
		t.Fatalf("rows after clear = %d, want 1 (the tombstone)", rowsAfterClear)
	}
	tomb := d30Str(t, pool,
		`SELECT outcome || '|' || txn_name FROM deletion_log WHERE household_id = $1`, hh)
	if !strings.Contains(tomb, "|Log cleared by bulkowner on ") {
		t.Fatalf("tombstone = %q", tomb)
	}
	if id := d30Int(t, pool,
		`SELECT COALESCE(txn_id, -1) FROM deletion_log WHERE household_id = $1`, hh); id != -1 {
		t.Fatalf("tombstone txn_id = %d, want NULL", id)
	}
}

// TestD30OwnershipTransfer (ADR-0003): only the current owner may transfer,
// the owner's password is verified before anything changes (a wrong
// password re-renders the page and moves nothing), and a successful
// transfer swaps the roles and leaves both parties holding the full admin
// grant. A single-member household has no recipient.
func TestD30OwnershipTransfer(t *testing.T) {
	a, pool := d30App(t)
	ctx := context.Background()

	hh := d30Household(t, pool, "Transfer Hh")
	owner := d30User(t, pool, hh, "xferowner", "ownerpass123")
	d30Membership(t, pool, owner, hh, "owner")
	member := d30User(t, pool, hh, "xfermember", "memberpass123")
	d30Membership(t, pool, member, hh, "member")
	d30HHGrant(t, pool, hh, member, true, false)

	transfer := func(sess *auth.Session, recip int64, pw string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{
			"csrf":      {sess.CSRFToken},
			"recipient": {strconv.FormatInt(recip, 10)},
			"password":  {pw},
		}
		rec := httptest.NewRecorder()
		a.handleTransferOwnership(rec, d30Req(t, ctx, sess, "/household/transfer-ownership", 0, form))
		return rec
	}

	// Non-owner initiator: flat 403, nothing moves.
	memberSess := &auth.Session{ID: "xm", Household: hh, UserID: member, CSRFToken: "tokm"}
	rec := transfer(memberSess, owner, "memberpass123")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner transfer = %d, want 403", rec.Code)
	}
	if d30Role(t, pool, owner, hh) != "owner" {
		t.Fatal("refused transfer changed the owner role")
	}

	// Wrong password: the page re-renders with the error and the roles are
	// untouched.
	ownerSess := &auth.Session{ID: "xo", Household: hh, UserID: owner, CSRFToken: "toko"}
	rec = transfer(ownerSess, member, "notthepassword")
	if rec.Code != http.StatusOK {
		t.Fatalf("wrong-password transfer = %d, want 200 (re-render)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "The password was incorrect") {
		t.Fatal("wrong-password re-render missing the error message")
	}
	if d30Role(t, pool, owner, hh) != "owner" || d30Role(t, pool, member, hh) != "member" {
		t.Fatal("wrong-password transfer changed the roles")
	}

	// Correct password: the swap, and both end up with the full admin grant.
	rec = transfer(ownerSess, member, "ownerpass123")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("transfer = %d body %q, want 303", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, url.QueryEscape("Ownership transferred: ")) {
		t.Fatalf("transfer flash = %q", loc)
	}
	if got := d30Role(t, pool, member, hh); got != "owner" {
		t.Fatalf("recipient role = %q, want owner", got)
	}
	if got := d30Role(t, pool, owner, hh); got != "member" {
		t.Fatalf("old owner role = %q, want member", got)
	}
	grants := d30Int(t, pool,
		`SELECT count(*) FROM household_grant
		 WHERE household_id = $1 AND is_admin AND can_read AND can_create AND can_edit
		   AND can_delete AND edit_mode = 'all' AND delete_mode = 'all'`, hh)
	if grants != 2 {
		t.Fatalf("full admin grant rows = %d, want 2 (both parties)", grants)
	}

	// Single-member household: the section has no recipient.
	hh2 := d30Household(t, pool, "Transfer Solo")
	solo := d30User(t, pool, hh2, "xfersolo", "")
	d30Membership(t, pool, solo, hh2, "owner")
	isOwner, recips, err := a.ownershipSection(ctx, hh2, solo)
	if err != nil {
		t.Fatalf("ownershipSection: %v", err)
	}
	if !isOwner || len(recips) != 0 {
		t.Fatalf("solo section: isOwner=%v recips=%+v (want true, none)", isOwner, recips)
	}
}
