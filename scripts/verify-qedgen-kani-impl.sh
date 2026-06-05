#!/usr/bin/env bash
set -euo pipefail

QEDGEN="${QEDGEN:-$HOME/.agents/skills/qedgen/tools/qedgen}"
SPEC="crates/loyal-hub-swap-program/verification/loyal_hub_swap.qedspec"
OUT_DIR="target/qedgen/loyal-hub-swap-program-impl"
KANI_IMPL_PATH="$OUT_DIR/programs/src/kani_impl.rs"
COMMITTED_KANI_IMPL_PATH="crates/loyal-hub-swap-program/src/kani_impl.rs"
KANI_IMPL_SCOPE="${KANI_IMPL_SCOPE:-smoke}"
KANI_IMPL_HARNESS="${KANI_IMPL_HARNESS:-}"
KANI_IMPL_EXACT="${KANI_IMPL_EXACT:-0}"
KANI_IMPL_MODE="${KANI_IMPL_MODE:-prove}"
KANI_OUTPUT_FORMAT="${KANI_OUTPUT_FORMAT:-terse}"
KANI_SOLVER="${KANI_SOLVER:-kissat}"
KANI_EXTRA_ARGS="${KANI_EXTRA_ARGS:-}"
KANI_HARNESS_TIMEOUT="${KANI_HARNESS_TIMEOUT:-}"
KANI_IMPL_UPDATE="${KANI_IMPL_UPDATE:-0}"
KANI_IMPL_DRIFT_CHECK="${KANI_IMPL_DRIFT_CHECK:-1}"

echo "Generating QEDGen Pinocchio Kani impl harnesses from $SPEC"
"$QEDGEN" codegen \
  --spec "$SPEC" \
  --target pinocchio \
  --output-dir "$OUT_DIR/programs" \
  --kani-impl \
  --kani-impl-output "$KANI_IMPL_PATH"

rustfmt "$KANI_IMPL_PATH"

if [[ "$KANI_IMPL_UPDATE" == "1" ]]; then
  cp "$KANI_IMPL_PATH" "$COMMITTED_KANI_IMPL_PATH"
  echo "Updated committed Loyal Hub kani_impl.rs from generated QEDGen output"
fi

if [[ "$KANI_IMPL_DRIFT_CHECK" == "1" ]]; then
  if ! diff -u "$COMMITTED_KANI_IMPL_PATH" "$KANI_IMPL_PATH"; then
    echo "committed Loyal Hub kani_impl.rs differs from freshly generated QEDGen output" >&2
    echo "rerun with KANI_IMPL_UPDATE=1 to refresh the committed generated harness" >&2
    exit 1
  fi
fi

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
    verify_set_max_fee_impl
    verify_set_paused_impl
    verify_withdraw_inventory_impl
    verify_swap_exact_in_impl
    verify_rebalance_inventory_impl
    verify_rebalance_inventory_2_impl
    verify_rebalance_inventory_4_impl
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

if grep -nE 'TODO|todo!|unimplemented!' "$KANI_IMPL_PATH"; then
  echo "generated Kani impl contains TODO or placeholder proof code" >&2
  exit 1
fi

harness_body() {
  local harness="$1"
  awk -v harness="$harness" '
    $0 ~ "^[[:space:]]*fn " harness "\\(" {
      in_body = 1
      depth = 0
    }
    in_body {
      print
      opens = gsub(/\{/, "{")
      closes = gsub(/\}/, "}")
      depth += opens - closes
      if (depth == 0) {
        exit
      }
    }
  ' "$KANI_IMPL_PATH"
}

assert_generated_harness_contract() {
  local harness="$1"
  local body
  body="$(harness_body "$harness")"

  if [[ -z "$body" ]]; then
    echo "generated Kani impl is missing harness: $harness" >&2
    exit 1
  fi

  if ! grep -Fq 'let _result = crate::process_instruction(' <<< "$body"; then
    echo "generated harness does not call real process_instruction: $harness" >&2
    exit 1
  fi

  local compact_body
  compact_body="$(tr -d '[:space:]' <<< "$body")"

  if ! grep -Fq 'assert!(_result.is_ok()' <<< "$compact_body"; then
    echo "generated harness is missing success assertion: $harness" >&2
    exit 1
  fi

  if ! grep -Fq 'kani::cover!(_result.is_ok()' <<< "$compact_body"; then
    echo "generated harness is missing success reachability cover: $harness" >&2
    exit 1
  fi

  local post_assertions
  post_assertions="$(
    grep -Ec '^[[:space:]]*assert_eq!\(' <<< "$body" || true
  )"
  if [[ "$post_assertions" -lt 2 ]]; then
    echo "generated harness is missing concrete post-state assertions: $harness" >&2
    exit 1
  fi
}

for harness in "${harnesses[@]}"; do
  assert_generated_harness_contract "$harness"
done

if [[ "$KANI_IMPL_MODE" == "static" ]]; then
  printf 'QEDGen Kani impl static gate ok: %s harness(es)\n' "${#harnesses[@]}"
  exit 0
fi

if [[ "$KANI_IMPL_MODE" != "prove" && "$KANI_IMPL_MODE" != "codegen" && "$KANI_IMPL_MODE" != "contracts" ]]; then
  echo "unknown KANI_IMPL_MODE: $KANI_IMPL_MODE" >&2
  echo "expected static, codegen, contracts, or prove" >&2
  exit 2
