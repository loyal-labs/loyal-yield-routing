#!/usr/bin/env bash
set -euo pipefail

# Isolated end-to-end verification for ASK-1978 health-poll contention.
#
# Covers two claims:
#
#   A. The reconciler's health emission is governed by the health interval.
#      Its outer loop used to call emit_fleet_reconciler_health unconditionally,
#      so an idle reconciler re-ran the expensive fleet_orchestration_status
#      aggregate on every 250ms recovery poll no matter what the interval said.
#
#   B. That load is what exhausts a downstream worker's connection pool. The
#      victim here is modelled on balance-sweep-ata-projector, which really does
#      run max_connections=5 with acquire_timeout=5s
#      (crates/balance-sweep-ata-observations/src/lib.rs) and really did exit
#      with PoolTimedOut on 2026-08-03.
#
# What this deliberately does NOT do: manufacture a connection failure. Nothing
# here caps the server's connection count, and the production PoolTimedOut is
# not reproduced. Neon's compute has far less headroom than a developer machine,
# so at the real three-process shape a laptop shows almost no contention, and
# inflating the reader count until timeouts appear would only be a different
# fabricated failure. Part B therefore asserts the causal input — the backend
# time the health poll takes from a realistically sized pool — and reports
# victim latency as directional evidence only.
#
# Uses no production credentials, no Render deployment, no Neon branch, and no
# external network access.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
worker_source="$repo_root/crates/loyal-fleet-worker/src/lib.rs"
health_source="$repo_root/crates/loyal-yield-orchestrator/src/fleet_orchestration/health.rs"

expected_interval_ms="${FLEET_HEALTH_INTERVAL_MS:-10000}"
recovery_poll_ms="${FLEET_HEALTH_RECOVERY_POLL_MS:-250}"
arm_seconds="${FLEET_HEALTH_ARM_SECONDS:-20}"
target_view_ms="${FLEET_HEALTH_TARGET_VIEW_MS:-2000}"

# Victim pool shape, taken from balance-sweep-ata-observations defaults.
victim_pool_size="${FLEET_HEALTH_VICTIM_POOL:-5}"
victim_acquire_timeout_ms="${FLEET_HEALTH_VICTIM_ACQUIRE_TIMEOUT_MS:-5000}"
victim_request_interval_ms="${FLEET_HEALTH_VICTIM_REQUEST_MS:-100}"

# Production shape: revalidate, execute, and reconcile share the constant.
load_sessions="${FLEET_HEALTH_LOAD_SESSIONS:-3}"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

for command_name in initdb pg_ctl psql rg awk python3; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

echo "== Static assertions against worker source"

rg --quiet \
  "^const FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS: u64 = 10_000;$" \
  "$worker_source" ||
  fail "FLEET_HEALTH_OBSERVATION_INTERVAL_MILLISECONDS is not 10_000"

