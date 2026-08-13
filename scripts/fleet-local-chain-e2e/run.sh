#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
FIXTURE_MANIFEST=""
CAPTURE_MAINNET_RPC=""
OUTPUT_DIR="$ROOT_DIR/artifacts/fleet-local-chain-e2e"
DATABASE_PORT=55461
VALIDATOR_RPC_PORT=18921
PROXY_RPC_PORT=18931
RPC_LOAD_SECONDS=3
RPC_LOAD_CLIENTS=4
RPC_LATENCY_MS=0
RPC_JITTER_MS=0
RPC_ERROR_EVERY=0
SKIP_BUILD=0
KEEP_TEMP=0

usage() {
  command cat <<'USAGE'
Usage: bun run fleet:local-chain-e2e -- [options]

Fixture source (choose exactly one):
  --fixture MANIFEST             Existing verified finalized Mainnet clone
  --capture-mainnet-rpc URL      Capture finalized public accounts read-only

Options:
  --output DIR                   Evidence root
  --database-port PORT           Disposable PostgreSQL loopback port
  --validator-rpc-port PORT      Stateful validator loopback RPC port
  --proxy-rpc-port PORT          Instrumented proxy loopback RPC port
  --rpc-load-seconds N           Concurrent synthetic RPC load duration
  --rpc-load-clients N           Concurrent synthetic RPC clients
  --rpc-latency-ms N             Added proxy latency per request
  --rpc-jitter-ms N              Deterministic added proxy jitter
  --rpc-error-every N            Fail every Nth proxied request; 0 disables
  --skip-build                   Reuse exact debug binaries
  --keep-temp                    Retain disposable ledger, DB, and keys
  --help                         Show this message

The capture mode performs finalized public Mainnet reads only. The execution
pipeline uses a disposable local validator, local PostgreSQL, ephemeral keys,
and simulated APY inputs. No production transaction or database write occurs.
USAGE
}

while (($#)); do
  case "$1" in
    --fixture) FIXTURE_MANIFEST=${2:?missing value}; shift 2 ;;
    --capture-mainnet-rpc) CAPTURE_MAINNET_RPC=${2:?missing value}; shift 2 ;;
    --output) OUTPUT_DIR=${2:?missing value}; shift 2 ;;
    --database-port) DATABASE_PORT=${2:?missing value}; shift 2 ;;
    --validator-rpc-port) VALIDATOR_RPC_PORT=${2:?missing value}; shift 2 ;;
    --proxy-rpc-port) PROXY_RPC_PORT=${2:?missing value}; shift 2 ;;
    --rpc-load-seconds) RPC_LOAD_SECONDS=${2:?missing value}; shift 2 ;;
    --rpc-load-clients) RPC_LOAD_CLIENTS=${2:?missing value}; shift 2 ;;
    --rpc-latency-ms) RPC_LATENCY_MS=${2:?missing value}; shift 2 ;;
    --rpc-jitter-ms) RPC_JITTER_MS=${2:?missing value}; shift 2 ;;
    --rpc-error-every) RPC_ERROR_EVERY=${2:?missing value}; shift 2 ;;
    --skip-build) SKIP_BUILD=1; shift ;;
    --keep-temp) KEEP_TEMP=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if test -n "$FIXTURE_MANIFEST" && test -n "$CAPTURE_MAINNET_RPC"; then
  echo "choose --fixture or --capture-mainnet-rpc, not both" >&2
  exit 2
fi
if test -z "$FIXTURE_MANIFEST" && test -z "$CAPTURE_MAINNET_RPC"; then
  echo "a verified fixture source is required" >&2
  usage >&2
  exit 2
fi
for value in "$DATABASE_PORT" "$VALIDATOR_RPC_PORT" "$PROXY_RPC_PORT" \
  "$RPC_LOAD_SECONDS" "$RPC_LOAD_CLIENTS" "$RPC_LATENCY_MS" "$RPC_JITTER_MS" \
  "$RPC_ERROR_EVERY"; do
  case "$value" in *[!0-9]*|"") echo "numeric options require unsigned integers" >&2; exit 2 ;; esac
done
for port in "$DATABASE_PORT" "$VALIDATOR_RPC_PORT" "$PROXY_RPC_PORT"; do
  if ((port < 1024 || port > 65535)); then echo "ports must be in 1024..65535" >&2; exit 2; fi
done
if ((DATABASE_PORT == VALIDATOR_RPC_PORT || DATABASE_PORT == PROXY_RPC_PORT || VALIDATOR_RPC_PORT == PROXY_RPC_PORT)); then
  echo "database, validator, and proxy ports must differ" >&2
  exit 2
fi
if ((RPC_LOAD_SECONDS < 1 || RPC_LOAD_CLIENTS < 1 || RPC_LOAD_CLIENTS > 64)); then
  echo "RPC load requires positive seconds and 1..64 clients" >&2
  exit 2
