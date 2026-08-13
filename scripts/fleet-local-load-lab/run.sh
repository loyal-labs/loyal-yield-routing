#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
OPPORTUNITIES=10000
DURATION_SECONDS=15
HEALTH_CLIENTS=3
DATABASE_PORT=55441
RPC_PORT=18899
RPC_LATENCY_MS=25
RPC_JITTER_MS=10
RPC_ERROR_EVERY=0
RPC_CLIENTS=16
OUTPUT_DIR="$ROOT_DIR/artifacts/fleet-local-load-lab"
KEEP_DATABASE=0
SKIP_BUILD=0

usage() {
  command cat <<'USAGE'
Usage: bun run fleet:local-load-lab -- [options]

Options:
  --opportunities N       Historical opportunity rows (default: 10000)
  --duration-seconds N    Concurrent load duration (default: 15)
  --health-clients N      Exact health-view clients (default: 3)
  --database-port N       Loopback PostgreSQL port (default: 55441)
  --rpc-port N            Loopback RPC emulator port (default: 18899)
  --rpc-latency-ms N      Base JSON-RPC latency (default: 25)
  --rpc-jitter-ms N       Deterministic added jitter (default: 10)
  --rpc-error-every N     Fail every Nth RPC request; 0 disables (default: 0)
  --rpc-clients N         Concurrent synthetic RPC clients (default: 16)
  --output DIR            Evidence root (default: artifacts/fleet-local-load-lab)
  --skip-build            Reuse already-built debug binaries
  --keep-database         Retain the temporary PostgreSQL directory
  --help                  Show this message

This is an isolated component load lab, not chain E2E. It refuses non-loopback
database/RPC targets, removes inherited production configuration, loads no key,
and attributes synthetic SQL/RPC separately from real process activity.
USAGE
}

while (($#)); do
  case "$1" in
    --opportunities) OPPORTUNITIES=${2:?missing value}; shift 2 ;;
    --duration-seconds) DURATION_SECONDS=${2:?missing value}; shift 2 ;;
    --health-clients) HEALTH_CLIENTS=${2:?missing value}; shift 2 ;;
    --database-port) DATABASE_PORT=${2:?missing value}; shift 2 ;;
    --rpc-port) RPC_PORT=${2:?missing value}; shift 2 ;;
    --rpc-latency-ms) RPC_LATENCY_MS=${2:?missing value}; shift 2 ;;
    --rpc-jitter-ms) RPC_JITTER_MS=${2:?missing value}; shift 2 ;;
    --rpc-error-every) RPC_ERROR_EVERY=${2:?missing value}; shift 2 ;;
    --rpc-clients) RPC_CLIENTS=${2:?missing value}; shift 2 ;;
    --output) OUTPUT_DIR=${2:?missing value}; shift 2 ;;
    --skip-build) SKIP_BUILD=1; shift ;;
    --keep-database) KEEP_DATABASE=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

for value in "$OPPORTUNITIES" "$DURATION_SECONDS" "$HEALTH_CLIENTS" \
  "$DATABASE_PORT" "$RPC_PORT" "$RPC_LATENCY_MS" "$RPC_JITTER_MS" \
  "$RPC_ERROR_EVERY" "$RPC_CLIENTS"; do
  case "$value" in *[!0-9]*|"") echo "numeric options require nonnegative integers" >&2; exit 2 ;; esac
done
if ((OPPORTUNITIES < 100 || DURATION_SECONDS < 1 || HEALTH_CLIENTS < 1)); then
  echo "opportunities must be >=100 and duration/clients must be positive" >&2
  exit 2
fi
if ((RPC_CLIENTS < 1 || RPC_CLIENTS > 256)); then
  echo "RPC clients must be in 1..256" >&2
  exit 2
fi
if ((DATABASE_PORT < 1024 || DATABASE_PORT > 65535 || RPC_PORT < 1024 || RPC_PORT > 65535)); then
  echo "ports must be in 1024..65535" >&2
  exit 2
