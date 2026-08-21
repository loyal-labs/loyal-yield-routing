#!/usr/bin/env bash

set -euo pipefail

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

routing_root=""
app_root=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --routing-root)
      routing_root="${2:-}"
      shift 2
      ;;
    --app-root)
      app_root="${2:-}"
      shift 2
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n "$routing_root" && -e "$routing_root/.git" ]] ||
  fail "--routing-root must point to a git worktree"
[[ -n "$app_root" && -e "$app_root/.git" ]] ||
  fail "--app-root must point to a git worktree"

for command_name in bun cargo git initdb pg_config pg_ctl rg; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

printf '== Client/backend boundary\n'
for route in \
  apps/web/src/app/api/smart-accounts/yield-optimization/cross-mint/policies/prepare/route.ts \
  apps/web/src/app/api/smart-accounts/yield-optimization/cross-mint/policies/confirm/route.ts \
  apps/web/src/app/api/smart-accounts/yield-optimization/cross-mint/delete/prepare/route.ts \
  apps/web/src/app/api/smart-accounts/yield-optimization/cross-mint/delete/confirm/route.ts; do
  [[ ! -e "$app_root/$route" ]] || fail "obsolete Autoswap route still exists: $route"
done

if rg --quiet \
  'prepareEarnAutoswapOnServer|postConfirmedEarnAutoswap|prepareEarnAutoswapDeleteOnServer|postConfirmedEarnAutoswapDelete|cross-mint/(policies|delete)/(prepare|confirm)' \
  "$app_root/apps/web/src"; then
  fail "web code still calls an Autoswap prepare/confirm backend path"
fi

readiness_route="$app_root/apps/web/src/app/api/smart-accounts/yield-optimization/cross-mint/delete/readiness/route.ts"
[[ -f "$readiness_route" ]] || fail "authenticated deletion-readiness route is missing"
if rg --quiet 'serializePrepared|prepared:' "$readiness_route"; then
  fail "deletion-readiness route still constructs or serializes a transaction"
fi

client_flow="$app_root/apps/web/src/lib/yield-optimization/earn-autoswap-client-flow.ts"
[[ -f "$client_flow" ]] || fail "client-side Autoswap flow module is missing"
if rg --quiet 'DEPLOYMENT_PK|deploymentPrivateKey|server-only|core/config/server' \
  "$client_flow" "$app_root/apps/web/src/hooks/use-smart-account-sidebar-data.ts"; then
  fail "client-reachable Autoswap code references server-only signer material"
fi

printf 'PASS: client/backend boundary\n'

if rg --quiet 'cross_mint_vault_controls|CrossMintVaultControl|crossMintVaultControls' \
  "$routing_root/crates" "$app_root/apps/web/src"; then
  fail "duplicate Autoswap control storage still exists"
fi
if rg --quiet 'AUTOSWAP_POLICY_TRANSACTIONS|SubscribeRequestFilterTransactions' \
  "$routing_root/crates/balance-sweep-ata-monitor/src"; then
  fail "shared LaserStream still contains a global transaction subscription"
fi

printf '== Disposable database contracts\n'
scratch_dir=$(mktemp -d /private/tmp/ask-2168-autoswap-client.XXXXXX)
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
postgres_log="$scratch_dir/postgres.log"
database_name="fleet_verify_ask_2168_autoswap_client_${$}"
port=$((55800 + ($$ % 400)))
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
"$(pg_config --bindir)/pg_ctl" \
  -D "$data_dir" \
  -l "$postgres_log" \
  -o "-p $port -h 127.0.0.1 -k $socket_dir" \
  start >/dev/null
started=true
"$(pg_config --bindir)/createdb" -h 127.0.0.1 -p "$port" "$database_name"
database_url="postgresql://127.0.0.1:$port/$database_name"

(
  cd "$routing_root"
  NEON_DATABASE_URL="$database_url" \
    cargo run -q -p loyal-yield-orchestrator --bin yield-migrations -- --apply
  AUTOSWAP_CLIENT_VERIFY_DATABASE_URL="$database_url" \
    cargo test -p loyal-yield-store \
      --test autoswap_client_projection_db -- --ignored --nocapture
  AUTOSWAP_REBALANCE_VERIFY_DATABASE_URL="$database_url" \
    cargo test -p loyal-yield-orchestrator \
      --bin fleet-orchestration-verifier \
      autoswap_rebalance_executes_opt_in_lock_queries -- --ignored --nocapture
)
printf 'PASS: disposable database contracts\n'

printf '== LaserStream and fleet contracts\n'
(
  cd "$routing_root"
  cargo test -p balance-sweep-ata-monitor autoswap_uses_targeted_accounts -- --nocapture
  cargo test -p loyal-yield-store autoswap_opt_in -- --nocapture
  cargo check -p balance-sweep-ata-monitor -p loyal-yield-store
  cargo fmt --all -- --check
)
printf 'PASS: LaserStream and fleet contracts\n'

printf '== Browser transaction contracts\n'
(
  cd "$app_root"
  bun test packages/smart-account-vaults/src/client.test.ts \
    --test-name-pattern cross-mint
  bun test apps/web/src/lib/yield-optimization/earn-autoswap-client-flow.test.ts
  bun run --cwd packages/smart-account-vaults typecheck
  bun run --cwd apps/web lint -- \
    --file src/hooks/use-smart-account-sidebar-data.ts \
    --file src/lib/yield-optimization/earn-autoswap-client-flow.ts \
    --file src/lib/yield-optimization/earn-autoswap-client-flow.test.ts \
    --file src/app/api/smart-accounts/yield-optimization/cross-mint/toggle/route.ts \
    --file src/app/api/smart-accounts/yield-optimization/cross-mint/state/route.ts \
    --file src/app/api/smart-accounts/yield-optimization/cross-mint/delete/readiness/route.ts
)
printf 'PASS: browser transaction contracts\n'

printf '== Worktree hygiene\n'
git -C "$routing_root" diff --check
git -C "$app_root" diff --check
printf 'PASS: worktree hygiene\n'

printf 'PASS: ASK-2168 Autoswap client-side verifier\n'
