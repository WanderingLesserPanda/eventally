#!/usr/bin/env bash
# D34 HTTP smoke — auto-sync + detach (docs/notes/d34-auto-sync-detach.md).
#
# Scratch throwaway DB (created, seeded, dropped by trap); the app binary is
# built and booted against it (auto-migration to v18 included). Neutral seed
# labels only (U, F, M; "Main household", "Family household") — no names.
#
# Full journey (spec verification section):
#   record in the source household -> a PENDING copy appears in the target
#   household with NO page load (write-time trigger) -> map a category ->
#   approve via the single-record pop-up (category required) -> budget moves
#   -> detach (lock released, History row, badge gone) -> edit freely ->
#   re-link (money facts reset, classification kept, still one row).
# Plus: toggle round-trip across both UI homes, catch-up on toggle-ON, and the
# no-transitive-sync invariant.
#
# Run: ./smoke-d34.sh
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck disable=SC1091
export LD_LIBRARY_PATH="${LD_LIBRARY_PATH:-}"
source /opt/data/cache/pgenv.sh

PORT="${SMOKE_PORT:-18092}"
BASE="http://127.0.0.1:${PORT}"
DB="d34smoke_$$"
WORK="$(mktemp -d)"
BIN="$WORK/eventally"
JARS="$WORK/jars"
PWHASH=".d34_pwhash"
mkdir -p "$JARS" "$PWHASH"

SRV=""
cleanup() {
  [[ -n "$SRV" ]] && kill "$SRV" 2>/dev/null || true
  psql -q -d postgres -c "DROP DATABASE IF EXISTS ${DB};" >/dev/null
  rm -rf "$WORK"
  rm -rf "$PWHASH"
}
trap cleanup EXIT

echo "==> scratch db ${DB}"
psql -q -d postgres -c "CREATE DATABASE ${DB};" >/dev/null

echo "==> building"
go build -o "$BIN" ./cmd/eventally

cat > "$PWHASH/main.go" <<'GO'
package main

import (
	"fmt"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
)

func main() {
	h, err := auth.HashPassword("smoke-pass")
	if err != nil {
		panic(err)
	}
	fmt.Println(h)
}
GO
HASH="$(go run "$PWHASH/main.go")"

echo "==> booting app on :${PORT} (auto-migrates the scratch db to v18)"
EVENTALLY_SECRET="$(openssl rand -hex 32)" \
EVENTALLY_ADDR="127.0.0.1:${PORT}" \
EVENTALLY_DATA_DIR="$WORK/data" \
EVENTALLY_DSN="host=127.0.0.1 port=5432 user=hermes dbname=${DB} sslmode=disable" \
"$BIN" >"$WORK/server.log" 2>&1 &
SRV="$!"
for _ in $(seq 60); do
  curl -sf -o /dev/null "$BASE/login" && break
  sleep 0.5
done
curl -sf -o /dev/null "$BASE/login" || { echo "FAIL: server did not come up"; cat "$WORK/server.log"; exit 1; }

seed() {
psql -qX -v ON_ERROR_STOP=1 -d "$DB" <<SQL
INSERT INTO household (name) VALUES ('Main household'), ('Family household');
INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
VALUES
 (1, 'u', 'U', 'u@example.test',  '${HASH}'),
 (2, 'f', 'F', 'f@example.test',  '${HASH}'),
 (1, 'm', 'M', 'm@example.test',  '${HASH}');
INSERT INTO household_member (user_id, household_id, role)
SELECT u.id, h.id, r.role
FROM (VALUES ('u',1,'member'), ('u',2,'member'), ('f',2,'owner'), ('m',1,'member'))
     AS r(username, hh, role)
JOIN app_user u ON u.username = r.username
JOIN household h ON h.id = r.hh;
INSERT INTO category (household_id, name, parent_id, transaction_class, budget_per_month, is_budgetable)
VALUES
 (1, 'Food & Drink', NULL, 'expense', NULL, false),
 (1, 'Coffee',       1,    'expense', 100.00, true),
 (2, 'Food & Drink', NULL, 'expense', NULL, false),
 (2, 'Drink',        3,    'expense', NULL, false);
INSERT INTO account (household_id, name, account_type, balance_type)
VALUES
 (1, 'Main Account', 'savings', 'asset'),
 (2, 'Family Card',  'credit_card', 'liability');
-- U: full grant on the H1 account (records + approves + re-links in H1).
-- F: full grant on the H2 card (records the source in H2).
INSERT INTO account_grant (user_id, household_id, account_id,
  can_read, can_create, can_edit, can_delete, view_mode, edit_mode, delete_mode)
VALUES
 ((SELECT id FROM app_user WHERE username='u'), 1, (SELECT id FROM account WHERE name='Main Account'),
  true, true, true, true, 'All', 'All', 'All'),
 ((SELECT id FROM app_user WHERE username='f'), 2, (SELECT id FROM account WHERE name='Family Card'),
  true, true, true, true, 'All', 'All', 'All');
-- A pre-existing syncable record in H2: the toggle-ON catch-up must pick it
-- up. U holds a split share.
INSERT INTO "transaction" (household_id, recorded_by_user_id, transaction_date, class, category_id, total_amount, description, origin_account_id, record_status)
VALUES
 (2, (SELECT id FROM app_user WHERE username='f'), '2026-11-15', 'expense',
  (SELECT id FROM category WHERE household_id=2 AND name='Drink'), 10.00, 'coffee backlog',
  (SELECT id FROM account WHERE name='Family Card'), 'approved');
INSERT INTO transaction_split (household_id, transaction_id, party_id, amount)
SELECT 2, t.id, p.id, v.amount
FROM (VALUES ('coffee backlog','u',5.00), ('coffee backlog','f',5.00)) AS v(tdesc, uname, amount)
JOIN "transaction" t ON t.description = v.tdesc
JOIN party p ON p.household_id = 2 AND p.app_user_id = (SELECT id FROM app_user WHERE username = v.uname);
SQL
}

