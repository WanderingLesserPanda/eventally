#!/usr/bin/env bash
# D32 HTTP smoke — icon picker + registry (docs/notes/d32-icon-picker.md,
# ADR-0009).
#
# Scratch throwaway DB (created, seeded, dropped by trap); the app binary is
# built and booted against it (auto-migration to v19 included). Neutral seed
# labels only (U, M, E; "Main household") — no names.
#
# Full journey (spec verification section):
#   picker markup on both forms (296 glyph cells + explicit "No icon" row) ->
#   category icon set by a PLAIN MEMBER via the form (no grant gate) ->
#   unknown keys rejected (form error, no row) -> account icon set by the
#   owner -> account icon refused (403) without the Edit grant -> icons
#   render on all six surfaces (dashboard, /txns list + detail, review,
#   accounts grid, cards, txn-form category select) with per-type defaults
#   for unpicked accounts -> no emojis anywhere on the accounts grid.
#
# Run: ./smoke-d32.sh
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck disable=SC1091
export LD_LIBRARY_PATH="${LD_LIBRARY_PATH:-}"
source /opt/data/cache/pgenv.sh

PORT="${SMOKE_PORT:-18091}"
BASE="http://127.0.0.1:${PORT}"
DB="d32smoke_$$"
WORK="$(mktemp -d)"
BIN="$WORK/eventally"
JARS="$WORK/jars"
PWHASH=".d32_pwhash"
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

echo "==> booting app on :${PORT} (auto-migrates the scratch db to v19)"
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
INSERT INTO household (name) VALUES ('Main household');
INSERT INTO app_user (default_household_id, username, display_name, email, password_hash)
VALUES
 (1, 'u', 'U', 'u@example.test', '${HASH}'),
 (1, 'm', 'M', 'm@example.test', '${HASH}'),
 (1, 'e', 'E', 'e@example.test', '${HASH}');
INSERT INTO household_member (user_id, household_id, role)
SELECT u.id, 1, r.role
FROM (VALUES ('u','owner'), ('m','member'), ('e','member')) AS r(username, role)
JOIN app_user u ON u.username = r.username;
-- E holds the household edit-all grant (may edit accounts).
INSERT INTO household_grant (user_id, household_id, can_read, can_create,
  can_edit, can_delete, edit_mode, delete_mode)
VALUES ((SELECT id FROM app_user WHERE username='e'), 1,
        true, true, true, false, 'all', 'created');
INSERT INTO category (household_id, name, parent_id, transaction_class, budget_per_month, is_budgetable, icon)
VALUES
 (1, 'Food & Drink', NULL, 'expense', 100.00, true,  'la-utensils'),
 (1, 'Coffee',       1,    'expense', NULL,   false, 'la-coffee');
INSERT INTO account (household_id, name, account_type, balance_type, icon)
VALUES
 (1, 'Main Card', 'credit_card', 'liability', NULL),
 (1, 'Piggy',     'savings',     'asset',     'la-piggy-bank'),
 (1, 'Wallet',    'ewallet',     'asset',     NULL);
-- Minimal account-level read grant on the Main Card for E (the record-read
-- gate in the account edit handler fires before the Edit gate).
INSERT INTO account_grant (user_id, household_id, account_id,
  can_read, can_create, can_edit, can_delete, view_mode, edit_mode, delete_mode)
VALUES ((SELECT id FROM app_user WHERE username='e'), 1,
        (SELECT id FROM account WHERE name='Main Card'),
        true, false, false, false, 'Related', 'Related', 'Related');
-- One approved + one pending expense in Coffee (icon la-coffee) this month.
INSERT INTO "transaction" (household_id, recorded_by_user_id, transaction_date, class, category_id, total_amount, description, origin_account_id, record_status)
VALUES
 (1, (SELECT id FROM app_user WHERE username='u'), date_trunc('month', current_date)::date + 1,
  'expense', (SELECT id FROM category WHERE name='Coffee'), 6.00, 'coffee',
  (SELECT id FROM account WHERE name='Main Card'), 'approved'),
 (1, (SELECT id FROM app_user WHERE username='u'), (date_trunc('month', current_date)::date + 1)::date,
  'expense', (SELECT id FROM category WHERE name='Coffee'), 4.00, 'coffee pending',
  (SELECT id FROM account WHERE name='Main Card'), 'pending');
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
has() { local out; out="$(page "$1" "$2")"; [[ "$out" == *"$3"* ]] || fail "$1 $2 — expected: $3"; }
not_has() { local out; out="$(page "$1" "$2")"; if [[ "$out" == *"$3"* ]]; then fail "$1 $2 — unexpected: $3"; fi; }
sql() { psql -tAq -d "$DB" -c "$1" || { echo "FAIL: psql: $1" >&2; exit 1; }; }