fi

for tool in bun cargo curl jq pg_config psql createdb solana solana-keygen solana-test-validator; do
  command -v "$tool" >/dev/null || { echo "required tool is missing: $tool" >&2; exit 1; }
done
PG_BIN=$(pg_config --bindir)
for tool in initdb pg_ctl; do
  test -x "$PG_BIN/$tool" || { echo "required PostgreSQL tool is missing: $PG_BIN/$tool" >&2; exit 1; }
done

unset DATABASE_URL NEON_DATABASE_URL TIMESCALEDB_URL SOLANA_RPC_URL SOLANA_WS_URL RPC_URL
unset HELIUS_RPC_URL HELIUS_API_KEY HYPERDX_ACCESS_KEY OBSERVABILITY_INGESTION_API_KEY
unset POLICY_KEYPAIR YIELD_ROUTE_FEE_PAYER_KEYPAIRS SOLANA_TESTING_PK YIELD_ROUTER_KEYPAIR
unset YIELD_ALT_CLUSTER YIELD_ROUTE_CLUSTER YIELD_ROUTE_POLICY_AUTHORITY

RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
RUN_STARTED_AT_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)
OUTPUT_DIR=$(mkdir -p "$OUTPUT_DIR" && cd "$OUTPUT_DIR" && pwd)
RUN_DIR="$OUTPUT_DIR/$RUN_ID"
mkdir -p "$RUN_DIR/workers" "$RUN_DIR/setup" "$RUN_DIR/load"
TEMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/loyal-fleet-chain-e2e.XXXXXX")
chmod 700 "$TEMP_ROOT"
PG_DATA="$TEMP_ROOT/postgres"
PG_LOG="$TEMP_ROOT/postgres.log"
LEDGER_DIR="$TEMP_ROOT/validator-ledger"
WALLET_KEYPAIR_FILE="$TEMP_ROOT/wallet.json"
POLICY_KEYPAIR_FILE="$TEMP_ROOT/policy.json"
WALLET_USDC_FILE="$TEMP_ROOT/wallet-usdc.json"
VALIDATOR_PROGRAM_DIR="$TEMP_ROOT/validator-programs"
DATABASE_NAME="fleet_e2e_${RUN_ID//[^0-9]/}"
DATABASE_URL="postgresql://127.0.0.1:${DATABASE_PORT}/${DATABASE_NAME}"
VALIDATOR_RPC_URL="http://127.0.0.1:${VALIDATOR_RPC_PORT}"
PROXY_RPC_URL="http://127.0.0.1:${PROXY_RPC_PORT}"
VALIDATOR_WS_URL="ws://127.0.0.1:$((VALIDATOR_RPC_PORT + 1))"
CARGO_TARGET_DIR=${CARGO_TARGET_DIR:-"$ROOT_DIR/target/fleet-local-chain-e2e"}
DATABASE_STARTED=0
VALIDATOR_PID=""
PROXY_PID=""
RPC_LOAD_PID=""
PLANNER_PID=""
PROJECTOR_PID=""

stop_pid() {
  local pid=${1:-}
  if test -n "$pid" && kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
}

cleanup() {
  local code=$?
  stop_pid "$RPC_LOAD_PID"
  stop_pid "$PLANNER_PID"
  stop_pid "$PROJECTOR_PID"
  stop_pid "$PROXY_PID"
  stop_pid "$VALIDATOR_PID"
  if ((DATABASE_STARTED)); then
    "$PG_BIN/pg_ctl" -D "$PG_DATA" -m fast stop >/dev/null 2>&1 || true
  fi
  cp "$PG_LOG" "$RUN_DIR/postgres.log" 2>/dev/null || true
  if ((KEEP_TEMP)); then
    echo "disposable state retained at $TEMP_ROOT"
  else
    case "$TEMP_ROOT" in
      "${TMPDIR:-/tmp}"/loyal-fleet-chain-e2e.*) rm -rf -- "$TEMP_ROOT" ;;
      *) echo "refusing to remove unexpected temporary path: $TEMP_ROOT" >&2 ;;
    esac
  fi
  exit "$code"
}
trap cleanup EXIT INT TERM

case "$DATABASE_URL" in postgresql://127.0.0.1:*"/fleet_e2e_"*) ;; *) echo "unsafe DB URL" >&2; exit 1 ;; esac
case "$VALIDATOR_RPC_URL:$PROXY_RPC_URL" in http://127.0.0.1:*:http://127.0.0.1:*) ;; *) echo "unsafe RPC URL" >&2; exit 1 ;; esac

