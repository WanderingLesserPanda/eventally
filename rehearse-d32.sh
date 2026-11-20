#!/usr/bin/env bash
# D32 cutover rehearsal — copy eventally_dev via TEMPLATE, boot the app
# against the copy (auto-migration v16 -> v19), and verify migration 000019
# is safe on the real data:
#
#   1.  goose version on the copy reaches 19 (auto-migration on boot)
#   2.  the icon columns are additive and empty: both tables gained
#       icon TEXT, and every pre-existing row is icon IS NULL (no backfill —
#       unpicked rows render with defaults on day one)
#   3.  existing dashboard/budget totals unchanged (transaction + category
#       actuals + budgets, compared before/after, full result sets)
#   4.  the full Go test suite green (house smoke suite + the D32 tests +
#       the D33/D34 isolation probes)
#
# STANDING GATE: the live eventally_dev migration happens only AFTER this
# rehearsal passes, an explicit user green light, and a fresh template-DB
# backup. This script never migrates eventally_dev itself.
#
# Run: ./rehearse-d32.sh
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck disable=SC1091
export LD_LIBRARY_PATH="${LD_LIBRARY_PATH:-}"
source /opt/data/cache/pgenv.sh

DEV_DB="eventally_dev"
REHEARSE="d32rehearse_$$"
PORT="${REHEARSE_PORT:-18097}"
BASE="http://127.0.0.1:${PORT}"
WORK="$(mktemp -d)"
BIN="$WORK/eventally"

SRV=""
cleanup() {
  [[ -n "$SRV" ]] && kill "$SRV" 2>/dev/null || true
  psql -q -d postgres -c "DROP DATABASE IF EXISTS ${REHEARSE};" >/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() { echo "FAIL: $1"; exit 1; }
sql() { psql -tAq -d "$1" -c "$2" || { echo "FAIL: psql ($1): $2" >&2; exit 1; }; }

# Totals snapshot: the data the dashboard/budget cards are built from.
# Full result sets, compared before (v16) and after (v19) the migration.
totals() { # $1=db
  {
    psql -tAq -d "$1" -c "
      SELECT household_id, category_id, coalesce(sum(total_amount),0)
      FROM \"transaction\"
      WHERE record_status <> 'void' AND class = 'expense'
        AND transaction_date >= date_trunc('month', current_date)
      GROUP BY 1,2 ORDER BY 1,2"
    psql -tAq -d "$1" -c "
      SELECT household_id, coalesce(sum(budget_per_month),0)
      FROM category WHERE is_budgetable GROUP BY 1 ORDER BY 1"
    psql -tAq -d "$1" -c "
      SELECT household_id, count(*), coalesce(sum(total_amount),0)
      FROM \"transaction\" GROUP BY 1 ORDER BY 1"
  } | md5sum | cut -d' ' -f1
}

echo "==> template-copying ${DEV_DB} -> ${REHEARSE}"
# TEMPLATE needs no other connections on the source.
psql -q -d postgres -c "
  SELECT pg_terminate_backend(pid) FROM pg_stat_activity
  WHERE datname = '${DEV_DB}' AND pid <> pg_backend_pid();" >/dev/null
createdb -T "$DEV_DB" "$REHEARSE" >/dev/null

BEFORE="$(totals "$REHEARSE")"
BEFORE_V="$(sql "$REHEARSE" "SELECT max(version_id) FROM goose_db_version")"
echo "    copy is at goose version ${BEFORE_V}"

echo "==> building"
go build -o "$BIN" ./cmd/eventally

echo "==> booting app on :${PORT} against the copy (auto-migrates v${BEFORE_V} -> v19)"
EVENTALLY_SECRET="$(openssl rand -hex 32)" \
EVENTALLY_ADDR="127.0.0.1:${PORT}" \
EVENTALLY_DATA_DIR="$WORK/data" \
EVENTALLY_DSN="host=127.0.0.1 port=5432 user=hermes dbname=${REHEARSE} sslmode=disable" \
"$BIN" >"$WORK/server.log" 2>&1 &
SRV="$!"
for _ in $(seq 60); do
  curl -sf -o /dev/null "$BASE/login" && break
  sleep 0.5
done
curl -sf -o /dev/null "$BASE/login" || { echo "FAIL: server did not come up"; cat "$WORK/server.log"; exit 1; }

echo "==> 1. goose version"
v="$(sql "$REHEARSE" "SELECT max(version_id) FROM goose_db_version")"
[[ "$v" == "19" ]] || fail "goose version $v, want 19 (see $WORK/server.log)"

echo "==> 2. icon columns are additive and empty (no backfill)"
cols="$(sql "$REHEARSE" "SELECT table_name || ':' || data_type FROM information_schema.columns WHERE table_name IN ('category','account') AND column_name='icon' ORDER BY 1")"
expected=$(printf "account:text\ncategory:text")
[[ "$cols" == "$expected" ]] || fail "icon columns: got [$cols], want [$expected]"
n="$(sql "$REHEARSE" "SELECT (SELECT count(*) FROM category WHERE icon IS NOT NULL) + (SELECT count(*) FROM account WHERE icon IS NOT NULL)")"
[[ "$n" == "0" ]] || fail "$n pre-existing rows have an icon (migration must not backfill)"

echo "==> 3. dashboard/budget totals unchanged"
AFTER="$(totals "$REHEARSE")"
[[ "$BEFORE" == "$AFTER" ]] || fail "totals changed: before=$BEFORE after=$AFTER"

echo "==> 4. Go test suite (house smoke + D32 + D33/D34 isolation probes)"
EVENTALLY_TEST_DSN="postgres://hermes@127.0.0.1:5432/postgres" go test ./... -count=1 | tail -8

echo
echo "REHEARSAL D32: all checks passed."
echo
echo "Standing gate — the live ${DEV_DB} migration (v${BEFORE_V} -> v19) happens only"
echo "after: (a) this rehearsal passes [done], (b) an explicit user green"
echo "light, and (c) a fresh template-DB backup."
