#!/usr/bin/env bash
# Real DB ownership/idempotency checks. Never accepts a production database URL.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
app_root="${1:-$root/../loyal-app}"
pg_bindir="$(pg_config --bindir)"
scratch="$(mktemp -d /tmp/earn-replay-verify.XXXXXX)"
port="$((59432 + RANDOM % 700))"
started=0
cleanup() {
  if [[ "$started" == 1 ]]; then
    "$pg_bindir/pg_ctl" -D "$scratch/data" -m immediate -w stop >/dev/null
  fi
  rm -rf "$scratch"
}
trap cleanup EXIT
mkdir -p "$scratch/socket"
"$pg_bindir/initdb" -D "$scratch/data" -A trust --no-locale -E UTF8 >/dev/null
"$pg_bindir/pg_ctl" -D "$scratch/data" -l "$scratch/postgres.log" \
  -o "-F -k '$scratch/socket' -p $port -c listen_addresses=127.0.0.1" -w start >/dev/null
started=1
psql_local() {
  "$pg_bindir/psql" -X -v ON_ERROR_STOP=1 -h "$scratch/socket" -p "$port" -U "$(id -un)" "$@"
}
psql_local -d postgres -c 'CREATE DATABASE earn_replay_verify' >/dev/null
url="postgresql://$(id -un)@127.0.0.1:$port/earn_replay_verify"
cd "$root"
NEON_DATABASE_URL="$url" NO_DNA=1 cargo run --quiet -p loyal-yield-orchestrator --bin yield-migrations -- --apply
for migration in 0001_add_user_yield_deposit_positions 0004_add_verifiable_earn_holdings 0011_add_packed_withdrawal_reserve_metadata 0012_add_withdrawal_source_metadata; do
  psql_local -d earn_replay_verify -f "$app_root/apps/web/src/lib/yield-optimization/migrations/$migration.sql" >/dev/null
done
EARN_REPLAY_TEST_DATABASE_URL="$url" NO_DNA=1 cargo test -p loyal-yield-store --lib \
  refund_cleanup_and_historical_withdrawal_replay --locked -- --ignored
