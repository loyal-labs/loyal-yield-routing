#!/usr/bin/env bash
set -euo pipefail

# Isolated end-to-end verification for ASK-1978 part 1.
#
# Reproduces the 2026-08-03 pool-starvation mechanism against an ephemeral
# local Postgres and shows the widened health-observation interval removes it.
#
# The production shape being modelled:
#   * `loyal_yield.fleet_orchestration_status` is a plain view whose CTE chain
#     re-aggregates from scratch on every read (~2s measured in production).
#   * Three worker processes (revalidate, execute, reconcile) each emit health
#     on a shared interval, awaiting the read inline under
#     `MissedTickBehavior::Skip`.
#   * When the read is slower than the interval, every process runs the
#     aggregate back-to-back at ~100% duty and holds a pooled connection while
#     it does, so unrelated work cannot acquire one.
#
# A worker's checked-out sqlx connection is modelled by a role with a hard
# CONNECTION LIMIT: pollers connect per iteration and hold a slot only while
# querying, so a victim client failing to connect is the same starvation the
# `PoolTimedOut` errors reported.
#
# Uses no production credentials, no Render deployment, no Neon branch, and no
# external network access.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
worker_source="$repo_root/crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs"
health_source="$repo_root/crates/loyal-yield-orchestrator/src/fleet_orchestration/health.rs"

# The constant under test, and the value it regressed from.
expected_interval_ms="${FLEET_HEALTH_INTERVAL_MS:-10000}"
regressed_interval_ms="${FLEET_HEALTH_REGRESSED_INTERVAL_MS:-1000}"
# Worker processes sharing the constant: revalidate, execute, reconcile.
poller_count="${FLEET_HEALTH_POLLERS:-3}"
# Connection budget the pollers contend for.
connection_limit="${FLEET_HEALTH_CONNECTION_LIMIT:-3}"
arm_seconds="${FLEET_HEALTH_ARM_SECONDS:-20}"
# Target cost for the synthetic status view. Must land above the regressed
# interval and below the fixed one, or neither arm proves anything.
target_view_ms="${FLEET_HEALTH_TARGET_VIEW_MS:-2000}"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

for command_name in initdb pg_ctl psql rg awk; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

for value in "$expected_interval_ms" "$regressed_interval_ms" "$poller_count" \
  "$connection_limit" "$arm_seconds" "$target_view_ms"; do
  case "$value" in
    ''|*[!0-9]*) fail "numeric settings must be positive integers (got '$value')" ;;
  esac
done

echo "== Static assertions against worker source"

# 1. The constant actually carries the fixed value.
rg --quiet \
  "^const FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS: u64 = 10_000;$" \
  "$worker_source" ||
  fail "FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS is not 10_000"

# 2. Both health emitters still read the status view, so the constant still
#    governs the load this verifier measures.
for emitter in emit_fleet_worker_health emit_fleet_reconciler_health; do
  awk -v fn="async fn $emitter" '
    index($0, fn) { capture = 1 }
    capture { print }
    capture && /^}$/ { exit }
  ' "$worker_source" | rg --fixed-strings --quiet "fleet_orchestration_status" ||
    fail "$emitter no longer reads fleet_orchestration_status"
done

# 3. Skip behaviour must stay: without it a widened interval would queue missed
#    ticks and reproduce the same back-to-back load.
skip_sites="$(rg --count --fixed-strings \
  "health_interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip)" \
  "$worker_source" || true)"
[[ "$skip_sites" == "2" ]] ||
  fail "expected 2 health_interval Skip sites, found ${skip_sites:-0}"

# 4. Stuck-stage thresholds must remain driven by the recovery poll interval,
#    not by the health interval — otherwise widening it would silently move
#    detection thresholds instead of only the sampling rate.
rg --fixed-strings --quiet \
  "FleetStageHealthPolicy::for_recovery_poll(recovery_poll_interval_milliseconds)" \
  "$health_source" ||
  fail "stuck-stage policy no longer derives from the recovery poll interval"

echo "PASS: constant is 10_000; both emitters unchanged; Skip intact; thresholds independent"
echo

scratch_dir="$(mktemp -d "${TMPDIR:-/tmp}/fleet-health-poll.XXXXXX")"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
mkdir -p "$socket_dir"
port="$((56432 + RANDOM % 1000))"
server_started=0

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    pg_ctl -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

initdb -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
pg_ctl -D "$data_dir" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1 -c max_connections=40" \
  -w start >/dev/null
