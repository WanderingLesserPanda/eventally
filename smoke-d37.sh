#!/usr/bin/env bash
# D37 HTTP smoke — the two-layout navigation rework.
#
# Scratch throwaway DB (created, seeded, dropped by trap); the app binary is
# built and booted against it (auto-migration included). Neutral seed labels
# only (O, M, U; "Main household", "Second household") — no names.
#
# Checks (per the goal doc's definition of done):
#   1.  daily pages: the flat left pane, the cogwheel settings entry, no
#       "Back to Home"; the correct item active per page
#   2.  "From other households" only for the two-household user (D33)
#   3.  settings sections: "Back to Home" pill + logo link, no cogwheel, the
#       settings pane with the right item active
#   4.  Invites/Grants hidden entirely for the non-admin member (403 direct)
#   5.  pre-nesting URLs 302 into the nested routes (query preserved)
#   6.  Categories page + fragments at /settings/categories*; a create POST
#       round-trips at the new URL
#   7.  the two sync surfaces: /sync stays the daily inbox, /settings/sync is
#       the auto-sync configuration
#
# Run: ./smoke-d37.sh
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck disable=SC1091
export LD_LIBRARY_PATH="${LD_LIBRARY_PATH:-}"
source /opt/data/cache/pgenv.sh

PORT="${SMOKE_PORT:-18094}"
BASE="http://127.0.0.1:${PORT}"
DB="d37smoke_$$"
WORK="$(mktemp -d)"
BIN="$WORK/eventally"
JARS="$WORK/jars"
# argon2id helper must live inside the module (internal/ import rule).
PWHASH=".d37_pwhash"
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
INSERT INTO household (name) VALUES ('Main household'), ('Second household');
INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
VALUES
 (1, 'o', 'O', 'o@example.test',  '${HASH}'),
 (1, 'm', 'M', 'm@example.test',  '${HASH}'),
 (1, 'u', 'U', 'u@example.test',  '${HASH}');
INSERT INTO household_member (user_id, household_id, role)
SELECT u.id, h.id, r.role
FROM (VALUES ('o',1,'owner'), ('m',1,'member'), ('u',1,'member'), ('u',2,'owner'))
     AS r(username, hh, role)
JOIN app_user u ON u.username = r.username
JOIN household h ON h.id = r.hh;
INSERT INTO category (household_id, name, parent_id, transaction_class, budget_per_month, is_budgetable)
VALUES
 (1, 'Groceries', NULL, 'expense', 400.00, true),
 (1, 'Dining out', 1,  'expense', 100.00, true);
INSERT INTO account (household_id, name, account_type, balance_type)
VALUES
 (1, 'Main Account', 'savings', 'asset');
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
code_of() { # $1=username $2=path -> GET status code
  curl -s -b "$JARS/$1" -o /dev/null -w '%{http_code}' "$BASE$2"
}
redir() { # $1=username $2=path -> Location header of the 30x
  curl -s -b "$JARS/$1" -o /dev/null -D - "$BASE$2" | tr -d '\r' | grep -i '^location:' | head -1 | sed 's/^[^:]*: *//'
}
has() { # $1=username $2=path $3=pattern
  page "$1" "$2" | grep -q -- "$3" || fail "$1 $2 — expected: $3"
}
not_has() { # $1=username $2=path $3=pattern
  if page "$1" "$2" | grep -q -- "$3"; then
    fail "$1 $2 — unexpected: $3"
  fi
}
sql() { psql -tAq -d "$DB" -c "$1" || { echo "FAIL: psql: $1" >&2; exit 1; }; }

login o; login m; login u

echo "==> 1. daily layout: flat pane + cogwheel + active item"
for p in / /capture /txns /cards /reconcile /recurring /review /accounts /merchants /parties; do
  has o "$p" '<div class="shell">'
  has o "$p" '<a class="cog-btn" href="/settings/household"'
  not_has o "$p" 'back-home'
  has o "$p" '<div class="side-foot">'
done
has o / 'class="active">Dashboard'
has o /txns 'class="active">Transactions'
has o /cards 'class="active">Cards'
has o /accounts 'class="active">Accounts'
# auxiliary pages: daily layout, no active pane item
has o /history '<div class="shell">'
not_has o /history 'class="active"'
not_has o /deletion-log 'class="active"'
# the flat pane carries the whole destination set on a daily page
has o / 'href="/capture"'
has o / 'href="/txns"'
# the daily brand is an inert span
has o / '<span class="brand">Eventally</span>'
not_has o / '<a class="brand" href="/">'