fi

kani_extra_args=()
if [[ -n "$KANI_EXTRA_ARGS" ]]; then
  # shellcheck disable=SC2206
  kani_extra_args=($KANI_EXTRA_ARGS)
fi
if [[ -n "$KANI_HARNESS_TIMEOUT" ]]; then
  kani_extra_args+=(-Z unstable-options)
  kani_extra_args+=(--harness-timeout "$KANI_HARNESS_TIMEOUT")
fi
if grep -Fq '#[kani::stub_verified(' "$KANI_IMPL_PATH"; then
  kani_extra_args+=(-Z function-contracts)
  kani_extra_args+=(-Z stubbing)
fi

(
  cd "$impl_crate"
  copied_kani_impl="crates/loyal-hub-swap-program/src/kani_impl.rs"
  echo "QEDGen Kani impl scope: $KANI_IMPL_SCOPE"
  echo "QEDGen Kani impl proofs selected: ${#harnesses[@]}"

  if grep -Fq '#[kani::stub_verified(' "$copied_kani_impl"; then
    echo "QEDGen Kani impl verified-stub contracts start"
    start_epoch="$(date +%s)"
    contract_log="$(mktemp "$impl_crate/kani-contracts.XXXXXX")"
    contract_args=(
      --manifest-path crates/loyal-hub-swap-program/Cargo.toml
      -Z function-contracts
      -Z stubbing
      --harness contract
      --output-format "$KANI_OUTPUT_FORMAT"
      --solver "$KANI_SOLVER"
    )
    if [[ -n "$KANI_HARNESS_TIMEOUT" ]]; then
      contract_args+=(-Z unstable-options)
      contract_args+=(--harness-timeout "$KANI_HARNESS_TIMEOUT")
    fi
    set +e
    cargo kani "${contract_args[@]}" 2>&1 | tee "$contract_log"
    contract_status="${PIPESTATUS[0]}"
    set -e
    elapsed="$(( $(date +%s) - start_epoch ))"
    if [[ "$contract_status" -ne 0 ]]; then
      printf 'QEDGen Kani impl verified-stub contracts failed (%ss)\n' "$elapsed" >&2
      exit 1
    fi
    printf 'QEDGen Kani impl verified-stub contracts ok (%ss)\n' "$elapsed"
  elif [[ "$KANI_IMPL_MODE" == "contracts" ]]; then
    echo "QEDGen Kani impl contracts mode: no verified stubs found"
    exit 0
  fi

  if [[ "$KANI_IMPL_MODE" == "contracts" ]]; then
    exit 0
  fi

  for ((i = 0; i < ${#harnesses[@]}; i++)); do
    harness="${harnesses[$i]}"
    args=(
      --manifest-path crates/loyal-hub-swap-program/Cargo.toml
      --harness "$harness"
      --output-format "$KANI_OUTPUT_FORMAT"
      --solver "$KANI_SOLVER"
    )
    if [[ "${#kani_extra_args[@]}" -gt 0 ]]; then
      args+=("${kani_extra_args[@]}")
    fi
    if [[ "$exact_harnesses" == "1" ]]; then
      args+=(--exact)
    fi
    if [[ "$KANI_IMPL_MODE" == "codegen" ]]; then
      args+=(-Z unstable-options)
      args+=(--only-codegen)
    fi

    start_epoch="$(date +%s)"
    printf '[%02d/%02d] QEDGen Kani impl %s start: %s\n' "$((i + 1))" "${#harnesses[@]}" "$KANI_IMPL_MODE" "$harness"
    run_log="$(mktemp "$impl_crate/kani-$harness.XXXXXX")"
    set +e
    cargo kani "${args[@]}" 2>&1 | tee "$run_log"
    cargo_status="${PIPESTATUS[0]}"
    set -e

    if [[ "$cargo_status" -eq 0 && "$KANI_IMPL_MODE" == "codegen" ]]; then
      elapsed="$(( $(date +%s) - start_epoch ))"
      printf '[%02d/%02d] QEDGen Kani impl codegen ok: %s (%ss)\n' "$((i + 1))" "${#harnesses[@]}" "$harness" "$elapsed"
    elif [[ "$cargo_status" -eq 0 ]]; then
      if ! grep -Fq '** 1 of 1 cover properties satisfied' "$run_log"; then
        elapsed="$(( $(date +%s) - start_epoch ))"
        printf '[%02d/%02d] QEDGen Kani impl proof unreachable: %s (%ss)\n' "$((i + 1))" "${#harnesses[@]}" "$harness" "$elapsed" >&2
        echo "expected Kani output to contain: ** 1 of 1 cover properties satisfied" >&2
        exit 1
      fi
      elapsed="$(( $(date +%s) - start_epoch ))"
      printf '[%02d/%02d] QEDGen Kani impl proof ok: %s (%ss)\n' "$((i + 1))" "${#harnesses[@]}" "$harness" "$elapsed"
    else
      elapsed="$(( $(date +%s) - start_epoch ))"
      printf '[%02d/%02d] QEDGen Kani impl proof failed: %s (%ss)\n' "$((i + 1))" "${#harnesses[@]}" "$harness" "$elapsed" >&2
      exit 1
    fi
  done
)
