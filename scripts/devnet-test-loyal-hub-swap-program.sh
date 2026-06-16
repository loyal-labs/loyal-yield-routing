#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

show_help() {
  cat <<'EOF'
Smoke-test a deployed Loyal Hub swap program on devnet or mainnet.

Defaults match the current devnet deployment:
  PROGRAM_ID=LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH
  FAUCET_ADDRESS=GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N
  FAUCET_KEYPAIR=$HOME/.config/solana/id.json
  USDC_MINT=4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU
  PYUSD_MINT=CXk2AMBfi3TwaEL2468s6zP8xq9NxTXjp9gjMgzeUynM

The script creates or reuses ./ADDR_1.json through ./ADDR_5.json, funds
those users, funds each configured lane, simulates and executes swaps and
rebalances, then withdraws smoke-test inventory back to the faucet ATA.
Set SMOKE_LANE_COUNT=2 to fund and clean up only lanes 0 and 1 when the
deployed config has more lanes than the smoke should exercise.

Set CLUSTER=m and MAINNET_PROGRAM_ID or PROGRAM_ID to run against mainnet.
Mainnet runs require CONFIRM_MAINNET=1, default to mainnet USDC/PYUSD mints,
and use the Jupiter rebalance-through-Hub shape because Jupiter is mainnet-only.
Set JUPITER_API_KEY in the environment if your Jupiter API plan requires it.
Set SMOKE_RESUME_AFTER_SWAP4=1 to resume a partial smoke run after the first
three swaps, first Jupiter rebalance batch, and fourth swap have already landed.
For custom mainnet RPC URLs that are not auto-detected, set PROGRAM_ID,
mainnet mints, JUPITER_REBALANCE_MODE=jupiter, ALLOW_JUPITER_CUSTOM_RPC=1,
and CONFIRM_MAINNET=1.

Run:
  bun run hub:devnet-smoke
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  show_help
  exit 0
fi

is_mainnet_cluster() {
  case "$CLUSTER" in
    m | mainnet | mainnet-beta | *mainnet*)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

CLUSTER="${CLUSTER:-d}"
DEVNET_PROGRAM_ID="${DEVNET_PROGRAM_ID:-LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH}"
DEVNET_FAUCET_ADDRESS="${DEVNET_FAUCET_ADDRESS:-GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N}"
DEVNET_USDC_MINT="${DEVNET_USDC_MINT:-4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU}"
DEVNET_PYUSD_MINT="${DEVNET_PYUSD_MINT:-CXk2AMBfi3TwaEL2468s6zP8xq9NxTXjp9gjMgzeUynM}"
MAINNET_USDC_MINT="${MAINNET_USDC_MINT:-EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v}"
MAINNET_PYUSD_MINT="${MAINNET_PYUSD_MINT:-2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo}"
MAINNET_PROGRAM_ID="${MAINNET_PROGRAM_ID:-}"

if is_mainnet_cluster; then
  DEFAULT_PROGRAM_ID="$MAINNET_PROGRAM_ID"
  DEFAULT_FAUCET_ADDRESS=""
  DEFAULT_USDC_MINT="$MAINNET_USDC_MINT"
  DEFAULT_PYUSD_MINT="$MAINNET_PYUSD_MINT"
  DEFAULT_REBALANCE_MODE="jupiter"
  DEFAULT_CLEANUP_MODE="excess"
else
  DEFAULT_PROGRAM_ID="$DEVNET_PROGRAM_ID"
  DEFAULT_FAUCET_ADDRESS="$DEVNET_FAUCET_ADDRESS"
  DEFAULT_USDC_MINT="$DEVNET_USDC_MINT"
  DEFAULT_PYUSD_MINT="$DEVNET_PYUSD_MINT"
  DEFAULT_REBALANCE_MODE="native"
  DEFAULT_CLEANUP_MODE="all"
fi

PROGRAM_ID="${PROGRAM_ID:-$DEFAULT_PROGRAM_ID}"
FAUCET_ADDRESS="${FAUCET_ADDRESS:-$DEFAULT_FAUCET_ADDRESS}"
FAUCET_KEYPAIR="${FAUCET_KEYPAIR:-$HOME/.config/solana/id.json}"
USDC_MINT="${USDC_MINT:-$DEFAULT_USDC_MINT}"
PYUSD_MINT="${PYUSD_MINT:-$DEFAULT_PYUSD_MINT}"
USER_COUNT="${USER_COUNT:-5}"
ADDR_PREFIX="${ADDR_PREFIX:-ADDR_}"
USER_FUND_UI="${USER_FUND_UI:-0.1}"
LANE_FUND_UI="${LANE_FUND_UI:-1}"
SMOKE_LANE_COUNT="${SMOKE_LANE_COUNT:-}"
SMOKE_RESUME_AFTER_FIRST_SWAPS="${SMOKE_RESUME_AFTER_FIRST_SWAPS:-0}"
SMOKE_RESUME_AFTER_FIRST_REBALANCE="${SMOKE_RESUME_AFTER_FIRST_REBALANCE:-0}"
SMOKE_RESUME_AFTER_SWAP4="${SMOKE_RESUME_AFTER_SWAP4:-0}"
SWAP_MAX_FEE_BPS="${SWAP_MAX_FEE_BPS:-}"
JUPITER_REBALANCE_MODE="${JUPITER_REBALANCE_MODE:-auto}"
if [[ "$JUPITER_REBALANCE_MODE" == "auto" ]]; then
  ACTIVE_REBALANCE_MODE="$DEFAULT_REBALANCE_MODE"
else
  ACTIVE_REBALANCE_MODE="$JUPITER_REBALANCE_MODE"
fi
CLEANUP_MODE="${CLEANUP_MODE:-$DEFAULT_CLEANUP_MODE}"
JUPITER_SLIPPAGE_BPS="${JUPITER_SLIPPAGE_BPS:-50}"
JUPITER_QUOTE_API="${JUPITER_QUOTE_API:-https://api.jup.ag/swap/v1/quote}"
JUPITER_SWAP_INSTRUCTIONS_API="${JUPITER_SWAP_INSTRUCTIONS_API:-https://api.jup.ag/swap/v1/swap-instructions}"
JUPITER_ALLOW_TREASURY_OUTPUT_BUFFER="${JUPITER_ALLOW_TREASURY_OUTPUT_BUFFER:-0}"

if [[ "$SMOKE_RESUME_AFTER_SWAP4" == "1" ]]; then
  SMOKE_RESUME_AFTER_FIRST_SWAPS=1
fi

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

jq_initial_state() {
  jq -r "$@" <<<"$INITIAL_STATE_JSON"
}

fund_recipient() {
  local mint="$1"
  local amount="$2"
  local recipient="$3"
  local balance
  local transfer_amount
  shift 3

  balance="$(token_balance_for_owner "$recipient" "$mint")"
  transfer_amount="$(awk -v balance="$balance" -v amount="$amount" 'BEGIN { deficit = amount - balance; if (deficit < 0) deficit = 0; printf "%.6f", deficit }')"
  if awk -v transfer_amount="$transfer_amount" 'BEGIN { exit !((transfer_amount + 0) <= 0) }'; then
    printf 'Skipping funding %s for %s; existing balance is at least %s\n' "$mint" "$recipient" "$amount"
    return
  fi

  spl-token transfer \
    -u "$CLUSTER" \
    --fee-payer "$FAUCET_KEYPAIR" \
    --owner "$FAUCET_KEYPAIR" \
    "$mint" \
    "$transfer_amount" \
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
  echo $(((amount_in * (10000 - SWAP_MAX_FEE_BPS) + 9999) / 10000))
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

jupiter_rebalance_inventory() {
  local label="$1"
  local lane_id="$2"
  local input_mint="$3"
  local output_mint="$4"
  local input_amount="$5"
  local output_top_up_amount="$6"
  local common_args=(
    --cluster "$CLUSTER"
    --keypair "$FAUCET_KEYPAIR"
    --program-id "$PROGRAM_ID"
    --lane-id "$lane_id"
    --input-mint "$input_mint"
    --output-mint "$output_mint"
    --hub-input-amount "$input_amount"
    --hub-output-top-up-amount "$output_top_up_amount"
    --slippage-bps "$JUPITER_SLIPPAGE_BPS"
    --quote-api "$JUPITER_QUOTE_API"
    --swap-instructions-api "$JUPITER_SWAP_INSTRUCTIONS_API"
  )
  if [[ "$JUPITER_ALLOW_TREASURY_OUTPUT_BUFFER" == "1" ]]; then
    common_args+=(--allow-treasury-output-buffer)
  fi

  log "Simulating $label through Jupiter"
  bun scripts/jupiter-hub-rebalance.mjs "${common_args[@]}" --simulate-only

  log "Executing $label through Jupiter"
  bun scripts/jupiter-hub-rebalance.mjs "${common_args[@]}"
}

rebalance_after_first_swaps() {
  if [[ "$SMOKE_RESUME_AFTER_SWAP4" == "1" ]]; then
    log "Skipping first rebalance batch for resume after swap 4"
    return
  fi

  case "$ACTIVE_REBALANCE_MODE" in
    native)
      rebalance_inventory "rebalance after first swaps" \
        --transfer mint:"$USDC_MINT" from_lane_id:0 to_lane_id:1 raw_token_amount:20000 \
        --transfer mint:"$PYUSD_MINT" from_lane_id:1 to_lane_id:0 raw_token_amount:20000
      ;;
    jupiter)
      if [[ "$SMOKE_RESUME_AFTER_FIRST_REBALANCE" == "1" ]]; then
        log "Skipping first Jupiter rebalance for resume"
      else
        jupiter_rebalance_inventory "rebalance lane 0 USDC to pyUSD after user 1" \
          0 "$USDC_MINT" "$PYUSD_MINT" 20000 "$(raw_out_for_fee 20000)"
      fi
      jupiter_rebalance_inventory "rebalance lane 1 pyUSD to USDC after user 2" \
        1 "$PYUSD_MINT" "$USDC_MINT" 20000 "$(raw_out_for_fee 20000)"
      jupiter_rebalance_inventory "rebalance lane 0 USDC to pyUSD after user 3" \
        0 "$USDC_MINT" "$PYUSD_MINT" 30000 "$(raw_out_for_fee 30000)"
      ;;
    skip)
      log "Skipping first rebalance batch"
      ;;
  esac
}

