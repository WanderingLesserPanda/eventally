#!/usr/bin/env bash
# D33 HTTP smoke — cross-household synced records.
#
# Scratch throwaway DB (created, seeded, dropped by trap); the app binary is
# built and booted against it (auto-migration to v17 included). Neutral seed
# labels only (U, F, P, M; "Main household", "Family household") — no names.
#
# Checks (per the D33 spec's verification section):
#   1.  nav-link visibility per membership (U sees it; P, M, F don't)
#   2.  /sync GET rows + states (T1 syncable; T2 recorded-only, T3 leg-funded
#       do not appear)
#   3.  POST /sync end-to-end (row appears on /txns for U and P, absent for M;
#       A-side "synced by" badge on the H2 source row, no household leak)
#   4.  dashboard card numbers (Coffee actuals include 6.00 for U, not M)
#   5.  duplicate POST no-op (exactly one row per source pointer)
#   6.  edit-lock: amount change 400; category change allowed
#   7.  "source changed" flag + re-sync refreshes amount, keeps category
#   8.  un-sync = ordinary delete (Deletion-log row; pointer freed)
#   9.  void cascade: voiding the source in H2 voids the H1 copy with a
#       History row; the syncable list then empties
#  10.  /sync empty state for M
#
# Run: ./smoke-d33.sh
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck disable=SC1091
export LD_LIBRARY_PATH="${LD_LIBRARY_PATH:-}"
source /opt/data/cache/pgenv.sh

PORT="${SMOKE_PORT:-18091}"
BASE="http://127.0.0.1:${PORT}"
DB="d33smoke_$$"
WORK="$(mktemp -d)"
BIN="$WORK/eventally"
JARS="$WORK/jars"
# argon2id helper must live inside the module (internal/ import rule).
PWHASH=".d33_pwhash"
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

# argon2id hash for the seed users (internal/auth is module-internal, so the
# helper lives in a scratch dir that is deleted by the trap).
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

echo "==> booting app on :${PORT} (auto-migrates the scratch db)"
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
curl -sf -o /dev/null "$BASE/login" || { echo "FAIL: server did not come up"; exit 1; }

