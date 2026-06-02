#!/usr/bin/env bash
set -euo pipefail

QEDGEN="${QEDGEN:-$HOME/.agents/skills/qedgen/tools/qedgen}"
SPEC="crates/loyal-hub-swap-program/verification/loyal_hub_swap.qedspec"
OUT_DIR="target/qedgen/loyal-hub-swap-program-impl"
KANI_IMPL_PATH="$OUT_DIR/programs/src/kani_impl.rs"
KANI_IMPL_SCOPE="${KANI_IMPL_SCOPE:-smoke}"
KANI_IMPL_HARNESS="${KANI_IMPL_HARNESS:-}"
KANI_IMPL_EXACT="${KANI_IMPL_EXACT:-0}"
KANI_OUTPUT_FORMAT="${KANI_OUTPUT_FORMAT:-terse}"

echo "Generating QEDGen Pinocchio Kani impl harnesses from $SPEC"
"$QEDGEN" codegen \
  --spec "$SPEC" \
  --target pinocchio \
  --output-dir "$OUT_DIR/programs" \
  --kani-impl \
  --kani-impl-output "$KANI_IMPL_PATH"

echo "Preparing temporary Loyal Hub crate with generated kani_impl.rs"
impl_crate="$(mktemp -d "${TMPDIR:-/tmp}/loyal-qedgen-kani-impl.XXXXXX")"
trap 'rm -rf "$impl_crate"' EXIT

rsync -a \
  --exclude .git \
  --exclude target \
  --exclude node_modules \
  ./ "$impl_crate"/
cp "$KANI_IMPL_PATH" "$impl_crate/crates/loyal-hub-swap-program/src/kani_impl.rs"

harnesses=()
exact_harnesses=0

if [[ -n "$KANI_IMPL_HARNESS" ]]; then
  IFS=',' read -r -a harnesses <<< "$KANI_IMPL_HARNESS"
  if [[ "$KANI_IMPL_EXACT" == "1" ]]; then
    exact_harnesses=1
  fi
elif [[ "$KANI_IMPL_SCOPE" == "smoke" ]]; then
  harnesses=(
    verify_initialize_config_impl
    verify_set_max_fee_impl
    verify_set_paused_impl
    verify_withdraw_inventory_impl
    verify_swap_exact_in_impl
    verify_rebalance_inventory_impl
    verify_rebalance_inventory_4_impl
    verify_rebalance_inventory_16_impl
  )
elif [[ "$KANI_IMPL_SCOPE" == "full" ]]; then
  while IFS= read -r harness; do
    harnesses+=("$harness")
  done < <(
    awk '
      /^[[:space:]]*#\[kani::proof\][[:space:]]*$/ { in_proof = 1; next }
      in_proof && /^[[:space:]]*#\[/ { next }
      in_proof && /^[[:space:]]*$/ { next }
      in_proof && /^[[:space:]]*fn [A-Za-z0-9_]+/ {
        sub(/.*fn /, "")
        sub(/\(.*/, "")
        print
        in_proof = 0
        next
      }
      in_proof { in_proof = 0 }
    ' "$KANI_IMPL_PATH"
  )
else
  echo "unknown KANI_IMPL_SCOPE: $KANI_IMPL_SCOPE" >&2
  echo "expected smoke or full" >&2
  exit 2
fi

if [[ "${#harnesses[@]}" -eq 0 ]]; then
  echo "no Kani impl proofs selected" >&2
  exit 2
fi

(
  cd "$impl_crate"
  echo "QEDGen Kani impl scope: $KANI_IMPL_SCOPE"
  echo "QEDGen Kani impl proofs selected: ${#harnesses[@]}"

  for ((i = 0; i < ${#harnesses[@]}; i++)); do
    harness="${harnesses[$i]}"
    args=(-p loyal-hub-swap-program --harness "$harness" --output-format "$KANI_OUTPUT_FORMAT")
    if [[ "$exact_harnesses" == "1" ]]; then
      args+=(--exact)
    fi

    start_epoch="$(date +%s)"
    printf '[%02d/%02d] QEDGen Kani impl proof start: %s\n' "$((i + 1))" "${#harnesses[@]}" "$harness"
    if cargo kani "${args[@]}"; then
      elapsed="$(( $(date +%s) - start_epoch ))"
      printf '[%02d/%02d] QEDGen Kani impl proof ok: %s (%ss)\n' "$((i + 1))" "${#harnesses[@]}" "$harness" "$elapsed"
    else
      elapsed="$(( $(date +%s) - start_epoch ))"
      printf '[%02d/%02d] QEDGen Kani impl proof failed: %s (%ss)\n' "$((i + 1))" "${#harnesses[@]}" "$harness" "$elapsed" >&2
      exit 1
    fi
  done
)
