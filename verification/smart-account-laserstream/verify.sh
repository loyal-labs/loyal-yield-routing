#!/usr/bin/env bash
set -euo pipefail

verifier_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
routing_root=""
app_root=""
scratch_dir="$(mktemp -d "/tmp/smart-account-laserstream-verifier.XXXXXX")"
data_dir="$scratch_dir/postgres"
socket_dir="$scratch_dir/socket"
postgres_log="$scratch_dir/postgres.log"
request_log="$scratch_dir/subscribe-request.json"
database_name="smart_account_laserstream_verify"
port="$((57432 + RANDOM % 1000))"
server_started=0

vault_a="Config1111111111111111111111111111111111111"
vault_b="BPFLoaderUpgradeab1e11111111111111111111111"
vault_c="LoaderV411111111111111111111111111111111111"
vault_d="SysvarInstructions1111111111111111111111111"
settings_a="Vote111111111111111111111111111111111111111"
settings_b="SysvarC1ock11111111111111111111111111111111"
settings_c="SysvarRecentB1ockHashes11111111111111111111"
settings_d="SysvarEpochSchedu1e111111111111111111111111"
wallet_a="Stake11111111111111111111111111111111111111"
wallet_b="ComputeBudget111111111111111111111111111111"
wallet_c="Vote111111111111111111111111111111111111111"
wallet_d="NativeLoader1111111111111111111111111111111"
policy_a="AddressLookupTab1e1111111111111111111111111"
setup_a="SysvarS1otHashes111111111111111111111111111"
policy_b="BPFLoader1111111111111111111111111111111111"
policy_c="Ed25519SigVerify111111111111111111111111111"
policy_d="MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr"
mint="So11111111111111111111111111111111111111112"
market="TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
reserve="SysvarRent111111111111111111111111111111111"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

pass_condition() {
  echo "PASS: $*"
}

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    "$pg_bindir/pg_ctl" -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

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

[[ -n "$routing_root" ]] || fail "--routing-root is required"
[[ -n "$app_root" ]] || fail "--app-root is required"
routing_root="$(cd "$routing_root" && pwd)"
app_root="$(cd "$app_root" && pwd)"
[[ "$routing_root" != "$verifier_root" ]] || fail "routing implementation must be a separate worktree"
[[ "$app_root" != "$verifier_root" ]] || fail "app implementation must be a separate worktree"

if [[ -x /opt/homebrew/opt/postgresql@17/bin/postgres ]]; then
  pg_bindir=/opt/homebrew/opt/postgresql@17/bin
else
  pg_bindir="$(pg_config --bindir)"
fi

for command_name in cargo jq rg git; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done
for postgres_command in initdb pg_ctl psql; do
  [[ -x "$pg_bindir/$postgres_command" ]] || fail "$postgres_command is required"
done

e2e_source="$routing_root/crates/balance-sweep-ata-monitor/src/bin/smart-account-laserstream-e2e.rs"
direct_source="$routing_root/crates/balance-sweep-ata-monitor/src/earn_reconciliation.rs"
[[ -f "$e2e_source" ]] || fail "production-backed E2E binary is missing"
[[ -f "$direct_source" ]] || fail "direct in-process Earn reconciler is missing"

for channel in \
  balance_sweep_wallet_atas \
  earn_policy_accounts \
  earn_vault_accounts \
  earn_idle_token_accounts \
  earn_obligations; do
  rg --quiet --fixed-strings "$channel" "$routing_root/crates/balance-sweep-ata-monitor/src" ||
    fail "missing production channel: $channel"
done
if rg --quiet --fixed-strings 'earn_smart_account_transactions' \
  "$routing_root/crates/balance-sweep-ata-monitor/src"; then
  fail "redundant transaction subscription is present"
fi
if rg --quiet --fixed-strings 'earn_reserves' \
  "$routing_root/crates/balance-sweep-ata-monitor/src"; then
  fail "reserve fan-out is present"
fi

for rejected in \
  earn_reconciliation_jobs \
  earn_reconciliation_receipts \
  record_earn_reconciliation_batch \
  lease_earn_reconciliation; do
  if rg --quiet --fixed-strings "$rejected" \
    "$routing_root/crates"; then
    fail "rejected durable handoff remains: $rejected"
  fi
done
if [[ -e "$routing_root/crates/loyal-fleet-worker/src/earn_reconciliation.rs" ]]; then
  fail "Earn reconciliation was moved into the fleet worker"