seed() {
psql -qX -v ON_ERROR_STOP=1 -d "$DB" <<SQL
INSERT INTO household (name) VALUES ('Main household'), ('Family household');
INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
VALUES
 (1, 'p', 'P', 'p@example.test',  '${HASH}'),
 (2, 'f', 'F', 'f@example.test',  '${HASH}'),
 (1, 'u', 'U', 'u@example.test',  '${HASH}'),
 (1, 'm', 'M', 'm@example.test',  '${HASH}');
INSERT INTO household_member (user_id, household_id, role)
SELECT u.id, h.id, r.role
FROM (VALUES ('p',1,'owner'), ('f',2,'owner'), ('u',1,'member'), ('u',2,'member'), ('m',1,'member'))
     AS r(username, hh, role)
JOIN app_user u ON u.username = r.username
JOIN household h ON h.id = r.hh;
INSERT INTO category (household_id, name, parent_id, transaction_class, budget_per_month, is_budgetable)
VALUES
 (1, 'Food & Drink', NULL, 'expense', NULL, false),
 (1, 'Coffee',       1,    'expense', 100.00, true),
 (1, 'Drink',        1,    'expense', 50.00, true),
 (2, 'Food & Drink', NULL, 'expense', NULL, false),
 (2, 'Drink',        4,    'expense', NULL, false);
INSERT INTO account (household_id, name, account_type, balance_type)
VALUES
 (1, 'Main Account', 'savings', 'asset'),
 (2, 'Family Card',  'credit_card', 'liability');
-- M: Related-mode read on the H1 account. U: full grant on the H1 account
-- and a Related-mode read on the H2 card (the surface that makes T1 part of
-- U's /txns there, and therefore syncable — D27).
INSERT INTO account_grant (user_id, household_id, account_id,
  can_read, can_create, can_edit, can_delete, view_mode, edit_mode, delete_mode)
VALUES
 ((SELECT id FROM app_user WHERE username='m'), 1, (SELECT id FROM account WHERE name='Main Account'),
  true, false, false, false, 'Related', 'Related', 'Related'),
 ((SELECT id FROM app_user WHERE username='u'), 1, (SELECT id FROM account WHERE name='Main Account'),
  true, true, true, true, 'All', 'All', 'All'),
 ((SELECT id FROM app_user WHERE username='u'), 2, (SELECT id FROM account WHERE name='Family Card'),
  true, false, false, false, 'Related', 'Related', 'Related');
-- H2 records: T1 syncable (U split 6.00), T2 recorded-only (no U split),
-- T3 settlement leg funded by U (related via the leg, not a split).
INSERT INTO "transaction" (household_id, recorded_by_user_id, transaction_date, class, category_id, total_amount, description, origin_account_id, record_status)
VALUES
 (2, (SELECT id FROM app_user WHERE username='f'), '2026-11-12', 'expense',
  (SELECT id FROM category WHERE household_id=2 AND name='Drink'), 12.00, 'coffee beverage',
  (SELECT id FROM account WHERE name='Family Card'), 'approved'),
 (2, (SELECT id FROM app_user WHERE username='u'), '2026-11-08', 'expense',
  (SELECT id FROM category WHERE household_id=2 AND name='Drink'), 15.00, 'lunch set',
  (SELECT id FROM account WHERE name='Family Card'), 'approved'),
 (2, (SELECT id FROM app_user WHERE username='f'), '2026-11-05', 'expense',
  (SELECT id FROM category WHERE household_id=2 AND name='Drink'), 20.00, 'streaming subscription',
  (SELECT id FROM account WHERE name='Family Card'), 'approved');
INSERT INTO transaction_split (household_id, transaction_id, party_id, amount)
SELECT 2, t.id, p.id, v.amount
FROM (VALUES ('coffee beverage', 'u', 6.00),
             ('coffee beverage', 'f', 6.00),
             ('lunch set', 'f', 15.00),
             ('streaming subscription', 'f', 20.00)) AS v(tdesc, uname, amount)
JOIN "transaction" t ON t.description = v.tdesc
JOIN party p ON p.household_id = 2 AND p.app_user_id = (SELECT id FROM app_user WHERE username = v.uname);
-- T3: a completed settlement with a U funding leg.
INSERT INTO settlement (household_id, account_id, amount, payment_date, notes)
VALUES (2, (SELECT id FROM account WHERE name='Family Card'), 20.00, '2026-11-05', '');
INSERT INTO settlement_funding (household_id, settlement_id, user_id, method, amount)
VALUES (2, (SELECT max(id) FROM settlement), (SELECT id FROM app_user WHERE username='u'), 'transfer', 20.00);
INSERT INTO settlement_allocation (household_id, payment_id, split_id, amount)
SELECT 2, (SELECT max(id) FROM settlement), s.id, 20.00
FROM transaction_split s
WHERE s.transaction_id = (SELECT id FROM "transaction" WHERE description='streaming subscription');
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
page() { # $1=username $2=path
  curl -s -b "$JARS/$1" "$BASE$2"
}
csrf() { # $1=username $2=path
  page "$1" "$2" | grep -o 'name="csrf" value="[^"]*"' | head -1 | sed 's/.*value="//;s/"//' || true
}
post() { # $1=username $2=path [$3=csrf-source path] [data...] -> status code
  local u="$1" path="$2"; shift 2
  local src="$path"
  if [[ $# -ge 1 && "$1" == "--csrf-src="* ]]; then
    src="${1#--csrf-src=}"
    shift
  fi
  local c; c="$(csrf "$u" "$src")"
  curl -s -b "$JARS/$u" -o /dev/null -w '%{http_code}' \
    --data-urlencode "csrf=$c" "$@" "$BASE$path"
}
has() { # $1=username $2=path $3=pattern
  page "$1" "$2" | grep -q "$3" || fail "$1 $2 — expected: $3"
}
not_has() { # $1=username $2=path $3=pattern
  if page "$1" "$2" | grep -q "$3"; then
    fail "$1 $2 — unexpected: $3"
  fi
}
sql() { psql -tAq -d "$DB" -c "$1" || { echo "FAIL: psql: $1" >&2; exit 1; }; }
table_rows() { # $1=username $2=path -> only the table regions (the page legend
  # outside the tables also mentions "source changed" — scope to the rows).
  page "$1" "$2" | sed -n '/<table/,/<\/table>/p'
}

login p; login f; login u; login m

echo "==> 1. nav-link visibility"
has u /txns 'From other households'
not_has p /txns 'From other households'
not_has m /txns 'From other households'
not_has f /txns 'From other households'

echo "==> 2. /sync GET rows + states"
has u /sync 'Family household'
has u /sync 'coffee beverage'
not_has u /sync 'lunch set'
not_has u /sync 'streaming subscription'

T1="$(sql "SELECT id FROM \"transaction\" WHERE description='coffee beverage'")"
T3="$(sql "SELECT id FROM \"transaction\" WHERE description='streaming subscription'")"
COFFEE="$(sql "SELECT id FROM category WHERE household_id=1 AND name='Coffee'")"
DRINKH1="$(sql "SELECT id FROM category WHERE household_id=1 AND name='Drink'")"

echo "==> 3. POST /sync end-to-end"
code="$(post u /sync --data-urlencode "source_household_id=2" \
  --data-urlencode "source_txn_id=$T1" --data-urlencode "category_id=$COFFEE")"
[[ "$code" == "303" ]] || fail "sync POST: HTTP $code"
has u /txns 'synced ✓'
has p /txns 'synced ✓'
not_has m /txns 'coffee beverage'

SYNCID="$(sql "SELECT id FROM \"transaction\" WHERE household_id=1 AND sync_source_household_id=2 AND sync_source_txn_id=$T1")"
[[ -n "$SYNCID" ]] || fail "synced row not found in H1"
# A-side badge on the source row (H2, viewed by F): the syncing member's
# name only. F holds no H1 membership, so 'Main household' must not appear
# anywhere on the page (the leak rule).
has f /txns 'synced by U'
not_has f /txns 'Main household'

echo "==> 4. dashboard card numbers (Food & Drink roll-up)"
page u / | grep -Pzoz '(?s)Food &amp; Drink.{0,400}?6\.00' >/dev/null || fail "U dashboard: Food & Drink actuals missing 6.00"
if page m / | grep -Pzoz '(?s)Food &amp; Drink.{0,400}?6\.00' >/dev/null; then
  fail "M dashboard: Food & Drink actuals should not include 6.00"
fi

echo "==> 5. duplicate POST no-op"
code="$(post u /sync --data-urlencode "source_household_id=2" \
  --data-urlencode "source_txn_id=$T1" --data-urlencode "category_id=$COFFEE")"
[[ "$code" == "303" ]] || fail "re-sync POST: HTTP $code"
n="$(sql "SELECT count(*) FROM \"transaction\" WHERE household_id=1 AND sync_source_household_id=2 AND sync_source_txn_id=$T1")"
[[ "$n" == "1" ]] || fail "duplicate sync produced $n rows"

