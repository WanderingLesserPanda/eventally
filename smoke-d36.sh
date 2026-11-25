#!/usr/bin/env bash
# D36 HTTP smoke — icon colours (docs/notes/d36-icon-colour-palette.md).
#
# Scratch throwaway DB (created, seeded, dropped by trap); the app binary is
# built and booted against it (auto-migration to v21 included). Neutral seed
# labels only (U, M; "Main household") — no names.
#
# Verified here (spec verification section):
#   * both forms carry the Colour popover (16 preset swatches + "No colour"
#     + custom hex + native picker) and the hidden `colour` input;
#   * a category saved with a PRESET and with a CUSTOM hex renders tinted
#     (style="color:#...") on the live surfaces (same shared iconTag helper
#     drives all six surfaces);
#   * an invalid hex is accepted (row created) but the colour is stored NULL
#     (ADR-0013);
#   * clearing the colour stores NULL (glyph falls back to inherited colour);
#   * the account colour is gated by the account Edit grant (owner 303, plain
#     member 403); the category colour is open to any household member.
#
# Run: ./smoke-d36.sh
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck disable=SC1091
export LD_LIBRARY_PATH="${LD_LIBRARY_PATH:-}"
source /opt/data/cache/pgenv.sh

PORT="${SMOKE_PORT:-18093}"
BASE="http://127.0.0.1:${PORT}"
DB="d36smoke_$$"
WORK="$(mktemp -d)"
BIN="$WORK/eventally"
JARS="$WORK/jars"
PWHASH=".d36_pwhash"
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

echo "==> booting app on :${PORT} (auto-migrates the scratch db to v21)"
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
 (1, 'u', 'U', 'u@example.test',  '${HASH}'),
 (1, 'm', 'M', 'm@example.test',  '${HASH}');
INSERT INTO household_member (user_id, household_id, role)
SELECT u.id, h.id, r.role
FROM (VALUES ('u',1,'owner'), ('m',1,'member')) AS r(username, hh, role)
JOIN app_user u ON u.username = r.username
JOIN household h ON h.id = r.hh;
-- One account owned by U. The creator-grant trigger (migration 000012) gives
-- the creator a full grant on insert, so no manual account_grant is needed.
-- M is a plain member (household membership only, no account grant) -> can
-- edit categories, cannot read/edit the account.
INSERT INTO account (household_id, name, account_type, balance_type, created_by_user_id)
VALUES
 (1, 'Main Account', 'savings', 'asset', (SELECT id FROM app_user WHERE username='u'));
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
has() { local body rc=0; body="$(page "$1" "$2")" || rc=$?
  if [[ "$body" != *"$3"* ]]; then fail "$1 $2 — expected: $3"; fi; }
not_has() { local body rc=0; body="$(page "$1" "$2")" || rc=$?
  if [[ "$body" == *"$3"* ]]; then fail "$1 $2 — unexpected: $3"; fi; }
sql() { psql -tAq -d "$DB" -c "$1" || { echo "FAIL: psql: $1" >&2; exit 1; }; }

ACC_ID="$(sql "SELECT id FROM account WHERE name='Main Account'")"

login u; login m

PRESET="b84810"   # Terracotta — a real palette swatch
CUSTOM="123abc"   # valid 6-hex, deliberately NOT in the 16-palette

# ---- 1. the Colour popover is present on both forms ----
echo "==> 1. Colour popover markup on both forms"
has u /categories/new 'colourPicker'
has u /categories/new 'name="colour"'
has u /categories/new 'No colour (default)'
has u /categories/new 'type="color"'
has u /categories/new 'colour-hex'
NSW="$(page u /categories/new | grep -o 'class="colour-swatch"' | wc -l | tr -d ' ' || true)"
[[ "$NSW" == "16" ]] || fail "category form swatches: $NSW, want 16"
has u /accounts/new 'colourPicker'
has u /accounts/new 'name="colour"'
has u /accounts/new 'type="color"'
NSWA="$(page u /accounts/new | grep -o 'class="colour-swatch"' | wc -l | tr -d ' ' || true)"
[[ "$NSWA" == "16" ]] || fail "account form swatches: $NSWA, want 16"