if ((!SKIP_BUILD)); then
  echo "Building exact local-chain and production role binaries"
  CARGO_TARGET_DIR="$CARGO_TARGET_DIR" cargo build -q -p loyal-yield-orchestrator \
    --bin fleet-mainnet-clone-capture --bin yield-migrations \
    --bin route-lookup-table-provisioner --bin route-lookup-table-shared-catalog \
    --bin fleet-opportunity-planner --bin fleet-health-projector \
    --bin fleet-route-confirmer
  CARGO_TARGET_DIR="$CARGO_TARGET_DIR" cargo build -q -p loyal-fleet-worker \
    --bin same-mint-reserve-swap
  CARGO_TARGET_DIR="$CARGO_TARGET_DIR" cargo build -q -p squads-test-harness \
    --bin fleet-local-chain-setup
fi

CAPTURE="$CARGO_TARGET_DIR/debug/fleet-mainnet-clone-capture"
MIGRATIONS="$CARGO_TARGET_DIR/debug/yield-migrations"
PROVISIONER="$CARGO_TARGET_DIR/debug/route-lookup-table-provisioner"
CATALOG="$CARGO_TARGET_DIR/debug/route-lookup-table-shared-catalog"
PLANNER="$CARGO_TARGET_DIR/debug/fleet-opportunity-planner"
PROJECTOR="$CARGO_TARGET_DIR/debug/fleet-health-projector"
CONFIRMER="$CARGO_TARGET_DIR/debug/fleet-route-confirmer"
FLEET_WORKER="$CARGO_TARGET_DIR/debug/same-mint-reserve-swap"
CHAIN_SETUP="$CARGO_TARGET_DIR/debug/fleet-local-chain-setup"
for binary in "$CAPTURE" "$MIGRATIONS" "$PROVISIONER" "$CATALOG" "$PLANNER" "$PROJECTOR" "$CONFIRMER" "$FLEET_WORKER" "$CHAIN_SETUP"; do
  test -x "$binary" || { echo "missing binary: $binary" >&2; exit 1; }
done

if test -n "$CAPTURE_MAINNET_RPC"; then
  FIXTURE_DIR="$TEMP_ROOT/mainnet-clone"
  echo "Capturing finalized public Mainnet accounts read-only"
  FLEET_FIXTURE_MAINNET_RPC_URL="$CAPTURE_MAINNET_RPC" \
    "$CAPTURE" --output "$FIXTURE_DIR" >"$RUN_DIR/setup/fixture-capture.json"
  FIXTURE_MANIFEST="$FIXTURE_DIR/manifest.json"
else
  FIXTURE_MANIFEST=$(cd "$ROOT_DIR" && cd "$(dirname "$FIXTURE_MANIFEST")" && pwd)/$(basename "$FIXTURE_MANIFEST")
fi

echo "Verifying finalized clone contract"
bun "$ROOT_DIR/scripts/fleet-local-chain-e2e/fixture.ts" verify "$FIXTURE_MANIFEST" \
  >"$RUN_DIR/setup/fixture-verify.json"
SOURCE_SLOT=$(jq -r .source.minimumContextSlot "$FIXTURE_MANIFEST")
CLONE_ACCOUNT_COUNT=$(jq -r '.accounts | length' "$FIXTURE_MANIFEST")

echo "Completing the LiteSVM prerequisite before starting the validator"
LITESVM_GATE_ROOT="$RUN_DIR/litesvm-gate"
LITESVM_GATE_ARGS=(--fixture "$FIXTURE_MANIFEST" --output "$LITESVM_GATE_ROOT")
if ((SKIP_BUILD)); then LITESVM_GATE_ARGS+=(--skip-build); fi
bash "$ROOT_DIR/scripts/fleet-local-chain-e2e/run-litesvm.sh" "${LITESVM_GATE_ARGS[@]}"
LITESVM_GATE_EVIDENCE=$(find "$LITESVM_GATE_ROOT" -mindepth 2 -maxdepth 2 \
  -type f -name evidence.json -print | sort | tail -1)
test -n "$LITESVM_GATE_EVIDENCE" || { echo "LiteSVM gate produced no evidence" >&2; exit 1; }
cp "$LITESVM_GATE_EVIDENCE" "$RUN_DIR/setup/litesvm-evidence.json"
bun "$ROOT_DIR/scripts/verify-fleet-litesvm-e2e.ts" verify \
  "$RUN_DIR/setup/litesvm-evidence.json"

solana-keygen new --no-bip39-passphrase --silent --outfile "$WALLET_KEYPAIR_FILE" >/dev/null
solana-keygen new --no-bip39-passphrase --silent --outfile "$POLICY_KEYPAIR_FILE" >/dev/null
"$CHAIN_SETUP" prepare-genesis --wallet-keypair "$WALLET_KEYPAIR_FILE" \
  --output "$WALLET_USDC_FILE" --amount-raw 1000000000 \
  >"$RUN_DIR/setup/local-wallet-account.json"
