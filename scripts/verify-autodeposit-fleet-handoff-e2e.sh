#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
routing_root="$(cd "$script_dir/.." && pwd)"
app_root="${LOYAL_APP_ROOT:-/Users/zotho/Dev/loyal/service-fixes/loyal-app}"
scratch_dir="$(mktemp -d "/tmp/autodeposit_verify_fleet_handoff.XXXXXX")"
database_name="autodeposit_verify_fleet_handoff"
base_port="$((25200 + RANDOM % 800))"
rpc_port="$base_port"
repair_rpc_port="$((base_port + 70))"
faucet_port="$((base_port + 2))"
gossip_port="$((base_port + 3))"
dynamic_start="$((base_port + 4))"
dynamic_end="$((base_port + 40))"
postgres_port="$((base_port + 50))"
postgres_data="$scratch_dir/postgres"
postgres_socket="$scratch_dir/postgres-socket"
postgres_log="$scratch_dir/postgres.log"
validator_log="$scratch_dir/validator.log"
first_log="$scratch_dir/repair-first.log"
second_log="$scratch_dir/repair-second.log"
trigger_log="$scratch_dir/trigger-after-repair.log"
negative_log="$scratch_dir/repair-negative.log"
negative_evidence_log="$scratch_dir/repair-negative-evidence.log"
negative_chain_log="$scratch_dir/repair-negative-chain.log"
timings_file="$scratch_dir/timings.ndjson"
validator_pid=""
repair_rpc_pid=""
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
  if [[ -n "$repair_rpc_pid" ]]; then
    kill "$repair_rpc_pid" >/dev/null 2>&1 || true
    wait "$repair_rpc_pid" >/dev/null 2>&1 || true
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
  SELECT NOT EXISTS (
    SELECT 1
    FROM loyal_yield.schema_migrations
    WHERE version = 69
  );
" | grep -qx 't' || fail "the bounded repair must not install migration 69"
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
solana-keygen new --no-bip39-passphrase --silent --force \
  --outfile "$scratch_dir/vault-owner.json" >/dev/null
for token_account in wallet-token vault-token fleet-reserve invalid-fleet-reserve fleet-liquidity-supply; do
  solana-keygen new --no-bip39-passphrase --silent --force \
    --outfile "$scratch_dir/$token_account.json" >/dev/null
done
solana-keygen new --no-bip39-passphrase --silent --force \
  --outfile "$scratch_dir/mint.json" >/dev/null
payer="$(solana-keygen pubkey "$scratch_dir/payer.json")"
recipient="$(solana-keygen pubkey "$scratch_dir/recipient.json")"
vault_owner="$(solana-keygen pubkey "$scratch_dir/vault-owner.json")"
mint="$(solana-keygen pubkey "$scratch_dir/mint.json")"
wallet_token_ata="$(solana-keygen pubkey "$scratch_dir/wallet-token.json")"
vault_token_ata="$(solana-keygen pubkey "$scratch_dir/vault-token.json")"
fleet_liquidity_supply="$(solana-keygen pubkey "$scratch_dir/fleet-liquidity-supply.json")"
fleet_reserve="$(solana-keygen pubkey "$scratch_dir/fleet-reserve.json")"
invalid_fleet_reserve="$(solana-keygen pubkey "$scratch_dir/invalid-fleet-reserve.json")"
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

send_confirmed_sol_transfer() {
  local output signature
  output="$(solana transfer "$recipient" 0.001 \
    --from "$scratch_dir/payer.json" --fee-payer "$scratch_dir/payer.json" \
    --allow-unfunded-recipient --commitment confirmed --url "$rpc_url" \
    --output json)"
  signature="$(jq -r '.signature' <<<"$output")"
  [[ -n "$signature" && "$signature" != "null" ]] || fail "missing local signature"
  printf '%s\n' "$signature"
}

pull_signature="$(send_confirmed_sol_transfer)"
fleet_signature="$(send_confirmed_sol_transfer)"
negative_valid_pull_signature="$(send_confirmed_sol_transfer)"
negative_valid_fleet_signature="$(send_confirmed_sol_transfer)"
negative_pull_signature="$(send_confirmed_sol_transfer)"
negative_fleet_signature="$(send_confirmed_sol_transfer)"