# Enumerate every health emitter callsite. A new ungated one is exactly the
# regression this section exists to catch, so the counts are pinned. Each count
# includes the emitter's own `async fn` definition line.
reconciler_occurrences="$(rg --count --fixed-strings "emit_fleet_reconciler_health(" "$worker_source" || true)"
worker_occurrences="$(rg --count --fixed-strings "emit_fleet_worker_health(" "$worker_source" || true)"
# Reconciler: inner select arm + gated outer-loop call, plus the definition.
[[ "$reconciler_occurrences" == "3" ]] ||
  fail "expected 3 emit_fleet_reconciler_health occurrences (2 callsites + definition), found ${reconciler_occurrences:-0}"
# Worker: --once path, FleetWorkerWakeup::Health arm, final report, plus the definition.
[[ "$worker_occurrences" == "4" ]] ||
  fail "expected 4 emit_fleet_worker_health occurrences (3 callsites + definition), found ${worker_occurrences:-0}"

# The reconciler's outer-loop emission must be gated, and both reconciler emit
# paths must record the emission so the gate stays accurate.
rg --fixed-strings --quiet "let health_due = options.once" "$worker_source" ||
  fail "reconciler outer-loop health emission is not gated by a health_due check"
rg --fixed-strings --quiet \
  "last_health_emit.is_none_or(|last| last.elapsed() >= health_emit_interval)" \
  "$worker_source" ||
  fail "health_due does not compare against the health emit interval"
emit_stamps="$(rg --count --fixed-strings "last_health_emit = Some(tokio::time::Instant::now());" "$worker_source" || true)"
[[ "$emit_stamps" == "2" ]] ||
  fail "expected both reconciler emit paths to stamp last_health_emit, found ${emit_stamps:-0}"

# Worker lanes reach their periodic emission only through the health-interval
# wakeup; the remaining callsites are the --once and final-report paths.
rg --fixed-strings --quiet "FleetWorkerWakeup::Health => {" "$worker_source" ||
  fail "worker lane no longer routes periodic health through FleetWorkerWakeup::Health"

for emitter in emit_fleet_worker_health emit_fleet_reconciler_health; do
  awk -v fn="async fn $emitter" '
    index($0, fn) { capture = 1 }
    capture { print }
    capture && /^}$/ { exit }
  ' "$worker_source" | rg --fixed-strings --quiet "fleet_orchestration_status" ||
    fail "$emitter no longer reads fleet_orchestration_status"
done

skip_sites="$(rg --count --fixed-strings \
  "health_interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip)" \
  "$worker_source" || true)"
[[ "$skip_sites" == "2" ]] ||
  fail "expected 2 health_interval Skip sites, found ${skip_sites:-0}"

# Widening the interval must not move stuck-stage thresholds: those derive from
# the recovery poll interval, a separate input.
rg --fixed-strings --quiet \
  "FleetStageHealthPolicy::for_recovery_poll(recovery_poll_interval_milliseconds)" \
  "$health_source" ||
  fail "stuck-stage policy no longer derives from the recovery poll interval"

echo "PASS: constant 10_000; 2 reconciler + 3 worker callsites, outer loop gated"
echo "PASS: both reconciler emit paths stamp last_health_emit; Skip intact; thresholds independent"
echo

scratch_dir="$(mktemp -d "${TMPDIR:-/tmp}/fleet-health-poll.XXXXXX")"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
slots_dir="$scratch_dir/slots"
mkdir -p "$socket_dir" "$slots_dir"
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
# Generous server limit on purpose: the victim's own pool is the only bounded
# resource, so nothing here can fail by server-side admission refusal.
pg_ctl -D "$data_dir" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1 -c max_connections=200" \
  -w start >/dev/null
server_started=1

psql_args=(
  -X
  --set=ON_ERROR_STOP=1
  --host="$socket_dir"
  --port="$port"
  --username="$(id -un)"
  --dbname=postgres
)

status_query="SELECT * FROM loyal_yield.fleet_orchestration_status WHERE cluster = 'mainnet-beta' ORDER BY opportunity_state NULLS LAST;"
# Representative downstream unit of work, not a trivial probe: it has to be real
# enough that contention shows up as a longer connection hold.
victim_query="SELECT opportunity_state, count(*), avg(value_bps) FROM loyal_yield.rebalance_opportunities WHERE vault_id % 97 = 0 GROUP BY opportunity_state;"

build_status_view() {
  psql "${psql_args[@]}" --set=rows="$1" >/dev/null <<'SQL'
DROP VIEW IF EXISTS loyal_yield.fleet_orchestration_status;
DROP TABLE IF EXISTS loyal_yield.rebalance_opportunities;
CREATE SCHEMA IF NOT EXISTS loyal_yield;

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

-- Mirrors the production shape: independent full-scan aggregate CTEs with no
-- shared intermediate, recomputed on every read. The readers below select all
-- columns, as the worker does; a bare count(*) would let the planner drop the
-- LEFT JOINs (inlined CTEs are provably unique on their GROUP BY keys) and
-- three of the four CTEs would never run.
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
SQL
}