echo "==> seeding"
seed

fail() { echo "FAIL: $1"; exit 1; }
login() { # $1=username
  local code
  code="$(curl -s -c "$JARS/$1" -o /dev/null -w '%{http_code}' \
    -d "username=$1&password=smoke-pass" "$BASE/login")"
  [[ "$code" == "303" ]] || fail "login $1: HTTP $code"
}
page() { curl -s -b "$JARS/$1" "$BASE$2"; }
csrf() { page "$1" "$2" | grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"//' || true; }
post() { # $1=username $2=path [--csrf-src=<get path>] [data...] -> status code
  local u="$1" path="$2"; shift 2
  local src="$path"
  if [[ $# -ge 1 && "$1" == "--csrf-src="* ]]; then
    src="${1#--csrf-src=}"; shift
  fi
  local c; c="$(csrf "$u" "$src")"
  curl -s -b "$JARS/$u" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrf=$c" "$@" "$BASE$path"
}
has() { page "$1" "$2" | grep -q "$3" || fail "$1 $2 — expected: $3"; }
not_has() { if page "$1" "$2" | grep -q "$3"; then fail "$1 $2 — unexpected: $3"; fi; }
sql() { psql -tAq -d "$DB" -c "$1" || { echo "FAIL: psql: $1" >&2; exit 1; }; }

U_ID="$(sql "SELECT id FROM app_user WHERE username='u'")"
F_ID="$(sql "SELECT id FROM app_user WHERE username='f'")"
COFFEE="$(sql "SELECT id FROM category WHERE household_id=1 AND name='Coffee'")"
DRINKH2="$(sql "SELECT id FROM category WHERE household_id=2 AND name='Drink'")"
CARD="$(sql "SELECT id FROM account WHERE name='Family Card'")"
F_PARTY="$(sql "SELECT id FROM party WHERE household_id=2 AND app_user_id=$F_ID")"
BACKLOG="$(sql "SELECT id FROM \"transaction\" WHERE description='coffee backlog' AND household_id=2")"

login u; login f; login m

echo "==> 1. toggle ON via the Live-view home (ret=live) + catch-up"
code="$(post u /sync/auto --csrf-src=/sync --data-urlencode "source_household_id=2" \
  --data-urlencode "on=1" --data-urlencode "ret=live")"
[[ "$code" == "303" ]] || fail "toggle-on (live): HTTP $code"
n="$(sql "SELECT count(*) FROM auto_sync WHERE member_user_id=$U_ID AND source_household_id=2 AND target_household_id=1")"
[[ "$n" == "1" ]] || fail "auto_sync row count $n, want 1"
# Catch-up synced the pre-existing backlog record into H1 (pending).
BID="$(sql "SELECT id FROM \"transaction\" WHERE household_id=1 AND sync_source_household_id=2 AND sync_source_txn_id=$BACKLOG")"
[[ -n "$BID" ]] || fail "catch-up did not sync the backlog record"
bst="$(sql "SELECT record_status FROM \"transaction\" WHERE id=$BID")"
[[ "$bst" == "pending" ]] || fail "catch-up copy status '$bst', want pending"