confirmation_statuses="$(curl -sf -X POST -H 'content-type: application/json' \
  --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"getSignatureStatuses\",\"params\":[[\"$pull_signature\",\"$fleet_signature\",\"$negative_valid_pull_signature\",\"$negative_valid_fleet_signature\",\"$negative_pull_signature\",\"$negative_fleet_signature\"],{\"searchTransactionHistory\":true}]}" \
  "$rpc_url")"
jq -e '
  all(.result.value[];
    (.confirmationStatus == "confirmed" or .confirmationStatus == "finalized") and
    .err == null
  )
' <<<"$confirmation_statuses" >/dev/null \
  || fail "local transactions must reach at least confirmed before recovery"

transaction_slot() {
  curl -sf -X POST -H 'content-type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"getTransaction\",\"params\":[\"$1\",{\"commitment\":\"confirmed\",\"maxSupportedTransactionVersion\":0}]}" \
    "$rpc_url" | jq -er '.result.slot'
}

pull_slot="$(transaction_slot "$pull_signature")"
fleet_slot="$(transaction_slot "$fleet_signature")"
negative_pull_slot="$(transaction_slot "$negative_pull_signature")"
negative_fleet_slot="$(transaction_slot "$negative_fleet_signature")"
negative_valid_pull_slot="$(transaction_slot "$negative_valid_pull_signature")"
negative_valid_fleet_slot="$(transaction_slot "$negative_valid_fleet_signature")"
(( fleet_slot > pull_slot )) || fail "Fleet transaction must confirm after pull"
(( negative_valid_pull_slot > fleet_slot )) || fail "negative valid pull must confirm after successful Fleet transaction"
(( negative_valid_fleet_slot > negative_valid_pull_slot )) || fail "negative valid Fleet transaction must confirm after negative valid pull"
(( negative_pull_slot > negative_valid_fleet_slot )) || fail "negative pull must confirm after valid negative Fleet transaction"
(( negative_fleet_slot > negative_pull_slot )) || fail "negative Fleet transaction must confirm after negative pull"

rpc_fixture_spec="$(jq -cn \
  --arg pull "$pull_signature" --argjson pullSlot "$pull_slot" \
  --arg fleet "$fleet_signature" --argjson fleetSlot "$fleet_slot" \
  --arg negativeValidPull "$negative_valid_pull_signature" --argjson negativeValidPullSlot "$negative_valid_pull_slot" \
  --arg negativeValidFleet "$negative_valid_fleet_signature" --argjson negativeValidFleetSlot "$negative_valid_fleet_slot" \
  --arg negativePull "$negative_pull_signature" --argjson negativePullSlot "$negative_pull_slot" \
  --arg negativeFleet "$negative_fleet_signature" --argjson negativeFleetSlot "$negative_fleet_slot" \
  --arg mint "$mint" --arg wallet "$wallet_token_ata" --arg vault "$vault_token_ata" \
  --arg reserve "$fleet_reserve" --arg invalidReserve "$invalid_fleet_reserve" \
  --arg supply "$fleet_liquidity_supply" \
  '{transactions: {
      ($pull): {kind: "pull", slot: $pullSlot},
      ($fleet): {kind: "fleet", slot: $fleetSlot},
      ($negativeValidPull): {kind: "pull", slot: $negativeValidPullSlot},
      ($negativeValidFleet): {kind: "fleet", slot: $negativeValidFleetSlot},
      ($negativePull): {kind: "unrelated", slot: $negativePullSlot},
      ($negativeFleet): {kind: "fleet_no_instruction", slot: $negativeFleetSlot}
    }, mint: $mint, wallet: $wallet, vault: $vault, reserve: $reserve,
       invalidReserve: $invalidReserve, supply: $supply}')"
