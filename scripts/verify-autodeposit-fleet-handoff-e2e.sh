#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
routing_root="$(cd "$script_dir/.." && pwd)"
app_root="${LOYAL_APP_ROOT:-/Users/zotho/Dev/loyal/service-fixes/loyal-app}"
scratch_dir="$(mktemp -d "/tmp/autodeposit_verify_fleet_handoff.XXXXXX")"
database_name="autodeposit_verify_fleet_handoff"
base_port="$((25200 + RANDOM % 800))"
rpc_port="$base_port"
faucet_port="$((base_port + 2))"
gossip_port="$((base_port + 3))"
dynamic_start="$((base_port + 4))"
dynamic_end="$((base_port + 40))"
postgres_port="$((base_port + 50))"
postgres_data="$scratch_dir/postgres"
postgres_socket="$scratch_dir/postgres-socket"
postgres_log="$scratch_dir/postgres.log"
validator_log="$scratch_dir/validator.log"
first_log="$scratch_dir/trigger-first.log"
second_log="$scratch_dir/trigger-second.log"
negative_log="$scratch_dir/trigger-negative.log"
timings_file="$scratch_dir/timings.ndjson"
validator_pid=""
postgres_started=0
node_modules_link_created=0
stage_started=0

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cleanup() {
  if [[ -n "$validator_pid" ]]; then
    kill "$validator_pid" >/dev/null 2>&1 || true
    wait "$validator_pid" >/dev/null 2>&1 || true
  fi
  if [[ "$postgres_started" -eq 1 ]]; then
    "$pg_bindir/pg_ctl" -D "$postgres_data" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  if [[ "$node_modules_link_created" -eq 1 ]]; then
    unlink "$routing_root/node_modules"
  fi
  if [[ "${AUTODEPOSIT_E2E_KEEP_SCRATCH:-0}" == "1" ]]; then
    echo "scratch_dir=$scratch_dir" >&2
  else
    rm -rf "$scratch_dir"
  fi
}
trap cleanup EXIT

stage_begin() {
  stage_started="$(date +%s)"
}

stage_end() {
  local name="$1"
  local elapsed="$(( $(date +%s) - stage_started ))"
  jq -cn --arg stage "$name" --argjson seconds "$elapsed" \
    '{stage: $stage, seconds: $seconds}' >> "$timings_file"
  echo "TIMING $name ${elapsed}s"
}

for command_name in bun cargo curl jq solana solana-keygen solana-test-validator; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done
[[ -d "$app_root" ]] || fail "LOYAL_APP_ROOT does not exist: $app_root"
[[ -d /Users/zotho/Dev/loyal/service-fixes/loyal-yield-routing/node_modules ]] ||
  fail "the primary routing checkout must have node_modules for the Bun executor"
node_modules_root="/Users/zotho/Dev/loyal/service-fixes/loyal-yield-routing/node_modules"
if [[ ! -e "$routing_root/node_modules" ]]; then
  ln -s "$node_modules_root" "$routing_root/node_modules"
  node_modules_link_created=1
fi

if [[ -x /opt/homebrew/opt/postgresql@17/bin/postgres ]]; then
  pg_bindir=/opt/homebrew/opt/postgresql@17/bin
else
  pg_bindir="$(pg_config --bindir)"
fi
for postgres_command in initdb pg_ctl psql; do
  [[ -x "$pg_bindir/$postgres_command" ]] || fail "$postgres_command is required"
done

common_git_dir="$(git -C "$routing_root" rev-parse --git-common-dir)"
common_root="$(cd "$(dirname "$common_git_dir")" && pwd)"
export CARGO_TARGET_DIR="${AUTODEPOSIT_E2E_TARGET_DIR:-$common_root/target/autodeposit-fleet-handoff-e2e}"

stage_begin
cargo build --quiet --manifest-path "$routing_root/Cargo.toml" \
  -p loyal-yield-orchestrator --bin yield-migrations \
  -p balance-sweep-autodeposit-trigger --bin balance-sweep-autodeposit-trigger
