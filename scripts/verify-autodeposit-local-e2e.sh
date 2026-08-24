#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
routing_root="$(cd "$script_dir/.." && pwd)"
app_root=""
scratch_dir="$(mktemp -d "/tmp/ask-2211-autodeposit-local-e2e.XXXXXX")"
postgres_data="$scratch_dir/postgres"
postgres_socket="$scratch_dir/postgres-socket"
postgres_log="$scratch_dir/postgres.log"
validator_log="$scratch_dir/validator.log"
realtime_log="$scratch_dir/realtime.log"
program_config_json="$scratch_dir/program-config.json"
usdc_mint_json="$scratch_dir/usdc-mint.json"
state_json="$scratch_dir/state.json"
subscribe_request_json="$scratch_dir/subscribe-request.json"
sse_event_json="$scratch_dir/setup-sse.json"
database_name="ask_2211_autodeposit_client_local_e2e"
base_port="$((24500 + RANDOM % 1200))"
rpc_port="$base_port"
faucet_port="$((base_port + 2))"
gossip_port="$((base_port + 3))"
dynamic_start="$((base_port + 4))"
dynamic_end="$((base_port + 40))"
realtime_port="$((base_port + 50))"
postgres_port="$((base_port + 60))"
validator_pid=""
realtime_pid=""
listener_pid=""
postgres_started=0
auth_secret="ask-2211-local-e2e-auth-secret-0000000000000000000000000000"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

pass() {
  echo "PASS: $*"
}

cleanup() {
  if [[ -n "$listener_pid" ]]; then
    kill "$listener_pid" >/dev/null 2>&1 || true
    wait "$listener_pid" >/dev/null 2>&1 || true
  fi
  if [[ -n "$realtime_pid" ]]; then
    kill "$realtime_pid" >/dev/null 2>&1 || true
    wait "$realtime_pid" >/dev/null 2>&1 || true
  fi
  if [[ -n "$validator_pid" ]]; then
    kill "$validator_pid" >/dev/null 2>&1 || true
    wait "$validator_pid" >/dev/null 2>&1 || true
  fi
  if [[ "$postgres_started" -eq 1 ]]; then
    "$pg_bindir/pg_ctl" -D "$postgres_data" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch_dir" 2>/dev/null || {
    sleep 0.2
    rm -rf "$scratch_dir"
  }
}
trap cleanup EXIT

while [[ $# -gt 0 ]]; do
  case "$1" in
    --app-root)
      app_root="${2:-}"
      shift 2
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n "$app_root" ]] || fail "--app-root is required"
app_root="$(cd "$app_root" && pwd)"
[[ "$app_root" != "$routing_root" ]] || fail "app and routing worktrees must be separate"

if [[ -x /opt/homebrew/opt/postgresql@17/bin/postgres ]]; then
  pg_bindir=/opt/homebrew/opt/postgresql@17/bin
else
  pg_bindir="$(pg_config --bindir)"
fi

for command_name in bun cargo curl jq solana-test-validator; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done
for postgres_command in initdb pg_ctl psql; do
  [[ -x "$pg_bindir/$postgres_command" ]] || fail "$postgres_command is required"
done
[[ -f "$app_root/apps/web/scripts/verify-autodeposit-local-chain.ts" ]] ||
  fail "app worktree is missing the Autodeposit local-chain verifier"
[[ -f "$routing_root/crates/squads-test-harness/fixtures/subscriptions/subscriptions_program.so" ]] ||
  fail "subscriptions program fixture is missing"

mkdir -p "$postgres_socket"
"$pg_bindir/initdb" -D "$postgres_data" -A trust --no-locale -E UTF8 >/dev/null
"$pg_bindir/pg_ctl" -D "$postgres_data" -l "$postgres_log" \
  -o "-F -k '$postgres_socket' -p $postgres_port -c listen_addresses=127.0.0.1" \
  -w start >/dev/null
postgres_started=1
"$pg_bindir/psql" -X --set=ON_ERROR_STOP=1 \
  --host="$postgres_socket" --port="$postgres_port" --username="$(id -un)" \
  --dbname=postgres --command="CREATE DATABASE $database_name" >/dev/null
database_url="postgresql://$(id -un)@127.0.0.1:${postgres_port}/${database_name}"

psql_verify() {
  "$pg_bindir/psql" -X --set=ON_ERROR_STOP=1 \
    --host="$postgres_socket" --port="$postgres_port" --username="$(id -un)" \
    --dbname="$database_name" "$@"
}

sql_scalar() {
  psql_verify -A -t --command="$1" | tr -d '[:space:]'
}

