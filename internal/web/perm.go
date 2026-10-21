// Package web — Gate 5: two-tier grant enforcement (ADR-0001, D27).
//
// This file is the permission brain: it loads the caller's two grant tiers
// (household-level over account objects, account-level over records) once
// per request and exposes the small decision set the handlers need.
//
//   household_grant: may the user read/create/edit/delete accounts AS
//     OBJECTS. Edit/delete are scoped by a created|all mode (a member's
//     reach is limited to accounts they created; owner/admins operate at
//     all).
//
//   account_grant: may the user read/create/edit/delete RECORDS on one
//     account. View/edit/delete are each Related|All. Related is the
//     member's slice (a transaction they recorded, are a split party in,
//     or paid as settlement payer); All is every record.
//
// The owner has implicit full access and is untouchable; admins are
// interchangeable below the owner (they manage every non-owner's grants).
// A plain member defaults to read-only.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// acctGrant is one account-level grant row (records on one account).
type acctGrant struct {
	accountID int64
	read      bool
	create    bool
	edit      bool
	del       bool
	viewMode  string // "Related" | "All"
	editMode  string // "Related" | "All"
	delMode   string // "Related" | "All"
}

// hGrant is the household-level grant (accounts as objects).
type hGrant struct {
	read     bool
	create   bool
	edit     bool
	del      bool
	editMode string // "created" | "all"
	delMode  string // "created" | "all"
	admin    bool
}

// perms is the fully-loaded permission set for one request.
type perms struct {
	userID   int64
	hh       int64
	owner    bool
	h        hGrant
	acct     map[int64]acctGrant // account_id -> grant (only accounts the user has a grant on)
}

// isOwner reports implicit full access (no grant rows are consulted).
func (p *perms) isOwner() bool { return p.owner }

// ---------------------------------------------------------------------------
// Load + context plumbing
// ---------------------------------------------------------------------------

// permCtxKey keys the loaded permission set on the request context.
type permCtxKey struct{}

// permLoad reads both grant tiers for the caller.
func (a *App) permLoad(ctx context.Context, hh, user int64) (*perms, error) {
	owner := false
	var role string
	err := a.Pool.QueryRow(ctx,
		`SELECT role FROM household_member WHERE user_id = $1 AND household_id = $2`,
		user, hh).Scan(&role)
	if err == nil && role == "owner" {
		owner = true
	}

	p := &perms{userID: user, hh: hh, owner: owner, acct: map[int64]acctGrant{}}

	if owner {
		// Implicit full: everything is All. Represent with a household
		// grant carrying all rights; account reads are handled in
		// anyAll/related which short-circuit on isOwner.
		p.h = hGrant{read: true, create: true, edit: true, del: true, editMode: "all", delMode: "all", admin: true}
		return p, nil
	}

	var (
		h        hGrant
		hRead    bool
		hCreate  bool
		hEdit    bool
		hDel     bool
		hEditM   string
		hDelM    string
		hAdmin   bool
		found    bool
	)
	err = a.Pool.QueryRow(ctx, `
		SELECT can_read, can_create, can_edit, can_delete,
		       edit_mode, delete_mode, is_admin
		FROM household_grant WHERE user_id = $1 AND household_id = $2`,
		user, hh).Scan(&hRead, &hCreate, &hEdit, &hDel, &hEditM, &hDelM, &hAdmin)
	if err == nil {
		found = true
		h = hGrant{read: hRead, create: hCreate, edit: hEdit, del: hDel, editMode: hEditM, delMode: hDelM, admin: hAdmin}
	}
	if !found {
		h = hGrant{read: false, create: false, edit: false, del: false, editMode: "created", delMode: "created", admin: false}
	}
	p.h = h

	rows, err := a.Pool.Query(ctx, `
		SELECT account_id, can_read, can_create, can_edit, can_delete,
		       view_mode, edit_mode, delete_mode
		FROM account_grant WHERE user_id = $1 AND household_id = $2`, user, hh)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g acctGrant
		if err := rows.Scan(&g.accountID, &g.read, &g.create, &g.edit, &g.del,
			&g.viewMode, &g.editMode, &g.delMode); err != nil {
			return nil, err
		}
		p.acct[g.accountID] = g
	}
	return p, rows.Err()
}