fi
rg --quiet --fixed-strings 'reconcile_normalized_earn_update' \
  "$routing_root/crates/balance-sweep-ata-monitor/src/lib.rs" ||
  fail "production event loop does not call direct Earn reconciliation"
rg --quiet --fixed-strings 'FixtureEarnChainReader' "$e2e_source" ||
  fail "E2E does not use the production engine through a deterministic chain reader"
if rg --quiet --fixed-strings 'handle.write(' \
  "$routing_root/crates/balance-sweep-ata-monitor/src/lib.rs"; then
  fail "Earn watch-set changes still use the SDK live-write path without replay"
fi
rg --quiet --fixed-strings 'session_requires_rebuild(session.is_none(), diff.has_changes(), earn_changed)' \
  "$routing_root/crates/balance-sweep-ata-monitor/src/main.rs" ||
  fail "Earn watch-set changes do not rebuild the replaying session"
rg --quiet --fixed-strings 'preserve_replay_from_slot' \
  "$routing_root/crates/balance-sweep-ata-monitor/src/main.rs" ||
  fail "Earn watch-set rebuild does not preserve the earlier replay start"

for removed_route in \
  "$app_root/apps/web/src/app/api/cron/earn-deposit-reconcile/route.ts" \
  "$app_root/apps/web/src/app/api/cron/earn-cleanup-reconcile/route.ts" \
  "$app_root/apps/web/scripts/earn-reconciliation-worker.ts"; do
  [[ ! -e "$removed_route" ]] || fail "removed Loyal App surface still exists: $removed_route"
done
if jq -e '.crons[]?.path | select(. == "/api/cron/earn-deposit-reconcile" or . == "/api/cron/earn-cleanup-reconcile")' \
  "$app_root/apps/web/vercel.json" >/dev/null; then
  fail "superseded Earn cron schedule is still enabled"
fi
pass_condition "direct account-only production architecture"

mkdir -p "$socket_dir"
"$pg_bindir/initdb" -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
if ! "$pg_bindir/pg_ctl" -D "$data_dir" -l "$postgres_log" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1" \
  -w start >/dev/null; then
  tail -80 "$postgres_log" >&2 || true
  fail "isolated PostgreSQL failed to start"
fi
server_started=1

"$pg_bindir/psql" -X --set=ON_ERROR_STOP=1 \
  --host="$socket_dir" --port="$port" --username="$(id -un)" \
  --dbname=postgres --command="CREATE DATABASE $database_name" >/dev/null
database_url="postgresql://$(id -un)@127.0.0.1:${port}/${database_name}"

psql_verify() {
  "$pg_bindir/psql" -X --set=ON_ERROR_STOP=1 \
    --host="$socket_dir" --port="$port" --username="$(id -un)" \
    --dbname="$database_name" "$@"
}

sql_scalar() {
  psql_verify -A -t --command="$1" | tr -d '[:space:]'
}

assert_scalar() {
  local expected="$1"
  local sql="$2"
  local description="$3"
  local actual
  actual="$(sql_scalar "$sql")"
  [[ "$actual" == "$expected" ]] ||
    fail "$description: expected '$expected', got '$actual'"
  pass_condition "$description"
}

echo "== Apply production routing and app-compatible Yield migrations"
(
  cd "$routing_root"
  NEON_DATABASE_URL="$database_url" NO_DNA=1 \
    cargo run --quiet -p loyal-yield-orchestrator --bin yield-migrations -- --apply
  NEON_DATABASE_URL="$database_url" NO_DNA=1 \
    cargo run --quiet -p loyal-yield-orchestrator --bin yield-migrations -- --check
)
assert_scalar "40:durable_autodeposit_operation,41:optimizer_epochs_latest_cluster_index,42:rebalance_opportunities_optimizer_epoch_index,43:rebalance_opportunities_health_aggregate_index,44:fleet_health_status_query_optimization,45:laserstream_replay_cursor" \
  "SELECT string_agg(version::text || ':' || name, ',' ORDER BY version) FROM loyal_yield.schema_migrations WHERE version >= 40" \
  "current-main migrations 40 through 44 and LaserStream migration 45 coexist"