WALLET_USDC_ADDRESS=$(jq -r .address "$RUN_DIR/setup/local-wallet-account.json")

VALIDATOR_ARGS=()
mkdir -p "$VALIDATOR_PROGRAM_DIR"
bun "$ROOT_DIR/scripts/fleet-local-chain-e2e/fixture.ts" prepare-validator \
  "$FIXTURE_MANIFEST" "$VALIDATOR_PROGRAM_DIR" >"$RUN_DIR/setup/validator-programs.json"
while IFS= read -r argument; do VALIDATOR_ARGS+=("$argument"); done < <(
  jq -r '.args[]' "$RUN_DIR/setup/validator-programs.json"
)
echo "Starting disposable stateful validator at fixture slot $SOURCE_SLOT"
solana-test-validator --ledger "$LEDGER_DIR" --reset --quiet --bind-address 127.0.0.1 \
  --rpc-port "$VALIDATOR_RPC_PORT" --faucet-port "$((VALIDATOR_RPC_PORT + 2))" \
  --gossip-port "$((VALIDATOR_RPC_PORT + 3))" \
  --dynamic-port-range "$((VALIDATOR_RPC_PORT + 4))-$((VALIDATOR_RPC_PORT + 104))" \
  --warp-slot "$SOURCE_SLOT" "${VALIDATOR_ARGS[@]}" \
  --account "$WALLET_USDC_ADDRESS" "$WALLET_USDC_FILE" \
  >"$RUN_DIR/validator.log" 2>&1 &
VALIDATOR_PID=$!
ready=0
for _ in $(seq 1 200); do
  if curl -fsS -X POST -H 'content-type: application/json' \
    --data '{"jsonrpc":"2.0","id":1,"method":"getHealth"}' "$VALIDATOR_RPC_URL" >/dev/null 2>&1; then
    ready=1; break
  fi
  sleep 0.1
done
if ((!ready)); then echo "validator did not become ready" >&2; exit 1; fi
bun "$ROOT_DIR/scripts/fleet-local-chain-e2e/fixture.ts" verify-live \
  "$FIXTURE_MANIFEST" "$VALIDATOR_RPC_URL" >"$RUN_DIR/setup/fixture-live-verify.json"

bun "$ROOT_DIR/scripts/fleet-local-chain-e2e/rpc-proxy.ts" \
  --port "$PROXY_RPC_PORT" --upstream "$VALIDATOR_RPC_URL" \
  --latency-ms "$RPC_LATENCY_MS" --jitter-ms "$RPC_JITTER_MS" \
  --error-every "$RPC_ERROR_EVERY" \
  --log "$RUN_DIR/rpc-requests.jsonl" --summary "$RUN_DIR/rpc-summary.json" \
  >"$RUN_DIR/rpc-proxy.log" 2>&1 &
PROXY_PID=$!
ready=0
for _ in $(seq 1 100); do
  if curl -fsS "$PROXY_RPC_URL/health" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.1
done
if ((!ready)); then echo "RPC proxy did not become ready" >&2; exit 1; fi

echo "Starting disposable PostgreSQL"
"$PG_BIN/initdb" -D "$PG_DATA" --auth=trust --no-locale --encoding=UTF8 >/dev/null
"$PG_BIN/pg_ctl" -D "$PG_DATA" -l "$PG_LOG" \
  -o "-h 127.0.0.1 -p $DATABASE_PORT -c timezone=UTC -c max_connections=160 -c log_lock_waits=on -c deadlock_timeout=200ms" \
  start >/dev/null
DATABASE_STARTED=1
createdb -h 127.0.0.1 -p "$DATABASE_PORT" "$DATABASE_NAME"
NEON_DATABASE_URL="$DATABASE_URL" "$MIGRATIONS" --apply >"$RUN_DIR/setup/migrations.log"
PSQL=(psql -X -v ON_ERROR_STOP=1 "$DATABASE_URL")
"${PSQL[@]}" -f "$ROOT_DIR/scripts/fleet-local-load-lab/timescale-compat.sql" >/dev/null

"$CHAIN_SETUP" setup --rpc-url "$PROXY_RPC_URL" --wallet-keypair "$WALLET_KEYPAIR_FILE" \
  --policy-keypair "$POLICY_KEYPAIR_FILE" >"$RUN_DIR/setup/local-chain.json"