fi
if ((DATABASE_PORT == RPC_PORT)); then
  echo "database and RPC ports must differ" >&2
  exit 2
fi

for tool in cargo bun curl pg_config psql pgbench createdb ps; do
  command -v "$tool" >/dev/null || { echo "required tool is missing: $tool" >&2; exit 1; }
done
PG_BIN=$(pg_config --bindir)
for tool in initdb pg_ctl; do
  test -x "$PG_BIN/$tool" || { echo "required PostgreSQL tool is missing: $PG_BIN/$tool" >&2; exit 1; }
done

unset DATABASE_URL NEON_DATABASE_URL TIMESCALEDB_URL SOLANA_RPC_URL SOLANA_WS_URL RPC_URL
unset HELIUS_RPC_URL HELIUS_API_KEY HYPERDX_ACCESS_KEY OBSERVABILITY_INGESTION_API_KEY
unset POLICY_KEYPAIR YIELD_ROUTE_FEE_PAYER_KEYPAIRS SOLANA_TESTING_PK YIELD_ROUTER_KEYPAIR

RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
RUN_STARTED_AT_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)
OUTPUT_DIR=$(mkdir -p "$OUTPUT_DIR" && cd "$OUTPUT_DIR" && pwd)
RUN_DIR="$OUTPUT_DIR/$RUN_ID"
mkdir -p "$RUN_DIR/workers" "$RUN_DIR/workloads"

TEMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/loyal-fleet-load-lab.XXXXXX")
PG_DATA="$TEMP_ROOT/postgres"
PG_LOG="$TEMP_ROOT/postgres.log"
DATABASE_NAME="fleet_e2e_${RUN_ID//[^0-9]/}"
DATABASE_URL="postgresql://127.0.0.1:${DATABASE_PORT}/${DATABASE_NAME}"
RPC_URL="http://127.0.0.1:${RPC_PORT}"
LOCAL_POLICY_AUTHORITY="62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"
CARGO_TARGET_DIR=${CARGO_TARGET_DIR:-"$ROOT_DIR/target/fleet-local-load-lab"}
DATABASE_STARTED=0
RPC_PID=""
RPC_LOAD_PID=""
SAMPLER_PID=""
WORKER_PIDS=()
WORKER_ROLES=()

stop_pid() {
  local pid=${1:-}
  if test -n "$pid" && kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
}

cleanup() {
  local exit_code=$?
  test -e "$TEMP_ROOT/sampling" && rm -f "$TEMP_ROOT/sampling"
  stop_pid "$SAMPLER_PID"
  for pid in "${WORKER_PIDS[@]:-}"; do stop_pid "$pid"; done
  stop_pid "$RPC_LOAD_PID"
  stop_pid "$RPC_PID"
  if ((DATABASE_STARTED)); then
    "$PG_BIN/pg_ctl" -D "$PG_DATA" -m fast stop >/dev/null 2>&1 || true
  fi
  cp "$PG_LOG" "$RUN_DIR/postgres.log" 2>/dev/null || true
  if ((KEEP_DATABASE)); then
    echo "temporary database retained at $TEMP_ROOT"
  else
    case "$TEMP_ROOT" in
      "${TMPDIR:-/tmp}"/loyal-fleet-load-lab.*) rm -rf -- "$TEMP_ROOT" ;;
      *) echo "refusing to remove unexpected temporary path: $TEMP_ROOT" >&2 ;;
    esac
  fi
  exit "$exit_code"
}
trap cleanup EXIT INT TERM

case "$DATABASE_URL" in
  postgresql://127.0.0.1:*"/fleet_e2e_"*) ;;
  *) echo "refusing database URL outside loopback fleet_e2e_*" >&2; exit 1 ;;
esac
case "$RPC_URL" in
  http://127.0.0.1:*) ;;
  *) echo "refusing RPC URL outside loopback" >&2; exit 1 ;;
esac