psql_verify --file="$app_root/apps/web/src/lib/yield-optimization/migrations/0001_add_user_yield_deposit_positions.sql" >/dev/null
psql_verify --file="$app_root/apps/web/src/lib/yield-optimization/migrations/0004_add_verifiable_earn_holdings.sql" >/dev/null
psql_verify --file="$app_root/apps/web/src/lib/yield-optimization/migrations/0013_add_earn_deposit_onboarding_attempts.sql" >/dev/null

echo "== Seed production-shaped identities and recovery candidates"
psql_verify <<SQL >/dev/null
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE app_users (
  id UUID PRIMARY KEY,
  subject_address TEXT NOT NULL
);
CREATE TABLE app_user_smart_accounts (
  user_id UUID NOT NULL,
  solana_env TEXT NOT NULL,
  settings_pda TEXT NOT NULL,
  state TEXT NOT NULL
);
INSERT INTO app_users (id, subject_address) VALUES
  ('00000000-0000-0000-0000-000000000001', '$wallet_a'),
  ('00000000-0000-0000-0000-000000000002', '$wallet_b'),
  ('00000000-0000-0000-0000-000000000003', '$wallet_c'),
  ('00000000-0000-0000-0000-000000000004', '$wallet_d');
INSERT INTO app_user_smart_accounts (user_id, solana_env, settings_pda, state) VALUES
  ('00000000-0000-0000-0000-000000000001', 'mainnet', '$settings_a', 'ready'),
  ('00000000-0000-0000-0000-000000000002', 'mainnet', '$settings_b', 'ready'),
  ('00000000-0000-0000-0000-000000000003', 'mainnet', '$settings_c', 'ready'),
  ('00000000-0000-0000-0000-000000000004', 'mainnet', '$settings_d', 'ready');

WITH route AS (
  INSERT INTO loyal_yield.route_policies (
    settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
    delegated_signers, threshold, route_modes, stable_mints, kamino_markets,
    kamino_liquidity_mints, active, last_seen_slot, last_seen_signature
  ) VALUES (
    '$settings_a', '$wallet_a', 101, '$policy_a', 1, '$vault_a',
    ARRAY['$wallet_a'], 1, ARRAY['kamino_deposit'], ARRAY['$mint'],
    ARRAY['$market'], ARRAY['$mint'], TRUE, 90, 'sig-policy-create-a'
  ) RETURNING id
)
INSERT INTO loyal_yield.earn_deposit_onboarding_attempts (
  wallet_address, delegated_signer, smart_account_address, settings, vault_index,
  vault_pubkey, policy_id, policy_account, policy_seed, route_policy_db_id,
  route_policy_signature, route_policy_confirmed_slot, setup_policy_id,
  setup_policy_account, setup_policy_seed, target_reserve, market, liquidity_mint,
  status, first_seen_at, updated_at
)
SELECT '$wallet_a', '$wallet_a', '$vault_a', '$settings_a', 1, '$vault_a',
  101, '$policy_a', 101, route.id, 'sig-policy-create-a', 90, 102, '$setup_a',
  102, '$reserve', '$market', '$mint', 'route_policy_confirmed', NOW(), NOW()
FROM route;

INSERT INTO loyal_yield.earn_deposit_onboarding_attempts (
  wallet_address, delegated_signer, smart_account_address, settings, vault_index,
  vault_pubkey, policy_id, policy_account, policy_seed, route_policy_signature,
  route_policy_confirmed_slot, target_reserve, market, liquidity_mint, status,
  first_seen_at, updated_at
) VALUES (
  '$wallet_b', '$wallet_b', '$vault_b', '$settings_b', 1, '$vault_b', 201,
  '$policy_b', 201, 'sig-policy-b', 105, '$reserve', '$market', '$mint',
  'route_policy_confirmed', NOW(), NOW()
);

