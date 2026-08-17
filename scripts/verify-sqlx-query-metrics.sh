#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

echo "[1/7] SQLx metrics behavioral contract"
[[ -f crates/loyal-observability/src/sqlx_metrics.rs ]] \
  || fail "crates/loyal-observability/src/sqlx_metrics.rs is missing"

test_output="$(mktemp)"
trap 'rm -f "$test_output"' EXIT
if ! cargo test -p loyal-observability --locked sqlx_metrics -- --nocapture 2>&1 \
  | tee "$test_output"; then
  fail "SQLx metrics behavioral tests failed"
fi

passed_tests="$(sed -nE 's/^test result: ok\. ([0-9]+) passed;.*/\1/p' "$test_output" | head -n 1)"
[[ "$passed_tests" =~ ^[0-9]+$ ]] \
  || fail "could not read the SQLx metrics behavioral test count"
(( passed_tests >= 5 )) \
  || fail "expected at least 5 SQLx metrics behavioral tests; observed $passed_tests"

echo "[2/7] loyal-observability regression suite"
cargo test -p loyal-observability --locked

echo "[3/7] loyal-observability standalone compile"
cargo check -p loyal-observability --locked

echo "[4/7] loyal-observability lint"
cargo clippy -p loyal-observability --locked -- -D warnings

echo "[5/7] loyal-observability formatting"
cargo fmt -p loyal-observability -- --check

echo "[6/7] dependency and privacy boundaries"
if rg -n 'loyal-observability' crates/loyal-yield-store/Cargo.toml; then
  fail "loyal-yield-store must not depend on loyal-observability"
fi

if rg -n 'db\.statement|db\.query\.text|db\.query\.summary' \
  crates/loyal-observability/src/sqlx_metrics.rs; then
  fail "SQL text and query summaries must not be read into or exported by SQLx metrics"
fi

echo "[7/7] patch hygiene"
git diff --check

echo "PASS: SQLx query and pool timing metrics satisfy the verifier"