U_ID="$(sql "SELECT id FROM app_user WHERE username='u'")"
M_ID="$(sql "SELECT id FROM app_user WHERE username='m'")"
E_ID="$(sql "SELECT id FROM app_user WHERE username='e'")"
COFFEE="$(sql "SELECT id FROM category WHERE name='Coffee'")"
FOOD="$(sql "SELECT id FROM category WHERE name='Food & Drink'")"
CARD="$(sql "SELECT id FROM account WHERE name='Main Card'")"
PENDING="$(sql "SELECT id FROM \"transaction\" WHERE description='coffee pending'")"
APPROVED="$(sql "SELECT id FROM \"transaction\" WHERE description='coffee'")"

login u; login m; login e

echo "==> 1. vendored Line Awesome serves (CSS + fonts, MIT license)"
code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/static/lineawesome/css/line-awesome.min.css")"
[[ "$code" == "200" ]] || fail "line-awesome.min.css: HTTP $code"
css="$(curl -s "$BASE/static/lineawesome/css/line-awesome.min.css")"
[[ "$css" == *Line\ Awesome* ]] || fail "CSS content missing"
# Regression (2026-11-20 live report): the CSS references its fonts
# relatively (../fonts/), so assert the URLs the BROWSER actually
# resolves — not just the fonts' canonical paths. Modern browsers only
# fetch woff/woff2, so that is the set we require to serve.
# resolve_ref DIR REF: resolve REF (possibly containing ../) against DIR.
# DIR must be a bare path (no scheme/host) — curl gets $BASE prepended.
resolve_ref() {
  local dir="$1" ref="$2" part
  local -a stack=(${dir//\// })
  ref="${ref#./}"
  for part in ${ref//\// }; do
    case "$part" in
      "") ;;
      ..) unset 'stack[-1]' ;;
      *) stack+=("$part") ;;
    esac
  done
  (IFS=/; echo "/${stack[*]}")
}
cssdir="/static/lineawesome/css"
badrefs=""
for ref in $(echo "$css" | grep -o 'url([^)]*)' | sed 's/^url(//; s/)$//' | tr -d '"'); do
  case "$ref" in *woff) ;; *) continue ;; esac
  p="$(resolve_ref "$cssdir" "$ref")"
  code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE$p" || true)"
  [[ "$code" == "200" ]] || badrefs="$badrefs $ref(->$p:$code)"
done
[[ -z "$badrefs" ]] || fail "font url() refs unresolved:$badrefs"
code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/static/lineawesome/fonts/la-solid-900.woff2")"
[[ "$code" == "200" ]] || fail "solid woff2 font: HTTP $code"
code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/static/lineawesome/LICENSE.txt")"
[[ "$code" == "200" ]] || fail "LICENSE.txt: HTTP $code"

echo "==> 2. category form: picker markup (296 glyphs + No-icon row + CSS link)"
CATFORM="$(page u /categories/new)"
# Note: bash string matching, not `echo | grep -q` — the page is >64KB and
# grep -q's early exit would SIGPIPE the writer under pipefail.
[[ "$CATFORM" == *'/static/lineawesome/css/line-awesome.min.css'* ]] || fail "category form: LA css link missing"
CELLS="$(echo "$CATFORM" | grep -o 'class="icon-cell" data-key="la-' | wc -l)"
[[ "$CELLS" == "296" ]] || fail "category picker cells $CELLS, want 296"
[[ "$CATFORM" == *'data-label="no icon (use default)"'* ]] || fail "picker missing 'No icon (use default)' row"
[[ "$CATFORM" == *"Alpine.data('iconPicker'"* ]] || fail "picker component not registered in the page"

echo "==> 3. plain member sets a category icon via the form (no grant gate)"
code="$(post m /categories --csrf-src=/categories/new \
  --data-urlencode "name=Snacks" --data-urlencode "transaction_class=expense" \
  --data-urlencode "icon=la-pizza-slice")"
[[ "$code" == "303" ]] || fail "member category create: HTTP $code"
[[ "$(sql "SELECT icon FROM category WHERE name='Snacks'")" == "la-pizza-slice" ]] || fail "icon not persisted"
# Brand-family glyph (Line Awesome Brands font) — must be storable too.
code="$(post m /categories --csrf-src=/categories/new \
  --data-urlencode "name=Crypto" --data-urlencode "transaction_class=expense" \
  --data-urlencode "icon=la-ethereum")"
[[ "$code" == "303" ]] || fail "member category create (brand icon): HTTP $code"
[[ "$(sql "SELECT icon FROM category WHERE name='Crypto'")" == "la-ethereum" ]] || fail "brand icon not persisted"
CRYPTO="$(sql "SELECT id FROM category WHERE name='Crypto'")"

echo "==> 4. unknown icon key rejected (form error, no row)"
body="$(curl -s -b "$JARS/m" --data-urlencode "csrf=$(csrf m /categories/new)" \
  --data-urlencode "name=Ghost" --data-urlencode "transaction_class=expense" \
  --data-urlencode "icon=la-bogus" "$BASE/categories")"