SETTINGS=$(jq -r .settings "$RUN_DIR/setup/local-chain.json")
VAULT_INDEX=$(jq -r .vaultIndex "$RUN_DIR/setup/local-chain.json")
VAULT=$(jq -r .vault "$RUN_DIR/setup/local-chain.json")
WALLET=$(jq -r .wallet "$RUN_DIR/setup/local-chain.json")
POLICY=$(jq -r .policy "$RUN_DIR/setup/local-chain.json")
ROUTE_POLICY_SEED=$(jq -r .routePolicySeed "$RUN_DIR/setup/local-chain.json")
ROUTE_POLICY=$(jq -r .routePolicy "$RUN_DIR/setup/local-chain.json")
POLICY_JSON=$(jq -c . "$POLICY_KEYPAIR_FILE")
WALLET_JSON=$(jq -c . "$WALLET_KEYPAIR_FILE")
MAIN_MARKET=$(jq -r .roots.mainMarket "$FIXTURE_MANIFEST")
PRIME_MARKET=$(jq -r .roots.primeMarket "$FIXTURE_MANIFEST")
MAIN_RESERVE=$(jq -r .roots.mainUsdcReserve "$FIXTURE_MANIFEST")
PRIME_RESERVE=$(jq -r .roots.primeUsdcReserve "$FIXTURE_MANIFEST")
USDC_MINT=$(jq -r .roots.usdcMint "$FIXTURE_MANIFEST")
OBSERVED_SLOT=$(solana slot --url "$VALIDATOR_RPC_URL")
MAIN_HASH=$(jq -r --arg address "$MAIN_RESERVE" '.accounts[] | select(.address == $address) | .dataSha256' "$FIXTURE_MANIFEST")
PRIME_HASH=$(jq -r --arg address "$PRIME_RESERVE" '.accounts[] | select(.address == $address) | .dataSha256' "$FIXTURE_MANIFEST")

"${PSQL[@]}" \
  -v settings="$SETTINGS" -v authority="$WALLET" -v policy="$POLICY" \
  -v route_policy_seed="$ROUTE_POLICY_SEED" -v route_policy="$ROUTE_POLICY" \
  -v vault_index="$VAULT_INDEX" -v vault="$VAULT" -v observed_slot="$OBSERVED_SLOT" \
  -v main_hash="$MAIN_HASH" -v prime_hash="$PRIME_HASH" \
  -v main_market="$MAIN_MARKET" -v prime_market="$PRIME_MARKET" \
  -v main_reserve="$MAIN_RESERVE" -v prime_reserve="$PRIME_RESERVE" \
  -v usdc_mint="$USDC_MINT" \
  -f "$ROOT_DIR/scripts/fleet-local-chain-e2e/seed.sql" >/dev/null

common_env() {
  env NEON_DATABASE_URL="$DATABASE_URL" TIMESCALEDB_URL="$DATABASE_URL" \
    SOLANA_RPC_URL="$PROXY_RPC_URL" SOLANA_WS_URL="$VALIDATOR_WS_URL" \
    YIELD_ALT_CLUSTER=localnet YIELD_ROUTE_CLUSTER=localnet \
    YIELD_ROUTE_POLICY_AUTHORITY="$POLICY" EARN_ROUTER_ENABLED_STABLE_MINTS="$USDC_MINT" \
    OBSERVABILITY_ENABLED=false RUST_LOG=warn "$@"
}
signer_env() {
  env NEON_DATABASE_URL="$DATABASE_URL" TIMESCALEDB_URL="$DATABASE_URL" \
    SOLANA_RPC_URL="$PROXY_RPC_URL" SOLANA_WS_URL="$VALIDATOR_WS_URL" \
    YIELD_ALT_CLUSTER=localnet YIELD_ROUTE_CLUSTER=localnet \
    YIELD_ROUTE_POLICY_AUTHORITY="$POLICY" EARN_ROUTER_ENABLED_STABLE_MINTS="$USDC_MINT" \
    POLICY_KEYPAIR="$POLICY_JSON" SOLANA_TESTING_PK="$WALLET_JSON" \
    OBSERVABILITY_ENABLED=false RUST_LOG=warn "$@"
}
policy_env() {
  env NEON_DATABASE_URL="$DATABASE_URL" TIMESCALEDB_URL="$DATABASE_URL" \
    SOLANA_RPC_URL="$PROXY_RPC_URL" SOLANA_WS_URL="$VALIDATOR_WS_URL" \
    YIELD_ALT_CLUSTER=localnet YIELD_ROUTE_CLUSTER=localnet \
    YIELD_ROUTE_POLICY_AUTHORITY="$POLICY" EARN_ROUTER_ENABLED_STABLE_MINTS="$USDC_MINT" \
    POLICY_KEYPAIR="$POLICY_JSON" \
    OBSERVABILITY_ENABLED=false RUST_LOG=warn "$@"
}

