#!/usr/bin/env bash
set -euo pipefail

QEDGEN="${QEDGEN:-$HOME/.agents/skills/qedgen/tools/qedgen}"
SPEC="crates/loyal-hub-swap-program/verification/loyal_hub_swap.qedspec"
OUT_DIR="target/qedgen/loyal-hub-swap-program/quasar"

rm -rf "$OUT_DIR"
"$QEDGEN" codegen \
  --spec "$SPEC" \
  --target quasar \
  --output-dir "$OUT_DIR"

cargo check --manifest-path "$OUT_DIR/Cargo.toml"