# ---- 2. plain member (m) creates a category with a PRESET colour ----
echo "==> 2. plain member creates a category with a preset colour"
code="$(post m /categories --csrf-src=/categories/new \
  --data-urlencode "name=Preset Cat" --data-urlencode "transaction_class=expense" \
  --data-urlencode "colour=${PRESET}")"
[[ "$code" == "303" ]] || fail "category create (preset): HTTP $code"
CID="$(sql "SELECT id FROM category WHERE household_id=1 AND name='Preset Cat'")"
[[ -n "$CID" ]] || fail "preset category not created"
[[ "$(sql "SELECT colour FROM category WHERE id=$CID")" == "$PRESET" ]] || fail "preset colour not stored"
has u /categories "style=\"color:#${PRESET}\""

# ---- 3. a CUSTOM hex (not in the palette) is accepted identically ----
echo "==> 3. custom hex (not in palette) is accepted"
code="$(post m /categories --csrf-src=/categories/new \
  --data-urlencode "name=Custom Cat" --data-urlencode "transaction_class=expense" \
  --data-urlencode "colour=${CUSTOM}")"
[[ "$code" == "303" ]] || fail "category create (custom): HTTP $code"
CID2="$(sql "SELECT id FROM category WHERE household_id=1 AND name='Custom Cat'")"
[[ -n "$CID2" ]] || fail "custom category not created"
[[ "$(sql "SELECT colour FROM category WHERE id=$CID2")" == "$CUSTOM" ]] || fail "custom colour not stored"
has u /categories "style=\"color:#${CUSTOM}\""

# ---- 4. an INVALID hex: row is created, colour stored NULL (ADR-0013) ----
echo "==> 4. invalid hex is rejected (row created, colour NULL)"
code="$(post m /categories --csrf-src=/categories/new \
  --data-urlencode "name=Bad Cat" --data-urlencode "transaction_class=expense" \
  --data-urlencode "colour=#GG0000")"
[[ "$code" == "303" ]] || fail "category create (bad hex): HTTP $code"
CID3="$(sql "SELECT id FROM category WHERE household_id=1 AND name='Bad Cat'")"
[[ -n "$CID3" ]] || fail "bad-hex category row not created"
[[ "$(sql "SELECT COALESCE(colour,'') FROM category WHERE id=$CID3")" == "" ]] \
  || fail "invalid hex stored, want NULL"

# ---- 5. clearing the colour returns NULL (glyph falls back to inherited) ----
echo "==> 5. clearing the colour stores NULL"
code="$(post m "/categories/$CID" --csrf-src="/categories/$CID" \
  --data-urlencode "name=Preset Cat" --data-urlencode "transaction_class=expense" \
  --data-urlencode "colour=")"
[[ "$code" == "303" ]] || fail "category edit (clear): HTTP $code"
[[ "$(sql "SELECT COALESCE(colour,'') FROM category WHERE id=$CID")" == "" ]] \
  || fail "clear did not NULL the colour"

# ---- 6. account colour: owner 303 + tinted; plain member 403 ----
echo "==> 6. account colour gated by the Edit grant"
code="$(post u "/accounts/$ACC_ID" --csrf-src="/accounts/$ACC_ID" \
  --data-urlencode "name=Main Account" --data-urlencode "account_type=savings" \
  --data-urlencode "balance_type=asset" --data-urlencode "colour=${PRESET}")"
[[ "$code" == "303" ]] || fail "account edit (owner): HTTP $code"
[[ "$(sql "SELECT colour FROM account WHERE id=$ACC_ID")" == "$PRESET" ]] || fail "account colour not stored"
has u /accounts "style=\"color:#${PRESET}\""

code="$(post m "/accounts/$ACC_ID" --csrf-src="/accounts/$ACC_ID" \
  --data-urlencode "name=Main Account" --data-urlencode "account_type=savings" \
  --data-urlencode "balance_type=asset" --data-urlencode "colour=${CUSTOM}")"
[[ "$code" == "403" ]] || fail "account edit (plain member): HTTP $code, want 403"
# The member's rejected write did not mutate the account colour.
[[ "$(sql "SELECT colour FROM account WHERE id=$ACC_ID")" == "$PRESET" ]] \
  || fail "rejected write mutated the account colour"

echo
echo "SMOKE D36: all checks passed"
