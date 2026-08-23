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

monitor_root="$routing_root/crates/balance-sweep-ata-monitor/src"
store_root="$routing_root/crates/loyal-yield-store"
migration="$store_root/migrations/0057_autodeposit_client_projection.sql"
db_test="$store_root/tests/autodeposit_client_projection_db.rs"

[[ -f "$migration" ]] || fail "migration 0057 is missing"
[[ -f "$db_test" ]] || fail "production-backed database contract test is missing"

printf '== LaserStream boundary\n'
rg --quiet 'earn_autodeposit_wallet_atas' "$monitor_root" ||
  fail "ready-wallet USDC ATA channel is missing"
rg --quiet 'earn_subscription_authorities' "$monitor_root" ||
  fail "subscription-authority channel is missing"
rg --quiet 'recurring_delegation' "$monitor_root/smart_account.rs" ||
  fail "configured recurring-delegation accounts are not watched"
if rg --quiet 'SubscribeRequestFilterTransactions|earn_smart_account_transactions' "$monitor_root"; then
  fail "a transaction subscription was added"
fi
git -C "$routing_root" diff --quiet origin/main -- render.yaml ||
  fail "ASK-2211 must not add or alter deployed services"
pass "one existing account-only LaserStream service owns Autodeposit observation"

printf '== Configuration and projection boundary\n'
for required in \
  autodeposit_vault_configs \
  autodeposit_chain_projections \
  desired_active \
  wallet_balance_floor_raw \
  expected_policy_account \
  expected_recurring_delegation \
  observation_start_slot \
  earn.autodeposit.changed; do
  rg --quiet --fixed-strings "$required" "$migration" "$store_root/src" "$monitor_root" ||
    fail "missing production contract: $required"
done
if rg --quiet 'setup_signature|close_signature|confirmed_slot' "$migration"; then
  fail "new projection schema depends on client confirmation evidence"
fi
pass "user configuration is separate from objective chain projection"

printf '== Production reconciliation contracts\n'
for required in \
  reconcile_autodeposit_snapshot \
  AutodepositProjectionStatus \
  Pending \
  Active \
  Closed \
  Inconsistent \
  effective_autodeposit_active; do
  rg --quiet --fixed-strings "$required" "$store_root/src" "$monitor_root" ||
    fail "missing production reconciler contract: $required"
done
pass "one snapshot reconciler derives effective Autodeposit state"

printf '== Disposable PostgreSQL contract\n'
scratch_dir="$(mktemp -d /private/tmp/ask-2211-autodeposit.XXXXXX)"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
postgres_log="$scratch_dir/postgres.log"
database_name="ask_2211_autodeposit_${$}"
port=$((56200 + ($$ % 500)))
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
    cargo test -p loyal-yield-store --test autodeposit_client_projection_db \
      -- --ignored --nocapture
)
pass "production migrations and store writers satisfy the database contract"

printf '== Focused Rust checks\n'
(
  cd "$routing_root"
  cargo test -p balance-sweep-ata-monitor autodeposit -- --nocapture
  cargo test -p loyal-yield-store autodeposit_client_projection -- --nocapture
  cargo check -p balance-sweep-ata-monitor -p loyal-yield-store -p loyal-yield-realtime
  cargo fmt --all -- --check
  git diff --check
)
pass "focused Rust, formatting, and worktree hygiene checks"

printf 'PASS: ASK-2211 Autodeposit is client-sent and LaserStream-reconciled\n'