WITH route AS (
  INSERT INTO loyal_yield.route_policies (
    settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
    delegated_signers, threshold, route_modes, stable_mints, kamino_markets,
    kamino_liquidity_mints, active, last_seen_slot, last_seen_signature
  ) VALUES (
    '$settings_d', '$wallet_d', 401, '$policy_d', 1, '$vault_d',
    ARRAY['$wallet_d'], 1, ARRAY['kamino_deposit'], ARRAY['$mint'],
    ARRAY['$market'], ARRAY['$mint'], TRUE, 105, 'sig-policy-d'
  ) RETURNING id
), vault AS (
  INSERT INTO loyal_yield.managed_vaults (
    settings, vault_index, vault_pubkey, active_policy_id, active
  ) SELECT '$settings_d', 1, '$vault_d', route.id, TRUE FROM route
  RETURNING id, active_policy_id
), initial_deposit AS (
  INSERT INTO loyal_yield.user_yield_position_deposits (
    deposit_signature, policy_signature, confirmed_slot, wallet_address,
    smart_account_address, settings, vault_index, vault_pubkey, policy_id,
    policy_account, policy_seed, target_reserve, market, liquidity_mint,
    target_supply_apy_bps, deposit_mint, principal_amount_raw, confirmed_at,
    created_at
  ) SELECT
    'sig-deposit-d-initial', 'sig-policy-d', 100, '$wallet_d', '$vault_d',
    '$settings_d', 1, '$vault_d', route.id, '$policy_d', 401, '$reserve',
    '$market', '$mint', NULL, '$mint', 2000000, NOW(), NOW()
  FROM route
  RETURNING id
), position AS (
  INSERT INTO loyal_yield.user_yield_positions (
    wallet_address, smart_account_address, settings, vault_index, vault_pubkey,
    policy_id, policy_account, policy_seed, initial_reserve, initial_market,
    initial_liquidity_mint, initial_supply_apy_bps, deposit_mint,
    principal_amount_raw, first_deposit_signature, last_deposit_signature,
    last_confirmed_slot, status, created_at, updated_at, current_reserve,
    current_market, current_liquidity_mint, current_amount_raw,
    current_observed_slot, current_observed_at
  ) SELECT
    '$wallet_d', '$vault_d', '$settings_d', 1, '$vault_d', route.id,
    '$policy_d', 401, '$reserve', '$market', '$mint', NULL, '$mint', 2000000,
    'sig-deposit-d-initial', 'sig-deposit-d-initial', 100, 'active', NOW(),
    NOW(), '$reserve', '$market', '$mint', 2100000, 100, NOW()
  FROM route
  RETURNING id
), holding AS (
  INSERT INTO loyal_yield.user_yield_position_holding_events (
    position_id, event_type, reserve, market, liquidity_mint, amount_raw,
    principal_delta_raw, holding_delta_raw, observed_slot, observed_at,
    source_signature, source_deposit_id, created_at
  ) SELECT
    position.id, 'deposit_initialized', '$reserve', '$market', '$mint',
    2100000, 2000000, 2100000, 100, NOW(), 'sig-deposit-d-initial',
    initial_deposit.id, NOW()
  FROM position CROSS JOIN initial_deposit
  RETURNING id, position_id
)
UPDATE loyal_yield.user_yield_positions position
SET last_holding_event_id = holding.id
FROM holding
WHERE position.id = holding.position_id;

INSERT INTO loyal_yield.earn_deposit_onboarding_attempts (
  wallet_address, delegated_signer, smart_account_address, settings, vault_index,
  vault_pubkey, policy_id, policy_account, policy_seed, route_policy_db_id,
  route_policy_signature, route_policy_confirmed_slot, deposit_signature,
  deposit_confirmed_slot, deposit_mint, principal_amount_raw, target_reserve,
  market, liquidity_mint, status, first_seen_at, updated_at
) SELECT
  '$wallet_d', '$wallet_d', '$vault_d', '$settings_d', 1, '$vault_d', 401,
  '$policy_d', 401, id, 'sig-policy-d', 105, 'sig-deposit-d-initial', 100,
  '$mint', 2000000, '$reserve', '$market', '$mint', 'complete', NOW(), NOW()
FROM loyal_yield.route_policies
WHERE policy_account = '$policy_d';

INSERT INTO loyal_yield.earn_deposit_onboarding_attempts (
  wallet_address, delegated_signer, smart_account_address, settings, vault_index,
  vault_pubkey, policy_id, policy_account, policy_seed, route_policy_db_id,
  route_policy_signature, route_policy_confirmed_slot, target_reserve, market,
  liquidity_mint, status, first_seen_at, updated_at
) SELECT
  '$wallet_d', '$wallet_d', '$vault_d', '$settings_d', 1, '$vault_d', 401,
  '$policy_d', 401, id, 'sig-policy-d', 105, '$reserve', '$market', '$mint',
  'route_policy_confirmed', NOW(), NOW()
FROM loyal_yield.route_policies
WHERE policy_account = '$policy_d';

