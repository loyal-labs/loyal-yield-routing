#!/usr/bin/env bash
# Run scripts/verify-workers-v2.sh against a throwaway local Postgres, set up
# the way the Workers v2 CI job sets up its service: the fixture databases,
# the Phase3 socket database, then the full verify. Timescale is not started,
# so its suites skip; missing SVM fixtures are built first.
set -euo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"

pg_bin="${PG_BIN:-}"
if test -z "$pg_bin"; then
  for candidate in /opt/homebrew/opt/postgresql@17/bin /opt/homebrew/opt/postgresql@18/bin "$(pg_config --bindir 2>/dev/null || true)"; do
    if test -x "$candidate/postgres"; then pg_bin="$candidate"; break; fi
  done
fi
test -x "$pg_bin/postgres" || { echo "no Postgres server found; set PG_BIN to its bin directory" >&2; exit 1; }
export PATH="$pg_bin:$PATH"

# With a database, the connected SVM tests require their fixtures.
export SVM_HARNESS="$repo/target/debug/examples/fleet-local-svm"
export MULTIPLY_FIXTURE_BIN="$repo/target/svm-fixtures/workers-v2-multiply"
export AUTODEPOSIT_TEST_SVM_FIXTURE_PATH="$repo/target/svm-fixtures/autodeposit.json"
export MOCK_YIELD_PROTOCOLS_PROGRAM_SO="$repo/target/deploy/mock_yield_protocols_program.so"
export SQUADS_SMART_ACCOUNT_PROGRAM_SO="$repo/crates/squads-test-harness/fixtures/squads/squads_smart_account_program.so"
for path in "$SVM_HARNESS" "$MULTIPLY_FIXTURE_BIN" "$AUTODEPOSIT_TEST_SVM_FIXTURE_PATH" "$MOCK_YIELD_PROTOCOLS_PROGRAM_SO"; do
  if ! test -s "$path"; then
    echo "building the connected SVM fixtures ($path is missing)"
    "$repo/scripts/build-svm-fixtures.sh"
    break
  fi
done

# The Phase3 tests require a socket under this prefix.
dir="/private/tmp/backyard-phase3-pg.$$"
mkdir "$dir"
cleanup() {
  pg_ctl -D "$dir/data" -m immediate stop >/dev/null 2>&1 || true
  rm -rf "$dir"
}
trap cleanup EXIT

port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
initdb -D "$dir/data" -U workers_v2 --auth=trust -E UTF8 --no-locale --no-sync >/dev/null
# The fixture refuses anything but a 127.0.0.1 TCP URL, and Phase3 refuses
# anything but the socket, so the server listens on both.
pg_ctl -D "$dir/data" -l "$dir/postgres.log" -w -o "-k $dir -p $port -c listen_addresses=127.0.0.1 -c fsync=off" start >/dev/null
createdb -h "$dir" -p "$port" -U workers_v2 workers_v2_bootstrap

started=$SECONDS
GITHUB_ENV="$dir/env" WORKERS_V2_DISPOSABLE=1 \
  WORKERS_V2_FIXTURE_URL="postgresql://workers_v2@127.0.0.1:$port/workers_v2_bootstrap" \
  python3 "$repo/scripts/workers-v2-fixture.py"
echo "fixture: $((SECONDS - started))s"
set -a
. "$dir/env"
set +a
psql -h "$dir" -p "$port" -U workers_v2 -d workers_v2_bootstrap -X -q -v ON_ERROR_STOP=1 -c 'CREATE DATABASE phase3_budget_test'
export PHASE3_TEST_DATABASE_URL="postgresql://workers_v2@/phase3_budget_test?host=$dir&port=$port"

log="${TMPDIR:-/tmp}/workers-v2-verify-db.log"
status=0
GOFLAGS=-v bash "$repo/scripts/verify-workers-v2.sh" >"$log" 2>&1 || status=$?
grep -E '^(ok|FAIL|---( FAIL)?|panic:)|"gate"' "$log" | grep -v -- '--- PASS\|--- SKIP' || true
echo "skipped tests by reason:"
# go test -v prints a skip's reason on the line before its --- SKIP.
awk '/^ *--- SKIP: / && previous ~ /^ +[^ ]+\.go:[0-9]+: / {sub(/^ +[^ ]+\.go:[0-9]+: /, "", previous); print previous} {previous = $0}' "$log" | sort | uniq -c | sort -rn
echo "full go test -v output: $log"
exit "$status"
