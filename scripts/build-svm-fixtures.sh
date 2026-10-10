#!/usr/bin/env bash
# Build the connected SVM fixtures the Workers v2 Go tests execute against:
# the mock yield protocols program, the fleet LiteSVM harness, the multiply
# fixture binary and the autodeposit fixture. Needs cargo-build-sbf on PATH.
set -euo pipefail
cd "$(dirname "$0")/.."
cargo build-sbf --tools-version v1.52 --manifest-path crates/mock-yield-protocols-program/Cargo.toml -- --locked
cargo build --locked -p squads-test-harness --example fleet-local-svm
mkdir -p target/svm-fixtures
cp "$(cargo test --locked --no-run -p squads-test-harness --test workers_v2_multiply --message-format=json | jq -r 'select(.executable != null) | .executable')" target/svm-fixtures/workers-v2-multiply
MOCK_YIELD_PROTOCOLS_PROGRAM_SO="$PWD/target/deploy/mock_yield_protocols_program.so" WORKERS_V2_AUTODEPOSIT_SVM_FIXTURE_OUTPUT="$PWD/target/svm-fixtures/autodeposit.json" cargo test --locked -p squads-test-harness --test workers_v2_autodeposit
test -s target/deploy/mock_yield_protocols_program.so
test -x target/debug/examples/fleet-local-svm
test -x target/svm-fixtures/workers-v2-multiply
test -s target/svm-fixtures/autodeposit.json