AUTODEPOSIT_RPC_FIXTURE="$rpc_fixture_spec" REPAIR_RPC_PORT="$repair_rpc_port" bun -e '
  import { createHash } from "node:crypto";
  import bs58 from "bs58";
  import { PublicKey } from "@solana/web3.js";
  const spec = JSON.parse(Bun.env.AUTODEPOSIT_RPC_FIXTURE!);
  const klend = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
  const discriminator = createHash("sha256")
    .update("global:deposit_reserve_liquidity_and_obligation_collateral_v2")
    .digest().subarray(0, 8);
  const depositData = Buffer.alloc(16);
  discriminator.copy(depositData, 0);
  depositData.writeBigUInt64LE(100n, 8);
  const depositAccounts = [
    spec.vault, spec.vault, spec.reserve, spec.reserve, spec.reserve,
    spec.mint, spec.supply, spec.supply, spec.supply, spec.vault,
    klend, klend, klend, klend, klend, klend, klend,
  ];
  const reserveData = Buffer.alloc(8624);
  createHash("sha256").update("account:Reserve").digest().subarray(0, 8).copy(reserveData, 0);
  Buffer.from(new PublicKey(spec.mint).toBytes()).copy(reserveData, 128);
  Buffer.from(new PublicKey(spec.supply).toBytes()).copy(reserveData, 160);
  const invalidReserveData = Buffer.alloc(8624);
  Buffer.from(new PublicKey(spec.mint).toBytes()).copy(invalidReserveData, 128);
  Buffer.from(new PublicKey(spec.supply).toBytes()).copy(invalidReserveData, 160);
  const tokenBalance = (accountIndex: number, amount: string) => ({
    accountIndex,
    mint: spec.mint,
    owner: "fixture-owner",
    uiTokenAmount: {amount, decimals: 0, uiAmount: Number(amount), uiAmountString: amount},
  });
  const transaction = (entry: {kind: string; slot: number}) => {
    if (entry.kind === "pull") return {
      slot: entry.slot,
      meta: {
        err: null,
        preTokenBalances: [tokenBalance(0, "400"), tokenBalance(1, "0")],
        postTokenBalances: [tokenBalance(0, "300"), tokenBalance(1, "100")],
      },
      transaction: {message: {accountKeys: [{pubkey: spec.wallet}, {pubkey: spec.vault}]}}
    };
    if (entry.kind === "fleet" || entry.kind === "fleet_no_instruction") return {
      slot: entry.slot,
      meta: {
        err: null,
        preTokenBalances: [tokenBalance(0, "100"), tokenBalance(2, "0")],
        postTokenBalances: [tokenBalance(0, "0"), tokenBalance(2, "100")],
        innerInstructions: entry.kind === "fleet" ? [{index: 0, instructions: [{
          programId: klend,
          accounts: depositAccounts,
          data: bs58.encode(depositData),
        }]}] : [],
      },
      transaction: {message: {accountKeys: [
        {pubkey: spec.vault}, {pubkey: spec.reserve}, {pubkey: spec.supply}, {pubkey: klend}
      ], instructions: []}}
    };
    return {
      slot: entry.slot,
      meta: {err: null, preTokenBalances: [], postTokenBalances: []},
      transaction: {message: {accountKeys: [{pubkey: "11111111111111111111111111111111"}]}}
    };
  };
  Bun.serve({
    port: Number(Bun.env.REPAIR_RPC_PORT),
    async fetch(request) {
      try {
        const body = await request.json();
        if (body.method === "getHealth") return Response.json({jsonrpc: "2.0", id: body.id, result: "ok"});
        if (body.method === "getSignatureStatuses") return Response.json({
          jsonrpc: "2.0", id: body.id, result: {value: body.params[0].map((signature) =>
            spec.transactions[signature] ? {err: null, confirmationStatus: "confirmed"} : null
          )}
        });
        if (body.method === "getAccountInfo") return Response.json({
          jsonrpc: "2.0", id: body.id, result: {value:
            body.params[0] === spec.reserve ? {
              owner: klend, data: [reserveData.toString("base64"), "base64"]
            } : body.params[0] === spec.invalidReserve ? {
              owner: klend, data: [invalidReserveData.toString("base64"), "base64"]
            } : null}
        });
        const entry = spec.transactions[body.params?.[0]];
        return Response.json({jsonrpc: "2.0", id: body.id, result: entry ? transaction(entry) : null});
      } catch (error) {
        console.error(error);
        return Response.json({jsonrpc: "2.0", id: null, error: {code: -32000, message: String(error)}}, {status: 500});
      }
    },
  });
  await new Promise(() => {});