echo "==> 6. edit lock"
ORIGIN_REF="party:$(sql "SELECT id FROM party WHERE household_id=1 AND name='Family household'")"
code="$(post u "/txns/$SYNCID" --data-urlencode "class=expense" \
  --data-urlencode "date=2026-11-12" --data-urlencode "total=7.00" \
  --data-urlencode "category_id=$COFFEE" --data-urlencode "merchant_id=0" \
  --data-urlencode "description=coffee beverage" --data-urlencode "notes=" \
  --data-urlencode "origin_ref=$ORIGIN_REF" --data-urlencode "destination_ref=" \
  --data-urlencode "split_mode=amount")"
[[ "$code" == "400" ]] || fail "amount change: HTTP $code, want 400 (locked)"
code="$(post u "/txns/$SYNCID" --data-urlencode "class=expense" \
  --data-urlencode "date=2026-11-12" --data-urlencode "total=6.00" \
  --data-urlencode "category_id=$DRINKH1" --data-urlencode "merchant_id=0" \
  --data-urlencode "description=coffee beverage" --data-urlencode "notes=" \
  --data-urlencode "origin_ref=$ORIGIN_REF" --data-urlencode "destination_ref=" \
  --data-urlencode "split_mode=amount")"
[[ "$code" == "303" ]] || fail "category change: HTTP $code, want 303"