drive_alt() {
  local label=$1
  local log="$RUN_DIR/setup/alt-$label.jsonl"
  local completed=0
  for iteration in $(seq 1 100); do
    policy_env "$PROVISIONER" --cluster localnet --execute --max-lamports 1000000000 \
      --max-operations 8 --rate-limit-ms 0 --concurrency 1 >>"$log"
    local pending requested
    pending=$("${PSQL[@]}" -At -c "SELECT count(*) FROM loyal_yield.lookup_table_operations WHERE operation_state NOT IN ('complete','permanent_failure','cancelled')")
    requested=$("${PSQL[@]}" -At -c "SELECT count(*) FROM loyal_yield.lookup_table_provisioning_requests WHERE request_status IN ('requested','planning','queued','failed')")
    if test "$pending" = 0 && test "$requested" = 0; then
      completed=$((completed + 1))
      if ((completed >= 2)); then return 0; fi
    else
      completed=0
    fi
    sleep 0.5
  done
  echo "ALT convergence timed out for $label" >&2
  return 1
}

echo "Publishing and provisioning the exact shared clone catalog"
common_env "$PROVISIONER" --cluster localnet --bootstrap-families --policy-pubkey "$POLICY" \
  --catalog-version local-main-prime-v1 --largest-atomic-expansion "$CLONE_ACCOUNT_COUNT" \
  --admin-write --reason local-e2e-bootstrap --updated-by local-e2e \
  >"$RUN_DIR/setup/alt-bootstrap.json"
common_env "$CATALOG" --cluster localnet --rpc-url "$PROXY_RPC_URL" \
  --catalog-version local-main-prime-v1 --enabled-stable-mints "$USDC_MINT" \
  >"$RUN_DIR/setup/catalog-dry.json"
CATALOG_DRY="$RUN_DIR/setup/catalog-dry.json"
common_env "$CATALOG" --cluster localnet --rpc-url "$PROXY_RPC_URL" \
  --catalog-version local-main-prime-v1 --enabled-stable-mints "$USDC_MINT" \
  --admin-write --reason local-e2e-publish --updated-by local-e2e \
  --expected-desired-set-hash "$(jq -r .approvalFence.expectedDesiredSetHash "$CATALOG_DRY")" \
  --expected-enabled-mints-hash "$(jq -r .approvalFence.expectedEnabledMintsHash "$CATALOG_DRY")" \
  --expected-ordered-address-hash "$(jq -r .approvalFence.expectedOrderedAddressHash "$CATALOG_DRY")" \
  --expected-reserve-set-hash "$(jq -r .approvalFence.expectedReserveSetHash "$CATALOG_DRY")" \
  --expected-reserve-count "$(jq -r .approvalFence.expectedReserveCount "$CATALOG_DRY")" \
  --expected-address-count "$(jq -r .approvalFence.expectedAddressCount "$CATALOG_DRY")" \
  --expected-minimum-source-slot "$(jq -r .approvalFence.expectedMinimumSourceSlot "$CATALOG_DRY")" \
  >"$RUN_DIR/setup/catalog-write.json"
drive_alt shared

common_env "$PROVISIONER" --cluster localnet --set-provisioner-pause --admin-write \
  --reason local-e2e-cutover --updated-by local-e2e >"$RUN_DIR/setup/alt-pause.json"
common_env "$PROVISIONER" --cluster localnet --precutover-probe --probe-vault-id 1 \
  >"$RUN_DIR/setup/alt-precutover-probe.json"
common_env "$PROVISIONER" --cluster localnet --activate-reusable-only --admin-write \
  --reason local-e2e-cutover --updated-by local-e2e >"$RUN_DIR/setup/alt-activate.json"
common_env "$PROVISIONER" --cluster localnet --clear-provisioner-pause --admin-write \
  --reason local-e2e-resume --updated-by local-e2e >"$RUN_DIR/setup/alt-resume.json"

run_with_alt_demand() {
  local label=$1
  shift
  # Policy/deposit setup discovers vault-scoped addresses phase by phase. A
  # fresh validator can require several finalized create/extend rounds before
  # the next simulation reveals the following prerequisite.
  for attempt in $(seq 1 24); do
    if signer_env "$@" >"$RUN_DIR/setup/$label-$attempt.out" 2>"$RUN_DIR/setup/$label-$attempt.err"; then
      return 0
    fi
    local active_leases
    active_leases=$("${PSQL[@]}" -At -c "SELECT count(*) FROM loyal_yield.lookup_table_usage_leases WHERE released_at IS NULL AND expires_at > now()")
    if test "$active_leases" != 0; then
      echo "$label leaked $active_leases active ALT usage leases after a failed prerequisite" >&2
      return 1
    fi
    local permanent_failures
    permanent_failures=$("${PSQL[@]}" -At -c \
      "SELECT count(*) FROM loyal_yield.lookup_table_operations WHERE operation_state='permanent_failure'")
    if test "$permanent_failures" != 0; then
      echo "$label encountered $permanent_failures permanent ALT failures" >&2
      return 1
    fi
    drive_alt "$label-$attempt"
  done
  echo "$label did not converge after 24 bounded prerequisite-discovery rounds" >&2
  return 1
}