now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }
sleep_ms() {
  if [[ "$1" -gt 0 ]]; then
    python3 -c "import time,sys; time.sleep(int(sys.argv[1])/1000)" "$1"
  fi
}

run_status_query() {
  psql "${psql_args[@]}" --quiet --tuples-only --no-align --command="$status_query" >/dev/null 2>&1 || true
}

echo "== Calibrating synthetic status view to ~${target_view_ms}ms"
seed_rows="${FLEET_HEALTH_SEED_ROWS:-120000}"
build_status_view "$seed_rows"
start_ms="$(now_ms)"; run_status_query; view_ms="$(( $(now_ms) - start_ms ))"
if [[ "$view_ms" -gt 0 ]]; then
  scaled_rows="$(( seed_rows * target_view_ms / view_ms ))"
  [[ "$scaled_rows" -lt 20000 ]] && scaled_rows=20000
  [[ "$scaled_rows" -gt 4000000 ]] && scaled_rows=4000000
  build_status_view "$scaled_rows"
  seed_rows="$scaled_rows"
  start_ms="$(now_ms)"; run_status_query; view_ms="$(( $(now_ms) - start_ms ))"
fi
echo "  rows=$seed_rows  status view cost=${view_ms}ms  worker processes=$load_sessions"
[[ "$view_ms" -gt "$recovery_poll_ms" ]] ||
  fail "status view cost ${view_ms}ms must exceed the ${recovery_poll_ms}ms recovery poll for the ungated path to run back-to-back"
[[ "$view_ms" -lt "$expected_interval_ms" ]] ||
  fail "status view cost ${view_ms}ms must stay under the ${expected_interval_ms}ms health interval"
echo "PASS: ${recovery_poll_ms}ms < ${view_ms}ms < ${expected_interval_ms}ms"

# Calibrate the victim's offered rate to its own uncontended cost so that an
# idle database needs ~1 concurrent connection. The pool then has 5x headroom,
# and only a real contention multiplier above 5x can exhaust it — which is the
# property under test, rather than a rate chosen to guarantee failure.
run_victim_query() {
  psql "${psql_args[@]}" --quiet --tuples-only --no-align --command="$victim_query" >/dev/null 2>&1 || true
}
start_ms="$(now_ms)"; run_victim_query; victim_ms="$(( $(now_ms) - start_ms ))"
if [[ -z "${FLEET_HEALTH_VICTIM_REQUEST_MS:-}" ]]; then
  victim_request_interval_ms="$victim_ms"
  [[ "$victim_request_interval_ms" -lt 25 ]] && victim_request_interval_ms=25
fi
echo "  victim query cost=${victim_ms}ms uncontended; offered every ${victim_request_interval_ms}ms (~1 connection of $victim_pool_size)"
echo

# --- Part A: reconciler emission cadence ------------------------------------
# Models the reconciler outer loop: claim nothing, optionally emit health, then
# wait one recovery poll. `ungated` is the pre-fix behaviour, `gated` is the fix.
reconciler_cadence() {
  local mode="$1" out="$2"
  local deadline=$(( $(now_ms) + arm_seconds * 1000 ))
  local reads=0 last_emit=0 now
  while [[ "$(now_ms)" -lt "$deadline" ]]; do
    now="$(now_ms)"
    if [[ "$mode" == "ungated" ]] || [[ $(( now - last_emit )) -ge "$expected_interval_ms" ]]; then
      run_status_query
      reads=$(( reads + 1 ))
      last_emit="$now"
    fi
    sleep_ms "$recovery_poll_ms"
  done
  echo "$reads" >"$out"
}

echo "== Part A: idle reconciler emission cadence (${arm_seconds}s, ${recovery_poll_ms}ms recovery poll)"
reconciler_cadence ungated "$scratch_dir/cadence-ungated"
reconciler_cadence gated "$scratch_dir/cadence-gated"
ungated_reads="$(cat "$scratch_dir/cadence-ungated")"
gated_reads="$(cat "$scratch_dir/cadence-gated")"
expected_gated=$(( arm_seconds * 1000 / expected_interval_ms + 1 ))
echo "  ungated (pre-fix): $ungated_reads status-view reads"
echo "  gated   (fix):     $gated_reads status-view reads"