' >"$scratch_dir/repair-rpc.log" 2>&1 &
repair_rpc_pid=$!
repair_rpc_url="http://127.0.0.1:$repair_rpc_port"
for _ in $(seq 1 20); do
  if curl -sf -X POST -H 'content-type: application/json' \
    --data '{"jsonrpc":"2.0","id":1,"method":"getHealth"}' "$repair_rpc_url" \
    | jq -e '.result == "ok"' >/dev/null; then
    break
  fi
  sleep 0.1
done
kill -0 "$repair_rpc_pid" >/dev/null 2>&1 || fail "local repair RPC fixture exited"

missing_instruction_log="$scratch_dir/missing-kamino-instruction.log"
if SOLANA_RPC_URL="$repair_rpc_url" NODE_PATH="$node_modules_root" \
  bun "$routing_root/scripts/verify-autodeposit-fleet-handoff-chain.ts" \
  --pull-signature "$pull_signature" --pull-slot "$pull_slot" \
  --fleet-signature "$negative_fleet_signature" --fleet-slot "$negative_fleet_slot" \
  --mint "$mint" --amount-raw 100 \
  --wallet-token-account "$wallet_token_ata" --vault-token-account "$vault_token_ata" \
  --target-reserve "$fleet_reserve" >"$missing_instruction_log" 2>&1; then
  fail "token deltas without an exact Kamino deposit instruction were accepted"
fi
rg -q 'exact Kamino deposit instructions, expected one' "$missing_instruction_log" \
  || fail "missing Kamino instruction did not fail at the instruction proof"

invalid_reserve_log="$scratch_dir/invalid-reserve.log"
if SOLANA_RPC_URL="$repair_rpc_url" NODE_PATH="$node_modules_root" \
  bun "$routing_root/scripts/verify-autodeposit-fleet-handoff-chain.ts" \
  --pull-signature "$pull_signature" --pull-slot "$pull_slot" \
  --fleet-signature "$fleet_signature" --fleet-slot "$fleet_slot" \
  --mint "$mint" --amount-raw 100 \
  --wallet-token-account "$wallet_token_ata" --vault-token-account "$vault_token_ata" \
  --target-reserve "$invalid_fleet_reserve" >"$invalid_reserve_log" 2>&1; then
  fail "KLend-owned data without the Reserve discriminator was accepted"
