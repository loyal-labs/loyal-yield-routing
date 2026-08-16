#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
scratch_dir=$(mktemp -d /private/tmp/cross-mint-store.XXXXXX)
data_dir="$scratch_dir/data"
postgres_log="$scratch_dir/postgres.log"
database_name="cross_mint_store_test_fleet_verify_${$}"
port=$((55400 + ($$ % 400)))

if [[ -x /opt/homebrew/opt/postgresql@17/bin/postgres ]]; then
  pg_bindir=/opt/homebrew/opt/postgresql@17/bin
else
  pg_bindir=$(pg_config --bindir)
fi

if [[ ! -x "$pg_bindir/postgres" ]]; then
  printf 'PostgreSQL server is unavailable under %s\n' "$pg_bindir" >&2
  exit 1
fi

started=false
cleanup() {
  if [[ "$started" == true ]]; then
    "$pg_bindir/pg_ctl" -D "$data_dir" -m fast stop >/dev/null 2>&1 || true
  fi
  rm -rf -- "$scratch_dir"
}
trap cleanup EXIT

while "$pg_bindir/pg_isready" -h 127.0.0.1 -p "$port" >/dev/null 2>&1; do
  port=$((port + 1))
done

"$pg_bindir/initdb" -D "$data_dir" -A trust --no-locale >/dev/null
"$pg_bindir/pg_ctl" \
  -D "$data_dir" \
  -l "$postgres_log" \
  -o "-p $port -h 127.0.0.1" \
  start >/dev/null
started=true
"$pg_bindir/createdb" -h 127.0.0.1 -p "$port" "$database_name"

database_url="postgresql://127.0.0.1:$port/$database_name"

cd "$repo_root"
NEON_DATABASE_URL="$database_url" \
  cargo run -q -p loyal-yield-orchestrator --bin yield-migrations -- --apply
CROSS_MINT_STORE_TEST_DATABASE_URL="$database_url" \
  cargo test -p loyal-yield-store \
    --test cross_mint_movement_db \
    --test cross_mint_swap_policy_db \
    -- --ignored --nocapture
cargo run -q -p loyal-yield-orchestrator \
  --bin fleet-orchestration-verifier -- \
  --implementation --json --isolated-database --database-url "$database_url"