stage_end build

stage_begin
mkdir -p "$postgres_socket"
"$pg_bindir/initdb" -D "$postgres_data" -A trust --no-locale -E UTF8 \
  --set=shared_memory_type=mmap \
  --set=dynamic_shared_memory_type=posix >/dev/null
"$pg_bindir/pg_ctl" -D "$postgres_data" -l "$postgres_log" \
  -o "-F -k '$postgres_socket' -p $postgres_port -c listen_addresses=127.0.0.1 -c shared_memory_type=mmap -c dynamic_shared_memory_type=posix" \
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

NEON_DATABASE_URL="$database_url" NO_DNA=1 \
  "$CARGO_TARGET_DIR/debug/yield-migrations" --apply >/dev/null
psql_verify -A -t --command="
  SELECT EXISTS (
    SELECT 1
    FROM loyal_yield.schema_migrations
    WHERE version = 69
      AND name = 'autodeposit_fleet_handoff_recovery'
  ) AND to_regprocedure(
    'loyal_yield.finalize_fleet_handoff_autodeposit(text,bigint,bigint,text,bigint)'
  ) IS NOT NULL;
" | grep -qx 't' || fail "migration 69 recovery function was not applied"
psql_verify --file="$app_root/apps/web/src/lib/yield-optimization/migrations/0001_add_user_yield_deposit_positions.sql" >/dev/null
psql_verify --file="$app_root/apps/web/src/lib/yield-optimization/migrations/0004_add_verifiable_earn_holdings.sql" >/dev/null
# The routing migration is intentionally tolerant of app-owned Earn tables not
# existing yet. Replay it after those base tables are present so a blank verifier
# database reaches the same cross-repository schema as production.
psql_verify --single-transaction \
  --file="$routing_root/crates/loyal-yield-store/migrations/0015_realtime_web_mobile_protocol.sql" >/dev/null
stage_end database_setup

stage_begin
solana-keygen new --no-bip39-passphrase --silent --force \
  --outfile "$scratch_dir/payer.json" >/dev/null
solana-keygen new --no-bip39-passphrase --silent --force \
  --outfile "$scratch_dir/recipient.json" >/dev/null
payer="$(solana-keygen pubkey "$scratch_dir/payer.json")"
recipient="$(solana-keygen pubkey "$scratch_dir/recipient.json")"
solana-test-validator --reset --quiet \
  --ledger "$scratch_dir/ledger" \
  --mint "$payer" \
  --rpc-port "$rpc_port" \
  --faucet-port "$faucet_port" \
  --gossip-port "$gossip_port" \
  --dynamic-port-range "$dynamic_start-$dynamic_end" \
  >"$validator_log" 2>&1 &
validator_pid=$!
rpc_url="http://127.0.0.1:$rpc_port"
for _ in $(seq 1 40); do
  if curl -sf -X POST -H 'content-type: application/json' \
    --data '{"jsonrpc":"2.0","id":1,"method":"getHealth"}' "$rpc_url" \
    | jq -e '.result == "ok"' >/dev/null; then
    break
  fi
  sleep 0.25
done
kill -0 "$validator_pid" >/dev/null 2>&1 || fail "local validator exited"

send_confirmed_transfer() {
  local output signature
  output="$(solana transfer "$recipient" 0.001 \
    --from "$scratch_dir/payer.json" --fee-payer "$scratch_dir/payer.json" \
    --allow-unfunded-recipient --commitment confirmed --url "$rpc_url" \
    --output json)"
  signature="$(jq -r '.signature' <<<"$output")"
  [[ -n "$signature" && "$signature" != "null" ]] || fail "missing local signature"
  printf '%s\n' "$signature"
}

pull_signature="$(send_confirmed_transfer)"
fleet_signature="$(send_confirmed_transfer)"
negative_pull_signature="$(send_confirmed_transfer)"
negative_fleet_signature="$(send_confirmed_transfer)"