// permFor loads (or reuses) the caller's permission set from the request
// context. Handlers call this once near the top.
func (a *App) permFor(r *http.Request) (*perms, error) {
	if p, ok := r.Context().Value(permCtxKey{}).(*perms); ok {
		return p, nil
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		return nil, errors.New("no session")
	}
	p, err := a.permLoad(r.Context(), sess.Household, sess.UserID)
	if err != nil {
		return nil, err
	}
	r = r.WithContext(context.WithValue(r.Context(), permCtxKey{}, p))
	return p, nil
}

// ---------------------------------------------------------------------------
// Household-level (account object) decisions
// ---------------------------------------------------------------------------

// permCanHRead reports whether the user may see account objects at all.
func permCanHRead(p *perms) bool { return p.isOwner() || p.h.read }

// permCreatedFilter returns the created-by predicate fragment for a
// household-level created|all mode, plus whether the mode narrows at all.
// (owner/admin = all -> narrow=false; member created -> narrow=true with a
// created_by check.)
func permModeAll(p *perms, mode string) bool {
	if p.isOwner() {
		return true
	}
	return mode == "all"
}

// canEditAccountObject: household-level edit on one account (object).
func (p *perms) canEditAccountObject(accountID, createdBy int64) bool {
	if p.isOwner() {
		return true
	}
	if !p.h.edit {
		return false
	}
	if p.h.editMode == "all" {
		return true
	}
	// created mode: only accounts they created.
	return createdBy == p.userID
}

// canDeleteAccountObject: household-level delete on one account (object).
func (p *perms) canDeleteAccountObject(accountID, createdBy int64) bool {
	if p.isOwner() {
		return true
	}
	if !p.h.del {
		return false
	}
	if p.h.delMode == "all" {
		return true
	}
	return createdBy == p.userID
}

// canCreateAccount: household-level create.
func (p *perms) canCreateAccount() bool {
	return p.isOwner() || p.h.create
}

// ---------------------------------------------------------------------------
// Account-level (record) decisions
// ---------------------------------------------------------------------------

// anyAllView reports whether the user has an All view grant on ANY of the
// given accounts (owner: always true). An empty set is false.
func (p *perms) anyAllView(ids []int64) bool {
	if p.isOwner() {
		return len(ids) > 0
	}
	for _, id := range ids {
		if g, ok := p.acct[id]; ok && g.read && g.viewMode == "All" {
			return true
		}
	}
	return false
}

// canReadRecords reports whether the user has a read grant on a specific
// account (records live only under an account grant). Owner: always.
func (p *perms) canReadRecords(accountID int64) bool {
	if p.isOwner() {
		return true
	}
	g, ok := p.acct[accountID]
	return ok && g.read
}

// canCreateOn: create grant on a specific account.
func (p *perms) canCreateOn(accountID int64) bool {
	if p.isOwner() {
		return true
	}
	g, ok := p.acct[accountID]
	return ok && g.create
}

// canEditRecordMode: the user's edit grant + mode on a specific account.
// Returns (canEdit, mode) where mode is "Related" or "All".
func (p *perms) canEditRecordMode(accountID int64) (bool, string) {
	if p.isOwner() {
		return true, "All"
	}
	g, ok := p.acct[accountID]
	if !ok || !g.edit {
		return false, "Related"
	}
	return true, g.editMode
}

// canDeleteRecordMode: the user's delete grant + mode on a specific account.
func (p *perms) canDeleteRecordMode(accountID int64) (bool, string) {
	if p.isOwner() {
		return true, "All"
	}
	g, ok := p.acct[accountID]
	if !ok || !g.del {
		return false, "Related"
	}
	return true, g.delMode
}