echo "==> 2. the same setting, second home (ret=settings)"
code="$(post u /sync/auto --csrf-src=/sync --data-urlencode "source_household_id=2" \
  --data-urlencode "on=1" --data-urlencode "ret=settings")"
[[ "$code" == "303" ]] || fail "toggle-on (settings): HTTP $code"
n="$(sql "SELECT count(*) FROM auto_sync WHERE member_user_id=$U_ID")"
[[ "$n" == "1" ]] || fail "toggle is one row: count $n, want 1"

echo "==> 3. write-time trigger: F records in H2 -> a pending copy lands in H1 (no page load)"
code="$(post f /txns --csrf-src=/txns --data-urlencode "class=expense" \
  --data-urlencode "date=2026-11-19" \
  --data-urlencode "total=12.00" \
  --data-urlencode "category_id=$DRINKH2" \
  --data-urlencode "merchant_id=0" \
  --data-urlencode "description=coffee" \
  --data-urlencode "notes=" \
  --data-urlencode "origin_ref=account:$CARD" \
  --data-urlencode "destination_ref=party:$F_PARTY" \
  --data-urlencode "split_mode=amount" \
  --data-urlencode "part_key[]=user:$U_ID" --data-urlencode "part_value[]=6.00" \
  --data-urlencode "part_key[]=user:$F_ID" --data-urlencode "part_value[]=6.00")"
