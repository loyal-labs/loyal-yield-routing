#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SCALES="10000,100000,1000000"
DURATION_SECONDS=10
HEALTH_CLIENTS=3
DATABASE_PORT=55439
OUTPUT_DIR="$ROOT_DIR/artifacts/fleet-db-load"
KEEP_DATABASE=0

usage() {
  command cat <<'USAGE'
Usage: bun run fleet:reproduce-db-load -- [options]

Options:
  --scales CSV             Opportunity row counts (default: 10000,100000,1000000)
  --duration-seconds N     Concurrent workload duration per scale (default: 10)
  --health-clients N       Concurrent production health-query clients (default: 3)
  --port N                 Loopback PostgreSQL port (default: 55439)
  --output DIR             Evidence directory (default: artifacts/fleet-db-load)
  --keep-database          Keep the temporary database directory after the run
  --help                   Show this message

The harness always creates its own PostgreSQL cluster and refuses non-loopback
connections. It unsets production database and Solana environment variables.
USAGE
}

while (($#)); do
  case "$1" in
    --scales)
      SCALES=${2:?missing value for --scales}
      shift 2
      ;;
    --duration-seconds)
      DURATION_SECONDS=${2:?missing value for --duration-seconds}
      shift 2
      ;;
    --health-clients)
      HEALTH_CLIENTS=${2:?missing value for --health-clients}
      shift 2
      ;;
    --port)
      DATABASE_PORT=${2:?missing value for --port}
      shift 2
      ;;
    --output)
      OUTPUT_DIR=${2:?missing value for --output}
      shift 2
      ;;
    --keep-database)
      KEEP_DATABASE=1
      shift
      ;;
    --help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

case "$SCALES" in
  *[!0-9,]*|"")
    echo "--scales must be a comma-separated list of positive integers" >&2
    exit 2
    ;;
esac
if ((DURATION_SECONDS < 1 || HEALTH_CLIENTS < 1 || DATABASE_PORT < 1024 || DATABASE_PORT > 65535)); then
  echo "duration, clients, or port is outside the supported range" >&2
  exit 2
fi

for tool in cargo bun pg_config psql pgbench createdb shasum; do
  command -v "$tool" >/dev/null || {
    echo "required tool is missing: $tool" >&2
    exit 1
  }
done

PG_BIN=$(pg_config --bindir)
for tool in initdb pg_ctl; do
  test -x "$PG_BIN/$tool" || {
    echo "required PostgreSQL tool is missing: $PG_BIN/$tool" >&2
    exit 1
  }
done

unset DATABASE_URL NEON_DATABASE_URL TIMESCALEDB_URL SOLANA_RPC_URL RPC_URL
unset HELIUS_RPC_URL HELIUS_API_KEY HYPERDX_ACCESS_KEY

RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
OUTPUT_DIR=$(mkdir -p "$OUTPUT_DIR" && cd "$OUTPUT_DIR" && pwd)
RUN_DIR="$OUTPUT_DIR/$RUN_ID"
mkdir -p "$RUN_DIR"

TEMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/loyal-fleet-load.XXXXXX")
PG_DATA="$TEMP_ROOT/postgres"
PG_LOG="$TEMP_ROOT/postgres.log"
DATABASE_NAME="fleet_verify_load_${RUN_ID//[^0-9]/}"
DATABASE_URL="postgresql://127.0.0.1:${DATABASE_PORT}/${DATABASE_NAME}"
DATABASE_STARTED=0

cleanup() {
  local exit_code=$?
  if ((DATABASE_STARTED)); then
    "$PG_BIN/pg_ctl" -D "$PG_DATA" -m fast stop >/dev/null 2>&1 || true
  fi
  cp "$PG_LOG" "$RUN_DIR/postgres.log" 2>/dev/null || true
  if ((KEEP_DATABASE)); then
    echo "temporary database retained at $TEMP_ROOT"
  else
    rm -rf -- "$TEMP_ROOT"
  fi
  exit "$exit_code"
}
trap cleanup EXIT INT TERM

echo "Initializing isolated PostgreSQL at $TEMP_ROOT"
"$PG_BIN/initdb" -D "$PG_DATA" --auth=trust --no-locale --encoding=UTF8 >/dev/null
"$PG_BIN/pg_ctl" \
  -D "$PG_DATA" \
  -l "$PG_LOG" \
  -o "-h 127.0.0.1 -p $DATABASE_PORT -c max_connections=40" \
  start >/dev/null
DATABASE_STARTED=1

createdb -h 127.0.0.1 -p "$DATABASE_PORT" "$DATABASE_NAME"

case "$DATABASE_URL" in
  postgresql://127.0.0.1:*"/fleet_verify_"*) ;;
  *)
    echo "refusing database URL outside the isolated fleet_verify loopback database" >&2
    exit 1
    ;;
esac

echo "Applying the repository's real loyal_yield migrations"
(
  cd "$ROOT_DIR"
  NEON_DATABASE_URL="$DATABASE_URL" \
    cargo run -q -p loyal-yield-orchestrator --bin yield-migrations -- --apply
)

