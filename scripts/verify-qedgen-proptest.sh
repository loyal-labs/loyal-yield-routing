#!/usr/bin/env bash
set -euo pipefail

QEDGEN="${QEDGEN:-$HOME/.agents/skills/qedgen/tools/qedgen}"
SPEC="crates/loyal-hub-swap-program/verification/loyal_hub_swap.qedspec"
OUT_DIR="target/qedgen/loyal-hub-swap-program"

rm -rf "$OUT_DIR/programs" "$OUT_DIR/tests"
mkdir -p "$OUT_DIR/tests"

cat > "$OUT_DIR/Cargo.toml" <<'TOML'
[package]
name = "loyal-hub-swap-qedgen-verification"
version = "0.1.0"
edition = "2021"
publish = false

[workspace]

[dev-dependencies]
proptest = "1.6"
TOML

"$QEDGEN" codegen \
  --spec "$SPEC" \
  --output-dir "$OUT_DIR/programs" \
  --proptest \
  --proptest-output "$OUT_DIR/tests/proptest.rs"

"$QEDGEN" verify \
  --spec "$SPEC" \
  --proptest \
  --proptest-path "$OUT_DIR/tests/proptest.rs"