WITH route AS (
  INSERT INTO loyal_yield.route_policies (
    settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
    delegated_signers, threshold, route_modes, stable_mints, kamino_markets,
    kamino_liquidity_mints, active, last_seen_slot, last_seen_signature
  ) VALUES (
    '$settings_c', '$wallet_c', 301, '$policy_c', 1, '$vault_c',
    ARRAY['$wallet_c'], 1, ARRAY['kamino_deposit'], ARRAY['$mint'],
    ARRAY['$market'], ARRAY['$mint'], TRUE, 100, 'sig-policy-c'
  ) RETURNING id
), vault AS (
  INSERT INTO loyal_yield.managed_vaults (
    settings, vault_index, vault_pubkey, active_policy_id, active
  ) SELECT '$settings_c', 1, '$vault_c', route.id, TRUE FROM route
  RETURNING id, active_policy_id
), snapshot AS (
  INSERT INTO loyal_yield.vault_position_snapshots (
    vault_id, policy_id, observed_slot, is_current
  ) SELECT vault.id, vault.active_policy_id, 100, TRUE FROM vault
  RETURNING id, vault_id
), position AS (
  INSERT INTO loyal_yield.user_yield_positions (
    wallet_address, smart_account_address, settings, vault_index, vault_pubkey,
    policy_id, policy_account, policy_seed, initial_reserve, initial_market,
    initial_liquidity_mint, initial_supply_apy_bps, deposit_mint,
    principal_amount_raw, first_deposit_signature, last_deposit_signature,
    last_confirmed_slot, status, created_at, updated_at, current_reserve,
    current_market, current_liquidity_mint, current_amount_raw,
    current_observed_slot, current_observed_at
  ) SELECT '$wallet_c', '$vault_c', '$settings_c', 1, '$vault_c',
    vault.active_policy_id, '$policy_c', 301, '$reserve', '$market', '$mint',
    500, '$mint', 9000000, 'sig-deposit-c', 'sig-deposit-c', 100, 'active',
    NOW(), NOW(), '$reserve', '$market', '$mint', 9000000, 100, NOW()
  FROM vault
  RETURNING id
), withdrawal AS (
  INSERT INTO loyal_yield.user_yield_position_withdrawals (
    withdrawal_signature, confirmed_slot, wallet_address, smart_account_address,
    settings, vault_index, vault_pubkey, policy_id, policy_account, policy_seed,
    target_reserve, market, liquidity_mint, withdrawn_amount_raw, mode,
    confirmed_at, created_at
  ) SELECT 'sig-withdraw-c', 114, '$wallet_c', '$vault_c', '$settings_c', 1,
    '$vault_c', vault.active_policy_id, '$policy_c', 301, '$reserve', '$market',
    '$mint', 9000000, 'full', NOW(), NOW() FROM vault
  RETURNING id
)
INSERT INTO loyal_yield.vault_reserve_positions_current (
  vault_id, reserve, market, liquidity_mint, amount_raw, has_value, snapshot_id,
  observed_slot, observed_at
)
SELECT snapshot.vault_id, '$reserve', '$market', '$mint', 9000000, TRUE,
  snapshot.id, 100, NOW() FROM snapshot;

INSERT INTO loyal_yield.vault_idle_token_balances_current (
  vault_id, mint, amount_raw, owner, token_account, observed_slot, observed_at,
  source_commitment, updated_at
)
SELECT id, '$mint', 1, '$vault_c',
  'KeccakSecp256k11111111111111111111111111111', 100, NOW(), 'confirmed', NOW()
FROM loyal_yield.managed_vaults WHERE vault_pubkey = '$vault_c';
SQL

run_fixture() {
  local events_file="$1"
  local chain_file="$2"
  (
    cd "$routing_root"
    NO_DNA=1 cargo run --quiet -p balance-sweep-ata-monitor \
      --bin smart-account-laserstream-e2e -- \
      --postgres-url "$database_url" \
      --stream-name earn-smart-account-verification \
      --watch-set "$verifier_root/fixtures/watch-set.json" \
      --events "$events_file" \
      --chain-fixtures "$chain_file" \
      --request-output "$request_log"
  )
}

echo "== Process policy-only, invisible deposit, shared binding, and positive cleanup"
run_fixture "$verifier_root/fixtures/phase-1.ndjson" \
  "$verifier_root/fixtures/chain-ready.json"