echo "== Isolated database and local chain"
(
  cd "$routing_root"
  NEON_DATABASE_URL="$database_url" NO_DNA=1 \
    cargo run --quiet -p loyal-yield-orchestrator --bin yield-migrations -- --apply
)
[[ "$(sql_scalar "SELECT max(version) FROM loyal_yield.schema_migrations")" == "65" ]] ||
  fail "isolated database did not apply Autodeposit target cluster migration 65"
psql_verify --command="
  INSERT INTO loyal_yield.realtime_configuration (singleton, solana_env)
  VALUES (TRUE, 'mainnet-beta')
  ON CONFLICT (singleton) DO UPDATE SET solana_env = EXCLUDED.solana_env;
  INSERT INTO loyal_yield.balance_sweep_targets (
    settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
    wallet, token_mint, wallet_token_ata, vault_token_ata, delegated_signers,
    threshold, max_amount_per_period, desired_active, chain_status,
    last_seen_slot, last_seen_signature
  ) VALUES (
    'migration-check-settings', 'migration-check-authority', 1,
    'migration-check-policy', 1, 'migration-check-vault',
    'migration-check-wallet', 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
    'migration-check-wallet-ata',
    'migration-check-vault-ata', ARRAY[]::TEXT[], 1, 1, TRUE, 'pending',
    1, 'migration-check-signature'
  );
" >/dev/null
psql_verify --file="$routing_root/crates/loyal-yield-store/migrations/0065_autodeposit_target_cluster.sql" >/dev/null
[[ "$(sql_scalar "SELECT cluster FROM loyal_yield.balance_sweep_targets WHERE policy_account = 'migration-check-policy'")" == "mainnet-beta" ]] ||
  fail "migration 65 did not repair a null-cluster Autodeposit target"
psql_verify --command="DELETE FROM loyal_yield.balance_sweep_targets WHERE policy_account = 'migration-check-policy'" >/dev/null
pass "migration 65 repairs existing null-cluster Autodeposit targets"
read -r program_config treasury usdc_mint < <(
  cd "$routing_root"
  cargo run --quiet -p loyal-local-e2e --bin autodeposit-local-genesis -- \
    "$program_config_json" "$usdc_mint_json"
)
[[ "$usdc_mint" == "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" ]] ||
  fail "genesis helper returned an unexpected USDC mint"

(
  cd "$routing_root"
  solana-test-validator \
    --reset \
    --ledger "$scratch_dir/ledger" \
    --rpc-port "$rpc_port" \
    --faucet-port "$faucet_port" \
    --gossip-port "$gossip_port" \
    --dynamic-port-range "$dynamic_start-$dynamic_end" \
    --bpf-program SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG \
      crates/squads-test-harness/fixtures/squads/squads_smart_account_program.so \
    --bpf-program De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44 \
      crates/squads-test-harness/fixtures/subscriptions/subscriptions_program.so \
    --account "$program_config" "$program_config_json" \
    --account "$usdc_mint" "$usdc_mint_json"
) >"$validator_log" 2>&1 &
validator_pid=$!

validator_ready=0
for _ in $(seq 1 200); do
  if curl --silent --fail \
    --header 'content-type: application/json' \
    --data '{"jsonrpc":"2.0","id":1,"method":"getHealth"}' \
    "http://127.0.0.1:$rpc_port" | jq -e '.result == "ok"' >/dev/null 2>&1; then
    validator_ready=1
    break
  fi
  sleep 0.1
done
if [[ "$validator_ready" -ne 1 ]]; then
  tail -80 "$validator_log" >&2 || true
  fail "isolated Solana validator did not become healthy"
fi
pass "isolated PostgreSQL and Solana validator are ready"

echo "== Web client builds and submits Autodeposit setup"
(
  cd "$app_root/apps/web"
  bun run scripts/verify-autodeposit-local-chain.ts setup \
    --rpc-url "http://127.0.0.1:$rpc_port" \
    --treasury "$treasury" \
    --output "$state_json"
)
[[ "$(wc -l < "$state_json.transactions.ndjson" | tr -d '[:space:]')" == "3" ]] ||
  fail "web setup did not produce the three expected finalized transactions"
jq -s -e \
  'map(.stage) == ["initialize_subscription_authority", "create_policy", "create_recurring_delegation"]' \
  "$state_json.transactions.ndjson" >/dev/null ||
  fail "web setup transaction sequence does not match the production stage machine"
pass "web client setup created the authority, policy, delegation, and ATA approval"