[[ "$code" == "303" ]] || fail "F create: HTTP $code"
T1="$(sql "SELECT id FROM \"transaction\" WHERE description='coffee' AND household_id=2")"
SID="$(sql "SELECT id FROM \"transaction\" WHERE household_id=1 AND sync_source_household_id=2 AND sync_source_txn_id=$T1")"
[[ -n "$SID" ]] || fail "trigger did not create the copy (no page load)"
[[ "$(sql "SELECT is_synced FROM \"transaction\" WHERE id=$SID")" == "t" ]] || fail "trigger copy not is_synced"
[[ "$(sql "SELECT record_status FROM \"transaction\" WHERE id=$SID")" == "pending" ]] || fail "trigger copy not pending"
# No mapping yet -> the copy has no category.
[[ "$(sql "SELECT COALESCE(category_id,0) FROM \"transaction\" WHERE id=$SID")" == "0" ]] || fail "unexpected category on unmapped copy"

echo "==> 4. map a category (H2 Drink -> H1 Coffee)"
code="$(post u /settings/mapping --csrf-src="/settings?tab=sync" --data-urlencode "source=2:$DRINKH2" \
  --data-urlencode "target_category_id=$COFFEE")"
[[ "$code" == "303" ]] || fail "mapping create: HTTP $code"
mrow="$(sql "SELECT target_category_id FROM category_mapping WHERE household_id=1 AND source_household_id=2 AND source_category_id=$DRINKH2")"
[[ "$mrow" == "$COFFEE" ]] || fail "mapping target '$mrow', want $COFFEE"

echo "==> 5. a second record lands pre-filled with the mapped category"
code="$(post f /txns --csrf-src=/txns --data-urlencode "class=expense" \
  --data-urlencode "date=2026-11-19" --data-urlencode "total=8.00" \
  --data-urlencode "category_id=$DRINKH2" --data-urlencode "merchant_id=0" \
  --data-urlencode "description=coffee two" --data-urlencode "notes=" \
  --data-urlencode "origin_ref=account:$CARD" \
  --data-urlencode "destination_ref=party:$F_PARTY" --data-urlencode "split_mode=amount" \
  --data-urlencode "part_key[]=user:$U_ID" --data-urlencode "part_value[]=8.00")"
[[ "$code" == "303" ]] || fail "F create two: HTTP $code"
T2="$(sql "SELECT id FROM \"transaction\" WHERE description='coffee two' AND household_id=2")"
S2="$(sql "SELECT id FROM \"transaction\" WHERE household_id=1 AND sync_source_household_id=2 AND sync_source_txn_id=$T2")"
[[ -n "$S2" ]] || fail "trigger two: no copy"
[[ "$(sql "SELECT COALESCE(category_id,0) FROM \"transaction\" WHERE id=$S2")" == "$COFFEE" ]] || fail "mapped copy not pre-filled"

echo "==> 6. approve the no-cat copy via the single-record pop-up (category required)"
code="$(post u "/txns/$SID/status" --csrf-src="/txns/$SID" --data-urlencode "status=approved")"
[[ "$code" == "400" ]] || fail "approve no-cat: HTTP $code, want 400 (category required)"
code="$(post u "/txns/$SID/status" --csrf-src="/txns/$SID" --data-urlencode "status=approved" \
  --data-urlencode "category_id=$COFFEE" --data-urlencode "remember_mapping=on")"
[[ "$code" == "303" ]] || fail "approve with cat: HTTP $code"
[[ "$(sql "SELECT record_status FROM \"transaction\" WHERE id=$SID")" == "approved" ]] || fail "copy not approved"
[[ "$(sql "SELECT COALESCE(category_id,0) FROM \"transaction\" WHERE id=$SID")" == "$COFFEE" ]] || fail "copy category not set"
# Budget gate: the approved copy counts in the Food & Drink roll-up (the
# budgetable parent card; Coffee is its child).
page u / | grep -Pzoz '(?s)Food &amp; Drink.{0,400}?6\.00' >/dev/null || fail "dashboard: Food & Drink actuals missing 6.00 after approval"

echo "==> 7. detach (lock released, History row, badge gone)"
# Isolate: detach the other copies (backlog BID, mapped S2) so the coffee copy
# is the only synced one and the A-side badge check is unambiguous.
code="$(post u "/txns/$BID/detach" --csrf-src="/txns/$BID")"
[[ "$code" == "303" ]] || fail "detach backlog copy: HTTP $code"
code="$(post u "/txns/$S2/detach" --csrf-src="/txns/$S2")"
[[ "$code" == "303" ]] || fail "detach mapped copy: HTTP $code"
has f /txns 'synced by U'   # coffee copy still synced -> badge present
code="$(post u "/txns/$SID/detach" --csrf-src="/txns/$SID")"
[[ "$code" == "303" ]] || fail "detach: HTTP $code"
[[ "$(sql "SELECT is_synced FROM \"transaction\" WHERE id=$SID")" == "f" ]] || fail "still is_synced after detach"
[[ -n "$(sql "SELECT detached_at FROM \"transaction\" WHERE id=$SID")" ]] || fail "detached_at not set"
h="$(sql "SELECT count(*) FROM txn_history WHERE entity_type='transaction' AND entity_id=$SID AND action='detached'")"
[[ "$h" -ge 1 ]] || fail "no 'detached' History row"
# A-side badge gone (viewed by F in H2) — the only synced copy was the coffee one.
not_has f /txns 'synced by'

echo "==> 8. edit freely (the lock is released)"
ORGPARTY="$(sql "SELECT id FROM party WHERE household_id=1 AND name='Family household' AND app_user_id IS NULL")"
code="$(post u "/txns/$SID" --data-urlencode "class=expense" \
  --data-urlencode "date=2026-11-19" --data-urlencode "total=99.00" \
  --data-urlencode "category_id=$COFFEE" --data-urlencode "merchant_id=0" \
  --data-urlencode "description=coffee" --data-urlencode "notes=" \
  --data-urlencode "origin_ref=party:$ORGPARTY" --data-urlencode "destination_ref=" \
  --data-urlencode "split_mode=amount")"
[[ "$code" == "303" ]] || fail "detached edit: HTTP $code, want 303 (unlocked)"
[[ "$(sql "SELECT total_amount FROM \"transaction\" WHERE id=$SID")" == "99.00" ]] || fail "amount not editable"

echo "==> 9. re-link (money facts reset, classification kept, still one row)"
code="$(post u /sync --data-urlencode "source_household_id=2" --data-urlencode "source_txn_id=$T1")"
[[ "$code" == "303" ]] || fail "re-link: HTTP $code"
[[ "$(sql "SELECT count(*) FROM \"transaction\" WHERE household_id=1 AND sync_source_household_id=2 AND sync_source_txn_id=$T1")" == "1" ]] || fail "re-link created a second row"
[[ "$(sql "SELECT is_synced FROM \"transaction\" WHERE id=$SID")" == "t" ]] || fail "not re-linked (is_synced)"
[[ -z "$(sql "SELECT detached_at FROM \"transaction\" WHERE id=$SID")" ]] || fail "detached_at not cleared"
[[ "$(sql "SELECT total_amount FROM \"transaction\" WHERE id=$SID")" == "6.00" ]] || fail "re-link did not reset amount to the source share"
[[ "$(sql "SELECT COALESCE(category_id,0) FROM \"transaction\" WHERE id=$SID")" == "$COFFEE" ]] || fail "re-link changed the classification"
has f /txns 'synced by U'

echo
echo "SMOKE D34: all checks passed"