server_started=1

admin_args=(
  -X
  --set=ON_ERROR_STOP=1
  --host="$socket_dir"
  --port="$port"
  --username="$(id -un)"
  --dbname=postgres
)

# Pollers and the victim share this bounded role, standing in for a worker pool.
psql "${admin_args[@]}" >/dev/null <<SQL
CREATE ROLE fleet_worker LOGIN CONNECTION LIMIT $connection_limit;
CREATE SCHEMA loyal_yield AUTHORIZATION fleet_worker;

CREATE TABLE loyal_yield.victim_probe (
  id BIGINT PRIMARY KEY,
  note TEXT NOT NULL
);
INSERT INTO loyal_yield.victim_probe
SELECT g, 'probe' FROM generate_series(1, 64) AS g;
SQL

worker_args=(
  -X
  --set=ON_ERROR_STOP=1
  --host="$socket_dir"
  --port="$port"
  --username=fleet_worker
  --dbname=postgres
)

seed_rows="${FLEET_HEALTH_SEED_ROWS:-120000}"

build_status_view() {
  local rows="$1"
  psql "${admin_args[@]}" --set=rows="$rows" >/dev/null <<'SQL'
DROP VIEW IF EXISTS loyal_yield.fleet_orchestration_status;
DROP TABLE IF EXISTS loyal_yield.rebalance_opportunities;

CREATE TABLE loyal_yield.rebalance_opportunities (
  id BIGINT PRIMARY KEY,
  cluster TEXT NOT NULL,
  opportunity_state TEXT NOT NULL,
  vault_id BIGINT NOT NULL,
  value_bps BIGINT NOT NULL,
  state_entered_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL
);

INSERT INTO loyal_yield.rebalance_opportunities
SELECT
  g,
  'mainnet-beta',
  (ARRAY['ready','waiting_alt','sender','confirmer','reconciler'])[1 + (g % 5)],
  g % 2500,
  (g::BIGINT * 7919) % 100000,
  now() - make_interval(secs => (g % 3600)),
  now() + make_interval(secs => (g % 900))
FROM generate_series(1, :rows) AS g;

ANALYZE loyal_yield.rebalance_opportunities;

-- Mirrors the production shape: several independent full-scan aggregate CTEs
-- with no shared intermediate, recomputed on every read.
CREATE VIEW loyal_yield.fleet_orchestration_status AS
WITH opportunity_status AS (
  SELECT cluster, opportunity_state,
         count(*) AS item_count,
         min(state_entered_at) AS oldest_state_entered_at,
         sum(value_bps) AS total_value_bps
  FROM loyal_yield.rebalance_opportunities
  GROUP BY cluster, opportunity_state
),
queue_status AS (
  SELECT cluster, opportunity_state,
         count(DISTINCT vault_id) AS vault_count,
         avg(value_bps) AS mean_value_bps
  FROM loyal_yield.rebalance_opportunities
  WHERE expires_at > now()
  GROUP BY cluster, opportunity_state
),
outbox_status AS (
  SELECT cluster, opportunity_state,
         count(*) FILTER (WHERE value_bps % 3 = 0) AS pending_count,
         max(state_entered_at) AS newest_state_entered_at
  FROM loyal_yield.rebalance_opportunities
  GROUP BY cluster, opportunity_state
),
submission_status AS (
  SELECT cluster, opportunity_state,
         count(*) FILTER (WHERE value_bps % 5 = 0) AS submitted_count,
         percentile_cont(0.5) WITHIN GROUP (ORDER BY value_bps) AS median_value_bps
  FROM loyal_yield.rebalance_opportunities
  GROUP BY cluster, opportunity_state
)
SELECT o.cluster, o.opportunity_state, o.item_count, o.oldest_state_entered_at,
       o.total_value_bps, q.vault_count, q.mean_value_bps, x.pending_count,
       x.newest_state_entered_at, s.submitted_count, s.median_value_bps
FROM opportunity_status o
LEFT JOIN queue_status q USING (cluster, opportunity_state)
LEFT JOIN outbox_status x USING (cluster, opportunity_state)
LEFT JOIN submission_status s USING (cluster, opportunity_state);

GRANT USAGE ON SCHEMA loyal_yield TO fleet_worker;
GRANT SELECT ON ALL TABLES IN SCHEMA loyal_yield TO fleet_worker;
SQL
}

now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }

measure_view_ms() {
  local start_ms end_ms
  start_ms="$(now_ms)"
  psql "${worker_args[@]}" --tuples-only --no-align \
    --command="SELECT * FROM loyal_yield.fleet_orchestration_status WHERE cluster = 'mainnet-beta' ORDER BY opportunity_state NULLS LAST;" >/dev/null
  end_ms="$(now_ms)"
  echo "$((end_ms - start_ms))"
}

echo "== Calibrating synthetic status view to ~${target_view_ms}ms"
build_status_view "$seed_rows"
view_ms="$(measure_view_ms)"

# One proportional correction is enough to land inside the required band.
if [[ "$view_ms" -gt 0 ]]; then
  scaled_rows="$(( seed_rows * target_view_ms / view_ms ))"
  [[ "$scaled_rows" -lt 20000 ]] && scaled_rows=20000
  [[ "$scaled_rows" -gt 4000000 ]] && scaled_rows=4000000
  if [[ "$scaled_rows" != "$seed_rows" ]]; then
    build_status_view "$scaled_rows"
    seed_rows="$scaled_rows"
    view_ms="$(measure_view_ms)"
  fi
fi

echo "  rows=$seed_rows  view cost=${view_ms}ms"

# The whole argument depends on the read being slower than the old interval and
# faster than the new one. Refuse to report a result outside that band.
[[ "$view_ms" -gt "$regressed_interval_ms" ]] ||
  fail "view cost ${view_ms}ms must exceed the regressed interval ${regressed_interval_ms}ms for the reproduction to be meaningful"
[[ "$view_ms" -lt "$expected_interval_ms" ]] ||
  fail "view cost ${view_ms}ms must stay under the fixed interval ${expected_interval_ms}ms"
echo "PASS: ${regressed_interval_ms}ms < ${view_ms}ms < ${expected_interval_ms}ms"
echo

run_arm() {
  local label="$1" interval_ms="$2" arm_dir="$3"
  mkdir -p "$arm_dir"
  local deadline=$(( $(now_ms) + arm_seconds * 1000 ))
  local pids=()

  local poller
  for (( poller = 1; poller <= poller_count; poller++ )); do
    (
      local busy_ms=0 iterations=0 started elapsed remaining
      # Independent Render services start at different times, so their health
      # ticks are not phase-locked. Spread the initial offset across the
      # interval rather than firing all pollers on the same edge.
      python3 -c "import time,sys; time.sleep(int(sys.argv[1])/1000)" \
        "$(( interval_ms * (poller - 1) / poller_count ))"
      while [[ "$(now_ms)" -lt "$deadline" ]]; do
        started="$(now_ms)"
        psql "${worker_args[@]}" --quiet --tuples-only --no-align \
          --command="SELECT * FROM loyal_yield.fleet_orchestration_status WHERE cluster = 'mainnet-beta' ORDER BY opportunity_state NULLS LAST;" \
          >/dev/null 2>&1 || true
        elapsed=$(( $(now_ms) - started ))
        busy_ms=$(( busy_ms + elapsed ))
        iterations=$(( iterations + 1 ))
        # tokio interval + MissedTickBehavior::Skip: a tick already due when the
        # await finishes fires immediately, so only a positive remainder sleeps.
        remaining=$(( interval_ms - elapsed ))
        if [[ "$remaining" -gt 0 ]]; then
          python3 -c "import time,sys; time.sleep(int(sys.argv[1])/1000)" "$remaining"
        fi
      done
      echo "$busy_ms $iterations" >"$arm_dir/poller-$poller"
    ) &
    pids+=($!)
  done

  # Victim: unrelated work needing a connection from the same budget.
  (
    local attempts=0 failures=0 started elapsed
    : >"$arm_dir/victim-latencies"
    while [[ "$(now_ms)" -lt "$deadline" ]]; do
      started="$(now_ms)"
      if psql "${worker_args[@]}" --quiet --tuples-only --no-align \
        --command="SELECT count(*) FROM loyal_yield.victim_probe;" >/dev/null 2>&1; then
        elapsed=$(( $(now_ms) - started ))
        echo "$elapsed" >>"$arm_dir/victim-latencies"
      else
        failures=$(( failures + 1 ))
      fi
      attempts=$(( attempts + 1 ))
      python3 -c "import time; time.sleep(0.1)"
    done
    echo "$attempts $failures" >"$arm_dir/victim"
  ) &
  pids+=($!)

  local pid
  for pid in "${pids[@]}"; do
    wait "$pid" || true
  done

  local total_busy=0 total_iterations=0 fields
  for (( poller = 1; poller <= poller_count; poller++ )); do
    fields="$(cat "$arm_dir/poller-$poller")"
    total_busy=$(( total_busy + $(echo "$fields" | awk '{print $1}') ))
    total_iterations=$(( total_iterations + $(echo "$fields" | awk '{print $2}') ))
  done

  local wall_ms=$(( arm_seconds * 1000 ))
  # Backend-seconds of status-view work per wall-second, summed over pollers.
  local duty_pct=$(( total_busy * 100 / (wall_ms * poller_count) ))
  local attempts failures victim_p95
  attempts="$(awk '{print $1}' "$arm_dir/victim")"
  failures="$(awk '{print $2}' "$arm_dir/victim")"
  victim_p95="$(sort -n "$arm_dir/victim-latencies" | awk '
    { values[NR] = $1 }
    END {
      if (NR == 0) { print "n/a"; exit }
      idx = int(NR * 0.95); if (idx < 1) idx = 1
      print values[idx]
    }')"

  printf '%s %s %s %s %s %s\n' \
    "$total_iterations" "$duty_pct" "$attempts" "$failures" "$victim_p95" "$total_busy" \
    >"$arm_dir/summary"

  echo "  $label: reads=$total_iterations  duty=${duty_pct}%  victim_attempts=$attempts  victim_conn_failures=$failures  victim_p95=${victim_p95}ms"
}