[[ "$ungated_reads" -gt "$(( expected_gated * 3 ))" ]] ||
  fail "negative control did not reproduce back-to-back reads ($ungated_reads); harness proves nothing"
echo "PASS: ungated reconciler reproduced $ungated_reads reads in ${arm_seconds}s"
[[ "$gated_reads" -le "$(( expected_gated + 1 ))" ]] ||
  fail "gated reconciler emitted $gated_reads reads, expected at most $(( expected_gated + 1 ))"
echo "PASS: gated reconciler held to $gated_reads reads (interval allows ~$expected_gated)"
echo

# --- Part B: database time the health poll takes from a real pool ------------
# What production actually died of is a client-side pool acquisition timeout.
# That cannot be reproduced honestly here: Neon's compute has far less headroom
# than a developer machine, so at the real three-process shape a laptop shows
# almost no contention. Inflating the reader count until timeouts appear would
# manufacture a different failure, which is what the first version of this
# script did wrong.
#
# So this part asserts the causal input instead: the backend-time the health
# poll takes away from a pool sized exactly like balance-sweep-ata-projector's
# (5 connections, 5s acquire timeout). Victim acquire and query latency are
# reported alongside as directional evidence, not asserted as failures.
acquire_slot() {
  local deadline=$(( $(now_ms) + victim_acquire_timeout_ms )) slot
  while :; do
    for (( slot = 1; slot <= victim_pool_size; slot++ )); do
      if mkdir "$slots_dir/$slot" 2>/dev/null; then
        echo "$slot"
        return 0
      fi
    done
    if [[ "$(now_ms)" -ge "$deadline" ]]; then
      return 1
    fi
    sleep_ms 10
  done
}