echo "Initializing isolated PostgreSQL at $TEMP_ROOT"
"$PG_BIN/initdb" -D "$PG_DATA" --auth=trust --no-locale --encoding=UTF8 >/dev/null
"$PG_BIN/pg_ctl" -D "$PG_DATA" -l "$PG_LOG" \
  -o "-h 127.0.0.1 -p $DATABASE_PORT -c timezone=UTC -c max_connections=220 -c log_lock_waits=on -c deadlock_timeout=200ms" \
  start >/dev/null
DATABASE_STARTED=1
createdb -h 127.0.0.1 -p "$DATABASE_PORT" "$DATABASE_NAME"

if ((!SKIP_BUILD)); then
  echo "Building the exact worker and migration binaries"
  CARGO_TARGET_DIR="$CARGO_TARGET_DIR" cargo build -q \
    -p loyal-yield-orchestrator \
    --bin yield-migrations \
    --bin fleet-opportunity-planner \
    --bin fleet-health-projector \
    --bin fleet-route-confirmer
  CARGO_TARGET_DIR="$CARGO_TARGET_DIR" cargo build -q \
    -p loyal-fleet-worker --bin same-mint-reserve-swap
fi

MIGRATIONS="$CARGO_TARGET_DIR/debug/yield-migrations"
PLANNER="$CARGO_TARGET_DIR/debug/fleet-opportunity-planner"
PROJECTOR="$CARGO_TARGET_DIR/debug/fleet-health-projector"
CONFIRMER="$CARGO_TARGET_DIR/debug/fleet-route-confirmer"
FLEET_WORKER="$CARGO_TARGET_DIR/debug/same-mint-reserve-swap"
for binary in "$MIGRATIONS" "$PLANNER" "$PROJECTOR" "$CONFIRMER" "$FLEET_WORKER"; do
  test -x "$binary" || { echo "missing binary: $binary" >&2; exit 1; }
done

echo "Applying real Yield migrations and local market compatibility schema"
NEON_DATABASE_URL="$DATABASE_URL" "$MIGRATIONS" --apply >"$RUN_DIR/migrations.log"
PSQL=(psql -X -v ON_ERROR_STOP=1 "$DATABASE_URL")
"${PSQL[@]}" -f "$ROOT_DIR/scripts/fleet-local-load-lab/timescale-compat.sql" >/dev/null
"${PSQL[@]}" -f "$ROOT_DIR/scripts/fleet-db-load/seed-base.sql" >/dev/null
"${PSQL[@]}" -v target_rows="$OPPORTUNITIES" -v cluster="localnet" \
  -f "$ROOT_DIR/scripts/fleet-db-load/seed-scale.sql" >/dev/null
"${PSQL[@]}" -f "$ROOT_DIR/scripts/fleet-local-load-lab/quiesce-real-worker-queues.sql" >/dev/null
"${PSQL[@]}" -c "VACUUM (ANALYZE) loyal_yield.rebalance_opportunities" \
  -c "VACUUM (ANALYZE) loyal_yield.signed_route_submissions" >/dev/null

echo "Proving durable dirty-row merging and edge-only notification"
"${PSQL[@]}" -c "DELETE FROM loyal_yield.fleet_planning_dirty_vaults WHERE cluster='localnet' AND vault_id=1" >/dev/null
(
  printf '%s\n' "LISTEN loyal_yield_fleet_planner_wakeup;" "SELECT pg_sleep(2);"
) | "${PSQL[@]}" >"$RUN_DIR/planner-notifications.log" &
NOTIFY_LISTENER_PID=$!
sleep 0.25
"${PSQL[@]}" -c "SELECT loyal_yield.enqueue_fleet_planning_dirty_vault(1, 'first', 10, now() + interval '2 seconds', 'localnet')" >/dev/null
"${PSQL[@]}" -c "SELECT loyal_yield.enqueue_fleet_planning_dirty_vault(1, 'second', 11, now() + interval '1 second', 'localnet')" >/dev/null
wait "$NOTIFY_LISTENER_PID"
"${PSQL[@]}" -At -c "
  SELECT json_build_object(
    'notificationCount', $(grep -c '^Asynchronous notification' "$RUN_DIR/planner-notifications.log" || true),
    'generation', generation,
    'reasons', reasons,
    'maximumObservedSlot', maximum_observed_slot,
    'availabilityMerged', available_at <= first_dirty_at + interval '2 seconds'
  )
  FROM loyal_yield.fleet_planning_dirty_vaults
  WHERE cluster='localnet' AND vault_id=1