// canAllEditOn: edit grant with All mode on a specific account.
func (p *perms) canAllEditOn(accountID int64) bool {
	if p.isOwner() {
		return true
	}
	g, ok := p.acct[accountID]
	return ok && g.edit && g.editMode == "All"
}

// canAllDeleteOn: delete grant with All mode on a specific account.
func (p *perms) canAllDeleteOn(accountID int64) bool {
	if p.isOwner() {
		return true
	}
	g, ok := p.acct[accountID]
	return ok && g.del && g.delMode == "All"
}

// ---------------------------------------------------------------------------
// SQL fragment builders
// ---------------------------------------------------------------------------

// accountReadCond builds an AND-joinable predicate that restricts a query to
// rows touching an account the user has a read grant on. $1 = user, $2 =
// household. For a single account column it yields:
//
//	EXISTS (SELECT 1 FROM account_grant ag
//	        WHERE ag.user_id=$1 AND ag.household_id=$2 AND ag.can_read
//	          AND ag.account_id = <expr>)
func accountReadCond(userCol string) string {
	return fmt.Sprintf(
		"EXISTS (SELECT 1 FROM account_grant ag WHERE ag.user_id = $1 AND ag.household_id = $2 AND ag.can_read AND ag.account_id = %s)",
		userCol)
}

// txnVisibleWhere builds the WHERE fragment (beyond household_id) that
// restricts a "transaction" query (alias `t`) to records the user may SEE:
//   - an account-grant read on one of the endpoints (the surface), AND
//   - view mode: an All-view grant on any endpoint short-circuits; otherwise
//     the user's related slice (recorded / split party / settlement payer).
//
// Placeholders: $uN = user id, $hN = household id. The caller appends the
// user id to its args list at position uN. Owner: "TRUE".
func txnVisibleWhere(p *perms, uN, hN int) string {
	return txnVisibleWhereAliased(p, uN, hN, "t")
}

// txnVisibleWhereAliased is txnVisibleWhere for a transaction alias other
// than `t` (e.g. dashboard aggregate subqueries alias it `a`). The fragment
// always consumes its $uN (user) placeholder so callers can bind args
// uniformly — pgx rejects queries with more bound args than placeholders.
func txnVisibleWhereAliased(p *perms, uN, hN int, alias string) string {
	if p.isOwner() {
		// Owner: every record is visible, but keep the user placeholder
		// consumed (a tautology) so arg counts stay uniform.
		return fmt.Sprintf("TRUE AND ($%d::bigint IS NOT NULL)", uN)
	}
	u, h := fmt.Sprintf("$%d", uN), fmt.Sprintf("$%d", hN)
	endpoints := fmt.Sprintf(
		"(COALESCE(%s.origin_account_id, -1), COALESCE(%s.destination_account_id, -1))", alias, alias)
	allView := fmt.Sprintf(
		"EXISTS (SELECT 1 FROM account_grant ag WHERE ag.user_id = %s AND ag.household_id = %s AND ag.can_read AND ag.view_mode = 'All' AND ag.account_id IN %s)",
		u, h, endpoints)
	readSurface := fmt.Sprintf(
		"EXISTS (SELECT 1 FROM account_grant ag WHERE ag.user_id = %s AND ag.household_id = %s AND ag.can_read AND ag.account_id IN %s)",
		u, h, endpoints)
	slice := relatedSliceSQLAliased(uN, alias)
	return fmt.Sprintf("(%s OR (%s AND %s))", allView, readSurface, slice)
}

// relatedSliceSQL is the boolean expression TRUE when the user is "related"
// to a transaction t: they recorded it, are a split party in it, or paid a
// settlement allocated to one of its splits. uN/hN are the user/household
// placeholder numbers ($uN = user id).
func relatedSliceSQL(uN int) string {
	return relatedSliceSQLAliased(uN, "t")
}

