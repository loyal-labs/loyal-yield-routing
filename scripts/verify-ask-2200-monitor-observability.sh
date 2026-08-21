#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

for command_name in cargo initdb pg_ctl createdb pg_config rg; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

required_metrics=(
  loyal.laserstream.cursor.slot
  loyal.earn.reconciliation.pending
  loyal.earn.reconciliation.failed_pending
  loyal.earn.reconciliation.oldest_pending_age
)
for metric_name in "${required_metrics[@]}"; do
  rg -q --fixed-strings "$metric_name" crates/balance-sweep-ata-monitor \
    || fail "missing metric contract: $metric_name"
done

rg -q --fixed-strings 'earn_reconciliation_job_failed' \
  crates/balance-sweep-ata-monitor \
  || fail "retained proof failures are not exported as operational errors"
rg -q --fixed-strings 'earn_reconciliation_consumer_failed' \
  crates/balance-sweep-ata-monitor \
  || fail "consumer-loop failures are not exported as operational errors"

if rg -q 'advanced_slots|cursor[_\.]advance(_rate|ment_speed)' \
  crates/balance-sweep-ata-monitor crates/loyal-observability; then
  fail "cursor speed has a duplicate local counter or calculation"
fi

scratch_dir="$(mktemp -d /private/tmp/ask-2200-observability.XXXXXX)"
data_dir="$scratch_dir/postgres"
postgres_log="$scratch_dir/postgres.log"
database_name="ask_2200_observability"
port=$((55800 + ($$ % 500)))
pg_bindir="$(pg_config --bindir)"
server_started=0

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    "$pg_bindir/pg_ctl" -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  if [[ "$scratch_dir" == /private/tmp/ask-2200-observability.* ]]; then
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

echo "== ASK-2200 focused Rust contracts"
NO_DNA=1 cargo test -p loyal-observability --locked
NO_DNA=1 cargo test -p balance-sweep-ata-monitor --lib monitor_observability

echo "== ASK-2200 isolated durable-state regression"
ASK_2200_TEST_DATABASE_URL="$database_url" \
  NO_DNA=1 cargo test -p balance-sweep-ata-monitor \
    --test monitor_observability_db -- --ignored --nocapture

echo "== ASK-2200 build and hygiene"
NO_DNA=1 cargo check -p loyal-observability -p loyal-yield-store \
  -p balance-sweep-ata-monitor --locked
NO_DNA=1 cargo fmt --all -- --check
git diff --check

echo "PASS: ASK-2200 authoritative monitor state, OTLP metrics, and errors verified"
echo "NOT RUN: image publication, deploys, ClickStack dashboards/alerts, and live canaries"