echo "== Arm A (negative self-test): regressed interval ${regressed_interval_ms}ms"
run_arm "regressed" "$regressed_interval_ms" "$scratch_dir/arm-a"
read -r a_reads a_duty a_attempts a_failures a_p95 a_busy <"$scratch_dir/arm-a/summary"
echo

echo "== Arm B (fixed): interval ${expected_interval_ms}ms"
run_arm "fixed" "$expected_interval_ms" "$scratch_dir/arm-b"
read -r b_reads b_duty b_attempts b_failures b_p95 b_busy <"$scratch_dir/arm-b/summary"
echo

echo "== Assertions"

# Negative self-test: if the regressed arm did not actually starve the budget,
# the fixed arm passing proves nothing.
[[ "$a_failures" -gt 0 ]] ||
  fail "negative self-test did not reproduce starvation at ${regressed_interval_ms}ms; harness proves nothing"
echo "PASS: regressed interval reproduced $a_failures victim connection failures"

[[ "$a_duty" -ge 80 ]] ||
  fail "regressed arm duty cycle ${a_duty}% should approach saturation"
echo "PASS: regressed interval kept pollers at ${a_duty}% duty (back-to-back reads)"

# Not asserted as zero: with the pool sized to the poller count, any moment all
# pollers happen to overlap still locks the victim out briefly. The claim is
# that starvation stops being the steady state, so require a large relative
# drop and a low absolute rate.
[[ $(( b_failures * 5 )) -le "$a_failures" ]] ||
  fail "fixed interval did not reduce victim connection failures 5x ($a_failures -> $b_failures)"
[[ $(( b_failures * 10 )) -lt "$b_attempts" ]] ||
  fail "fixed interval still failed $b_failures of $b_attempts victim connections (>10%)"
echo "PASS: victim connection failures fell $a_failures -> $b_failures (of $b_attempts attempts)"

[[ "$b_duty" -lt 40 ]] ||
  fail "fixed arm duty cycle ${b_duty}% should be well below saturation"
echo "PASS: fixed interval dropped duty cycle to ${b_duty}%"

[[ "$b_reads" -lt "$a_reads" ]] ||
  fail "fixed interval did not reduce status-view read count ($b_reads vs $a_reads)"
echo "PASS: status-view reads fell from $a_reads to $b_reads"

echo
echo "PASS: fleet health-poll contention verification"
echo "  synthetic view cost:        ${view_ms}ms (rows=$seed_rows)"
echo "  worker pollers:             $poller_count against CONNECTION LIMIT $connection_limit"
echo "  arm duration:               ${arm_seconds}s each"
echo "  reads   ${regressed_interval_ms}ms -> ${expected_interval_ms}ms:  $a_reads -> $b_reads"
echo "  duty    ${regressed_interval_ms}ms -> ${expected_interval_ms}ms:  ${a_duty}% -> ${b_duty}%"
echo "  victim connection failures: $a_failures -> $b_failures"
echo "  victim p95 latency:         ${a_p95}ms -> ${b_p95}ms"