confirmation_statuses="$(curl -sf -X POST -H 'content-type: application/json' \
  --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"getSignatureStatuses\",\"params\":[[\"$pull_signature\",\"$fleet_signature\",\"$negative_pull_signature\",\"$negative_fleet_signature\"],{\"searchTransactionHistory\":true}]}" \
  "$rpc_url")"
jq -e '
  [.result.value[] | {
    confirmationStatus,
    err
  }] == [
    {confirmationStatus: "confirmed", err: null},
    {confirmationStatus: "confirmed", err: null},
    {confirmationStatus: "confirmed", err: null},
    {confirmationStatus: "confirmed", err: null}
  ]
' <<<"$confirmation_statuses" >/dev/null \
  || fail "local transactions must be confirmed but not finalized before recovery"

transaction_slot() {
  curl -sf -X POST -H 'content-type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"getTransaction\",\"params\":[\"$1\",{\"commitment\":\"confirmed\",\"maxSupportedTransactionVersion\":0}]}" \
    "$rpc_url" | jq -er '.result.slot'
}

pull_slot="$(transaction_slot "$pull_signature")"
fleet_slot="$(transaction_slot "$fleet_signature")"
negative_pull_slot="$(transaction_slot "$negative_pull_signature")"
negative_fleet_slot="$(transaction_slot "$negative_fleet_signature")"
(( fleet_slot > pull_slot )) || fail "Fleet transaction must confirm after pull"
(( negative_pull_slot > fleet_slot )) || fail "negative pull must confirm after successful Fleet transaction"
(( negative_fleet_slot > negative_pull_slot )) || fail "negative Fleet transaction must confirm after negative pull"
stage_end local_chain