// relatedSliceSQLAliased is relatedSliceSQL for a transaction alias other
// than `t`.
func relatedSliceSQLAliased(uN int, alias string) string {
	const tpl = `(
		COALESCE(@A.recorded_by_user_id, 0) = @U
		OR EXISTS (SELECT 1 FROM transaction_split s
			JOIN party p ON p.id = s.party_id
			WHERE s.transaction_id = @A.id AND p.app_user_id = @U)
		OR EXISTS (SELECT 1 FROM settlement_allocation pa
			JOIN settlement st ON st.id = pa.payment_id
			JOIN transaction_split s ON s.id = pa.split_id
			WHERE s.transaction_id = @A.id AND st.paid_by_user_id = @U))`
	out := strings.ReplaceAll(tpl, "@A", alias)
	out = strings.ReplaceAll(out, "@U", "$"+strconv.Itoa(uN))
	return out
}

// txnWriteSurface returns the account endpoints of a transaction spec (the
// accounts a new/edited txn would touch). Party endpoints contribute nothing.
func txnWriteSurface(originType string, originRef int64, destType string, destRef int64) []int64 {
	var ids []int64
	if originType == "account" && originRef > 0 {
		ids = append(ids, originRef)
	}
	if destType == "account" && destRef > 0 {
		ids = append(ids, destRef)
	}
	return ids
}

// canCreateTxn reports whether the user may create a transaction touching
// exactly the given account surface: a create grant on every account in the
// set. Owner: always (an empty set is fine).
func (p *perms) canCreateTxn(surface []int64) bool {
	if p.isOwner() {
		return true
	}
	if len(surface) == 0 {
		return true // party-only move: no ledger surface to gate
	}
	for _, id := range surface {
		g, ok := p.acct[id]
		if !ok || !g.create {
			return false
		}
	}
	return true
}

// canEditTxn reports whether the user may edit a transaction with the given
// account surface: an edit grant on every account, with the mode checked
// against "related". Owner: always. A Related mode requires the user to be in
// the slice (the caller passes inSlice=true when they are).
func (p *perms) canEditTxn(surface []int64, inSlice bool) bool {
	if p.isOwner() {
		return true
	}
	if len(surface) == 0 {
		return inSlice // party-only edit: allowed if related
	}
	for _, id := range surface {
		g, ok := p.acct[id]
		if !ok || !g.edit {
			return false
		}
		if g.editMode == "Related" && !inSlice {
			return false
		}
	}
	return true
}

// canDeleteTxn reports whether the user may delete/void a transaction with
// the given account surface: a delete grant on every account, mode-checked.
// Owner: always.
func (p *perms) canDeleteTxn(surface []int64, inSlice bool) bool {
	if p.isOwner() {
		return true
	}
	if len(surface) == 0 {
		return inSlice
	}
	for _, id := range surface {
		g, ok := p.acct[id]
		if !ok || !g.del {
			return false
		}
		if g.delMode == "Related" && !inSlice {
			return false
		}
	}
	return true
}

// canCreateRecurring reports whether the user may create a recurring template
// landing on accountID (its single endpoint account). Owner: always.
func (p *perms) canCreateRecurring(accountID int64) bool {
	return p.canCreateOn(accountID)
}

// canEditRecurring reports whether the user may edit a recurring template
// landing on accountID and (if inSlice is true) in which they are a split
// party. Owner: always.
func (p *perms) canEditRecurring(accountID int64, inSlice bool) bool {
	if p.isOwner() {
		return true
	}
	g, ok := p.acct[accountID]
	if !ok || !g.edit {
		return false
	}
	if g.editMode == "Related" && !inSlice {
		return false
	}
	return true
}

// canDeleteRecurring is the R5 author-scoped rule: only the template's
// creator may delete it (owner: always). A split party may see/edit (with a
// grant) but NOT delete.
func (p *perms) canDeleteRecurring(creatorID int64) bool {
	if p.isOwner() {
		return true
	}
	return creatorID == p.userID
}

// isSplitParty reports whether the user is named in a split_spec key list
// ("user:N" entries match the user id).
func isSplitParty(user int64, keys []string) bool {
	for _, k := range keys {
		typ, id, err := parseParticipantKey(k)
		if err != nil {
			continue
		}
		if typ == "household_user" && id == user {
			return true
		}
	}
	return false
}

