#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

show_help() {
  cat <<'EOF'
Smoke-test a deployed Loyal Hub swap program on devnet.

Defaults match the current devnet deployment:
  PROGRAM_ID=LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH
  FAUCET_ADDRESS=GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N
  FAUCET_KEYPAIR=$HOME/.config/solana/id.json
  USDC_MINT=4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU
  PYUSD_MINT=CXk2AMBfi3TwaEL2468s6zP8xq9NxTXjp9gjMgzeUynM

The script creates or reuses ./ADDR_1.json through ./ADDR_5.json, funds
those users, funds each configured lane, simulates and executes swaps and
rebalances, then withdraws remaining lane inventory back to the faucet ATA.

Run:
  bun run hub:devnet-smoke
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  show_help
  exit 0
fi

CLUSTER="${CLUSTER:-d}"
PROGRAM_ID="${PROGRAM_ID:-LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH}"
FAUCET_ADDRESS="${FAUCET_ADDRESS:-GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N}"
FAUCET_KEYPAIR="${FAUCET_KEYPAIR:-$HOME/.config/solana/id.json}"
USDC_MINT="${USDC_MINT:-4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU}"
PYUSD_MINT="${PYUSD_MINT:-CXk2AMBfi3TwaEL2468s6zP8xq9NxTXjp9gjMgzeUynM}"
USER_COUNT="${USER_COUNT:-5}"
ADDR_PREFIX="${ADDR_PREFIX:-ADDR_}"
USER_FUND_UI="${USER_FUND_UI:-0.1}"
LANE_FUND_UI="${LANE_FUND_UI:-1}"
SWAP_MAX_FEE_BPS="${SWAP_MAX_FEE_BPS:-}"

HUB_CLI=(cargo run -q -p loyal-hub-cli -- -u "$CLUSTER" -k "$FAUCET_KEYPAIR" --program-id "$PROGRAM_ID")

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

log() {
  printf '\n==> %s\n' "$*"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

run_hub_step() {
  local label="$1"
  shift

  log "Simulating $label"
  "${HUB_CLI[@]}" --simulate "$@"

  log "Executing $label"
  "${HUB_CLI[@]}" "$@"
}

refresh_state() {
  STATE_JSON="$("${HUB_CLI[@]}" --json state)"
}

jq_state() {
  jq -r "$@" <<<"$STATE_JSON"
}

fund_recipient() {
  local mint="$1"
  local amount="$2"
  local recipient="$3"
  shift 3

  if token_balance_for_owner_at_least "$recipient" "$mint" "$amount"; then
    printf 'Skipping funding %s for %s; existing balance is at least %s\n' "$mint" "$recipient" "$amount"
    return
  fi

  spl-token transfer \
    -u "$CLUSTER" \
    --fee-payer "$FAUCET_KEYPAIR" \
    --owner "$FAUCET_KEYPAIR" \
    "$mint" \
    "$amount" \
    "$recipient" \
    --allow-unfunded-recipient \
    --fund-recipient \
    "$@"
}

token_account_for_owner_optional() {
  local owner="$1"
  local mint="$2"
  local account

  account="$(spl-token accounts -u "$CLUSTER" --owner "$owner" --addresses-only "$mint" 2>/dev/null | awk 'NF {print; exit}' || true)"
  printf '%s\n' "$account"
}

token_account_for_owner() {
  local owner="$1"
  local mint="$2"
  local account

  account="$(token_account_for_owner_optional "$owner" "$mint")"
  [[ -n "$account" ]] || die "missing token account for owner $owner mint $mint"
  printf '%s\n' "$account"
}

token_balance_for_owner() {
  local owner="$1"
  local mint="$2"
  local account
  local balance

  account="$(token_account_for_owner_optional "$owner" "$mint")"
  if [[ -z "$account" ]]; then
    printf '0\n'
    return
  fi

  balance="$(spl-token balance -u "$CLUSTER" --address "$account" 2>/dev/null | awk 'NF {print; exit}' || true)"
  if [[ -z "$balance" ]]; then
    printf '0\n'
  else
    printf '%s\n' "$balance"
  fi
}

token_balance_for_owner_at_least() {
  local owner="$1"
  local mint="$2"
  local amount="$3"
  local balance

  balance="$(token_balance_for_owner "$owner" "$mint")"
  awk -v balance="$balance" -v amount="$amount" 'BEGIN { exit !((balance + 0) >= (amount + 0)) }'
}

raw_out_for_fee() {
  local amount_in="$1"
  echo $((amount_in * (10000 - SWAP_MAX_FEE_BPS) / 10000))
}

swap_exact_in() {
  local label="$1"
  local user_index="$2"
  local lane_id="$3"
  local input_mint="$4"
  local output_mint="$5"
  local user_input="$6"
  local user_output="$7"
  local amount_in="$8"
  local amount_out

  amount_out="$(raw_out_for_fee "$amount_in")"

  run_hub_step "$label" \
    --signer "${USER_KEYS[$user_index]}" \
    swap-exact-in \
    --user-vault "${USER_PUBKEYS[$user_index]}" \
    --user-input-token-account "$user_input" \
    --user-output-token-account "$user_output" \
    --input-mint "$input_mint" \
    --output-mint "$output_mint" \
    --hub-authorizer "$FAUCET_ADDRESS" \
    --amount-in "$amount_in" \
    --amount-out "$amount_out" \
    --min-out "$amount_out" \
    --max-fee-bps "$SWAP_MAX_FEE_BPS" \
    --lane-id "$lane_id"
}

rebalance_inventory() {
  local label="$1"
  shift

  run_hub_step "$label" rebalance-inventory "$@"
}

withdraw_inventory() {
  local lane_id="$1"
  local mint="$2"
  local amount="$3"
  local destination="$4"

  run_hub_step "withdraw lane $lane_id mint $mint amount $amount" \
    withdraw-inventory \
    --destination-token-account "$destination" \
    --mint "$mint" \
    --amount "$amount" \
    --lane-id "$lane_id"
}

require_command cargo
require_command jq
require_command solana-keygen
require_command spl-token
require_command awk

[[ "$USER_COUNT" -ge 5 ]] || die "USER_COUNT must be at least 5 for the five-swap smoke flow"
[[ -f "$FAUCET_KEYPAIR" ]] || die "faucet keypair not found: $FAUCET_KEYPAIR"
ACTUAL_FAUCET_ADDRESS="$(solana-keygen pubkey "$FAUCET_KEYPAIR")"
[[ "$ACTUAL_FAUCET_ADDRESS" == "$FAUCET_ADDRESS" ]] || die "faucet keypair pubkey $ACTUAL_FAUCET_ADDRESS does not match FAUCET_ADDRESS $FAUCET_ADDRESS"

log "Reading deployed hub state"
refresh_state
[[ "$(jq_state '.initialized')" == "true" ]] || die "hub config is not initialized for $PROGRAM_ID on cluster $CLUSTER"

LANE_COUNT="$(jq_state '.lane_count // 0')"
[[ "$LANE_COUNT" -ge 2 ]] || die "expected at least 2 configured lanes, got $LANE_COUNT"

CONFIG_MAX_FEE_BPS="$(jq_state '.max_fee_bps // empty')"
[[ -n "$CONFIG_MAX_FEE_BPS" ]] || die "hub state did not include max_fee_bps"
if [[ -z "$SWAP_MAX_FEE_BPS" ]]; then
  SWAP_MAX_FEE_BPS="$CONFIG_MAX_FEE_BPS"
fi
[[ "$SWAP_MAX_FEE_BPS" -le "$CONFIG_MAX_FEE_BPS" ]] || die "SWAP_MAX_FEE_BPS=$SWAP_MAX_FEE_BPS exceeds config max_fee_bps=$CONFIG_MAX_FEE_BPS"

for mint in "$USDC_MINT" "$PYUSD_MINT"; do
  jq_state --arg mint "$mint" '.allowed_mints[] | select(. == $mint)' | grep -q . || die "mint $mint is not allowed by hub config"
done

declare -a USER_KEYS=()
declare -a USER_PUBKEYS=()
declare -a USER_USDC_ATAS=()
declare -a USER_PYUSD_ATAS=()

log "Creating or reusing $USER_COUNT cached user addresses"
for ((i = 1; i <= USER_COUNT; i += 1)); do
  keypair="$ROOT_DIR/${ADDR_PREFIX}${i}.json"
  if [[ ! -f "$keypair" ]]; then
    solana-keygen new --silent --no-bip39-passphrase -o "$keypair"
  fi

  pubkey="$(solana-keygen pubkey "$keypair")"
  USER_KEYS+=("$keypair")
  USER_PUBKEYS+=("$pubkey")
  printf 'ADDR_%s %s\n' "$i" "$pubkey"
done

log "Funding users with $USER_FUND_UI USDC and $USER_FUND_UI pyUSD each"
for pubkey in "${USER_PUBKEYS[@]}"; do
  fund_recipient "$USDC_MINT" "$USER_FUND_UI" "$pubkey"
  fund_recipient "$PYUSD_MINT" "$USER_FUND_UI" "$pubkey"
done

log "Resolving user token accounts"
for pubkey in "${USER_PUBKEYS[@]}"; do
  USER_USDC_ATAS+=("$(token_account_for_owner "$pubkey" "$USDC_MINT")")
  USER_PYUSD_ATAS+=("$(token_account_for_owner "$pubkey" "$PYUSD_MINT")")
done

log "Funding each configured lane with $LANE_FUND_UI USDC and $LANE_FUND_UI pyUSD"
for ((lane_id = 0; lane_id < LANE_COUNT; lane_id += 1)); do
  lane_authority="$(jq_state --argjson lane "$lane_id" '.lanes[] | select(.lane_id == $lane) | .authority')"
  [[ -n "$lane_authority" && "$lane_authority" != "null" ]] || die "missing lane authority for lane $lane_id"

  fund_recipient "$USDC_MINT" "$LANE_FUND_UI" "$lane_authority" --allow-non-system-account-recipient
  fund_recipient "$PYUSD_MINT" "$LANE_FUND_UI" "$lane_authority" --allow-non-system-account-recipient
done

log "Hub state after funding"
"${HUB_CLI[@]}" state

swap_exact_in "swap user 1 lane 0 USDC to pyUSD" 0 0 "$USDC_MINT" "$PYUSD_MINT" "${USER_USDC_ATAS[0]}" "${USER_PYUSD_ATAS[0]}" 20000
swap_exact_in "swap user 2 lane 1 pyUSD to USDC" 1 1 "$PYUSD_MINT" "$USDC_MINT" "${USER_PYUSD_ATAS[1]}" "${USER_USDC_ATAS[1]}" 20000
swap_exact_in "swap user 3 lane 0 USDC to pyUSD" 2 0 "$USDC_MINT" "$PYUSD_MINT" "${USER_USDC_ATAS[2]}" "${USER_PYUSD_ATAS[2]}" 30000

rebalance_inventory "rebalance after first swaps" \
  --transfer mint:"$USDC_MINT" from_lane_id:0 to_lane_id:1 raw_token_amount:20000 \
  --transfer mint:"$PYUSD_MINT" from_lane_id:1 to_lane_id:0 raw_token_amount:20000

swap_exact_in "swap user 4 lane 1 USDC to pyUSD" 3 1 "$USDC_MINT" "$PYUSD_MINT" "${USER_USDC_ATAS[3]}" "${USER_PYUSD_ATAS[3]}" 10000
swap_exact_in "swap user 5 lane 0 pyUSD to USDC" 4 0 "$PYUSD_MINT" "$USDC_MINT" "${USER_PYUSD_ATAS[4]}" "${USER_USDC_ATAS[4]}" 15000

rebalance_inventory "rebalance after final swaps" \
  --transfer mint:"$USDC_MINT" from_lane_id:0 to_lane_id:1 raw_token_amount:5000 \
  --transfer mint:"$PYUSD_MINT" from_lane_id:1 to_lane_id:0 raw_token_amount:5000

log "Withdrawing remaining lane inventory back to the faucet"
refresh_state
for ((lane_id = 0; lane_id < LANE_COUNT; lane_id += 1)); do
  for mint in "$USDC_MINT" "$PYUSD_MINT"; do
    amount="$(jq_state --argjson lane "$lane_id" --arg mint "$mint" '.lanes[] | select(.lane_id == $lane) | .inventory[] | select(.mint == $mint) | (.amount // 0)')"
    if [[ "$amount" -gt 0 ]]; then
      destination="$(token_account_for_owner "$FAUCET_ADDRESS" "$mint")"
      withdraw_inventory "$lane_id" "$mint" "$amount" "$destination"
    else
      printf 'lane %s mint %s has no inventory to withdraw\n' "$lane_id" "$mint"
    fi
  done
done

log "Final hub state"
"${HUB_CLI[@]}" state