echo "Creating policies and an initial Main USDC position through normal paths"
run_with_alt_demand policy "$FLEET_WORKER" --settings "$SETTINGS" --vault-index "$VAULT_INDEX" \
  --cluster localnet --update-policy --update-active-policy --rpc-url "$PROXY_RPC_URL" --execute
run_with_alt_demand deposit "$FLEET_WORKER" --settings "$SETTINGS" --vault-index "$VAULT_INDEX" \
  --cluster localnet --deposit-main-usdc 500000000 --rpc-url "$PROXY_RPC_URL" --execute

snapshot_chain() {
  local name=$1
  local current_snapshot_slot validator_slot
  current_snapshot_slot=$("${PSQL[@]}" -At -c \
    "SELECT COALESCE(max(observed_slot), 0) FROM loyal_yield.vault_position_snapshots WHERE vault_id = 1")
  for _ in $(seq 1 100); do
    validator_slot=$(solana slot --url "$VALIDATOR_RPC_URL")
    if ((validator_slot > current_snapshot_slot)); then break; fi
    sleep 0.1
  done
  if ((validator_slot <= current_snapshot_slot)); then
    echo "validator slot did not advance beyond current snapshot slot $current_snapshot_slot" >&2
    return 1
  fi
  common_env "$FLEET_WORKER" --settings "$SETTINGS" --vault-index "$VAULT_INDEX" \
    --cluster localnet --source-reserve "$MAIN_RESERVE" --target-reserve "$PRIME_RESERVE" \
    --reconcile-current-positions --reconcile-from-chain \
    --reconcile-reserve "$MAIN_RESERVE" --reconcile-reserve "$PRIME_RESERVE" \
    --read-only --rpc-url "$PROXY_RPC_URL" \
    >"$RUN_DIR/$name.out" 2>"$RUN_DIR/$name.err"
}
snapshot_chain chain-before

echo "Refreshing simulated market verification immediately before planning"
"${PSQL[@]}" -c "
  UPDATE kamino.local_verified_reserve_updates
  SET observed_at = clock_timestamp(),
      verified_at = clock_timestamp(),
      market_price_last_updated_ts = extract(epoch FROM clock_timestamp())::BIGINT
  WHERE verification_source = 'local_fixture';
" >/dev/null
"${PSQL[@]}" -At -c "
  SELECT json_build_object(
    'source', 'continuously-refreshed-local-fixture',
    'refreshedAt', max(verified_at),
    'rowCount', count(*),
    'minimumPriceTimestamp', min(market_price_last_updated_ts)
  )
  FROM kamino.local_verified_reserve_updates
  WHERE verification_source = 'local_fixture'
" >"$RUN_DIR/simulated-market-input.json"

echo "Starting the fenced health projector"
common_env "$PROJECTOR" --cluster localnet --refresh-interval-seconds 5 \
  --lease-seconds 15 >"$RUN_DIR/workers/health-projector.jsonl" 2>&1 &
PROJECTOR_PID=$!
projector_ready=0
for _ in $(seq 1 100); do
  if grep -q '"status":"fleet_health_snapshot_refreshed"' \
    "$RUN_DIR/workers/health-projector.jsonl" 2>/dev/null; then
    projector_ready=1
    break
  fi
  sleep 0.1
done
if ((!projector_ready)); then
  echo "health projector did not publish a fresh snapshot" >&2
  exit 1
fi

echo "Publishing a real opportunity and driving all production fleet roles"
common_env "$PLANNER" --once --json --cluster localnet >"$RUN_DIR/workers/planner.json"
if ! jq -e '(.publishedCount // 0) > 0' "$RUN_DIR/workers/planner.json" >/dev/null; then
  echo "planner did not publish a localnet fleet opportunity" >&2
  jq '{status, publishedCount, opportunityCount, outcome, marketCoverage}' \
    "$RUN_DIR/workers/planner.json" >&2 || true
  exit 1
fi
common_env "$PLANNER" --json --cluster localnet --poll-interval-seconds 1 \
  --full-sweep-interval-seconds 30 \
  >"$RUN_DIR/workers/planner-daemon.jsonl" 2>&1 &
PLANNER_PID=$!
bun "$ROOT_DIR/scripts/fleet-local-load-lab/rpc-load.ts" --url "$PROXY_RPC_URL" \
  --duration-seconds "$RPC_LOAD_SECONDS" --concurrency "$RPC_LOAD_CLIENTS" \
  --summary "$RUN_DIR/load/rpc-load-summary.json" >"$RUN_DIR/load/rpc-load.log" 2>&1 &
RPC_LOAD_PID=$!

