#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
sql_file="$script_dir/repair-autodeposit-fleet-handoff.sql"
claim_token=""
execution_id=""
scheduled_slot_id=""
decision_id=""
apply=false

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --claim-token)
      claim_token="${2:-}"
      shift 2
      ;;
    --execution-id)
      execution_id="${2:-}"
      shift 2
      ;;
    --scheduled-slot-id)
      scheduled_slot_id="${2:-}"
      shift 2
      ;;
    --decision-id)
      decision_id="${2:-}"
      shift 2
      ;;
    --execute)
      apply=true
      shift
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n "$claim_token" ]] || fail "--claim-token is required"
for id_name in execution_id scheduled_slot_id decision_id; do
  id_value="${!id_name}"
  [[ "$id_value" =~ ^[1-9][0-9]*$ ]] || fail "--${id_name//_/-} must be a positive integer"
done
[[ -n "${NEON_DATABASE_URL:-}" ]] || fail "NEON_DATABASE_URL is required"
[[ -n "${SOLANA_RPC_URL:-}" ]] || fail "SOLANA_RPC_URL is required"
for command_name in bun curl jq psql; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

connection_json="$(bun -e '
  const value = Bun.env.NEON_DATABASE_URL;
  if (!value) throw new Error("NEON_DATABASE_URL is required");
  const url = new URL(value);
  console.log(JSON.stringify({
    host: url.hostname,
    port: url.port || "5432",
    database: decodeURIComponent(url.pathname.slice(1)),
    user: decodeURIComponent(url.username),
    password: decodeURIComponent(url.password),
  }));
')"
export PGHOST="$(jq -er '.host' <<<"$connection_json")"
export PGPORT="$(jq -er '.port' <<<"$connection_json")"
export PGDATABASE="$(jq -er '.database' <<<"$connection_json")"
export PGUSER="$(jq -er '.user' <<<"$connection_json")"
export PGPASSWORD="$(jq -er '.password' <<<"$connection_json")"
export PGOPTIONS="${PGOPTIONS:-} -c lock_timeout=5s -c statement_timeout=30s"

run_sql() {
  local should_apply="$1"
  local expected_pull_signature="${2:-}"
  local expected_pull_slot="${3:-}"
  local expected_fleet_signature="${4:-}"
  local expected_fleet_slot="${5:-}"
  local expected_liquidity_mint="${6:-}"
  local expected_amount_raw="${7:-}"
  local expected_wallet_token_ata="${8:-}"
  local expected_vault_token_ata="${9:-}"
  local expected_fleet_target_reserve="${10:-}"
  psql -X -qAt --set=ON_ERROR_STOP=1 \
    --set=claim_token="$claim_token" \
    --set=execution_id="$execution_id" \
    --set=scheduled_slot_id="$scheduled_slot_id" \
    --set=decision_id="$decision_id" \
    --set=expected_pull_signature="$expected_pull_signature" \
    --set=expected_pull_slot="$expected_pull_slot" \
    --set=expected_fleet_signature="$expected_fleet_signature" \
    --set=expected_fleet_slot="$expected_fleet_slot" \
    --set=expected_liquidity_mint="$expected_liquidity_mint" \
    --set=expected_amount_raw="$expected_amount_raw" \
    --set=expected_wallet_token_ata="$expected_wallet_token_ata" \
    --set=expected_vault_token_ata="$expected_vault_token_ata" \
    --set=expected_fleet_target_reserve="$expected_fleet_target_reserve" \
    --set=apply="$should_apply" \
    --file="$sql_file"
}

preview="$(run_sql false)"
jq -e '.status == "ready" or .status == "already_completed"' <<<"$preview" >/dev/null \
  || fail "database preflight did not return a repairable state"

pull_signature="$(jq -er '.pullSignature' <<<"$preview")"
pull_slot="$(jq -er '.pullSlot' <<<"$preview")"
fleet_signature="$(jq -er '.fleetSignature' <<<"$preview")"
fleet_slot="$(jq -er '.fleetSlot' <<<"$preview")"
liquidity_mint="$(jq -er '.liquidityMint' <<<"$preview")"
amount_raw="$(jq -er '.amountRaw' <<<"$preview")"
wallet_token_ata="$(jq -er '.walletTokenAta' <<<"$preview")"
vault_token_ata="$(jq -er '.vaultTokenAta' <<<"$preview")"
fleet_target_reserve="$(jq -er '.fleetTargetReserve' <<<"$preview")"
[[ "$amount_raw" =~ ^[1-9][0-9]*$ ]] || fail "database preflight returned an invalid amount"

NODE_PATH="$script_dir/../node_modules" bun "$script_dir/verify-autodeposit-fleet-handoff-chain.ts" \
  --pull-signature "$pull_signature" \
  --pull-slot "$pull_slot" \
  --fleet-signature "$fleet_signature" \
  --fleet-slot "$fleet_slot" \
  --mint "$liquidity_mint" \
  --amount-raw "$amount_raw" \
  --wallet-token-account "$wallet_token_ata" \
  --vault-token-account "$vault_token_ata" \
  --target-reserve "$fleet_target_reserve" >/dev/null

result="$preview"
if [[ "$apply" == true ]]; then
  result="$(run_sql true \
    "$pull_signature" "$pull_slot" "$fleet_signature" "$fleet_slot" \
    "$liquidity_mint" "$amount_raw" "$wallet_token_ata" "$vault_token_ata" \
    "$fleet_target_reserve")"
fi

jq -cn \
  --arg mode "$([[ "$apply" == true ]] && echo execute || echo dry_run)" \
  --arg commitment confirmed \
  --argjson result "$result" \
  '{mode: $mode, commitment: $commitment, result: $result}'