PSQL=(psql -X -v ON_ERROR_STOP=1 "$DATABASE_URL")
"${PSQL[@]}" -f "$ROOT_DIR/scripts/fleet-db-load/seed-base.sql" >/dev/null

GIT_DIRTY=false
if test -n "$(git -C "$ROOT_DIR" status --porcelain --untracked-files=all)"; then
  GIT_DIRTY=true
fi
HARNESS_SHA256=$(
  find "$ROOT_DIR/scripts/fleet-db-load" -type f -print |
    LC_ALL=C sort |
    while IFS= read -r file; do shasum -a 256 "$file"; done |
    shasum -a 256 |
    awk '{print $1}'
)

cat >"$RUN_DIR/run-config.json" <<JSON
{
  "runId": "$RUN_ID",
  "scales": "$SCALES",
  "durationSeconds": $DURATION_SECONDS,
  "healthClients": $HEALTH_CLIENTS,
  "database": "ephemeral $(psql --version)",
  "databaseHost": "127.0.0.1",
  "databaseNameGuard": "fleet_verify_*",
  "blockchain": "synthetic local snapshot updates; no RPC client",
  "gitCommit": "$(git -C "$ROOT_DIR" rev-parse HEAD)",
  "gitDirty": $GIT_DIRTY,
  "harnessSha256": "$HARNESS_SHA256",
  "host": "$(uname -sm)"
}
JSON

IFS=',' read -r -a SCALE_VALUES <<<"$SCALES"
previous_scale=0
for scale in "${SCALE_VALUES[@]}"; do
  if ((scale <= 0 || scale < previous_scale)); then
    echo "scales must be positive and monotonically increasing" >&2
    exit 2
  fi
  previous_scale=$scale
  SCENARIO_DIR="$RUN_DIR/scale-$scale"
  mkdir -p "$SCENARIO_DIR"

  echo "Seeding real queue tables to $scale opportunities"
  "${PSQL[@]}" \
    -v target_rows="$scale" \
    -v cluster="localnet" \
    -f "$ROOT_DIR/scripts/fleet-db-load/seed-scale.sql" >/dev/null
  "${PSQL[@]}" -c "VACUUM (ANALYZE) loyal_yield.rebalance_opportunities" \
    -c "VACUUM (ANALYZE) loyal_yield.orchestration_outbox" \
    -c "VACUUM (ANALYZE) loyal_yield.signed_route_submissions" >/dev/null

  "${PSQL[@]}" -At \
    -v target_rows="$scale" \
    -v cluster="localnet" \
    -f "$ROOT_DIR/scripts/fleet-db-load/collect-metrics.sql" \
    >"$SCENARIO_DIR/database.json"
  "${PSQL[@]}" -At \
    -v cluster="localnet" \
    -f "$ROOT_DIR/scripts/fleet-db-load/explain-health.sql" \
    >"$SCENARIO_DIR/explain.json"

  echo "Measuring isolated production health query at scale $scale"
  pgbench -n -M prepared -c 1 -j 1 -t 5 \
    -l --log-prefix="$SCENARIO_DIR/baseline-health" \
    -f "$ROOT_DIR/scripts/fleet-db-load/workloads/health.sql" \
    "$DATABASE_URL" >"$SCENARIO_DIR/baseline-health.stdout"

  echo "Running local worker, user, and mock-chain load for ${DURATION_SECONDS}s"
  submission_rows=$((scale / 4))
  if ((submission_rows < 1)); then
    submission_rows=1
  fi
  pids=()
  roles=(health executor confirmer reconciler planner user mock-chain)
  clients=("$HEALTH_CLIENTS" 1 1 1 1 2 1)
  scripts=(health executor confirmer reconciler planner user mock-chain)
  for index in "${!roles[@]}"; do
    role=${roles[$index]}
    pgbench -n -M prepared \
      -c "${clients[$index]}" -j "${clients[$index]}" \
      -T "$DURATION_SECONDS" \
      -D opportunity_rows="$scale" \
      -D submission_rows="$submission_rows" \
      -l --log-prefix="$SCENARIO_DIR/$role" \
      -f "$ROOT_DIR/scripts/fleet-db-load/workloads/${scripts[$index]}.sql" \
      "$DATABASE_URL" >"$SCENARIO_DIR/$role.stdout" 2>"$SCENARIO_DIR/$role.stderr" &
    pids+=("$!")
  done
  workload_failed=0
  for pid in "${pids[@]}"; do
    if ! wait "$pid"; then
      workload_failed=1
    fi
  done
  if ((workload_failed)); then
    echo "at least one workload failed; inspect $SCENARIO_DIR/*.stderr" >&2
    exit 1
  fi
  "${PSQL[@]}" -c "
    DELETE FROM loyal_yield.orchestration_outbox
    WHERE event_kind = 'local_user_load'
  " >/dev/null
done

bun "$ROOT_DIR/scripts/fleet-db-load/report.ts" "$RUN_DIR"
echo "Evidence: $RUN_DIR/evidence.md"