jq -e '
  .request_count == 1 and
  .commitment == "confirmed" and
  (.accounts | keys | sort) == ([
    "balance_sweep_wallet_atas",
    "earn_idle_token_accounts",
    "earn_obligations",
    "earn_policy_accounts",
    "earn_vault_accounts"
  ] | sort) and
  .transactions == {} and
  ([.accounts[] | length] | all(. > 0)) and
  ([.accounts[] | . == (sort | unique)] | all)
' "$request_log" >/dev/null || fail "captured SubscribeRequest is unsafe or incomplete"
pass_condition "account-only multi-channel subscription"

assert_scalar "setup_policy_confirmed" \
  "SELECT status FROM loyal_yield.earn_deposit_onboarding_attempts WHERE vault_pubkey = '$vault_a'" \
  "policy-only onboarding advanced"
assert_scalar "2" \
  "SELECT count(*) FROM loyal_yield.route_policies WHERE vault_pubkey = '$vault_a' AND active" \
  "route and setup policies recorded"
assert_scalar "1" \
  "SELECT count(*) FROM loyal_yield.managed_vaults WHERE vault_pubkey = '$vault_a' AND active AND setup_policy_id IS NOT NULL" \
  "policy-only managed vault recorded"
assert_scalar "1" \
  "SELECT count(*) FROM loyal_yield.user_yield_position_deposits WHERE deposit_signature = 'sig-deposit-b' AND principal_amount_raw = 5000000" \
  "invisible deposit ledger recorded"
assert_scalar "5000000:5250000:118:active" \
  "SELECT principal_amount_raw || ':' || current_amount_raw || ':' || current_observed_slot || ':' || status::text FROM loyal_yield.user_yield_positions WHERE vault_pubkey = '$vault_b'" \
  "invisible deposit keeps principal separate from later chain-observed holding"
assert_scalar "5250000:5000000:5250000:118" \
  "SELECT event.amount_raw || ':' || event.principal_delta_raw || ':' || event.holding_delta_raw || ':' || event.observed_slot FROM loyal_yield.user_yield_position_holding_events event JOIN loyal_yield.user_yield_position_deposits deposit ON deposit.id = event.source_deposit_id WHERE deposit.deposit_signature = 'sig-deposit-b' AND event.event_type = 'deposit_initialized'" \
  "invisible deposit holding event uses the observed amount and context slot"
assert_scalar "0" \
  "SELECT count(*) FROM loyal_yield.user_yield_positions position JOIN loyal_yield.user_yield_position_holding_events event ON event.id = position.last_holding_event_id WHERE position.current_reserve IS DISTINCT FROM event.reserve OR position.current_market IS DISTINCT FROM event.market OR position.current_liquidity_mint IS DISTINCT FROM event.liquidity_mint OR position.current_amount_raw IS DISTINCT FROM event.amount_raw OR position.current_observed_slot IS DISTINCT FROM event.observed_slot OR position.current_observed_at IS DISTINCT FROM event.observed_at" \
  "canonical position projection matches its latest holding event"
assert_scalar "complete" \
  "SELECT status FROM loyal_yield.earn_deposit_onboarding_attempts WHERE vault_pubkey = '$vault_b'" \
  "invisible deposit onboarding completed"
assert_scalar "7000000:7400000:119:active" \
  "SELECT principal_amount_raw || ':' || current_amount_raw || ':' || current_observed_slot || ':' || status::text FROM loyal_yield.user_yield_positions WHERE vault_pubkey = '$vault_d'" \
  "top-up adds principal while projecting the later observed holding"
assert_scalar "7400000:5000000:5300000:119:deposit_top_up" \
  "SELECT event.amount_raw || ':' || event.principal_delta_raw || ':' || event.holding_delta_raw || ':' || event.observed_slot || ':' || event.event_type::text FROM loyal_yield.user_yield_position_holding_events event JOIN loyal_yield.user_yield_position_deposits deposit ON deposit.id = event.source_deposit_id WHERE deposit.deposit_signature = 'sig-deposit-d'" \
  "top-up holding delta is measured from the previous projection"
assert_scalar "2:2" \
  "SELECT (SELECT count(*) FROM loyal_yield.user_yield_position_deposits WHERE vault_pubkey = '$vault_d') || ':' || (SELECT count(*) FROM loyal_yield.user_yield_position_holding_events event JOIN loyal_yield.user_yield_positions position ON position.id = event.position_id WHERE position.vault_pubkey = '$vault_d')" \
  "top-up preserves one deposit and holding event per signature"