rebalance_after_final_swaps() {
  case "$ACTIVE_REBALANCE_MODE" in
    native)
      rebalance_inventory "rebalance after final swaps" \
        --transfer mint:"$USDC_MINT" from_lane_id:0 to_lane_id:1 raw_token_amount:5000 \
        --transfer mint:"$PYUSD_MINT" from_lane_id:1 to_lane_id:0 raw_token_amount:5000
      ;;
    jupiter)
      jupiter_rebalance_inventory "rebalance lane 1 USDC to pyUSD after user 4" \
        1 "$USDC_MINT" "$PYUSD_MINT" 10000 "$(raw_out_for_fee 10000)"
      jupiter_rebalance_inventory "rebalance lane 0 pyUSD to USDC after user 5" \
        0 "$PYUSD_MINT" "$USDC_MINT" 15000 "$(raw_out_for_fee 15000)"
      ;;
    skip)
      log "Skipping final rebalance batch"
      ;;
  esac
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

cleanup_amount_for() {
  local lane_id="$1"
  local mint="$2"
  local current_amount="$3"
  local initial_amount

  case "$CLEANUP_MODE" in
    all)
      printf '%s\n' "$current_amount"
      ;;
    excess)
      initial_amount="$(jq_initial_state --argjson lane "$lane_id" --arg mint "$mint" '[.lanes[]? | select(.lane_id == $lane) | .inventory[]? | select(.mint == $mint) | (.amount // 0)] | first // 0')"
      if [[ "$current_amount" -gt "$initial_amount" ]]; then
        echo $((current_amount - initial_amount))
      else
        printf '0\n'
      fi
      ;;
    none)
      printf '0\n'
      ;;
    *)
      die "unsupported CLEANUP_MODE=$CLEANUP_MODE; expected all, excess, or none"
      ;;
  esac
}