" >"$RUN_DIR/planner-coalescing.json"

echo "Starting loopback Solana JSON-RPC emulator"
bun "$ROOT_DIR/scripts/fleet-local-load-lab/rpc-emulator.ts" \
  --port "$RPC_PORT" \
  --latency-ms "$RPC_LATENCY_MS" \
  --jitter-ms "$RPC_JITTER_MS" \
  --error-every "$RPC_ERROR_EVERY" \
  --summary "$RUN_DIR/rpc-summary.json" \
  --log "$RUN_DIR/rpc-requests.jsonl" \
  >"$RUN_DIR/rpc-emulator.log" 2>&1 &
RPC_PID=$!
rpc_ready=0
for _ in $(seq 1 100); do
  if curl -fsS "$RPC_URL/health" >/dev/null 2>&1; then rpc_ready=1; break; fi
  sleep 0.1
done
if ((!rpc_ready)); then echo "RPC emulator did not become ready" >&2; exit 1; fi

echo "Measuring the real deterministic planner algorithm"
"$PLANNER" --once --dry-run --benchmark --json \
  --count "$OPPORTUNITIES" --rounds 5 >"$RUN_DIR/planner-benchmark.json"

echo "Measuring direct history aggregation and publishing one fenced snapshot"
pgbench -n -M prepared -c 1 -j 1 -t 1 -l \
  --log-prefix="$RUN_DIR/workloads/source-health" \
  -f "$ROOT_DIR/scripts/fleet-local-load-lab/workloads/health-source.sql" \
  "$DATABASE_URL" >"$RUN_DIR/workloads/source-health.stdout"
env NEON_DATABASE_URL="$DATABASE_URL" YIELD_ROUTE_CLUSTER=localnet \
  OBSERVABILITY_ENABLED=false RUST_LOG=warn \
  "$PROJECTOR" --once --cluster localnet --refresh-interval-seconds 5 --lease-seconds 15 \
  >"$RUN_DIR/workers/health-projector.log" 2>&1
env NEON_DATABASE_URL="$DATABASE_URL" YIELD_ROUTE_CLUSTER=localnet \
  OBSERVABILITY_ENABLED=false RUST_LOG=warn \
  "$PROJECTOR" --once --cluster localnet --refresh-interval-seconds 5 --lease-seconds 15 \
  >"$RUN_DIR/workers/health-projector-contender.log" 2>&1
"${PSQL[@]}" -c "SELECT pg_stat_reset()" >/dev/null
pgbench -n -M prepared -c 10 -j 10 -t 10 \
  -f "$ROOT_DIR/scripts/fleet-db-load/workloads/health.sql" \
  "$DATABASE_URL" >"$RUN_DIR/workloads/health-hot-path.stdout"
"${PSQL[@]}" -At -c "
  SELECT json_build_object(
    'transactions', 100,
    'tempFiles', temp_files,
    'tempBytes', temp_bytes
  )
  FROM pg_stat_database
  WHERE datname = current_database()
" >"$RUN_DIR/health-hot-path.json"
"${PSQL[@]}" -c "SELECT pg_stat_reset()" >/dev/null

start_worker() {
  local role=$1
  shift
  echo "Starting real fleet role: $role"
  env \
    NEON_DATABASE_URL="$DATABASE_URL" \
    TIMESCALEDB_URL="$DATABASE_URL" \
    SOLANA_RPC_URL="$RPC_URL" \
    SOLANA_WS_URL="ws://127.0.0.1:$RPC_PORT" \
    YIELD_ALT_CLUSTER=localnet \
    YIELD_ROUTE_CLUSTER=localnet \
    YIELD_ROUTE_POLICY_AUTHORITY="$LOCAL_POLICY_AUTHORITY" \
    OBSERVABILITY_ENABLED=false \
    RUST_LOG=warn \
    "$@" >"$RUN_DIR/workers/$role.log" 2>&1 &
  WORKER_PIDS+=("$!")
  WORKER_ROLES+=("$role")
}