// isRelatedToTxn reports whether the user is in the related slice of an
// existing transaction: they recorded it, are a split party in it, or paid a
// settlement allocated to one of its splits.
func (a *App) isRelatedToTxn(ctx context.Context, hh, user, txnID int64) (bool, error) {
	var n int
	err := a.Pool.QueryRow(ctx, `
		SELECT count(*) FROM "transaction" t
		WHERE t.id = $1 AND t.household_id = $2 AND (`+relatedSliceSQL(3)+`)`,
		txnID, hh, user).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// anyReadOn reports whether the user has a read grant on at least one of the
// given accounts (the account-surface precondition for seeing records).
// Owner: always (an empty set is false).
func (p *perms) anyReadOn(ids []int64) bool {
	if p.isOwner() {
		return len(ids) > 0
	}
	for _, id := range ids {
		if g, ok := p.acct[id]; ok && g.read {
			return true
		}
	}
	return false
}

// canSeeTxn mirrors txnVisibleWhere in Go: the user may see a transaction if
// they have an All-view grant on any surface account, OR a read grant on the
// surface AND are in the related slice. Owner: always.
func (p *perms) canSeeTxn(surface []int64, inSlice bool) bool {
	if p.isOwner() {
		return true
	}
	if p.anyAllView(surface) {
		return true
	}
	return p.anyReadOn(surface) && inSlice
}

// txnAccess loads a transaction's account surface and whether the user is in
// its related slice. Used by the single-record read/write handlers.
func (a *App) txnAccess(ctx context.Context, hh, user, txnID int64) ([]int64, bool, error) {
	var oAcc, dAcc int64
	err := a.Pool.QueryRow(ctx,
		`SELECT COALESCE(origin_account_id, 0), COALESCE(destination_account_id, 0)
		 FROM "transaction" WHERE id = $1 AND household_id = $2`, txnID, hh).Scan(&oAcc, &dAcc)
	if err != nil {
		return nil, false, err
	}
	var surface []int64
	if oAcc > 0 {
		surface = append(surface, oAcc)
	}
	if dAcc > 0 {
		surface = append(surface, dAcc)
	}
	rel, err := a.isRelatedToTxn(ctx, hh, user, txnID)
	return surface, rel, err
}

// familyRoot returns the card-family root id for account id (D19). For a
// child, the parent; for a parent/standalone, itself. Non-card accounts
// return themselves.
func (a *App) familyRoot(ctx context.Context, hh, id int64) (int64, error) {
	var root int64
	err := a.Pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT p.id FROM account c
		               JOIN account p ON p.id = c.parent_id
		               WHERE c.id = $1 AND c.household_id = $2), $1)`, id, hh).Scan(&root)
	return root, err
}

// settlementSurface returns the account ids that define a settlement's
// permission surface (anchor rule, Q7): the settlement's own account PLUS the
// whole card family behind it, plus every account family behind its allocated
// splits. Roots and members are both included because grants live on the
// member accounts. The D6 source asset account is NOT included (it counts
// only for create-with-transfer).
func (a *App) settlementSurface(ctx context.Context, hh, settlementID int64) ([]int64, error) {
	// Family member ids for account id: self + active parent/children (D19).
	familyOf := func(id int64) []int64 {
		rows, err := a.Pool.Query(ctx, `
			SELECT id FROM account
			WHERE household_id = $1 AND account_type = 'credit_card'
			  AND (id = $2 OR parent_id = $2)
			ORDER BY id`, hh, id)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var x int64
			if err := rows.Scan(&x); err != nil {
				return nil
			}
			out = append(out, x)
		}
		return out
	}
	seen := map[int64]bool{}
	var out []int64
	add := func(x int64) {
		if x > 0 && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	roots := map[int64]bool{}
	addAccount := func(acc int64) error {
		if acc == 0 {
			return nil
		}
		root, err := a.familyRoot(ctx, hh, acc)
		if err != nil {
			return err
		}
		if roots[root] {
			return nil
		}
		roots[root] = true
		add(acc)
		for _, m := range familyOf(acc) {
			add(m)
		}
		return nil
	}
	// The settlement's own account (+ its family).
	var acc int64
	err := a.Pool.QueryRow(ctx, `
		SELECT COALESCE(account_id, 0) FROM settlement
		WHERE id = $1 AND household_id = $2`, settlementID, hh).Scan(&acc)
	if err != nil {
		return nil, err
	}
	if err := addAccount(acc); err != nil {
		return nil, err
	}
	// Allocated splits' transaction account families.
	rows, err := a.Pool.Query(ctx, `
		SELECT DISTINCT t.origin_account_id
		FROM settlement_allocation pa
		JOIN settlement st ON st.id = pa.payment_id
		JOIN transaction_split s ON s.id = pa.split_id
		JOIN "transaction" t ON t.id = s.transaction_id
		WHERE st.id = $1 AND st.household_id = $2`, settlementID, hh)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var aAcc int64
		if err := rows.Scan(&aAcc); err != nil {
			rows.Close()
			return nil, err
		}
		if err := addAccount(aAcc); err != nil {
			rows.Close()
			return nil, err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// canSeeSettlement reports whether the user may read/delete the settlement:
// a read grant on the settlement surface (All short-circuits; otherwise the
// user must be the payer or a split party in one of the allocated splits).
func (a *App) canSeeSettlement(ctx context.Context, p *perms, settlementID int64) (bool, error) {
	if p.isOwner() {
		return true, nil
	}
	surf, err := a.settlementSurface(ctx, p.hh, settlementID)
	if err != nil {
		return false, err
	}
	// Any All-view on the surface -> visible.
	if p.anyAllView(surf) {
		return true, nil
	}
	// Otherwise the related slice: payer of this settlement, or a split
	// party in one of the allocated splits.
	var payer int64
	err = a.Pool.QueryRow(ctx, `
		SELECT COALESCE(paid_by_user_id, 0) FROM settlement
		WHERE id = $1 AND household_id = $2`, settlementID, p.hh).Scan(&payer)
	if err != nil {
		return false, err
	}
	if payer == p.userID {
		return true, nil
	}
	var split int
	err = a.Pool.QueryRow(ctx, `
		SELECT count(*) FROM settlement_allocation pa
		JOIN settlement st ON st.id = pa.payment_id
		JOIN transaction_split s ON s.id = pa.split_id
		JOIN party pt ON pt.id = s.party_id
		WHERE st.id = $1 AND st.household_id = $2 AND pt.app_user_id = $3`,
		settlementID, p.hh, p.userID).Scan(&split)
	if err != nil {
		return false, err
	}
	return split > 0, nil
}

// cardFamilyIDs returns the member accounts of a card family (self + active
// children) for access checks.
func (a *App) cardFamilyIDs(ctx context.Context, hh, cardID int64) ([]int64, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT id FROM account
		WHERE household_id = $1 AND account_type = 'credit_card'
		  AND (id = $2 OR parent_id = $2)
		ORDER BY id`, hh, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// canReadCardFamily reports whether the user has a read grant on at least
