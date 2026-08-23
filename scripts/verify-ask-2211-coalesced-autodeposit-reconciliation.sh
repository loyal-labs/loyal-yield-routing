#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

pass() {
  printf 'PASS: %s\n' "$*"
}

routing_root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
[[ -e "$routing_root/.git" ]] || fail "routing root is not a Git worktree"

for command_name in cargo git initdb pg_config pg_ctl rg; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

store_root="$routing_root/crates/loyal-yield-store"
monitor_root="$routing_root/crates/balance-sweep-ata-monitor"
migration="$store_root/migrations/0061_coalesced_autodeposit_reconciliation.sql"
db_test="$store_root/tests/autodeposit_reconciliation_requests_db.rs"

[[ -f "$migration" ]] || fail "migration 0061 is missing"
[[ -f "$db_test" ]] || fail "coalesced reconciliation database contract test is missing"

printf '== Bounded queue schema\n'
for required in \
  autodeposit_reconciliation_requests \
  requested_slot \
  processed_slot \
  claim_owner \
  claim_expires_at; do
  rg --quiet --fixed-strings "$required" "$migration" "$store_root/src" ||
    fail "missing bounded queue contract: $required"
done
if rg --ignore-case --quiet 'raw_evidence|account_data|event_payload|vault_payload|transaction_payload' "$migration"; then
  fail "bounded queue stores raw blockchain or event payloads"
fi
rg --quiet 'PRIMARY KEY.*target_id|UNIQUE.*target_id' "$migration" ||
  fail "request cardinality is not bounded to one row per target"
pass "request schema is bounded and stores no raw chain history"

printf '== Account-only observation boundary\n'
if rg --quiet 'SubscribeRequestFilterTransactions|earn_smart_account_transactions' "$monitor_root/src"; then
  fail "a LaserStream transaction subscription was added"
fi
rg --quiet 'get_multiple_accounts_with_config' "$monitor_root/src/earn_reconciliation.rs" ||
  fail "finalized batched account snapshot is missing"
rg --quiet 'EARN_RECONCILIATION_CONCURRENCY: usize = 4' "$monitor_root/src/main.rs" ||
  fail "unrelated Earn vaults are still processed by one global worker"
pass "LaserStream remains an invalidation stream and RPC remains the snapshot authority"

printf '== Disposable PostgreSQL behavior contract\n'
scratch_dir="$(mktemp -d /private/tmp/ask-2211-coalesced.XXXXXX)"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
postgres_log="$scratch_dir/postgres.log"
database_name="ask_2211_coalesced_${$}"
port=$((56800 + ($$ % 300)))
started=false
cleanup() {
  if [[ "$started" == true ]]; then
    "$(pg_config --bindir)/pg_ctl" -D "$data_dir" -m fast stop >/dev/null 2>&1 || true
  fi
  rm -rf -- "$scratch_dir"
}
trap cleanup EXIT
mkdir -p "$socket_dir"
"$(pg_config --bindir)/initdb" -D "$data_dir" -A trust --no-locale >/dev/null
"$(pg_config --bindir)/pg_ctl" -D "$data_dir" -l "$postgres_log" \
  -o "-p $port -h 127.0.0.1 -k $socket_dir" start >/dev/null
started=true
"$(pg_config --bindir)/createdb" -h 127.0.0.1 -p "$port" "$database_name"
database_url="postgresql://127.0.0.1:$port/$database_name"

(
  cd "$routing_root"
  NEON_DATABASE_URL="$database_url" NO_DNA=1 \
    cargo run -q -p loyal-yield-orchestrator --bin yield-migrations -- --apply
  ASK_2211_VERIFY_DATABASE_URL="$database_url" \
    cargo test -p loyal-yield-store --test autodeposit_reconciliation_requests_db \
      -- --ignored --nocapture
)
pass "coalescing, high-water, lease, and independent-target contracts hold"

printf '== Focused Rust checks\n'
(
  cd "$routing_root"
  cargo test -p balance-sweep-ata-monitor autodeposit -- --nocapture
  cargo test -p balance-sweep-ata-monitor policy_discovery_event_key -- --nocapture
  cargo test -p loyal-yield-store --lib autodeposit -- --nocapture
  cargo check -p balance-sweep-ata-monitor -p loyal-yield-store
  cargo fmt --all -- --check
  git diff --check
)
pass "focused Rust, formatting, and worktree checks pass"

printf 'PASS: ASK-2211 Autodeposit reconciliation is coalesced and bounded\n'