assert_scalar "sig-deposit-d-initial:2000000:100" \
  "SELECT deposit_signature || ':' || principal_amount_raw || ':' || deposit_confirmed_slot FROM loyal_yield.earn_deposit_onboarding_attempts WHERE vault_pubkey = '$vault_d' AND deposit_signature = 'sig-deposit-d-initial'" \
  "top-up preserves the completed historical onboarding attempt"
assert_scalar "sig-deposit-d:5000000:113" \
  "SELECT deposit_signature || ':' || principal_amount_raw || ':' || deposit_confirmed_slot FROM loyal_yield.earn_deposit_onboarding_attempts WHERE vault_pubkey = '$vault_d' AND deposit_signature = 'sig-deposit-d'" \
  "top-up completes only the active onboarding attempt"
assert_scalar "2" \
  "SELECT count(*) FROM loyal_yield.earn_deposit_onboarding_attempts WHERE vault_pubkey = '$vault_d' AND status = 'complete'" \
  "top-up leaves both onboarding attempts as distinct history"
assert_scalar "9000000:active" \
  "SELECT principal_amount_raw || ':' || status::text FROM loyal_yield.user_yield_positions WHERE vault_pubkey = '$vault_c'" \
  "positive cleanup proof wrote no zero state"
assert_scalar "115" \
  "SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name = 'earn-smart-account-verification'" \
  "cursor advanced only after phase-one convergence"

echo "== Record full withdrawal candidate for deposited vault B"
psql_verify <<SQL >/dev/null
INSERT INTO loyal_yield.user_yield_position_withdrawals (
  withdrawal_signature, confirmed_slot, wallet_address, smart_account_address,
  settings, vault_index, vault_pubkey, policy_id, policy_account, policy_seed,
  target_reserve, market, liquidity_mint, withdrawn_amount_raw, mode,
  confirmed_at, created_at
)
SELECT 'sig-withdraw-b', 120, wallet_address, smart_account_address, settings,
  vault_index, vault_pubkey, policy_id, policy_account, policy_seed,
  initial_reserve, initial_market, initial_liquidity_mint, 5000000, 'full',
  NOW(), NOW()
FROM loyal_yield.user_yield_positions WHERE vault_pubkey = '$vault_b';
SQL

echo "== Prove forced failure is atomic"
if SMART_ACCOUNT_E2E_FAIL_BEFORE_COMMIT_EVENT_KEY=cleanup-b-closed \
  run_fixture "$verifier_root/fixtures/phase-2.ndjson" \
    "$verifier_root/fixtures/chain-ready.json"; then
  fail "fault-injected direct reconciliation unexpectedly succeeded"
fi
assert_scalar "active" \
  "SELECT status::text FROM loyal_yield.user_yield_positions WHERE vault_pubkey = '$vault_b'" \
  "forced failure left canonical position unchanged"
assert_scalar "115" \
  "SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name = 'earn-smart-account-verification'" \
  "forced failure did not advance cursor"

echo "== Prove RPC lag blocks only the unproven event"
if run_fixture "$verifier_root/fixtures/phase-2.ndjson" \
  "$verifier_root/fixtures/chain-lag.json"; then
  fail "below-min-context cleanup unexpectedly succeeded"
fi
assert_scalar "closed" \
  "SELECT status::text FROM loyal_yield.user_yield_positions WHERE vault_pubkey = '$vault_b'" \
  "earlier confirmed cleanup committed before later RPC lag"
assert_scalar "active" \
  "SELECT status::text FROM loyal_yield.user_yield_positions WHERE vault_pubkey = '$vault_c'" \
  "RPC lag wrote no cleanup state"
assert_scalar "130" \
  "SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name = 'earn-smart-account-verification'" \
  "cursor stopped at last proven event"

echo "== Retry with ready proof and replay everything"
run_fixture "$verifier_root/fixtures/phase-2.ndjson" \
  "$verifier_root/fixtures/chain-ready.json"
run_fixture "$verifier_root/fixtures/phase-1.ndjson" \
  "$verifier_root/fixtures/chain-ready.json"
run_fixture "$verifier_root/fixtures/phase-2.ndjson" \
  "$verifier_root/fixtures/chain-ready.json"

assert_scalar "131" \
  "SELECT durable_slot FROM loyal_yield.laserstream_replay_cursors WHERE consumer_name = 'earn-smart-account-verification'" \
  "restart replay converged with monotonic cursor"