// one member of the card family (the family is shown partially/hidden
// otherwise). Owner: always.
func (p *perms) canReadCardFamily(ids []int64) bool {
	if p.isOwner() {
		return len(ids) > 0
	}
	for _, id := range ids {
		if g, ok := p.acct[id]; ok && g.read {
			return true
		}
	}
	return false
}

// canCreateCardFamily reports whether the user has a create grant on every
// member of the card family (needed to post a settlement onto it). Owner:
// always. An empty family is false.
func (p *perms) canCreateCardFamily(ids []int64) bool {
	if p.isOwner() {
		return len(ids) > 0
	}
	for _, id := range ids {
		g, ok := p.acct[id]
		if !ok || !g.create {
			return false
		}
	}
	return true
}

// settlementCreateSurface returns the account surface a settlement create
// touches (anchor rule + D6): the settlement's own card family plus, when a
// ledger transfer is written, the source account.
func settlementCreateSurface(cardFam []int64, ledger bool, sourceID int64) []int64 {
	ids := append([]int64{}, cardFam...)
	if ledger && sourceID > 0 {
		ids = append(ids, sourceID)
	}
	return ids
}

// canCreateSettlement reports whether the user may record a settlement on
// the card family, with an optional D6 transfer from sourceID (create
// grants on the whole surface, all-involvement). Owner: always.
func (p *perms) canCreateSettlement(surface []int64) bool {
	return p.canCreateTxn(surface)
}