require_command cargo
require_command jq
require_command solana-keygen
require_command spl-token
require_command awk
if [[ "$ACTIVE_REBALANCE_MODE" == "jupiter" ]]; then
  require_command bun
fi

case "$ACTIVE_REBALANCE_MODE" in
  native | jupiter | skip) ;;
  *) die "unsupported JUPITER_REBALANCE_MODE=$JUPITER_REBALANCE_MODE; expected auto, native, jupiter, or skip" ;;
esac

case "$CLEANUP_MODE" in
  all | excess | none) ;;
  *) die "unsupported CLEANUP_MODE=$CLEANUP_MODE; expected all, excess, or none" ;;
esac

if [[ "$ACTIVE_REBALANCE_MODE" == "jupiter" ]] && ! is_mainnet_cluster && [[ "${ALLOW_JUPITER_CUSTOM_RPC:-}" != "1" ]]; then
  die "Jupiter rebalance is mainnet-only; use CLUSTER=m/mainnet-beta, set ALLOW_JUPITER_CUSTOM_RPC=1 for a custom mainnet RPC URL, or choose JUPITER_REBALANCE_MODE=native/skip"
fi

[[ "$USER_COUNT" -ge 5 ]] || die "USER_COUNT must be at least 5 for the five-swap smoke flow"
[[ -n "$PROGRAM_ID" ]] || die "PROGRAM_ID or MAINNET_PROGRAM_ID is required for CLUSTER=$CLUSTER"
[[ -f "$FAUCET_KEYPAIR" ]] || die "faucet keypair not found: $FAUCET_KEYPAIR"
ACTUAL_FAUCET_ADDRESS="$(solana-keygen pubkey "$FAUCET_KEYPAIR")"
if [[ -z "$FAUCET_ADDRESS" ]]; then
  FAUCET_ADDRESS="$ACTUAL_FAUCET_ADDRESS"