start_worker planner "$PLANNER" \
  --json \
  --poll-interval-seconds 1 --full-sweep-interval-seconds 30 \
  --dirty-batch-size 256 --max-opportunities-per-wave 128
"$FLEET_WORKER" --fleet-worker revalidate --role-probe >"$RUN_DIR/workers/revalidator.log"
"$FLEET_WORKER" --fleet-worker execute --role-probe >"$RUN_DIR/workers/executor.log"
start_worker confirmer "$CONFIRMER" --execute --cluster localnet \
  --rpc-url "$RPC_URL" --ws-url "ws://127.0.0.1:$RPC_PORT" \
  --batch-size 128 --broadcast-concurrency 16 --poll-interval-milliseconds 1000
start_worker reconciler "$FLEET_WORKER" --fleet-reconciler \
  --concurrency 64 --batch-size 32 --poll-interval-milliseconds 250 \
  --position-sweep-interval-seconds 3600 --cluster localnet --rpc-url "$RPC_URL"

printf 'captured_at_utc,role,pid,cpu_percent,rss_kib,elapsed\n' >"$RUN_DIR/process-samples.csv"
touch "$TEMP_ROOT/sampling"
sample_processes() {
  while test -e "$TEMP_ROOT/sampling"; do
    local now
    now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    for index in "${!WORKER_PIDS[@]}"; do
      local pid=${WORKER_PIDS[$index]}
      local role=${WORKER_ROLES[$index]}
      if kill -0 "$pid" 2>/dev/null; then
        ps -p "$pid" -o %cpu=,rss=,etime= | awk -v now="$now" -v role="$role" -v pid="$pid" \
          '{gsub(/^ +| +$/, ""); printf "%s,%s,%s,%s,%s,%s\n", now, role, pid, $1, $2, $3}' \
          >>"$RUN_DIR/process-samples.csv"
      fi
    done
    sleep 0.5
  done
}
sample_processes &
SAMPLER_PID=$!

"${PSQL[@]}" -At -f "$ROOT_DIR/scripts/fleet-local-load-lab/collect-metrics.sql" \
  >"$RUN_DIR/database-before.json"
pgbench -n -M prepared -c 1 -j 1 -t 5 -l \
  --log-prefix="$RUN_DIR/workloads/baseline-health" \
  -f "$ROOT_DIR/scripts/fleet-db-load/workloads/health.sql" \
  "$DATABASE_URL" >"$RUN_DIR/workloads/baseline-health.stdout"

echo "Running ${DURATION_SECONDS}s of concurrent worker-shaped load"
bun "$ROOT_DIR/scripts/fleet-local-load-lab/rpc-load.ts" \
  --url "$RPC_URL" --duration-seconds "$DURATION_SECONDS" \
  --concurrency "$RPC_CLIENTS" --summary "$RUN_DIR/rpc-load-summary.json" \
  >"$RUN_DIR/rpc-load.log" 2>&1 &