// canDeleteSettlementFamily reports whether the user has any delete grant on
// the settlement's card family (a prerequisite for deleting; the Related vs
// All mode + slice is enforced by canSeeSettlement / canAllDeleteOn). Owner:
// always.
func (p *perms) canDeleteSettlementFamily(ids []int64) bool {
	if p.isOwner() {
		return true
	}
	for _, id := range ids {
		if g, ok := p.acct[id]; ok && g.del {
			return true
		}
	}
	return false
}

// visibleFamily returns the members of a card family the caller has a read
// grant on (owner: all). Card-family pages render PARTIALLY over this subset
// and are HIDDEN (empty / 403) when it is empty — ADR-0001, "partial/hidden".
func (a *App) visibleFamily(p *perms, fam []int64) []int64 {
	if p.isOwner() {
		return fam
	}
	var vis []int64
	for _, id := range fam {
		if g, ok := p.acct[id]; ok && g.read {
			vis = append(vis, id)
		}
	}
	return vis
}

// canAllDeleteOnFamily reports whether the user has an All-mode delete grant
// on any member of the family (clears the Related-mode slice check). Owner:
// always.
func (p *perms) canAllDeleteOnFamily(ids []int64) bool {
	if p.isOwner() {
		return true
	}
	for _, id := range ids {
		if g, ok := p.acct[id]; ok && g.del && g.delMode == "All" {
			return true
		}
	}
	return false
}

// anyEditOnFamily reports whether the user has an edit grant (any mode) on at
// least one member of the family — the write precondition for family-scope
// records like a reconciliation row. Owner: always.
func (p *perms) anyEditOnFamily(ids []int64) bool {
	if p.isOwner() {
		return len(ids) > 0
	}
	for _, id := range ids {
		if g, ok := p.acct[id]; ok && g.edit {
			return true
		}
	}
	return false
}

// strictAccountHasTiedRecords reports whether an account still has any ledger
// records (a hard delete is only legal when this is false). Used by the
// strict-delete rule: an account is deletable only with zero tied records.
func (a *App) strictAccountHasTiedRecords(ctx context.Context, hh, accountID int64) (bool, error) {
	var n int
	err := a.Pool.QueryRow(ctx, `
		SELECT count(*) FROM "transaction" t
		WHERE t.household_id = $1 AND t.record_status <> 'void'
		  AND (t.origin_account_id = $2 OR t.destination_account_id = $2)`, hh, accountID).Scan(&n)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	var n2 int
	err = a.Pool.QueryRow(ctx, `
		SELECT count(*) FROM settlement s WHERE s.household_id = $1 AND s.account_id = $2`, hh, accountID).Scan(&n2)
	if err != nil {
		return false, err
	}
	if n2 > 0 {
		return true, nil
	}
	var n3 int
	err = a.Pool.QueryRow(ctx, `
		SELECT count(*) FROM account c WHERE c.household_id = $1 AND c.parent_id = $2`, hh, accountID).Scan(&n3)
	if err != nil {
		return false, err
	}
	return n3 > 0, nil
}

// permForTest constructs a perms without a DB (unit coverage of the decision
// functions).
func permForTest(userID, hh int64, owner bool, h hGrant, acct map[int64]acctGrant) *perms {
	return &perms{userID: userID, hh: hh, owner: owner, h: h, acct: acct}
}