assert_scalar "1:1" \
  "SELECT (SELECT count(*) FROM loyal_yield.user_yield_position_deposits WHERE deposit_signature = 'sig-deposit-b') || ':' || (SELECT count(*) FROM loyal_yield.user_yield_position_holding_events event JOIN loyal_yield.user_yield_position_deposits deposit ON deposit.id = event.source_deposit_id WHERE deposit.deposit_signature = 'sig-deposit-b')" \
  "replay created no duplicate deposit accounting"
assert_scalar "2:2" \
  "SELECT (SELECT count(*) FROM loyal_yield.user_yield_position_deposits WHERE vault_pubkey = '$vault_d') || ':' || (SELECT count(*) FROM loyal_yield.user_yield_position_holding_events event JOIN loyal_yield.user_yield_positions position ON position.id = event.position_id WHERE position.vault_pubkey = '$vault_d')" \
  "replay created no duplicate top-up accounting"
assert_scalar "2" \
  "SELECT count(*) FROM loyal_yield.user_yield_positions WHERE vault_pubkey IN ('$vault_b', '$vault_c') AND status = 'closed' AND principal_amount_raw = 0 AND current_amount_raw = 0" \
  "both cleanup classes closed and zeroed positions"
assert_scalar "2" \
  "SELECT count(*) FROM loyal_yield.managed_vaults WHERE vault_pubkey IN ('$vault_b', '$vault_c') AND NOT active" \
  "both cleanup classes deactivated managed vaults"
assert_scalar "0" \
  "SELECT count(*) FROM loyal_yield.vault_reserve_positions_current current JOIN loyal_yield.managed_vaults vault ON vault.id = current.vault_id WHERE vault.vault_pubkey IN ('$vault_b', '$vault_c') AND (current.amount_raw <> 0 OR current.has_value)" \
  "cleanup zeroed every reserve row"
assert_scalar "0" \
  "SELECT count(*) FROM loyal_yield.vault_idle_token_balances_current current JOIN loyal_yield.managed_vaults vault ON vault.id = current.vault_id WHERE vault.vault_pubkey IN ('$vault_b', '$vault_c') AND current.amount_raw <> 0" \
  "cleanup zeroed every idle row"
assert_scalar "0" \
  "SELECT count(*) FROM loyal_yield.route_policies WHERE vault_pubkey IN ('$vault_b', '$vault_c') AND active" \
  "cleanup deactivated canonical policies"
assert_scalar "sig-cleanup-b" \
  "SELECT last_seen_signature FROM loyal_yield.route_policies WHERE policy_account = '$policy_b'" \
  "confirm-missed cleanup retained policy-close evidence"
assert_scalar "sig-withdraw-c" \
  "SELECT last_seen_signature FROM loyal_yield.route_policies WHERE policy_account = '$policy_c'" \
  "cleanup-pending retained withdrawal evidence"
assert_scalar "0" \
  "SELECT count(*) FROM loyal_yield.balance_sweep_wallet_balance_events" \
  "Earn updates created no balance-sweep wallet events"
assert_scalar "0" \
  "SELECT count(*) FROM loyal_yield.balance_sweep_surplus_lots" \
  "Earn updates created no balance-sweep lots"
assert_scalar "0" \
  "SELECT count(*) FROM loyal_yield.balance_sweep_executions" \
  "Earn updates created no balance-sweep executions"
assert_scalar ":" \
  "SELECT COALESCE(to_regclass('loyal_yield.earn_reconciliation_jobs')::text, '') || ':' || COALESCE(to_regclass('loyal_yield.earn_reconciliation_receipts')::text, '')" \
  "rejected durable handoff tables are absent"

echo "== Run focused production checks"
(
  cd "$routing_root"
  NO_DNA=1 cargo fmt --all -- --check
  NO_DNA=1 cargo test -p balance-sweep-ata-monitor
  echo "PASS: fresh replaying session on Earn watch-set changes"
  echo "PASS: production principal proof nets balances per owner"
  echo "PASS: failed Earn proof wakes the supervisor immediately"
  NO_DNA=1 cargo check -p balance-sweep-ata-monitor -p loyal-yield-store \
    -p loyal-yield-orchestrator --bin yield-migrations
  git diff --check
)
(
  cd "$app_root"
  git diff --check
)

echo "PASS: LaserStream account updates directly reconcile canonical Earn state"