[[ "$body" == *'Unknown icon.'* ]] || fail "unknown icon not rejected"
[[ "$(sql "SELECT count(*) FROM category WHERE name='Ghost'")" == "0" ]] || fail "rejected key created a row"

echo "==> 5. plain member edits the icon"
code="$(post m "/categories/$COFFEE" --csrf-src="/categories/$COFFEE" \
  --data-urlencode "name=Coffee" --data-urlencode "transaction_class=expense" \
  --data-urlencode "budgetable=on" --data-urlencode "icon=la-mug-hot")"
[[ "$code" == "303" ]] || fail "member category edit: HTTP $code"
[[ "$(sql "SELECT icon FROM category WHERE id=$COFFEE")" == "la-mug-hot" ]] || fail "edit not persisted"

echo "==> 6. account form: picker markup"
ACCFORM="$(page u /accounts/new)"
[[ "$ACCFORM" == *'class="icon-grid"'* ]] || fail "account form: picker grid missing"
[[ "$ACCFORM" == *'data-label="no icon (use default)"'* ]] || fail "account picker missing 'No icon' row"

echo "==> 7. owner sets an account icon via the form"
code="$(post u /accounts --csrf-src=/accounts/new \
  --data-urlencode "name=Vault" --data-urlencode "account_type=cash" \
  --data-urlencode "balance_type=asset" --data-urlencode "icon=la-dollar-sign")"
[[ "$code" == "303" ]] || fail "owner account create: HTTP $code"
[[ "$(sql "SELECT icon FROM account WHERE name='Vault'")" == "la-dollar-sign" ]] || fail "account icon not persisted"
# A form error (duplicate name) re-renders the account form with the picker intact.
body="$(curl -s -b "$JARS/u" --data-urlencode "csrf=$(csrf u /accounts/new)" \
  --data-urlencode "name=Piggy" --data-urlencode "account_type=savings" \
  --data-urlencode "balance_type=asset" --data-urlencode "icon=la-coins" "$BASE/accounts")"
[[ "$body" == *'An account with this name already exists.'* ]] || fail "duplicate-name error missing"
[[ "$body" == *'class="icon-grid"'* ]] || fail "error re-render lost the picker"

echo "==> 8. account icon refused without the Edit grant (403, nothing written)"
# CSRF from a page each caller may load (their session token is page-independent).
code="$(post m "/accounts/$CARD" --csrf-src=/accounts \
  --data-urlencode "name=Main Card" --data-urlencode "account_type=credit_card" \
  --data-urlencode "balance_type=liability" --data-urlencode "icon=la-coins")"
[[ "$code" == "403" ]] || fail "member without Edit grant: HTTP $code, want 403"
[[ "$(sql "SELECT COALESCE(icon,'') FROM account WHERE id=$CARD")" == "" ]] || fail "403 still wrote an icon"
# E (edit-all grant + account read) can set it.
code="$(post e "/accounts/$CARD" --csrf-src=/accounts \
  --data-urlencode "name=Main Card" --data-urlencode "account_type=credit_card" \
  --data-urlencode "balance_type=liability" --data-urlencode "icon=la-coins")"
[[ "$code" == "303" ]] || fail "edit-all grantee: HTTP $code, want 303"
[[ "$(sql "SELECT icon FROM account WHERE id=$CARD")" == "la-coins" ]] || fail "grantee icon not persisted"

echo "==> 9. icons render on all six surfaces (picked + defaults, no emojis)"
# Dashboard budget/spend card: the top-level Food & Drink row.
has u / 'la-utensils'
# /txns list + detail: the Coffee category icon (la-mug-hot after the edit).
has u /txns 'la-mug-hot'
has u "/txns/$APPROVED" 'la-mug-hot'
# Review queue: the pending Coffee row.
has u /review 'la-mug-hot'
# Accounts grid: the picked icons ...
has u /accounts 'la-piggy-bank'
has u /accounts 'la-dollar-sign'
has u /accounts 'la-coins'
# ... and the per-type default for the unpicked ewallet.
has u /accounts 'la-mobile-alt'
# Cards page: the credit card's picked icon.
has u /cards 'la-coins'
# Txn-form category select carries the category icons.
has u /txns/new 'la-mug-hot'
# Brand glyphs render with the lab style class (Line Awesome Brands font) —
# regression for blank ethereum/apple-pay in the live image (2026-11-20):
# the solid Free font has no brand codepoints, so the class must select lab.
has u /categories/new 'class="lab la-ethereum"'
has u "/categories/$CRYPTO" "iconPicker('la-ethereum', 'lab')"
# No emoji glyphs anywhere on the accounts grid (grep -P without -q:
# -q would early-exit and SIGPIPE the writer under pipefail on big pages).
ACCPAGE="$(page u /accounts)"
if [[ -n "$(printf '%s' "$ACCPAGE" | grep -Po '[\x{1F000}-\x{1FAFF}]')" ]]; then fail "accounts grid still renders an emoji"; fi

echo
echo "SMOKE D32: all checks passed"