run_load_arm() {
  local mode="$1" arm_dir="$2"
  mkdir -p "$arm_dir"
  rm -rf "${slots_dir:?}"/*
  local results="$arm_dir/results"
  : >"$results"
  local deadline=$(( $(now_ms) + arm_seconds * 1000 ))
  local load_pids=() victim_pids=() session pid

  # Production shape: three processes share the constant. `continuous` is the
  # ungated reconciler re-reading back-to-back; `interval` is all three paced.
  for (( session = 1; session <= load_sessions; session++ )); do
    (
      busy_ms=0
      reads=0
      sleep_ms "$(( expected_interval_ms * (session - 1) / load_sessions ))"
      while [[ "$(now_ms)" -lt "$deadline" ]]; do
        started="$(now_ms)"
        run_status_query
        elapsed=$(( $(now_ms) - started ))
        busy_ms=$(( busy_ms + elapsed ))
        reads=$(( reads + 1 ))
        if [[ "$mode" == "interval" ]]; then
          sleep_ms "$(( expected_interval_ms - elapsed ))"
        fi
      done
      echo "$busy_ms $reads" >"$arm_dir/load-$session"
    ) &
    load_pids+=($!)
  done

  while [[ "$(now_ms)" -lt "$deadline" ]]; do
    (
      queued="$(now_ms)"
      if slot="$(acquire_slot)"; then
        acquired="$(now_ms)"
        run_victim_query
        rmdir "$slots_dir/$slot" 2>/dev/null || true
        echo "ok $(( acquired - queued )) $(( $(now_ms) - acquired ))" >>"$results"
      else
        echo "timeout $victim_acquire_timeout_ms 0" >>"$results"
      fi
    ) &
    victim_pids+=($!)
    sleep_ms "$victim_request_interval_ms"
  done

  for pid in "${victim_pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  for pid in "${load_pids[@]}"; do wait "$pid" 2>/dev/null || true; done

  local total_busy=0 total_reads=0 fields
  for (( session = 1; session <= load_sessions; session++ )); do
    fields="$(cat "$arm_dir/load-$session")"
    total_busy=$(( total_busy + $(echo "$fields" | awk '{print $1}') ))
    total_reads=$(( total_reads + $(echo "$fields" | awk '{print $2}') ))
  done
  # Backend-seconds of status-view work per wall-second, summed over processes.
  local duty_pct=$(( total_busy * 100 / (arm_seconds * 1000 * load_sessions) ))

  local total timeouts acquire_p95 query_p95
  total="$(wc -l <"$results" | tr -d ' ')"
  timeouts="$(awk '$1 == "timeout"' "$results" | wc -l | tr -d ' ')"
  percentile() {
    awk -v col="$1" '$1 == "ok" {print $col}' "$results" | sort -n | awk '
      { v[NR] = $1 }
      END { if (NR == 0) { print "n/a"; exit } i = int(NR * 0.95); if (i < 1) i = 1; print v[i] }'
  }
  acquire_p95="$(percentile 2)"
  query_p95="$(percentile 3)"

  printf '%s %s %s %s %s %s\n' \
    "$total_busy" "$total_reads" "$duty_pct" "$total" "$timeouts" "$acquire_p95" \
    >"$arm_dir/summary"
  echo "  $mode: status_backend_time=${total_busy}ms over $total_reads reads (duty ${duty_pct}%)"
  echo "     victim requests=$total  acquire_timeouts=$timeouts  acquire_p95=${acquire_p95}ms  query_p95=${query_p95}ms"
}

echo "== Part B: status-view backend time against a projector-shaped pool"
echo "   ($load_sessions worker processes; victim pool $victim_pool_size, acquire timeout ${victim_acquire_timeout_ms}ms)"
echo "-- Arm 1 (negative control): reconciler ungated, reading back-to-back"
run_load_arm continuous "$scratch_dir/load-continuous"
read -r c_busy c_reads c_duty c_total c_timeouts c_acquire <"$scratch_dir/load-continuous/summary"
echo "-- Arm 2 (fixed): all processes paced by the ${expected_interval_ms}ms interval"
run_load_arm interval "$scratch_dir/load-interval"
read -r i_busy i_reads i_duty i_total i_timeouts i_acquire <"$scratch_dir/load-interval/summary"
echo

echo "== Assertions"
[[ "$c_duty" -ge 80 ]] ||
  fail "negative control only reached ${c_duty}% duty; it must saturate to prove anything"
echo "PASS: ungated load held the status view at ${c_duty}% duty ($c_reads reads)"

[[ "$i_duty" -lt 40 ]] ||
  fail "interval-paced load still ran at ${i_duty}% duty"
echo "PASS: interval-paced load dropped to ${i_duty}% duty ($i_reads reads)"

[[ $(( i_busy * 3 )) -le "$c_busy" ]] ||
  fail "status-view backend time did not fall at least 3x (${c_busy}ms -> ${i_busy}ms)"
echo "PASS: status-view backend time fell ${c_busy}ms -> ${i_busy}ms"

# Directional only. This machine has too much headroom for the pool to time out
# at the production process count, so a timeout here is a bonus, not the proof.
[[ "$i_timeouts" -le "$c_timeouts" ]] ||
  fail "interval-paced arm produced more acquire timeouts ($c_timeouts -> $i_timeouts)"
echo "PASS: victim acquire timeouts did not regress ($c_timeouts -> $i_timeouts)"
echo

echo "PASS: fleet health-poll contention verification"
echo "  status view cost:              ${view_ms}ms (rows=$seed_rows)"
echo "  idle reconciler reads/${arm_seconds}s:     $ungated_reads ungated -> $gated_reads gated"
echo "  status-view backend time:      ${c_busy}ms -> ${i_busy}ms"
echo "  status-view duty cycle:        ${c_duty}% -> ${i_duty}%"
echo "  victim pool:                   $victim_pool_size connections, ${victim_acquire_timeout_ms}ms acquire timeout"
echo "  victim acquire p95:            ${c_acquire}ms -> ${i_acquire}ms"
echo "  victim acquire timeouts:       $c_timeouts -> $i_timeouts (not the proof; see Part B note)"
