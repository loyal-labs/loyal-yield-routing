#!/usr/bin/env bash
set -euo pipefail

KANI_OUTPUT_FORMAT="${KANI_OUTPUT_FORMAT:-terse}"
HUB_KANI_IMPL_HARNESS="${HUB_KANI_IMPL_HARNESS:-}"

harnesses=()

if [[ -n "$HUB_KANI_IMPL_HARNESS" ]]; then
  IFS=',' read -r -a harnesses <<< "$HUB_KANI_IMPL_HARNESS"
else
  harnesses=(
    verify_live_set_max_fee_updates_config
    verify_live_set_paused_updates_config
    verify_live_rebalance_inventory_moves_one_projected_token_balance
    verify_live_rebalance_inventory_moves_two_projected_token_balances
    verify_live_rebalance_inventory_moves_four_projected_token_balances
    verify_live_rebalance_inventory_moves_eight_projected_token_balances
    verify_live_rebalance_inventory_moves_max_projected_token_balances
    verify_live_swap_exact_in_moves_projected_token_balances
    verify_live_withdraw_inventory_moves_projected_token_balances
  )
fi

echo "Loyal Hub live Kani impl proofs selected: ${#harnesses[@]}"

for ((i = 0; i < ${#harnesses[@]}; i++)); do
  harness="${harnesses[$i]}"
  start_epoch="$(date +%s)"
  printf '[%02d/%02d] Live Kani impl proof start: %s\n' "$((i + 1))" "${#harnesses[@]}" "$harness"
  if cargo kani -p loyal-hub-swap-program --harness "$harness" --output-format "$KANI_OUTPUT_FORMAT"; then
    elapsed="$(( $(date +%s) - start_epoch ))"
    printf '[%02d/%02d] Live Kani impl proof ok: %s (%ss)\n' "$((i + 1))" "${#harnesses[@]}" "$harness" "$elapsed"
  else
    elapsed="$(( $(date +%s) - start_epoch ))"
    printf '[%02d/%02d] Live Kani impl proof failed: %s (%ss)\n' "$((i + 1))" "${#harnesses[@]}" "$harness" "$elapsed" >&2
    exit 1
  fi
done