echo "==> 7. source changed + re-sync"
T2="$(sql "SELECT id FROM \"transaction\" WHERE description='lunch set'")"
U_ID="$(sql "SELECT id FROM app_user WHERE username='u'")"
F_ID="$(sql "SELECT id FROM app_user WHERE username='f'")"
code="$(post f "/txns/$T1" --data-urlencode "class=expense" \
  --data-urlencode "date=2026-11-12" --data-urlencode "total=14.00" \
  --data-urlencode "category_id=$(sql "SELECT id FROM category WHERE household_id=2 AND name='Drink'")" \
  --data-urlencode "merchant_id=0" --data-urlencode "description=coffee beverage" \
  --data-urlencode "notes=" \
  --data-urlencode "origin_ref=account:$(sql "SELECT id FROM account WHERE name='Family Card'")" \
  --data-urlencode "destination_ref=party:$(sql "SELECT id FROM party WHERE household_id=2 AND app_user_id=$F_ID")" \
  --data-urlencode "split_mode=amount" \
  --data-urlencode "part_key[]=user:$U_ID" --data-urlencode "part_value[]=7.00" \
  --data-urlencode "part_key[]=user:$F_ID" --data-urlencode "part_value[]=7.00")"
[[ "$code" == "303" ]] || fail "F edit of T1: HTTP $code"
if ! table_rows u /sync | grep -q 'source changed'; then
  fail "u /sync — expected the 'source changed' badge after the source edit"
fi
code="$(post u /sync --data-urlencode "source_household_id=2" \
  --data-urlencode "source_txn_id=$T1" --data-urlencode "category_id=$COFFEE")"
[[ "$code" == "303" ]] || fail "re-sync POST: HTTP $code"
amt="$(sql "SELECT total_amount FROM \"transaction\" WHERE id=$SYNCID")"
[[ "$amt" == "7.00" ]] || fail "re-sync amount: $amt, want 7.00"
# Step 6 re-classified the copy to Drink; re-sync must keep the local
# classification (it refreshes money facts only).
cat="$(sql "SELECT category_id FROM \"transaction\" WHERE id=$SYNCID")"
[[ "$cat" == "$DRINKH1" ]] || fail "re-sync changed category to $cat, want $DRINKH1 (kept)"
if table_rows u /sync | grep -q 'source changed'; then
  fail "u /sync — 'source changed' badge should be gone after re-sync"
fi

echo "==> 8. un-sync = ordinary delete"
code="$(post u /txns/bulk-delete --csrf-src=/ --data-urlencode "ids[]=$SYNCID")"
[[ "$code" == "303" ]] || fail "bulk delete: HTTP $code"
out="$(sql "SELECT outcome FROM deletion_log WHERE household_id=1 AND txn_id=$SYNCID")"
[[ "$out" == "deleted" ]] || fail "deletion_log: '$out', want deleted"
[[ -z "$(sql "SELECT id FROM \"transaction\" WHERE id=$SYNCID")" ]] || fail "row still present"
# The A-side badge is render-time: with the copy gone, no chip remains.
not_has f /txns 'synced by'

echo "==> 9. re-sync after delete, then void cascade"
code="$(post u /sync --data-urlencode "source_household_id=2" \
  --data-urlencode "source_txn_id=$T1" --data-urlencode "category_id=$COFFEE")"
[[ "$code" == "303" ]] || fail "re-sync POST: HTTP $code"
NEWID="$(sql "SELECT id FROM \"transaction\" WHERE household_id=1 AND sync_source_household_id=2 AND sync_source_txn_id=$T1")"
[[ -n "$NEWID" && "$NEWID" != "$SYNCID" ]] || fail "re-sync after delete: id '$NEWID'"
code="$(post f "/txns/$T1/status" --csrf-src=/ --data-urlencode "status=void")"
[[ "$code" == "303" ]] || fail "void T1: HTTP $code"
st="$(sql "SELECT record_status FROM \"transaction\" WHERE id=$NEWID")"
[[ "$st" == "void" ]] || fail "cascade: H1 copy '$st', want void"
h="$(sql "SELECT count(*) FROM txn_history WHERE entity_type='transaction' AND entity_id=$NEWID AND action='voided'")"
[[ "$h" -ge 1 ]] || fail "no History row for the cascaded void"
not_has u /sync 'coffee beverage'

echo "==> 10. /sync empty state for M"
has m /sync 'empty'

echo
echo "SMOKE D33: all checks passed"