RPC_LOAD_PID=$!
submission_rows=$((OPPORTUNITIES / 4))
((submission_rows < 1)) && submission_rows=1
LOAD_PIDS=()
roles=(health executor confirmer reconciler planner user mock-chain)
clients=("$HEALTH_CLIENTS" 1 1 1 1 2 1)
scripts=(
  "$ROOT_DIR/scripts/fleet-db-load/workloads/health.sql"
  "$ROOT_DIR/scripts/fleet-local-load-lab/workloads/executor-pressure.sql"
  "$ROOT_DIR/scripts/fleet-db-load/workloads/confirmer.sql"
  "$ROOT_DIR/scripts/fleet-db-load/workloads/reconciler.sql"
  "$ROOT_DIR/scripts/fleet-db-load/workloads/planner.sql"
  "$ROOT_DIR/scripts/fleet-db-load/workloads/user.sql"
  "$ROOT_DIR/scripts/fleet-db-load/workloads/mock-chain.sql"
)
for index in "${!roles[@]}"; do
  role=${roles[$index]}
  if test "$role" = health; then
    # Production workers emit health every ten seconds. One request per client
    # per second is intentionally 10x harsher while keeping the model bounded.
    pgbench -n -M prepared -c "${clients[$index]}" -j "${clients[$index]}" \
      -T "$DURATION_SECONDS" -R "$HEALTH_CLIENTS" \
      -D opportunity_rows="$OPPORTUNITIES" -D submission_rows="$submission_rows" \
      -l --log-prefix="$RUN_DIR/workloads/$role" \
      -f "${scripts[$index]}" "$DATABASE_URL" \
      >"$RUN_DIR/workloads/$role.stdout" 2>"$RUN_DIR/workloads/$role.stderr" &
  else
    pgbench -n -M prepared -c "${clients[$index]}" -j "${clients[$index]}" \
      -T "$DURATION_SECONDS" \
      -D opportunity_rows="$OPPORTUNITIES" -D submission_rows="$submission_rows" \
      -l --log-prefix="$RUN_DIR/workloads/$role" \
      -f "${scripts[$index]}" "$DATABASE_URL" \
      >"$RUN_DIR/workloads/$role.stdout" 2>"$RUN_DIR/workloads/$role.stderr" &
  fi
  LOAD_PIDS+=("$!")
done
load_failed=0
for pid in "${LOAD_PIDS[@]}"; do if ! wait "$pid"; then load_failed=1; fi; done
if ((load_failed)); then echo "one or more pgbench workloads failed" >&2; exit 1; fi
if ! wait "$RPC_LOAD_PID"; then echo "RPC load generator failed" >&2; exit 1; fi
RPC_LOAD_PID=""

printf 'role,pid,alive_before_shutdown\n' >"$RUN_DIR/process-status.csv"
for index in "${!WORKER_PIDS[@]}"; do
  pid=${WORKER_PIDS[$index]}
  role=${WORKER_ROLES[$index]}
  alive=false
  kill -0 "$pid" 2>/dev/null && alive=true
  printf '%s,%s,%s\n' "$role" "$pid" "$alive" >>"$RUN_DIR/process-status.csv"
done

rm -f "$TEMP_ROOT/sampling"
stop_pid "$SAMPLER_PID"
SAMPLER_PID=""
"${PSQL[@]}" -At -f "$ROOT_DIR/scripts/fleet-local-load-lab/collect-metrics.sql" \
  >"$RUN_DIR/database-after.json"
curl -fsS "$RPC_URL/metrics" >"$RUN_DIR/rpc-summary.json"

for pid in "${WORKER_PIDS[@]}"; do stop_pid "$pid"; done
WORKER_PIDS=()
stop_pid "$RPC_PID"
RPC_PID=""

printf '{"runId":"%s","startedAtUtc":"%s","opportunities":%s,"durationSeconds":%s,"healthClients":%s,"healthRequestsPerClientPerSecond":1,"databaseHost":"127.0.0.1","databaseNameGuard":"fleet_e2e_*","rpcHost":"127.0.0.1","rpcLatencyMs":%s,"rpcJitterMs":%s,"rpcErrorEvery":%s,"rpcClients":%s,"gitCommit":"%s","host":"%s"}\n' \
  "$RUN_ID" "$RUN_STARTED_AT_UTC" "$OPPORTUNITIES" "$DURATION_SECONDS" "$HEALTH_CLIENTS" \
  "$RPC_LATENCY_MS" "$RPC_JITTER_MS" "$RPC_ERROR_EVERY" "$RPC_CLIENTS" \
  "$(git -C "$ROOT_DIR" rev-parse HEAD)" "$(uname -sm)" >"$RUN_DIR/run-config.json"

bun "$ROOT_DIR/scripts/fleet-local-load-lab/report.ts" "$RUN_DIR"
echo "Evidence: $RUN_DIR/evidence.md"
