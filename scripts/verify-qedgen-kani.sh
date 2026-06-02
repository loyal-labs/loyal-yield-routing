#!/usr/bin/env bash
set -euo pipefail

QEDGEN="${QEDGEN:-$HOME/.agents/skills/qedgen/tools/qedgen}"
SPEC="crates/loyal-hub-swap-program/verification/loyal_hub_swap.qedspec"
OUT_DIR="target/qedgen/loyal-hub-swap-program"
KANI_PATH="$OUT_DIR/programs/tests/kani.rs"
KANI_SCOPE="${KANI_SCOPE:-smoke}"
KANI_JOBS="${KANI_JOBS:-}"
KANI_HARNESS="${KANI_HARNESS:-}"
KANI_HARNESS_TIMEOUT="${KANI_HARNESS_TIMEOUT:-}"
KANI_OUTPUT_FORMAT="${KANI_OUTPUT_FORMAT:-terse}"
KANI_EXACT="${KANI_EXACT:-0}"
KANI_REGEN="${KANI_REGEN:-1}"
KANI_SOURCE="${KANI_SOURCE:-$KANI_PATH}"
QEDGEN_KANI_SKIP_GUARD_PROOFS="${QEDGEN_KANI_SKIP_GUARD_PROOFS:-}"

if [[ "$KANI_REGEN" == "1" ]]; then
  echo "Generating QEDGen Kani harnesses from $SPEC"
  codegen_env=()
  if [[ -z "$QEDGEN_KANI_SKIP_GUARD_PROOFS" && "$KANI_SCOPE" == "smoke" && -z "$KANI_HARNESS" ]]; then
    codegen_env+=(QEDGEN_KANI_SKIP_GUARD_PROOFS=1)
    echo "Skipping generated guard-rejection proofs for smoke scope"
  elif [[ -n "$QEDGEN_KANI_SKIP_GUARD_PROOFS" ]]; then
    codegen_env+=(QEDGEN_KANI_SKIP_GUARD_PROOFS="$QEDGEN_KANI_SKIP_GUARD_PROOFS")
  fi

  env "${codegen_env[@]}" "$QEDGEN" codegen \
    --spec "$SPEC" \
    --output-dir "$OUT_DIR/programs" \
    --kani \
    --kani-output "$KANI_PATH"
  KANI_SOURCE="$KANI_PATH"
else
  echo "Using cached QEDGen Kani harness from $KANI_SOURCE"
fi

if [[ ! -f "$KANI_SOURCE" ]]; then
  echo "Kani harness not found: $KANI_SOURCE" >&2
  echo "run with KANI_REGEN=1 or pass KANI_SOURCE=/path/to/kani.rs" >&2
  exit 2
fi

echo "Preparing temporary Kani crate"
kani_crate="$(mktemp -d "${TMPDIR:-/tmp}/loyal-qedgen-kani.XXXXXX")"
trap 'rm -rf "$kani_crate"' EXIT

mkdir -p "$kani_crate/src"
cp "$KANI_SOURCE" "$kani_crate/src/lib.rs"
printf '%s\n' \
  '[package]' \
  'name = "loyal-qedgen-kani-harness"' \
  'version = "0.1.0"' \
  'edition = "2021"' \
  '' \
  '[lib]' \
  'path = "src/lib.rs"' \
  > "$kani_crate/Cargo.toml"

base_args=(kani --tests --output-format "$KANI_OUTPUT_FORMAT")

if [[ -n "$KANI_JOBS" ]]; then
  base_args+=(-j "$KANI_JOBS")
fi

if [[ -n "$KANI_HARNESS_TIMEOUT" ]]; then
  base_args+=(-Z unstable-options --harness-timeout "$KANI_HARNESS_TIMEOUT")
fi

harnesses=()
exact_harnesses=0

if [[ -n "$KANI_HARNESS" ]]; then
  IFS=',' read -r -a harnesses <<< "$KANI_HARNESS"
  if [[ "$KANI_EXACT" == "1" ]]; then
    exact_harnesses=1
  fi
elif [[ "$KANI_SCOPE" == "smoke" ]]; then
  exact_harnesses=1
  harnesses=(
    verify_initialize_config_effect_admin_key
    verify_initialize_config_effect_hub_authorizer_key
    verify_initialize_config_effect_inventory_rebalancer_key
    verify_initialize_config_effect_lane_count
    verify_set_max_fee_effect_max_fee_bps
    verify_set_paused_effect_paused
    verify_rebalance_inventory_preserves_config_domain_preserved
    cover_lane_rebalance
  )
elif [[ "$KANI_SCOPE" == "full" ]]; then
  exact_harnesses=1
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
    ' "$KANI_SOURCE"
  )
else
  echo "unknown KANI_SCOPE: $KANI_SCOPE" >&2
  echo "expected smoke or full" >&2
  exit 2
fi

if [[ "${#harnesses[@]}" -eq 0 ]]; then
  echo "no Kani proofs selected" >&2
  exit 2
fi

(
  cd "$kani_crate"
  export RUSTFLAGS="${RUSTFLAGS:+$RUSTFLAGS }-Awarnings"
  echo "Kani scope: $KANI_SCOPE"
  echo "Kani proofs selected: ${#harnesses[@]}"

  for ((i = 0; i < ${#harnesses[@]}; i++)); do
    harness="${harnesses[$i]}"
    args=("${base_args[@]}")
    if [[ "$exact_harnesses" == "1" ]]; then
      args+=(--exact)
    fi
    args+=(--harness "$harness")

    start_epoch="$(date +%s)"
    printf '[%02d/%02d] Kani proof start: %s\n' "$((i + 1))" "${#harnesses[@]}" "$harness"
    if cargo "${args[@]}"; then
      elapsed="$(( $(date +%s) - start_epoch ))"
      printf '[%02d/%02d] Kani proof ok: %s (%ss)\n' "$((i + 1))" "${#harnesses[@]}" "$harness" "$elapsed"
    else
      elapsed="$(( $(date +%s) - start_epoch ))"
      printf '[%02d/%02d] Kani proof failed: %s (%ss)\n' "$((i + 1))" "${#harnesses[@]}" "$harness" "$elapsed" >&2
      exit 1
    fi
  done
)