echo "==> 2. From other households only for the two-household user"
has u / 'From other households'
not_has o / 'From other households'
not_has m / 'From other households'

echo "==> 3. settings layout: back-to-home pill + logo link, no cogwheel"
for p in /settings/household /settings/categories /settings/invites /settings/grants /settings/sync; do
  has o "$p" '<a class="back-home" href="/">'
  has o "$p" '>Back to Home</a>'
  has o "$p" '<a class="brand" href="/">Eventally</a>'
  not_has o "$p" 'cog-btn'
  has o "$p" '<div class="side-foot">'
done
has o /settings/household 'class="active">Household'
has o /settings/categories 'class="active">Categories'
has o /settings/invites 'class="active">Invites'
has o /settings/grants 'class="active">Grants'
has o /settings/sync 'class="active">Sync'

echo "==> 4. admin-only entries hidden for the non-admin member"
not_has m /settings/household 'href="/settings/invites"'
not_has m /settings/household 'href="/settings/grants"'
has m /settings/household 'href="/settings/household"'
has m /settings/sync 'href="/settings/sync"'
not_has m /settings/sync 'href="/settings/invites"'
c="$(code_of m /settings/invites)"; [[ "$c" == "403" ]] || fail "m /settings/invites: HTTP $c, want 403"
c="$(code_of m /settings/grants)";  [[ "$c" == "403" ]] || fail "m /settings/grants: HTTP $c, want 403"
# the member's sections still render for them
c="$(code_of m /settings/household)"; [[ "$c" == "200" ]] || fail "m /settings/household: HTTP $c"
c="$(code_of m /settings/sync)";      [[ "$c" == "200" ]] || fail "m /settings/sync: HTTP $c"

echo "==> 5. pre-nesting URLs redirect"
check_redir() { # $1=from $2=want
  local c l
  c="$(code_of o "$1")"
  [[ "$c" == "302" ]] || fail "o $1: HTTP $c, want 302"
  l="$(redir o "$1")"
  [[ "$l" == "$2" ]] || fail "o $1: Location '$l', want '$2'"
}
check_redir "/settings" "/settings/household"
check_redir "/settings?tab=invites" "/settings/invites"
check_redir "/settings?tab=grants" "/settings/grants"
check_redir "/settings?tab=sync" "/settings/sync"
check_redir "/settings?tab=bogus" "/settings/household"
check_redir "/categories" "/settings/categories"
check_redir "/categories?class=income" "/settings/categories?class=income"
check_redir "/categories/new" "/settings/categories/new"
check_redir "/categories/7" "/settings/categories/7"

echo "==> 6. categories at /settings/categories (page, fragments, create)"
has o /settings/categories 'x-data="catsPage()"'
has o /settings/categories "fetchText('/settings/categories/pane"
has o /settings/categories 'href="/settings/categories/new"'
has o /settings/categories 'Groceries'
c="$(code_of o /settings/categories/pane?class=expense)"; [[ "$c" == "200" ]] || fail "pane fragment: HTTP $c"
has o '/settings/categories/pane?class=expense' 'Groceries'
GID="$(sql "SELECT id FROM category WHERE household_id=1 AND name='Groceries'")"
c="$(code_of o "/settings/categories/detail?id=$GID")"; [[ "$c" == "200" ]] || fail "detail fragment: HTTP $c"
has o "/settings/categories/detail?id=$GID" 'Dining out'
# create POST round-trips at the new URL
code="$(post o /settings/categories --csrf-src=/settings/categories \
  --data-urlencode "name=Smoke snacks" --data-urlencode "transaction_class=expense" \
  --data-urlencode "parent_id=0" --data-urlencode "budgetable=" --data-urlencode "icon=" --data-urlencode "colour=")"
[[ "$code" == "303" ]] || fail "category create POST: HTTP $code"
has o /settings/categories 'Smoke snacks'
sqlid="$(sql "SELECT id FROM category WHERE household_id=1 AND name='Smoke snacks'")"
[[ -n "$sqlid" ]] || fail "Smoke snacks not in DB"

echo "==> 7. the two sync surfaces"
# /sync stays the daily inbox (U is in two households; H2 has no records yet
# so the inbox lists the other household with an empty record list).
has u /sync 'Second household'
has u /sync '<a class="cog-btn" href="/settings/household"'
not_has u /sync 'back-home'
# /settings/sync carries the auto-sync configuration.
has o /settings/sync 'Auto-sync'
has o /settings/sync 'back-home'
has u /settings/sync 'Second household'

echo
echo "SMOKE D37: all checks passed"