fi
rg -q 'not a valid pinned KLend Reserve account' "$invalid_reserve_log" \
  || fail "invalid Reserve data did not fail at the layout proof"
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
       ARRAY['$mint'], ARRAY['market-e2e'],
       ARRAY['$mint'], $pull_slot, '$pull_signature');

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
       'vault-e2e', '$payer', '$wallet_token_ata', '$vault_token_ata',
       '$mint',
       '$wallet_token_ata', '$vault_token_ata', ARRAY['$payer'], 1, 1000000,
       TRUE, 'active', $fleet_slot, $fleet_slot, '$fleet_signature', 'mainnet-beta');

    INSERT INTO loyal_yield.balance_sweep_lot_claims
      (claim_token, target_id, amount_raw, status, autodeposit_deposit_plan)
    VALUES
      ('claim-e2e', 1, 100, 'selected', jsonb_build_object(
        'version', 1, 'amountRaw', '100', 'reserve', 'direct-plan-reserve',
        'market', 'direct-plan-market',
        'liquidityMint', '$mint',
        'target', jsonb_build_object(
          'id', '1', 'managedVaultId', '1', 'settings', 'settings-e2e',
          'vaultIndex', 1, 'wallet', '$payer', 'walletUsdcAta', '$wallet_token_ata',
          'walletTokenAta', '$wallet_token_ata', 'vaultPubkey', 'vault-e2e',
          'vaultUsdcAta', '$vault_token_ata', 'vaultTokenAta', '$vault_token_ata',
          'tokenMint', '$mint',
          'routePolicyAccount', 'route-policy-e2e', 'routePolicySeed', '7',
          'currentReserve', 'initial-reserve', 'currentMarket', 'initial-market',
          'currentLiquidityMint', '$mint'
        )
      ));

    INSERT INTO loyal_yield.balance_sweep_scheduled_slots
      (id, target_id, token_mint, eligible_after, status, claim_token)
    VALUES
      (1, 1, '$mint', now(), 'selected', 'claim-e2e');

    INSERT INTO loyal_yield.balance_sweep_executions
      (id, target_id, signature, slot, source_wallet_ata, destination_vault_ata,
       token_mint, source_token_ata, destination_token_ata, amount_raw,
       source_pre_balance_raw, source_post_balance_raw,
       destination_pre_balance_raw, destination_post_balance_raw,
       source_commitment, raw_evidence, decoded_evidence, received_at,
       decoded_at, dedupe_key)
    VALUES
      (1, 1, '$pull_signature', $pull_slot, '$wallet_token_ata', '$vault_token_ata',
       '$mint',
       '$wallet_token_ata', '$vault_token_ata', 100, 100, 0, 0, 100,
       'confirmed', '{\"source\":\"single-vault-autodeposit-executor\"}',
       jsonb_build_object(
         'sequence', 'subscription_pull_then_mandatory_kamino_deposit',
         'status', 'partial_executed_pull_idle_vault_deposited',
         'idleVaultDepositDecisionId', '1',
         'kaminoDepositSignature', '$fleet_signature',
         'kaminoDepositSlot', '$fleet_slot'
       ), now(), now(), '1:autodeposit-pull:$pull_signature');

    UPDATE loyal_yield.balance_sweep_executions
    SET kamino_deposit_signature = '$fleet_signature',
        completed_at = now()
    WHERE id = 1;

    INSERT INTO loyal_yield.vault_position_snapshots
      (id, vault_id, policy_id, observed_slot, observed_at, chain_slot, is_current)
    VALUES (1, 1, 1, $fleet_slot, now(), $fleet_slot, TRUE);

    INSERT INTO loyal_yield.vault_position_snapshot_positions
      (snapshot_id, reserve, market, liquidity_mint, amount_raw,
       supply_apy_bps, has_value)
    VALUES
      (1, '$fleet_reserve', 'fleet-actual-market',
       '$mint', 600, 425, TRUE);

    INSERT INTO loyal_yield.rebalance_decisions
      (id, vault_id, status, target_reserve, liquidity_mint,
       source_liquidity_mint, target_liquidity_mint, amount_raw,
       source_apy_bps, target_apy_bps, estimated_edge_bps,
       decision_reason, execution_plan, idempotency_key, signature,
       submitted_slot, confirmed_slot, post_snapshot_id)
    VALUES
      (1, 1, 'confirmed', '$fleet_reserve',
       '$mint',
       '$mint',
       '$mint', 100, 0, 425, 425,
       'idle_vault_liquidity_available',
       jsonb_build_object(
         'kind', 'idle_vault_deposit',
         'idle_token_account', '$vault_token_ata',
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
       '$mint', 300,
       '$mint', 600,
       'newer-reserve', 'newer-market',
       '$mint', 777,
       $fleet_slot + 1, now(), 'initial-deposit-e2e', 'newer-deposit-e2e',
       $fleet_slot + 1, 'active', now(), now());

    INSERT INTO loyal_yield.user_yield_position_deposits
      (id, deposit_signature, policy_signature, confirmed_slot, wallet_address,
       smart_account_address, settings, vault_index, vault_pubkey, policy_id,
       policy_account, policy_seed, target_reserve, market, liquidity_mint,
       target_supply_apy_bps, deposit_mint, principal_amount_raw,
       confirmed_at, created_at)
    VALUES
      (1, '$fleet_signature', '$fleet_signature', $fleet_slot, '$payer',
       'vault-e2e', 'settings-e2e', 1, 'vault-e2e', 7,
       'route-policy-e2e', 7, '$fleet_reserve', 'fleet-actual-market',
       '$mint', 425,
       '$mint', 100, now(), now());

    INSERT INTO loyal_yield.user_yield_position_holding_events
      (id, position_id, event_type, reserve, market, liquidity_mint, amount_raw,
       principal_delta_raw, holding_delta_raw, observed_slot, observed_at,
       source_signature, source_deposit_id, source_rebalance_decision_id,
       source_snapshot_id, created_at)
    VALUES
      (1, 1, 'deposit_top_up', '$fleet_reserve', 'fleet-actual-market',
       '$mint', 600,
       100, NULL, $fleet_slot, now(), '$fleet_signature', 1, 1, 1, now());
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

run_repair() {
  local output_file="$1"
  local claim_token="$2"
  local decision_id="$3"
  local execution_id="$4"
  local scheduled_slot_id="$5"
  NEON_DATABASE_URL="$database_url" \
  SOLANA_RPC_URL="$repair_rpc_url" \
  AUTODEPOSIT_LOCAL_POSTGRES_E2E=1 \
  bash "$routing_root/scripts/repair-autodeposit-fleet-handoff.sh" \
    --claim-token "$claim_token" \
    --decision-id "$decision_id" \
    --execution-id "$execution_id" \
    --scheduled-slot-id "$scheduled_slot_id" \
    --execute >"$output_file" 2>&1
}

stage_begin
run_repair "$first_log" claim-e2e 1 1 1
run_repair "$second_log" claim-e2e 1 1 1
run_trigger "$trigger_log"
stage_end execution_twice

if rg -n \
  'autodeposit transaction effect remains ambiguous after blockhash expiry|loyal\.operational_error|autodeposit_[a-z_]*ambiguous|"status"[[:space:]]*:[[:space:]]*"error"' \
  "$first_log" "$second_log" "$trigger_log"; then
  fail "Autodeposit emitted an operational or ambiguity error"
fi
rg -q '"status":"completed"' "$first_log" \
  || fail "first explicit repair did not complete"
rg -q '"status":"already_completed"' "$second_log" \
  || fail "second explicit repair did not prove idempotence"
rg -q 'targets_scanned=0' "$trigger_log" \
  || fail "trigger still found recovery work after explicit repair"

proof="$(psql_verify -A -t --command="
  SELECT jsonb_build_object(
    'claimStatus', (SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = 'claim-e2e'),
    'slotStatus', (SELECT status::text FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = 1),
    'executionCompleted', (SELECT completed_at IS NOT NULL FROM loyal_yield.balance_sweep_executions WHERE id = 1),
    'recoverySource', (SELECT decoded_evidence ->> 'recoverySource' FROM loyal_yield.balance_sweep_executions WHERE id = 1),
    'fleetDecisionId', (SELECT decoded_evidence ->> 'fleetDecisionId' FROM loyal_yield.balance_sweep_executions WHERE id = 1),
    'executionDepositId', (SELECT yield_deposit_id FROM loyal_yield.balance_sweep_executions WHERE id = 1),
    'executionPositionId', (SELECT yield_position_id FROM loyal_yield.balance_sweep_executions WHERE id = 1),
    'depositExecutionId', (SELECT balance_sweep_execution_id FROM loyal_yield.user_yield_position_deposits WHERE id = 1),
    'depositSlotId', (SELECT balance_sweep_scheduled_slot_id FROM loyal_yield.user_yield_position_deposits WHERE id = 1),
    'pullAttempts', (SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts WHERE claim_token = 'claim-e2e' AND operation_kind = 'pull'),
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
  .recoverySource == "explicit_fleet_decision" and
  .fleetDecisionId == "1" and
  .executionDepositId == 1 and
  .executionPositionId == 1 and
  .depositExecutionId == 1 and
  .depositSlotId == 1 and
  .pullAttempts == 0 and
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
      'liquidityMint', '$mint',
      'target', jsonb_build_object(
        'id', '1', 'managedVaultId', '1', 'settings', 'settings-e2e',
        'vaultIndex', 1, 'wallet', '$payer', 'walletUsdcAta', '$wallet_token_ata',
        'walletTokenAta', '$wallet_token_ata', 'vaultPubkey', 'vault-e2e',
        'vaultUsdcAta', '$vault_token_ata', 'vaultTokenAta', '$vault_token_ata',
        'tokenMint', '$mint',
        'routePolicyAccount', 'route-policy-e2e', 'routePolicySeed', '7',
        'currentReserve', 'newer-reserve', 'currentMarket', 'newer-market',
        'currentLiquidityMint', '$mint'
      )
    ));

  INSERT INTO loyal_yield.balance_sweep_scheduled_slots
    (id, target_id, token_mint, eligible_after, status, claim_token)
  VALUES
    (2, 1, '$mint', now(), 'selected',
     'claim-bad-provenance-e2e');

  INSERT INTO loyal_yield.balance_sweep_executions
    (id, target_id, signature, slot, source_wallet_ata, destination_vault_ata,
     token_mint, source_token_ata, destination_token_ata, amount_raw,
     source_pre_balance_raw, source_post_balance_raw,
     destination_pre_balance_raw, destination_post_balance_raw,
     source_commitment, raw_evidence, decoded_evidence, received_at,
     decoded_at, dedupe_key)
  VALUES
    (2, 1, '$negative_pull_signature', $negative_pull_slot,
     '$wallet_token_ata', '$vault_token_ata',
     '$mint',
     '$wallet_token_ata', '$vault_token_ata', 100, 100, 0, 0, 100,
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
    (2, '$fleet_reserve', 'fleet-actual-market',
     '$mint', 877, 425, TRUE);

  INSERT INTO loyal_yield.rebalance_decisions
    (id, vault_id, status, target_reserve, liquidity_mint,
     source_liquidity_mint, target_liquidity_mint, amount_raw,
     source_apy_bps, target_apy_bps, estimated_edge_bps,
     decision_reason, execution_plan, idempotency_key, signature,
     submitted_slot, confirmed_slot, post_snapshot_id)
  VALUES
    (2, 1, 'confirmed', '$fleet_reserve',
     '$mint',
     '$mint',
     '$mint', 100, 0, 425, 425,
     'idle_vault_liquidity_available',
     jsonb_build_object(
       'kind', 'idle_vault_deposit',
       'idle_token_account', '$vault_token_ata',
       'idle_observed_slot', $negative_pull_slot,
       'idle_vault_liquidity_amount_raw', 100
     ), 'bad-fleet-handoff-e2e', '$negative_fleet_signature',
     $negative_fleet_slot, $negative_fleet_slot, 2);

  INSERT INTO loyal_yield.user_yield_position_deposits
    (id, deposit_signature, policy_signature, confirmed_slot, wallet_address,
     smart_account_address, settings, vault_index, vault_pubkey, policy_id,
     policy_account, policy_seed, target_reserve, market, liquidity_mint,
     target_supply_apy_bps, deposit_mint, principal_amount_raw,
     confirmed_at, created_at)
  VALUES
    (2, '$negative_fleet_signature', '$negative_fleet_signature',
     $negative_fleet_slot, '$payer', 'vault-e2e', 'settings-e2e', 1,
     'vault-e2e', 7, 'route-policy-e2e', 7, '$fleet_reserve',
     'fleet-actual-market',
     '$mint', 425,
     '$mint', 100, now(), now());

  INSERT INTO loyal_yield.user_yield_position_holding_events
    (id, position_id, event_type, reserve, market, liquidity_mint, amount_raw,
     principal_delta_raw, holding_delta_raw, observed_slot, observed_at,
     source_signature, source_deposit_id, source_rebalance_decision_id,
     source_snapshot_id, created_at)
  VALUES
    (2, 1, 'deposit_top_up', '$fleet_reserve', 'fleet-actual-market',
     '$mint', 877,
     100, NULL, $negative_fleet_slot, now(), '$negative_fleet_signature',
     2, 2, 2, now());
" >/dev/null

if run_repair "$negative_chain_log" claim-bad-provenance-e2e 2 2 2; then
  fail "unrelated successful transactions were accepted as Autodeposit proof"
fi
if ! rg -q 'token balance for .* is not unique|pull source token delta' "$negative_chain_log"; then
  sed -n '1,20p' "$negative_chain_log" >&2
  fail "unrelated successful transaction did not fail the token-effect check"
fi

psql_verify --command="
  UPDATE loyal_yield.balance_sweep_executions
  SET signature = '$negative_valid_pull_signature',
      slot = $negative_valid_pull_slot,
      dedupe_key = '1:autodeposit-pull:$negative_valid_pull_signature',
      yield_deposit_id = 1
  WHERE id = 2;

  UPDATE loyal_yield.vault_position_snapshots
  SET observed_slot = $negative_valid_fleet_slot,
      chain_slot = $negative_valid_fleet_slot
  WHERE id = 2;

  UPDATE loyal_yield.rebalance_decisions
  SET signature = '$negative_valid_fleet_signature',
      submitted_slot = $negative_valid_fleet_slot,
      confirmed_slot = $negative_valid_fleet_slot,
      execution_plan = execution_plan || jsonb_build_object(
        'idle_observed_slot', $negative_valid_pull_slot
      )
  WHERE id = 2;

  UPDATE loyal_yield.user_yield_position_deposits
  SET deposit_signature = '$negative_valid_fleet_signature',
      policy_signature = '$negative_valid_fleet_signature',
      confirmed_slot = $negative_valid_fleet_slot
  WHERE id = 2;

  UPDATE loyal_yield.user_yield_position_holding_events
  SET source_signature = '$negative_valid_fleet_signature',
      observed_slot = $negative_valid_fleet_slot
  WHERE id = 2;
" >/dev/null

if run_repair "$negative_log" claim-bad-provenance-e2e 2 2 2; then
  fail "bad Fleet provenance was accepted"
fi
rg -q 'conflicting durable linkage would be overwritten' "$negative_log" \
  || fail "conflicting durable linkage did not fail closed"

psql_verify --command="
  UPDATE loyal_yield.balance_sweep_executions
  SET yield_deposit_id = NULL,
      decoded_evidence = decoded_evidence || jsonb_build_object(
        'idleVaultLastDepositDecisionId', '999',
        'idleVaultLastDepositSignature', '$negative_valid_fleet_signature',
        'idleVaultLastDepositSlot', '$negative_valid_fleet_slot',
        'idleVaultLastDepositAmountRaw', '100'
      )
  WHERE id = 2;
" >/dev/null
if run_repair "$negative_evidence_log" claim-bad-provenance-e2e 2 2 2; then
  fail "conflicting Fleet attribution was accepted"
fi
rg -q 'conflicting Fleet attribution would be overwritten' "$negative_evidence_log" \
  || fail "conflicting Fleet attribution did not fail closed"

negative_proof="$(psql_verify -A -t --command="
  SELECT jsonb_build_object(
    'claimStatus', (SELECT status::text FROM loyal_yield.balance_sweep_lot_claims WHERE claim_token = 'claim-bad-provenance-e2e'),
    'slotStatus', (SELECT status::text FROM loyal_yield.balance_sweep_scheduled_slots WHERE id = 2),
    'executionCompleted', (SELECT completed_at IS NOT NULL FROM loyal_yield.balance_sweep_executions WHERE id = 2),
    'executionDepositId', (SELECT yield_deposit_id FROM loyal_yield.balance_sweep_executions WHERE id = 2),
    'attributedDecisionId', (SELECT decoded_evidence ->> 'idleVaultLastDepositDecisionId' FROM loyal_yield.balance_sweep_executions WHERE id = 2),
    'fleetDepositExecutionId', (SELECT balance_sweep_execution_id FROM loyal_yield.user_yield_position_deposits WHERE id = 2),
    'fleetDepositSlotId', (SELECT balance_sweep_scheduled_slot_id FROM loyal_yield.user_yield_position_deposits WHERE id = 2),
    'topUpAttempts', (SELECT count(*) FROM loyal_yield.balance_sweep_transaction_attempts WHERE claim_token = 'claim-bad-provenance-e2e' AND operation_kind = 'top_up'),
    'deposits', (SELECT count(*) FROM loyal_yield.user_yield_position_deposits WHERE deposit_signature = '$negative_valid_fleet_signature')
  )
" | tr -d '\n')"
jq -e '
  .claimStatus == "selected" and
  .slotStatus == "selected" and
  .executionCompleted == false and
  .executionDepositId == null and
  .attributedDecisionId == "999" and
  .fleetDepositExecutionId == null and
  .fleetDepositSlotId == null and
  .topUpAttempts == 0 and
  .deposits == 1
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
