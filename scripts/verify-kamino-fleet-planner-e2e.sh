#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scratch="$(mktemp -d /tmp/kamino-fleet-planner-e2e.XXXXXX)"
data="$scratch/postgres"
socket="$scratch/socket"
port="$((59600 + RANDOM % 200))"
server_started=0

fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() {
  if [[ "$server_started" == 1 ]]; then
    pg_ctl -D "$data" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch"
}
trap cleanup EXIT

for command_name in go cargo initdb pg_ctl createdb psql; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

echo "== Go verifier-first checks"
cd "$root/go/kamino-fleet-planner"
go test ./...
go test -race ./internal/fleet -run 'Test(Decode|Plan|EconomicKey|Snapshot)'
echo "PASS: deterministic planner, frozen KLend decoder, and adversarial slot fences"

echo "== Disposable PostgreSQL"
mkdir -p "$socket"
initdb -D "$data" -A trust --no-locale -E UTF8 >/dev/null
pg_ctl -D "$data" \
  -o "-F -k '$socket' -p $port -c listen_addresses=127.0.0.1" \
  -w start >/dev/null
server_started=1
createdb -h "$socket" -p "$port" fleet
database_url="postgresql://$(id -un)@127.0.0.1:$port/fleet"

cd "$root"
cargo build -p loyal-yield-orchestrator --bin yield-migrations >/dev/null
set +e
migration_output="$(NEON_DATABASE_URL="$database_url" target/debug/yield-migrations --apply 2>&1)"
migration_status=$?
set -e
if [[ "$migration_status" -ne 0 ]]; then
  # 0071 is intentionally bound to one pre-existing production row and cannot
  # run on a blank database. Everything before it is transactional and remains
  # applied; install only this change after checking the exact known fence.
  [[ "$migration_output" == *"Backyard Phase 1 canonical route cardinality drifted"* ]] || {
    echo "$migration_output" >&2
    fail "base migrations failed before the known production-bound activation"
  }
  [[ "$(psql "$database_url" -X -Atc "SELECT count(*) FROM pg_tables WHERE schemaname='loyal_yield' AND tablename IN ('optimizer_epochs','rebalance_opportunities','multiply_route_states')")" == "3" ]] ||
    fail "base durable fleet schema is incomplete"
  psql "$database_url" -X -v ON_ERROR_STOP=1 \
    -f crates/loyal-yield-store/migrations/0072_kamino_fleet_planner_owner.sql >/dev/null
fi
[[ "$(psql "$database_url" -X -Atc "SELECT to_regclass('loyal_yield.kamino_fleet_planner_owners') IS NOT NULL")" == "t" ]] ||
  fail "Kamino fleet planner owner migration is missing"
echo "PASS: production queue schema and fenced owner schema are available"

echo "== Confirmed RPC to durable W3 queue"
cd "$root/go/kamino-fleet-planner"
FLEET_TEST_DATABASE_URL="$database_url" go test ./internal/fleet \
  -run 'Test(StoreIntegration|WorkerIntegration)' -count=1 -v

echo "PASS: coherent confirmed reserve updates planned from memory and reached the existing revalidate queue"
echo "PASS: restart watermark, economic idempotency, active-work exclusion, and stale-owner fencing verified"
