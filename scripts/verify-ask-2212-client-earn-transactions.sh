#!/usr/bin/env bash
set -u

ROUTING_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP_DIR="${LOYAL_APP_DIR:-/private/tmp/loyal-app-ASK-2212-client-earn-transactions}"
FAILURES=0

pass() {
  echo "PASS: $1"
}

fail() {
  echo "FAIL: $1"
  FAILURES=$((FAILURES + 1))
}

require_match() {
  local description="$1"
  local pattern="$2"
  shift 2
  if rg -q --glob '!**/node_modules/**' "$pattern" "$@"; then
    pass "$description"
  else
    fail "$description"
  fi
}

reject_match() {
  local description="$1"
  local pattern="$2"
  shift 2
  if rg -q --glob '!**/node_modules/**' "$pattern" "$@"; then
    fail "$description"
  else
    pass "$description"
  fi
}

run_check() {
  local description="$1"
  shift
  if "$@"; then
    pass "$description"
  else
    fail "$description"
  fi
}

if [[ ! -d "$APP_DIR/.git" && ! -f "$APP_DIR/.git" ]]; then
  echo "FAIL: Loyal App worktree is missing at $APP_DIR"
  exit 1
fi

require_match \
  "web uses shared client deposit builder" \
  'prepareEarnUsdcDeposit' \
  "$APP_DIR/apps/web/src"
require_match \
  "web uses shared client withdrawal builder" \
  'prepareEarnUsdcWithdraw' \
  "$APP_DIR/apps/web/src"
require_match \
  "web uses shared client cleanup builder" \
  'prepareEarnUsdcCleanup' \
  "$APP_DIR/apps/web/src"
require_match \
  "web uses client policy and vault refund builders" \
  'prepareEarnPolicyRefund|prepareEarnVaultAccountsRefund' \
  "$APP_DIR/apps/web/src"
require_match \
  "mobile uses deposit withdrawal cleanup and refund client builders" \
  'prepareEarnUsdcDeposit|prepareEarnUsdcWithdraw|prepareEarnUsdcCleanup|prepareEarnVaultAccountsRefund' \
  "$APP_DIR/apps/mobile/src"

reject_match \
  "supported clients do not call operation-specific prepare confirm or reconcile APIs" \
  '/api/smart-accounts/(yield-optimization|mobile/earn)/(deposits?|withdrawals?|withdraw|deposit|policy-refunds|policies)/.*(prepare|confirm|reconcile)' \
  "$APP_DIR/apps/web/src/components" \
  "$APP_DIR/apps/web/src/hooks" \
  "$APP_DIR/apps/web/src/features" \
  "$APP_DIR/apps/mobile/src"

require_match \
  "routing has deposit withdrawal cleanup and refund chain mutations" \
  'Deposit\(|Withdrawal\(|Cleanup\(|Refund\(' \
  "$ROUTING_DIR/crates/balance-sweep-ata-monitor/src" \
  "$ROUTING_DIR/crates/loyal-yield-store/src"
reject_match \
  "routing classification does not depend on onboarding or full-withdrawal seed rows" \
  'context\.onboarding|context\.full_withdrawal|EarnOnboardingContext|EarnFullWithdrawalContext' \
  "$ROUTING_DIR/crates/balance-sweep-ata-monitor/src/earn_reconciliation.rs" \
  "$ROUTING_DIR/crates/loyal-yield-store/src/types.rs"
require_match \
  "routing watches supported wallet token accounts" \
  'wallet_token' \
  "$ROUTING_DIR/crates/balance-sweep-ata-monitor/src/smart_account.rs"
require_match \
  "routing uses confirmed canonical reads" \
  'fn (earn_snapshot_config|earn_transaction_config)' \
  "$ROUTING_DIR/crates/balance-sweep-ata-monitor/src/earn_reconciliation.rs"
reject_match \
  "Earn reconciliation does not request finalized RPC state" \
  'CommitmentConfig::finalized\(\)' \
  "$ROUTING_DIR/crates/balance-sweep-ata-monitor/src/earn_reconciliation.rs"
require_match \
  "routing fences canonical reads at the triggering slot" \
  'min_context_slot: Some\((update\.slot|minimum_slot|min_context_slot)\)' \
  "$ROUTING_DIR/crates/balance-sweep-ata-monitor/src/earn_reconciliation.rs"

for scenario in \
  initial_deposit_without_onboarding \
  top_up_without_onboarding \
  partial_withdrawal_from_chain \
  multi_step_full_withdrawal_from_chain \
  cleanup_without_seed_withdrawal \
  policy_refund_from_chain \
  vault_refund_from_chain \
  replay_is_idempotent \
  same_slot_siblings_all_complete
do
  require_match \
    "verifier scenario $scenario exists" \
    "$scenario" \
    "$ROUTING_DIR/crates/balance-sweep-ata-monitor/src" \
    "$ROUTING_DIR/crates/loyal-yield-store/tests" \
    "$ROUTING_DIR/crates/loyal-yield-store/src"
done