fi
[[ "$ACTUAL_FAUCET_ADDRESS" == "$FAUCET_ADDRESS" ]] || die "faucet keypair pubkey $ACTUAL_FAUCET_ADDRESS does not match FAUCET_ADDRESS $FAUCET_ADDRESS"
if { is_mainnet_cluster || [[ "$ACTIVE_REBALANCE_MODE" == "jupiter" ]]; } && [[ "${CONFIRM_MAINNET:-}" != "1" ]]; then
  die "mainnet smoke tests move real funds; set CONFIRM_MAINNET=1 to continue"
fi

log "Reading deployed hub state"
refresh_state
INITIAL_STATE_JSON="$STATE_JSON"
[[ "$(jq_state '.initialized')" == "true" ]] || die "hub config is not initialized for $PROGRAM_ID on cluster $CLUSTER"

LANE_COUNT="$(jq_state '.lane_count // 0')"
[[ "$LANE_COUNT" -ge 2 ]] || die "expected at least 2 configured lanes, got $LANE_COUNT"
if [[ -z "$SMOKE_LANE_COUNT" ]]; then
  SMOKE_LANE_COUNT="$LANE_COUNT"
fi
[[ "$SMOKE_LANE_COUNT" =~ ^[0-9]+$ ]] || die "SMOKE_LANE_COUNT must be an integer"
[[ "$SMOKE_LANE_COUNT" -ge 2 ]] || die "SMOKE_LANE_COUNT must be at least 2"
[[ "$SMOKE_LANE_COUNT" -le "$LANE_COUNT" ]] || die "SMOKE_LANE_COUNT=$SMOKE_LANE_COUNT exceeds configured lane_count=$LANE_COUNT"

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

if [[ "$SMOKE_RESUME_AFTER_FIRST_SWAPS" == "1" ]]; then
  log "Skipping user funding for resume after first swaps"
else
  log "Funding users with $USER_FUND_UI USDC and $USER_FUND_UI pyUSD each"
  for pubkey in "${USER_PUBKEYS[@]}"; do
    fund_recipient "$USDC_MINT" "$USER_FUND_UI" "$pubkey"
    fund_recipient "$PYUSD_MINT" "$USER_FUND_UI" "$pubkey"
  done
fi