stage_begin
psql_verify --command="
    INSERT INTO loyal_yield.route_policies
      (id, settings, authority, policy_seed, policy_account, vault_index,
       vault_pubkey, delegated_signers, threshold, route_modes, stable_mints,
       kamino_markets, kamino_liquidity_mints, last_seen_slot, last_seen_signature)
    VALUES
      (1, 'settings-e2e', '$payer', 7, 'route-policy-e2e', 1,
       'vault-e2e', ARRAY['$payer'], 1, ARRAY['same_mint_kamino'],
       ARRAY['EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v'], ARRAY['market-e2e'],
       ARRAY['EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v'], $pull_slot, '$pull_signature');

    INSERT INTO loyal_yield.managed_vaults
      (id, settings, vault_index, vault_pubkey, active_policy_id)
    VALUES (1, 'settings-e2e', 1, 'vault-e2e', 1);

    INSERT INTO loyal_yield.balance_sweep_targets
      (id, settings, authority, policy_seed, policy_account, vault_index,
       vault_pubkey, wallet, wallet_usdc_ata, vault_usdc_ata, token_mint,
       wallet_token_ata, vault_token_ata, delegated_signers, threshold,
       max_amount_per_period, desired_active, chain_status,
       chain_observation_slot, last_seen_slot, last_seen_signature, cluster)
    VALUES
      (1, 'settings-e2e', '$payer', 8, 'autodeposit-policy-e2e', 1,
       'vault-e2e', '$payer', 'wallet-ata-e2e', 'vault-ata-e2e',
       'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
       'wallet-ata-e2e', 'vault-ata-e2e', ARRAY['$payer'], 1, 1000000,
       TRUE, 'active', $fleet_slot, $fleet_slot, '$fleet_signature', 'mainnet-beta');

    INSERT INTO loyal_yield.balance_sweep_lot_claims
      (claim_token, target_id, amount_raw, status, autodeposit_deposit_plan)
    VALUES
      ('claim-e2e', 1, 100, 'selected', jsonb_build_object(
        'version', 1, 'amountRaw', '100', 'reserve', 'direct-plan-reserve',
        'market', 'direct-plan-market',
        'liquidityMint', 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
        'target', jsonb_build_object(
          'id', '1', 'managedVaultId', '1', 'settings', 'settings-e2e',
          'vaultIndex', 1, 'wallet', '$payer', 'walletUsdcAta', 'wallet-ata-e2e',
          'walletTokenAta', 'wallet-ata-e2e', 'vaultPubkey', 'vault-e2e',
          'vaultUsdcAta', 'vault-ata-e2e', 'vaultTokenAta', 'vault-ata-e2e',
          'tokenMint', 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
          'routePolicyAccount', 'route-policy-e2e', 'routePolicySeed', '7',
          'currentReserve', 'initial-reserve', 'currentMarket', 'initial-market',
          'currentLiquidityMint', 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v'
        )
      ));

    INSERT INTO loyal_yield.balance_sweep_scheduled_slots
      (id, target_id, token_mint, eligible_after, status, claim_token)
    VALUES
      (1, 1, 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', now(), 'selected', 'claim-e2e');

    INSERT INTO loyal_yield.balance_sweep_transaction_attempts
      (claim_token, target_id, scheduled_slot_id, operation_kind, attempt_number,
       amount_raw, source_pre_balance_raw, destination_pre_balance_raw, signature,
       signed_transaction_base64, signed_transaction_sha256, recent_blockhash,
       last_valid_block_height, attempt_state, broadcast_count, confirmed_slot)
    VALUES
      ('claim-e2e', 1, 1, 'pull', 1, 100, 100, 0, '$pull_signature',
       'c2lnbmVk', repeat('a', 64), 'local-blockhash', 999999999,
       'confirmed', 1, $pull_slot);

    INSERT INTO loyal_yield.balance_sweep_executions
      (id, target_id, signature, slot, source_wallet_ata, destination_vault_ata,
       token_mint, source_token_ata, destination_token_ata, amount_raw,
       source_pre_balance_raw, source_post_balance_raw,
       destination_pre_balance_raw, destination_post_balance_raw,
       source_commitment, raw_evidence, decoded_evidence, received_at,
       decoded_at, dedupe_key)
    VALUES
      (1, 1, '$pull_signature', $pull_slot, 'wallet-ata-e2e', 'vault-ata-e2e',
       'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
       'wallet-ata-e2e', 'vault-ata-e2e', 100, 100, 0, 0, 100,
       'confirmed', '{\"source\":\"single-vault-autodeposit-executor\"}',
       '{\"sequence\":\"subscription_pull_then_mandatory_kamino_deposit\"}',
       now(), now(), '1:autodeposit-pull:$pull_signature');

    INSERT INTO loyal_yield.vault_position_snapshots
      (id, vault_id, policy_id, observed_slot, observed_at, chain_slot, is_current)
    VALUES (1, 1, 1, $fleet_slot, now(), $fleet_slot, TRUE);

    INSERT INTO loyal_yield.vault_position_snapshot_positions
      (snapshot_id, reserve, market, liquidity_mint, amount_raw,
       supply_apy_bps, has_value)
    VALUES
      (1, 'fleet-actual-reserve', 'fleet-actual-market',
       'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', 600, 425, TRUE);

    INSERT INTO loyal_yield.rebalance_decisions
      (id, vault_id, status, target_reserve, liquidity_mint,
       source_liquidity_mint, target_liquidity_mint, amount_raw,
       source_apy_bps, target_apy_bps, estimated_edge_bps,
       decision_reason, execution_plan, idempotency_key, signature,
       submitted_slot, confirmed_slot, post_snapshot_id)
    VALUES
      (1, 1, 'confirmed', 'fleet-actual-reserve',
       'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
       'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
       'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', 100, 0, 425, 425,
       'idle_vault_liquidity_available',
       jsonb_build_object(
         'kind', 'idle_vault_deposit',
         'idle_token_account', 'vault-ata-e2e',
         'idle_observed_slot', $pull_slot,
         'idle_vault_liquidity_amount_raw', 100
       ), 'fleet-handoff-e2e',
       '$fleet_signature', $fleet_slot, $fleet_slot, 1);

    INSERT INTO loyal_yield.user_yield_positions
      (id, wallet_address, smart_account_address, settings, vault_index,
       vault_pubkey, policy_id, policy_account, policy_seed, initial_reserve,
       initial_market, initial_liquidity_mint, initial_supply_apy_bps,
       deposit_mint, principal_amount_raw, current_reserve, current_market,
       current_liquidity_mint, current_amount_raw, current_observed_slot,
       current_observed_at, first_deposit_signature, last_deposit_signature,
       last_confirmed_slot, status, created_at, updated_at)
    VALUES
      (1, '$payer', 'vault-e2e', 'settings-e2e', 1, 'vault-e2e', 7,
       'route-policy-e2e', 7, 'initial-reserve', 'initial-market',
       'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', 300,
       'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', 500,
       'newer-reserve', 'newer-market',
       'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', 777,
       $fleet_slot + 1, now(), 'initial-deposit-e2e', 'newer-deposit-e2e',
       $fleet_slot + 1, 'active', now(), now());
  " >/dev/null
stage_end fixture

run_trigger() {
  local output_file="$1"
  NEON_DATABASE_URL="$database_url" \
  SOLANA_RPC_URL="$rpc_url" \
  NODE_PATH="$node_modules_root" \
  AUTODEPOSIT_LOCAL_POSTGRES_E2E=1 \
  OTEL_SDK_DISABLED=true \
  RUST_LOG=info \
  "$CARGO_TARGET_DIR/debug/balance-sweep-autodeposit-trigger" \
    --once --disable-realtime-listen --execute-eligible --execute-limit 1 \
    --executor-command "NODE_PATH='$node_modules_root' AUTODEPOSIT_LOCAL_POSTGRES_E2E=1 bun '$routing_root/scripts/execute-autodeposit-policy.ts' --require-lot-claim" \
    >"$output_file" 2>&1
}

stage_begin
run_trigger "$first_log"
run_trigger "$second_log"
stage_end execution_twice

if rg -n \
  'autodeposit transaction effect remains ambiguous after blockhash expiry|loyal\.operational_error|autodeposit_[a-z_]*ambiguous|"status"[[:space:]]*:[[:space:]]*"error"' \
  "$first_log" "$second_log"; then
  fail "Autodeposit emitted an operational or ambiguity error"
fi
rg -q 'autodeposit_completed' "$first_log" || fail "first pass did not complete recovery"
rg -q 'targets_scanned=0' "$second_log" || fail "second pass did not prove an empty recovery queue"

proof="$(psql_verify -A -t --command="
  SELECT jsonb_build_object(
    'claimStatus', (SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = 'claim-e2e'),
    'slotStatus', (SELECT status::text FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = 1),
    'executionCompleted', (SELECT completed_at IS NOT NULL FROM loyal_yield.balance_sweep_executions WHERE id = 1),
    'recoverySource', (SELECT decoded_evidence ->> 'recoverySource' FROM loyal_yield.balance_sweep_executions WHERE id = 1),
    'fleetDecisionId', (SELECT decoded_evidence ->> 'fleetDecisionId' FROM loyal_yield.balance_sweep_executions WHERE id = 1),
    'topUpAttempts', (SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts WHERE operation_kind = 'top_up'),
    'deposits', (SELECT count(*) FROM loyal_yield.user_yield_position_deposits WHERE deposit_signature = '$fleet_signature'),
    'principalRaw', (SELECT principal_amount_raw FROM loyal_yield.user_yield_positions WHERE id = 1),
    'currentReserve', (SELECT current_reserve FROM loyal_yield.user_yield_positions WHERE id = 1),
    'currentAmountRaw', (SELECT current_amount_raw FROM loyal_yield.user_yield_positions WHERE id = 1),
    'currentObservedSlot', (SELECT current_observed_slot FROM loyal_yield.user_yield_positions WHERE id = 1),
    'lastDepositSignature', (SELECT last_deposit_signature FROM loyal_yield.user_yield_positions WHERE id = 1),
    'lastRebalanceDecisionId', (SELECT last_rebalance_decision_id FROM loyal_yield.user_yield_positions WHERE id = 1),
    'holdingEvents', (SELECT count(*) FROM loyal_yield.user_yield_position_holding_events WHERE source_signature = '$fleet_signature' AND source_rebalance_decision_id = 1 AND source_snapshot_id = 1)
  )
" | tr -d '\n')"

jq -e --argjson expectedCurrentObservedSlot "$((fleet_slot + 1))" '
  .claimStatus == "executed" and
  .slotStatus == "executed" and
  .executionCompleted == true and
  .recoverySource == "fleet_idle_handoff" and
  .fleetDecisionId == "1" and
  .topUpAttempts == 0 and
  .deposits == 1 and
  .principalRaw == 600 and
  .currentReserve == "newer-reserve" and
  .currentAmountRaw == 777 and
  .currentObservedSlot == $expectedCurrentObservedSlot and
  .lastDepositSignature == "newer-deposit-e2e" and
  .lastRebalanceDecisionId == null and
  .holdingEvents == 1
' <<<"$proof" >/dev/null || fail "durable recovery proof failed: $proof"

psql_verify --command="
  INSERT INTO loyal_yield.balance_sweep_lot_claims
    (claim_token, target_id, amount_raw, status, autodeposit_deposit_plan)
  VALUES
    ('claim-bad-provenance-e2e', 1, 100, 'selected', jsonb_build_object(
      'version', 1, 'amountRaw', '100', 'reserve', 'direct-plan-reserve',
      'market', 'direct-plan-market',
      'liquidityMint', 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
      'target', jsonb_build_object(
        'id', '1', 'managedVaultId', '1', 'settings', 'settings-e2e',
        'vaultIndex', 1, 'wallet', '$payer', 'walletUsdcAta', 'wallet-ata-e2e',
        'walletTokenAta', 'wallet-ata-e2e', 'vaultPubkey', 'vault-e2e',
        'vaultUsdcAta', 'vault-ata-e2e', 'vaultTokenAta', 'vault-ata-e2e',
        'tokenMint', 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
        'routePolicyAccount', 'route-policy-e2e', 'routePolicySeed', '7',
        'currentReserve', 'newer-reserve', 'currentMarket', 'newer-market',
        'currentLiquidityMint', 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v'
      )
    ));

  INSERT INTO loyal_yield.balance_sweep_scheduled_slots
    (id, target_id, token_mint, eligible_after, status, claim_token)
  VALUES
    (2, 1, 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', now(), 'selected',
     'claim-bad-provenance-e2e');

  INSERT INTO loyal_yield.balance_sweep_transaction_attempts
    (claim_token, target_id, scheduled_slot_id, operation_kind, attempt_number,
     amount_raw, source_pre_balance_raw, destination_pre_balance_raw, signature,
     signed_transaction_base64, signed_transaction_sha256, recent_blockhash,
     last_valid_block_height, attempt_state, broadcast_count, confirmed_slot)
  VALUES
    ('claim-bad-provenance-e2e', 1, 2, 'pull', 1, 100, 100, 0,
     '$negative_pull_signature', 'c2lnbmVk', repeat('b', 64),
     'local-blockhash-negative', 999999999, 'confirmed', 1, $negative_pull_slot);

  INSERT INTO loyal_yield.balance_sweep_executions
    (id, target_id, signature, slot, source_wallet_ata, destination_vault_ata,
     token_mint, source_token_ata, destination_token_ata, amount_raw,
     source_pre_balance_raw, source_post_balance_raw,
     destination_pre_balance_raw, destination_post_balance_raw,
     source_commitment, raw_evidence, decoded_evidence, received_at,
     decoded_at, dedupe_key)
  VALUES
    (2, 1, '$negative_pull_signature', $negative_pull_slot,
     'wallet-ata-e2e', 'vault-ata-e2e',
     'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
     'wallet-ata-e2e', 'vault-ata-e2e', 100, 100, 0, 0, 100,
     'confirmed', '{\"source\":\"single-vault-autodeposit-executor\"}',
     '{\"sequence\":\"subscription_pull_then_mandatory_kamino_deposit\"}',
     now(), now(), '1:autodeposit-pull:$negative_pull_signature');

  INSERT INTO loyal_yield.vault_position_snapshots
    (id, vault_id, policy_id, observed_slot, observed_at, chain_slot, is_current)
  VALUES (2, 1, 1, $negative_fleet_slot, now(), $negative_fleet_slot, FALSE);

  INSERT INTO loyal_yield.vault_position_snapshot_positions
    (snapshot_id, reserve, market, liquidity_mint, amount_raw,
     supply_apy_bps, has_value)
  VALUES
    (2, 'bad-provenance-reserve', 'fleet-actual-market',
     'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', 877, 425, TRUE);

  INSERT INTO loyal_yield.rebalance_decisions
    (id, vault_id, status, target_reserve, liquidity_mint,
     source_liquidity_mint, target_liquidity_mint, amount_raw,
     source_apy_bps, target_apy_bps, estimated_edge_bps,
     decision_reason, execution_plan, idempotency_key, signature,
     submitted_slot, confirmed_slot, post_snapshot_id)
  VALUES
    (2, 1, 'confirmed', 'bad-provenance-reserve',
     'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
     'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v',
     'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', 100, 0, 425, 425,
     'idle_vault_liquidity_available',
     jsonb_build_object(
       'kind', 'idle_vault_deposit',
       'idle_token_account', 'wrong-vault-ata-e2e',
       'idle_observed_slot', $negative_pull_slot - 1,
       'idle_vault_liquidity_amount_raw', 100
     ), 'bad-fleet-handoff-e2e', '$negative_fleet_signature',
     $negative_fleet_slot, $negative_fleet_slot, 2);
" >/dev/null

run_trigger "$negative_log"
rg -q 'autodeposit_deposit_handoff_ambiguous' "$negative_log" \
  || fail "bad Fleet provenance did not emit the deposit-handoff ambiguity alert"
if rg -q 'autodeposit transaction effect remains ambiguous after blockhash expiry' "$negative_log"; then
  fail "bad Fleet provenance emitted the old blockhash-expiry alert"
fi

negative_proof="$(psql_verify -A -t --command="
  SELECT jsonb_build_object(
    'claimStatus', (SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = 'claim-bad-provenance-e2e'),
    'slotStatus', (SELECT status::text FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = 2),
    'executionCompleted', (SELECT completed_at IS NOT NULL FROM loyal_yield.balance_sweep_executions WHERE id = 2),
    'topUpAttempts', (SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts WHERE claim_token = 'claim-bad-provenance-e2e' AND operation_kind = 'top_up'),
    'deposits', (SELECT count(*) FROM loyal_yield.user_yield_position_deposits WHERE deposit_signature = '$negative_fleet_signature')
  )
" | tr -d '\n')"
jq -e '
  .claimStatus == "selected" and
  .slotStatus == "selected" and
  .executionCompleted == false and
  .topUpAttempts == 0 and
  .deposits == 0
' <<<"$negative_proof" >/dev/null \
  || fail "bad-provenance fail-closed proof failed: $negative_proof"

jq -cn \
  --arg status pass \
  --arg pullSignature "$pull_signature" \
  --arg fleetSignature "$fleet_signature" \
  --argjson proof "$proof" \
  --argjson negativeProof "$negative_proof" \
  --slurpfile timings "$timings_file" \
  '{status: $status, scenario: "legacy_autodeposit_pull_consumed_by_fleet", pullSignature: $pullSignature, fleetSignature: $fleetSignature, proof: $proof, negativeProof: $negativeProof, timings: $timings}'