require_match \
  "web refetches projected Earn state after SSE invalidation" \
  'earn\.(position|transaction|onboarding).*changed|reconcileRelated|mutate\(' \
  "$APP_DIR/apps/web/src/features/earn-realtime" \
  "$APP_DIR/apps/web/src/components" \
  "$APP_DIR/apps/web/src/hooks"
require_match \
  "mobile refetches projected Earn state after SSE invalidation" \
  'invalidate|refetch|refresh' \
  "$APP_DIR/apps/mobile/src/features/earn-realtime"

require_match \
  "isolated web E2E guards against confirmation API calls" \
  'FORBIDDEN_EARN_API_PATTERN|forbiddenApiRequests' \
  "$APP_DIR/apps/web/scripts/verify-earn-client-local-chain.ts"
require_match \
  "isolated web E2E covers initial top-up partial and full cash flow" \
  'initial_deposit|top_up|partial_withdrawal|full_withdrawal' \
  "$APP_DIR/apps/web/scripts/verify-earn-client-local-chain.ts" \
  "$ROUTING_DIR/scripts/verify-earn-client-local-e2e.sh"
require_match \
  "isolated web E2E recovers an existing projected policy from stale client state" \
  'resolveRequiredClientEarnPolicy|projectedPolicyRefreshCount|projected-earn-state-output' \
  "$APP_DIR/apps/web/scripts/verify-earn-client-local-chain.ts" \
  "$ROUTING_DIR/scripts/verify-earn-client-local-e2e.sh" \
  "$ROUTING_DIR/crates/balance-sweep-ata-monitor/src/bin/earn-client-local-e2e.rs"
require_match \
  "client and projection share the confirmed accounting boundary" \
  'confirmedSlot|wait-confirmed|wait_for_confirmed' \
  "$APP_DIR/apps/web/scripts/verify-earn-client-local-chain.ts" \
  "$ROUTING_DIR/scripts/verify-earn-client-local-e2e.sh"
require_match \
  "isolated routing worktrees share the primary Cargo build cache" \
  'git-common-dir|CARGO_TARGET_DIR' \
  "$ROUTING_DIR/scripts/verify-earn-client-local-e2e.sh"

run_check \
  "isolated web client Earn chain projection and SSE E2E passes" \
  bash "$ROUTING_DIR/scripts/verify-earn-client-local-e2e.sh" --app-root "$APP_DIR"

run_check \
  "routing focused chain projection scenarios pass" \
  cargo test --manifest-path "$ROUTING_DIR/Cargo.toml" -p balance-sweep-ata-monitor earn_reconciliation
run_check \
  "routing store contract checks pass" \
  cargo test --manifest-path "$ROUTING_DIR/Cargo.toml" -p loyal-yield-store --lib earn_reconciliation
run_check \
  "routing crates compile" \
  cargo check --manifest-path "$ROUTING_DIR/Cargo.toml" -p balance-sweep-ata-monitor -p loyal-yield-store
run_check \
  "routing formatting passes" \
  cargo fmt --manifest-path "$ROUTING_DIR/Cargo.toml" --all -- --check
run_check \
  "shared client SDK focused Earn tests pass" \
  bash -lc "cd '$APP_DIR' && bun test packages/smart-account-vaults/src/client.test.ts -t 'builds standalone earn routing policy setup metadata|preserves partial liquidity intent|multi-program Earn cleanup transfers' && cd packages/smart-account-vaults && bun run typecheck"
run_check \
  "mobile Earn client tests pass" \
  bash -lc "cd '$APP_DIR/apps/mobile' && npm test -- --runInBand src/lib/solana/earn"
run_check \
  "web and mobile TypeScript checks pass" \
  bash -lc "cd '$APP_DIR' && bunx tsc --noEmit --pretty false -p apps/web/tsconfig.json && cd apps/mobile && node_modules/.bin/tsc --noEmit --pretty false"
run_check \
  "changed web TypeScript passes scoped lint" \
  bash -lc "cd '$APP_DIR' && env ESLINT_USE_FLAT_CONFIG=false node_modules/.bin/eslint apps/web/src/hooks/use-smart-account-sidebar-data.ts apps/web/src/hooks/use-active-earn-position.ts apps/web/src/components/wallet-workspace/facelift/use-earn-actions.ts apps/web/src/components/wallet-workspace/earn-transactions-pane.tsx apps/web/src/app/api/smart-accounts/yield-optimization/policy-refunds/scan/route.ts apps/web/src/app/api/smart-accounts/mobile/earn/policy-refunds/scan/route.ts apps/web/src/app/api/smart-accounts/mobile/earn/withdraw/cleanup/prepare-context/route.ts"
run_check \
  "changed mobile TypeScript passes scoped lint" \
  bash -lc "cd '$APP_DIR/apps/mobile' && node_modules/.bin/eslint src/lib/solana/earn/deposit.ts src/lib/solana/earn/withdraw.ts src/lib/solana/earn/refund.ts src/lib/solana/earn/earn-api.ts src/lib/solana/earn/__tests__/withdraw.test.ts"

if ((FAILURES > 0)); then
  echo "FAIL: ASK-2212 client-built confirmed-chain-projected Earn architecture ($FAILURES checks failed)"
  exit 1
fi

echo "PASS: ASK-2212 client-built confirmed-chain-projected Earn architecture"