terminal=0
for cycle in $(seq 1 20); do
  policy_env "$FLEET_WORKER" --fleet-worker revalidate --cluster localnet \
    --rpc-url "$PROXY_RPC_URL" --concurrency 1 --fused-execute-concurrency 0 --once \
    >"$RUN_DIR/workers/revalidator-$cycle.jsonl" 2>&1 || true
  drive_alt "route-$cycle"
  # Satisfied ALT demand only dirties the vault. The long-running production
  # planner consumes that wakeup and owns economic re-admission.
  sleep 0.5
  policy_env "$FLEET_WORKER" --fleet-worker revalidate --cluster localnet \
    --rpc-url "$PROXY_RPC_URL" --concurrency 1 --fused-execute-concurrency 0 --once \
    >>"$RUN_DIR/workers/revalidator-$cycle.jsonl" 2>&1 || true
  policy_env "$FLEET_WORKER" --fleet-worker execute --cluster localnet \
    --rpc-url "$PROXY_RPC_URL" --concurrency 1 --once \
    >"$RUN_DIR/workers/executor-$cycle.jsonl" 2>&1 || true
  common_env "$CONFIRMER" --execute --once --cluster localnet --rpc-url "$PROXY_RPC_URL" \
    --ws-url "$VALIDATOR_WS_URL" --batch-size 8 --broadcast-concurrency 1 \
    --poll-interval-milliseconds 250 >"$RUN_DIR/workers/confirmer-$cycle.jsonl" 2>&1 || true
  common_env "$FLEET_WORKER" --fleet-reconciler --cluster localnet --rpc-url "$PROXY_RPC_URL" \
    --concurrency 1 --batch-size 8 --poll-interval-milliseconds 250 \
    --position-sweep-interval-seconds 3600 --once \
    >"$RUN_DIR/workers/reconciler-$cycle.jsonl" 2>&1 || true
  terminal=$("${PSQL[@]}" -At -c "SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE cluster='localnet' AND submission_state='reconciled'")
  if test "$terminal" -gt 0; then break; fi
  sleep 0.5
done
stop_pid "$PLANNER_PID"
PLANNER_PID=""
wait "$RPC_LOAD_PID" || true
RPC_LOAD_PID=""
if test "$terminal" = 0; then
  echo "fleet pipeline did not reach a reconciled submission" >&2
  exit 1
fi
snapshot_chain chain-after

"${PSQL[@]}" -At -f "$ROOT_DIR/scripts/fleet-local-chain-e2e/collect-evidence.sql" \
  >"$RUN_DIR/database-before-rerun.json"
echo "Re-running every role to prove terminal exactly-once behavior"
common_env "$PLANNER" --once --json --cluster localnet >"$RUN_DIR/workers/planner-rerun.json" 2>&1
policy_env "$FLEET_WORKER" --fleet-worker revalidate --cluster localnet \
  --rpc-url "$PROXY_RPC_URL" --concurrency 1 --fused-execute-concurrency 0 --once \
  >"$RUN_DIR/workers/revalidator-rerun.jsonl" 2>&1
policy_env "$FLEET_WORKER" --fleet-worker execute --cluster localnet \
  --rpc-url "$PROXY_RPC_URL" --concurrency 1 --once \
  >"$RUN_DIR/workers/executor-rerun.jsonl" 2>&1
common_env "$CONFIRMER" --execute --once --cluster localnet --rpc-url "$PROXY_RPC_URL" \
  --ws-url "$VALIDATOR_WS_URL" --batch-size 8 --broadcast-concurrency 1 \
  --poll-interval-milliseconds 250 >"$RUN_DIR/workers/confirmer-rerun.jsonl" 2>&1
common_env "$FLEET_WORKER" --fleet-reconciler --cluster localnet --rpc-url "$PROXY_RPC_URL" \
  --concurrency 1 --batch-size 8 --poll-interval-milliseconds 250 \
  --position-sweep-interval-seconds 3600 --once \
  >"$RUN_DIR/workers/reconciler-rerun.jsonl" 2>&1
snapshot_chain chain-after-rerun
stop_pid "$PROJECTOR_PID"
PROJECTOR_PID=""
"${PSQL[@]}" -At -f "$ROOT_DIR/scripts/fleet-local-chain-e2e/collect-evidence.sql" \
  >"$RUN_DIR/database-after-rerun.json"
stop_pid "$PROXY_PID"
PROXY_PID=""
bun "$ROOT_DIR/scripts/verify-fleet-local-chain-e2e.ts" assemble "$RUN_DIR" \
  --started-at "$RUN_STARTED_AT_UTC" --settings "$SETTINGS" --vault "$VAULT" \
  --policy "$POLICY" --main-reserve "$MAIN_RESERVE" --prime-reserve "$PRIME_RESERVE" \
  --clone-accounts "$CLONE_ACCOUNT_COUNT" --source-slot "$SOURCE_SLOT"
bun "$ROOT_DIR/scripts/verify-fleet-local-chain-e2e.ts" verify "$RUN_DIR/evidence.json"
echo "FULL_CHAIN_E2E: PASS - $RUN_DIR/evidence.md"
