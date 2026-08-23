#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

routing_root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
app_root="${2:-${LOYAL_APP_DIR:-}}"

[[ -e "$routing_root/.git" ]] || fail "routing root is not a Git worktree"
[[ -n "$app_root" && -e "$app_root/.git" ]] || fail "Loyal App worktree is required"

for command_name in bun cargo git initdb pg_config pg_ctl rg; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

web_api="$app_root/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit"
mobile_api="$app_root/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit"
web_client="$app_root/apps/web/src/hooks/use-smart-account-sidebar-data.ts"
mobile_client="$app_root/apps/mobile/src/lib/solana/earn/autodeposit.ts"
monitor_root="$routing_root/crates/balance-sweep-ata-monitor/src"
store_root="$routing_root/crates/loyal-yield-store"
executor="$routing_root/scripts/execute-autodeposit-policy.ts"

printf '== Client transaction ownership\n'
for removed_route in \
  "$web_api/setup/prepare/route.ts" \
  "$web_api/setup/confirm/route.ts" \
  "$web_api/close/prepare/route.ts" \
  "$web_api/close/confirm/route.ts" \
  "$mobile_api/setup/prepare/route.ts" \
  "$mobile_api/setup/confirm/route.ts" \
  "$mobile_api/close/prepare/route.ts" \
  "$mobile_api/close/confirm/route.ts"; do
  [[ ! -e "$removed_route" ]] || fail "legacy transaction endpoint remains: $removed_route"
done

rg --quiet 'prepareEarnUsdcAutodepositSetup|prepareEarnAutodepositSetup' \
  "$web_client" "$mobile_client" || fail "clients do not prepare setup locally"
rg --quiet 'prepareEarnUsdcAutodepositClose|prepareEarnAutodepositClose' \
  "$web_client" "$mobile_client" || fail "clients do not prepare close locally"
if rg --quiet 'autodeposit/(setup|close)/(prepare|confirm)' \
  "$app_root/apps/web/src" "$app_root/apps/mobile/src"; then
  fail "a client or backend module still uses a legacy Autodeposit transaction endpoint"
fi
if rg --quiet 'getSignaturesForAddress|searchTransactionHistory' \
  "$web_api" "$mobile_api" "$monitor_root"; then
  fail "Autodeposit still scans transaction history"
fi

for kept_route in \
  "$app_root/apps/web/src/app/api/smart-accounts/yield-optimization/earn-state/route.ts" \
  "$web_api/floor/confirm/route.ts" \
  "$web_api/toggle/confirm/route.ts" \
  "$web_api/sweeps/execute/route.ts" \
  "$mobile_api/state/route.ts" \
  "$mobile_api/floor/confirm/route.ts" \
  "$mobile_api/toggle/confirm/route.ts" \
  "$mobile_api/sweeps/execute/route.ts"; do
  [[ -f "$kept_route" ]] || fail "required backend control is missing: $kept_route"
done

printf '== LaserStream discovery boundary\n'
if rg --quiet 'SubscribeRequestFilterTransactions|earn_smart_account_transactions' "$monitor_root"; then
  fail "Autodeposit added a transaction subscription"
fi
if rg --quiet 'autodeposit/(intent|register)|observation_start_slot|expected_policy_account' \
  "$app_root/apps/web/src" "$app_root/apps/mobile/src"; then
  fail "client watch registration remains"
fi
rg --quiet 'policy_transaction_for' "$monitor_root/earn_reconciliation.rs" ||
  fail "stable-account update cannot discover the exact policy transaction"
rg --quiet 'load_earn_subscription_targets' "$monitor_root" ||
  fail "existing smart-account watch catalog is not used"

printf '== Single target state\n'
active_store_sources=(
  "$store_root/src/store.rs"
  "$store_root/src/types.rs"
  "$monitor_root"
)
if rg --quiet 'autodeposit_vault_configs|autodeposit_chain_projections' "${active_store_sources[@]}"; then
  fail "production code still depends on parallel Autodeposit state tables"
fi
for required in \
  desired_active \
  chain_status \
  chain_observation_slot \
  bootstrap_generation \
  earn.autodeposit.configuration.changed; do
  rg --quiet --fixed-strings "$required" "$store_root/migrations" "$store_root/src" "$monitor_root" ||
    fail "missing single-target contract: $required"
done

rg --quiet 't\.desired_active' "$executor" ||
  fail "executor does not enforce desired Autodeposit intent"
rg --quiet "t\.chain_status = 'active'" "$executor" ||
  fail "executor does not enforce finalized active chain state"
if rg --quiet 't\.active|t\.lifecycle_status|[[:space:]]lifecycle_status[[:space:]]*=' "$executor"; then
  fail "executor still reads the removed target lifecycle columns"
fi

printf '== Focused behavior checks\n'
scratch_dir="$(mktemp -d /private/tmp/ask-2211-single-target.XXXXXX)"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
postgres_log="$scratch_dir/postgres.log"
database_name="ask_2211_single_target_${$}"
port=$((56700 + ($$ % 400)))
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
)

(
  cd "$routing_root"
  ASK_2211_VERIFY_DATABASE_URL="$database_url" \
    cargo test -p loyal-yield-store --test autodeposit_single_target_db -- --ignored --nocapture
  cargo test -p balance-sweep-ata-monitor autodeposit -- --nocapture
  cargo check -p balance-sweep-ata-monitor -p loyal-yield-store -p loyal-yield-realtime
  cargo fmt --all -- --check
  git diff --check
)

(
  cd "$app_root/apps/web"
  bun test src/features/earn-realtime src/lib/yield-optimization/earn-autodeposit-client-flow.test.ts
  bun run lint
)

(
  cd "$app_root/apps/mobile"
  npm test -- --runInBand src/lib/solana/earn
  npx expo lint
)

git -C "$app_root" diff --check

printf 'PASS: ASK-2211 Autodeposit is client-sent and account-projected\n'
