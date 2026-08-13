#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
FIXTURE_MANIFEST=""
OUTPUT_DIR="$ROOT_DIR/artifacts/fleet-litesvm-e2e"
SKIP_BUILD=0

usage() {
  command cat <<'USAGE'
Usage: bun run fleet:litesvm-e2e -- --fixture MANIFEST [options]

Options:
  --fixture MANIFEST  Verified finalized Mainnet clone manifest
  --output DIR        Evidence root
  --skip-build        Reuse the existing mock protocol SBF
  --help              Show this message

This stage starts no RPC node, database, or network listener. It loads the
captured accounts into LiteSVM and executes the Main-to-Prime route in-process.
USAGE
}

while (($#)); do
  case "$1" in
    --fixture) FIXTURE_MANIFEST=${2:?missing value}; shift 2 ;;
    --output) OUTPUT_DIR=${2:?missing value}; shift 2 ;;
    --skip-build) SKIP_BUILD=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if test -z "$FIXTURE_MANIFEST"; then
  echo "--fixture is required" >&2
  usage >&2
  exit 2
fi
for tool in bun cargo; do
  command -v "$tool" >/dev/null || { echo "required tool is missing: $tool" >&2; exit 1; }
done

unset DATABASE_URL NEON_DATABASE_URL TIMESCALEDB_URL SOLANA_RPC_URL SOLANA_WS_URL RPC_URL
unset HELIUS_RPC_URL HELIUS_API_KEY HYPERDX_ACCESS_KEY OBSERVABILITY_INGESTION_API_KEY
unset POLICY_KEYPAIR YIELD_ROUTE_FEE_PAYER_KEYPAIRS SOLANA_TESTING_PK YIELD_ROUTER_KEYPAIR
unset YIELD_ALT_CLUSTER YIELD_ROUTE_CLUSTER YIELD_ROUTE_POLICY_AUTHORITY

FIXTURE_MANIFEST=$(cd "$ROOT_DIR" && cd "$(dirname "$FIXTURE_MANIFEST")" && pwd)/$(basename "$FIXTURE_MANIFEST")
OUTPUT_DIR=$(mkdir -p "$OUTPUT_DIR" && cd "$OUTPUT_DIR" && pwd)
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
RUN_DIR="$OUTPUT_DIR/$RUN_ID"
mkdir -p "$RUN_DIR"

echo "Verifying the finalized fixture before LiteSVM"
bun "$ROOT_DIR/scripts/fleet-local-chain-e2e/fixture.ts" verify "$FIXTURE_MANIFEST" \
  >"$RUN_DIR/offline-fixture-verification.json"

if ((!SKIP_BUILD)); then
  echo "Building the deterministic Kamino test SBF"
  cargo build-sbf \
    --manifest-path "$ROOT_DIR/crates/mock-yield-protocols-program/Cargo.toml" \
    --sbf-out-dir "$ROOT_DIR/target/deploy"
fi
test -f "$ROOT_DIR/target/deploy/mock_yield_protocols_program.so" || {
  echo "mock protocol SBF is missing; rerun without --skip-build" >&2
  exit 1
}

echo "Loading and reading back every captured account in LiteSVM"
cargo run -q -p squads-test-harness --bin fleet-litesvm-fixture-verifier -- \
  --manifest "$FIXTURE_MANIFEST" >"$RUN_DIR/litesvm-fixture.json"

echo "Executing the exact Main-to-Prime route topology in LiteSVM"
cargo test -q -p squads-test-harness --test reusable_alt_v0_matrix \
  reusable_alt_v0_matrix_compiles_covers_and_executes_every_earn_shape \
  -- --exact --nocapture >"$RUN_DIR/litesvm-route-test.log" 2>&1

bun "$ROOT_DIR/scripts/verify-fleet-litesvm-e2e.ts" assemble "$RUN_DIR"
bun "$ROOT_DIR/scripts/verify-fleet-litesvm-e2e.ts" verify "$RUN_DIR/evidence.json"
echo "LITESVM_E2E: PASS - $RUN_DIR/evidence.md"
echo "VALIDATOR_NODE_E2E: READY"