log "Resolving user token accounts"
for pubkey in "${USER_PUBKEYS[@]}"; do
  USER_USDC_ATAS+=("$(token_account_for_owner "$pubkey" "$USDC_MINT")")
  USER_PYUSD_ATAS+=("$(token_account_for_owner "$pubkey" "$PYUSD_MINT")")
done

if [[ "$SMOKE_RESUME_AFTER_FIRST_SWAPS" == "1" ]]; then
  log "Skipping lane funding for resume after first swaps"
else
  log "Funding first $SMOKE_LANE_COUNT lanes with $LANE_FUND_UI USDC and $LANE_FUND_UI pyUSD"
  for ((lane_id = 0; lane_id < SMOKE_LANE_COUNT; lane_id += 1)); do
    lane_authority="$(jq_state --argjson lane "$lane_id" '.lanes[] | select(.lane_id == $lane) | .authority')"
    [[ -n "$lane_authority" && "$lane_authority" != "null" ]] || die "missing lane authority for lane $lane_id"

    fund_recipient "$USDC_MINT" "$LANE_FUND_UI" "$lane_authority" --allow-non-system-account-recipient
    fund_recipient "$PYUSD_MINT" "$LANE_FUND_UI" "$lane_authority" --allow-non-system-account-recipient
  done
fi

log "Hub state after funding"
"${HUB_CLI[@]}" state

if [[ "$SMOKE_RESUME_AFTER_FIRST_SWAPS" == "1" ]]; then
  log "Resuming after first three swaps"
else
  swap_exact_in "swap user 1 lane 0 USDC to pyUSD" 0 0 "$USDC_MINT" "$PYUSD_MINT" "${USER_USDC_ATAS[0]}" "${USER_PYUSD_ATAS[0]}" 20000
  swap_exact_in "swap user 2 lane 1 pyUSD to USDC" 1 1 "$PYUSD_MINT" "$USDC_MINT" "${USER_PYUSD_ATAS[1]}" "${USER_USDC_ATAS[1]}" 20000
  swap_exact_in "swap user 3 lane 0 USDC to pyUSD" 2 0 "$USDC_MINT" "$PYUSD_MINT" "${USER_USDC_ATAS[2]}" "${USER_PYUSD_ATAS[2]}" 30000
fi

rebalance_after_first_swaps

if [[ "$SMOKE_RESUME_AFTER_SWAP4" == "1" ]]; then
  log "Skipping swap user 4 for resume after swap 4"
else
  swap_exact_in "swap user 4 lane 1 USDC to pyUSD" 3 1 "$USDC_MINT" "$PYUSD_MINT" "${USER_USDC_ATAS[3]}" "${USER_PYUSD_ATAS[3]}" 10000
fi
swap_exact_in "swap user 5 lane 0 pyUSD to USDC" 4 0 "$PYUSD_MINT" "$USDC_MINT" "${USER_PYUSD_ATAS[4]}" "${USER_USDC_ATAS[4]}" 15000

rebalance_after_final_swaps

log "Withdrawing remaining lane inventory back to the faucet"
refresh_state
for ((lane_id = 0; lane_id < LANE_COUNT; lane_id += 1)); do
  if [[ "$lane_id" -ge "$SMOKE_LANE_COUNT" ]]; then
    continue
  fi

  for mint in "$USDC_MINT" "$PYUSD_MINT"; do
    current_amount="$(jq_state --argjson lane "$lane_id" --arg mint "$mint" '[.lanes[]? | select(.lane_id == $lane) | .inventory[]? | select(.mint == $mint) | (.amount // 0)] | first // 0')"
    amount="$(cleanup_amount_for "$lane_id" "$mint" "$current_amount")"
    if [[ "$amount" -gt 0 ]]; then
      destination="$(token_account_for_owner "$FAUCET_ADDRESS" "$mint")"
      withdraw_inventory "$lane_id" "$mint" "$amount" "$destination"
    else
      printf 'lane %s mint %s has no cleanup inventory to withdraw under CLEANUP_MODE=%s\n' "$lane_id" "$mint" "$CLEANUP_MODE"
    fi
  done
done

log "Final hub state"
"${HUB_CLI[@]}" state