(
  cd "$routing_root"
  NEON_DATABASE_URL="$database_url" \
  REALTIME_AUTH_SECRET="$auth_secret" \
  REALTIME_ALLOWED_ORIGINS="http://127.0.0.1:3000" \
  REALTIME_HEARTBEAT_SECONDS=1 \
  PORT="$realtime_port" \
    cargo run --quiet -p loyal-yield-realtime
) >"$realtime_log" 2>&1 &
realtime_pid=$!

realtime_ready=0
for _ in $(seq 1 600); do
  if curl --silent --fail "http://127.0.0.1:$realtime_port/readyz" >/dev/null 2>&1; then
    realtime_ready=1
    break
  fi
  sleep 0.1
done
if [[ "$realtime_ready" -ne 1 ]]; then
  tail -80 "$realtime_log" >&2 || true
  fail "local realtime service did not become ready"
fi

(
  cd "$app_root/apps/web"
  bun run scripts/verify-autodeposit-local-chain.ts listen \
    --auth-secret "$auth_secret" \
    --events-url "http://127.0.0.1:$realtime_port/events" \
    --expected-reason "allowance_created" \
    --state "$state_json" \
    --output "$sse_event_json"
) &
listener_pid=$!

connected=0
for _ in $(seq 1 100); do
  if curl --silent --fail "http://127.0.0.1:$realtime_port/metrics" |
    grep -q '^loyal_realtime_active_connections 1$'; then
    connected=1
    break
  fi
  sleep 0.1
done
[[ "$connected" -eq 1 ]] || fail "web SSE consumer did not connect"

echo "== Emulated LaserStream notifications drive monitor reconciliation"
(
  cd "$routing_root"
  cargo run --quiet -p balance-sweep-ata-monitor \
    --bin autodeposit-targeted-account-local-e2e -- \
    --postgres-url "$database_url" \
    --rpc-url "http://127.0.0.1:$rpc_port" \
    --state "$state_json" \
    --transactions "$state_json.transactions.ndjson" \
    --subscribe-request-output "$subscribe_request_json"
)
wait "$listener_pid"
listener_pid=""

[[ "$(sql_scalar "SELECT count(*) FROM loyal_yield.balance_sweep_targets WHERE desired_active AND chain_status = 'active' AND policy_account IS NOT NULL AND subscription_authority IS NOT NULL AND recurring_delegation IS NOT NULL")" == "1" ]] ||
  fail "monitor did not project exactly one active Autodeposit target"
[[ "$(sql_scalar "SELECT count(*) FROM loyal_yield.autodeposit_reconciliation_requests WHERE processed_slot >= requested_slot AND last_error IS NULL")" == "1" ]] ||
  fail "Autodeposit reconciliation request was not fully processed"
[[ "$(sql_scalar "SELECT count(*) FROM loyal_yield.earn_reconciliation_jobs WHERE consumer_name = 'ask-2211-local-autodeposit' AND completed_at IS NOT NULL AND last_error IS NULL")" == "3" ]] ||
  fail "not every emulated LaserStream notification completed durably"
[[ "$(sql_scalar "SELECT count(*) FROM loyal_yield.realtime_events WHERE event_type = 'earn.autodeposit.configuration.changed' AND reason = 'allowance_created'")" == "1" ]] ||
  fail "chain activation did not emit exactly one allowance_created event"
jq -e \
  '.commitment == "finalized" and (.transactions | length) == 0 and (.accounts.earn_autodeposit_wallet_atas | length) == 1 and (.accounts.earn_subscription_authorities | length) == 1 and (.accounts.earn_recurring_delegations | length) == 1' \
  "$subscribe_request_json" >/dev/null ||
  fail "refreshed monitor subscription is not finalized and account-only"
jq -e \
  '.event.eventType == "earn.autodeposit.configuration.changed" and .event.reason == "allowance_created" and .refreshPlan.earnState == true and .refreshPlan.transactions == true' \
  "$sse_event_json" >/dev/null ||
  fail "web SSE Autodeposit invalidation is incorrect"
pass "monitor projected finalized chain state and web consumed its realtime invalidation"

for removed_route in \
  "$app_root/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/setup/confirm/route.ts" \
  "$app_root/apps/web/src/app/api/smart-accounts/mobile/earn/autodeposit/setup/confirm/route.ts"; do
  [[ ! -e "$removed_route" ]] || fail "client setup confirmation route still writes database state: $removed_route"
done
pass "Autodeposit setup has no client confirmation API database write"
pass "ASK-2211 isolated local Autodeposit setup, reconciliation, realtime, and web SSE E2E"
