#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

for command_name in cargo initdb pg_config pg_ctl createdb; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

scratch_dir="$(mktemp -d /private/tmp/autodeposit-reconciliation-alerts.XXXXXX)"
data_dir="$scratch_dir/postgres"
postgres_log="$scratch_dir/postgres.log"
database_name="autodeposit_reconciliation_alerts"
port=$((56300 + ($$ % 400)))
pg_bindir="$(pg_config --bindir)"
server_started=0

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    "$pg_bindir/pg_ctl" -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  if [[ "$scratch_dir" == /private/tmp/autodeposit-reconciliation-alerts.* ]]; then
    rm -rf -- "$scratch_dir"
  fi
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
  -w start >/dev/null
server_started=1
"$pg_bindir/createdb" -h 127.0.0.1 -p "$port" "$database_name"
database_url="postgresql://127.0.0.1:$port/$database_name"

echo "== Isolated production-schema database"
NEON_DATABASE_URL="$database_url" NO_DNA=1 \
  cargo run --quiet -p loyal-yield-orchestrator --bin yield-migrations -- --apply

echo "== Autodeposit reconciliation RPC-lag E2E"
AUTODEPOSIT_RECONCILIATION_ALERTS_TEST_DATABASE_URL="$database_url" NO_DNA=1 \
  cargo test -p balance-sweep-ata-monitor \
    --test autodeposit_reconciliation_alerts_db \
    rpc_lag_retries_resolve_with_expected_alerts -- --ignored --exact --nocapture

echo "== Focused contracts and hygiene"
NO_DNA=1 cargo test -p balance-sweep-ata-monitor --lib \
  earn_reconciliation::tests::minimum_context_slot_lag_uses_the_shared_delayed_alert_policy \
  -- --exact
NO_DNA=1 cargo check -p balance-sweep-ata-monitor -p loyal-yield-store --locked
cargo fmt --all -- --check
git diff --check

echo "PASS: immediate and transient Autodeposit reconciliation drained with zero alerts"
echo "PASS: persistent RPC lag emitted one Autodeposit-specific alert at attempt 6 and then drained"
