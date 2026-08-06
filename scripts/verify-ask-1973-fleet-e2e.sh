#!/usr/bin/env bash
set -euo pipefail

# Complete local E2E verification for the ASK-1973 crate-boundary refactor.
#
# The verifier deliberately combines five independent proof surfaces:
#   1. release builds of every durable fleet binary from its owning crate;
#   2. the production-sized isolated database verifier;
#   3. a 10,000-vault deterministic planner replay;
#   4. one real, concurrent, one-shot run of all six production fleet roles,
#      including a 4,160-job fail-closed worker queue cohort;
#   5. the production-shaped fleet health-poll contention harness.
#
# It never reads or writes production databases, RPCs, Render, or registries.
# POLICY_KEYPAIR is required only because the production worker startup path
# rejects a silently mis-mounted signer. The process-level queue contains only
# deliberately incomplete local fixtures that must fail before a chain read or
# transaction, and OBSERVABILITY_ENABLED=false prevents remote telemetry.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

for command_name in bun cargo initdb pg_ctl createdb psql jq rg awk python3; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

[[ -n "${POLICY_KEYPAIR:-}" ]] || fail \
  "POLICY_KEYPAIR must be injected for the real worker startup path"

runtime_tmp_root="${ASK1973_RUNTIME_TMPDIR:-/tmp}"
evidence_dir="${ASK1973_EVIDENCE_DIR:-$(mktemp -d "$runtime_tmp_root/ask1973-fleet-e2e-evidence.XXXXXX")}"
mkdir -p "$evidence_dir"
scratch_dir="$(mktemp -d "$runtime_tmp_root/ask1973-fleet-e2e-runtime.XXXXXX")"
data_dir="$scratch_dir/postgres"
socket_dir="$scratch_dir/socket"
mkdir -p "$socket_dir"
port="$((58500 + RANDOM % 500))"
server_started=0
rpc_stub_pid=0
fleet_pids=()

cleanup() {
  local pid
  for pid in "${fleet_pids[@]:-}"; do
    kill "$pid" >/dev/null 2>&1 || true
  done
  if [[ "$rpc_stub_pid" -ne 0 ]]; then
    kill "$rpc_stub_pid" >/dev/null 2>&1 || true
    wait "$rpc_stub_pid" 2>/dev/null || true
  fi
  if [[ "$server_started" -eq 1 ]]; then
    pg_ctl -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  if [[ "$scratch_dir" == "$runtime_tmp_root/ask1973-fleet-e2e-runtime."* ]]; then
    rm -rf "$scratch_dir"
  fi
}
trap cleanup EXIT

echo "== Building durable fleet binaries"
cargo build --release --locked \
  -p loyal-yield-orchestrator \
  --bin yield-migrations \
  --bin fleet-opportunity-planner \
  --bin fleet-route-confirmer \
  --bin route-lookup-table-provisioner \
  --bin fleet-orchestration-verifier \
  -p loyal-fleet-worker \
  --bin same-mint-reserve-swap

for binary in \
  yield-migrations \
  fleet-opportunity-planner \
  fleet-route-confirmer \
  route-lookup-table-provisioner \
  fleet-orchestration-verifier \
  same-mint-reserve-swap; do
  [[ -x "target/release/$binary" ]] || fail "target/release/$binary is missing"
done
echo "PASS: all durable fleet binaries were built from the refactored crate graph"
echo

echo "== Starting disposable PostgreSQL"
initdb -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
pg_ctl -D "$data_dir" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1 -c max_connections=400" \
  -w start >/dev/null
server_started=1
createdb -h "$socket_dir" -p "$port" fleet_verify
database_url="postgresql://$(id -un)@127.0.0.1:$port/fleet_verify"

NEON_DATABASE_URL="$database_url" target/release/yield-migrations --apply \
  >"$evidence_dir/migrations.log"

# The planner uses only these two Timescale relations during the process-level
# pass. They are recreated locally with the production column contract; no
# Timescale extension or external database is involved.
psql -X --set=ON_ERROR_STOP=1 "$database_url" >/dev/null <<'SQL'
CREATE SCHEMA kamino;

CREATE TABLE kamino.supported_reserves (
  market TEXT NOT NULL,
  liquidity_mint TEXT NOT NULL,
  reserve TEXT NOT NULL,
  market_name TEXT,
  symbol TEXT,
  risk_baskets TEXT[] NOT NULL DEFAULT '{}',
  source TEXT NOT NULL DEFAULT 'kamino-api',
  active BOOLEAN NOT NULL DEFAULT TRUE,
  fetched_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE kamino.latest_verified_reserve_updates (
  event_id BIGINT NOT NULL,
  account_data_hash TEXT NOT NULL,
  observed_at TIMESTAMPTZ NOT NULL,
  slot BIGINT NOT NULL,
  verified_at TIMESTAMPTZ NOT NULL,
  verified_slot BIGINT NOT NULL,
  verification_commitment TEXT NOT NULL,
  verification_source TEXT NOT NULL,
  reserve TEXT NOT NULL,
  market TEXT,
  market_name TEXT,
  liquidity_mint TEXT NOT NULL,
  symbol TEXT,
  mint_decimals INTEGER NOT NULL,
  reserve_last_update_slot BIGINT NOT NULL,
  reserve_last_update_stale BOOLEAN NOT NULL,
  reserve_price_status SMALLINT NOT NULL,
  available_amount DOUBLE PRECISION NOT NULL,
  borrowed_amount DOUBLE PRECISION NOT NULL,
  total_supply_amount DOUBLE PRECISION NOT NULL,
  market_price_usd DOUBLE PRECISION NOT NULL,
  market_price_last_updated_ts BIGINT NOT NULL,
  utilization DOUBLE PRECISION NOT NULL,
  borrow_apy DOUBLE PRECISION NOT NULL,
  supply_apy DOUBLE PRECISION NOT NULL
);
SQL
echo "PASS: migrations 1-32 and the isolated market-read contract are available"
echo

echo "== Production-sized isolated database verification"
target/release/fleet-orchestration-verifier \
  --implementation \
  --json \
  --isolated-database \
  --database-url "$database_url" \
  >"$evidence_dir/isolated-database-verifier.json"

jq -e '
  .requestedScope == "ISOLATED_DATABASE"
  and .requestedScopeStatus == "PASS"
  and .isolatedDatabase == "PASS"
  and .firstBlockingCheck == null
' "$evidence_dir/isolated-database-verifier.json" >/dev/null || \
  fail "isolated database verifier did not pass"
echo "PASS: 4,160 runnable + 10,000 ALT-cold + 10,000 inert load and concurrency/fence checks"
echo

echo "== 10,000-vault planning replay"
target/release/fleet-opportunity-planner \
  --once --dry-run --benchmark --json \
  --count 10000 --rounds 7 --seed 327480054092 \
  >"$evidence_dir/planner-replay.json"
jq -e '
  .status == "pass"
  and .mode == "deterministic_in_memory_replay"
  and .inputCount == 10000
  and .rounds == 7
  and .childProcessesSpawned == 0
  and .economicPriorityOrdered == true
  and .planningP95Micros < .planningLimitMicros
' "$evidence_dir/planner-replay.json" >/dev/null || \
  fail "10,000-vault planner replay did not pass"
echo "PASS: 10,000-vault planner replay stayed ordered and below its 10-second p95 gate"
echo

echo "== Concurrent one-shot run of all six production fleet roles"
export NEON_DATABASE_URL="$database_url"
export TIMESCALEDB_URL="$database_url"
export SOLANA_RPC_URL="http://127.0.0.1:18999"
export SOLANA_WS_URL="ws://127.0.0.1:18999"
export YIELD_ALT_CLUSTER="localnet"
export YIELD_ALT_MAX_LAMPORTS="1000000"
export YIELD_ALT_BUDGET_WINDOW_SECONDS="3600"
export OBSERVABILITY_ENABLED="false"
export RUST_LOG="warn"
unset YIELD_ROUTE_FEE_PAYER_KEYPAIRS SOLANA_TESTING_PK YIELD_ROUTER_KEYPAIR

# Give the actual revalidation and execution processes a production-sized
# durable queue cohort. Each distinct active vault owns one active slot. The
# execution plan is intentionally incomplete, so the workers must claim,
# classify, and durably terminalize every row without reading chain state. This
# is a fail-closed process/load check, not a simulated successful transaction.
psql -X --set=ON_ERROR_STOP=1 "$database_url" >/dev/null <<'SQL'
WITH policy AS (
  INSERT INTO loyal_yield.route_policies (
    settings,
    authority,
    policy_seed,
    policy_account,
    vault_index,
    vault_pubkey,
    delegated_signers,
    threshold,
    route_modes,
    stable_mints,
    kamino_markets,
    kamino_liquidity_mints,
    active,
    last_seen_slot,
    last_seen_signature
  ) VALUES (
    'ask1973-process-load-settings',
    'ask1973-process-load-authority',
    1973,
    'ask1973-process-load-policy',
    0,
    'ask1973-process-load-vault',
    ARRAY['ask1973-process-load-signer'],
    1,
    ARRAY['same_mint_kamino'],
    ARRAY['ask1973-process-load-mint'],
    ARRAY['ask1973-process-load-market'],
    ARRAY['ask1973-process-load-mint'],
    TRUE,
    1,
    'ask1973-process-load-signature'
  )
  RETURNING id
), vaults AS (
  INSERT INTO loyal_yield.managed_vaults (
    settings,
    vault_index,
    vault_pubkey,
    active_policy_id,
    active
  )
  SELECT
    'ask1973-process-load-settings',
    sequence::SMALLINT,
    'ask1973-process-load-vault',
    policy.id,
    TRUE
  FROM policy
  CROSS JOIN generate_series(1, 4160) AS sequence
  RETURNING id, vault_index
), epoch AS (
  INSERT INTO loyal_yield.optimizer_epochs (
    cluster,
    epoch_key,
    market_slot,
    observed_at,
    expires_at,
    market_state
  ) VALUES (
    'localnet',
    'ask1973-process-load-epoch',
    1,
    clock_timestamp(),
    clock_timestamp() + INTERVAL '1 hour',
    '{}'::jsonb
  )
  RETURNING id
)
INSERT INTO loyal_yield.rebalance_opportunities (
  cluster,
  idempotency_key,
  vault_id,
  optimizer_epoch_id,
  route_fingerprint,
  requirements_fingerprint,
  source_reserve,
  target_reserve,
  liquidity_mint,
  amount_raw,
  principal_usd_micros,
  source_apy_bps,
  target_apy_bps,
  estimated_edge_bps,
  estimated_cost_lamports,
  annual_yield_gain_usd_micros,
  expected_net_gain_usd_micros,
  economic_priority,
  scheduler_priority_anchor,
  priority_version,
  opportunity_state,
  execution_plan,
  expires_at
)
SELECT
  'localnet',
  'ask1973-process-load-' || vaults.vault_index,
  vaults.id,
  epoch.id,
  'ask1973-route-' || vaults.vault_index,
  'ask1973-requirements-' || vaults.vault_index,
  'ask1973-source-reserve',
  'ask1973-target-reserve',
  'ask1973-process-load-mint',
  1000000,
  1000000,
  100,
  200,
  100,
  0,
  1000000,
  1000000,
  1000000 + vaults.vault_index,
  0,
  'ask1973-process-load-v1',
  CASE WHEN vaults.vault_index <= 2080 THEN 'revalidate' ELSE 'ready' END,
  '{}'::jsonb,
  clock_timestamp() + INTERVAL '1 hour'
FROM vaults
CROSS JOIN epoch;
SQL

process_load_seeded="$(psql -X -At "$database_url" -c \
  "SELECT count(*) FROM loyal_yield.rebalance_opportunities WHERE idempotency_key LIKE 'ask1973-process-load-%'")"
[[ "$process_load_seeded" == "4160" ]] || \
  fail "expected 4,160 process-load jobs, seeded $process_load_seeded"

# Every RPC-owning role proves its explicit cluster binding at startup. Serve
# only getGenesisHash with a non-canonical hash accepted exclusively for
# localnet. Any accidental transaction, account, fee, or status call receives
# a JSON-RPC method-not-found response and therefore fails closed.
bun -e '
  Bun.serve({
    hostname: "127.0.0.1",
    port: 18999,
    async fetch(request) {
      let payload;
      try {
        payload = await request.json();
      } catch {
        return Response.json({ jsonrpc: "2.0", id: null, error: { code: -32700, message: "parse error" } });
      }
      const respond = (call) => {
        console.log(call?.method ?? "<missing-method>");
        return call?.method === "getGenesisHash"
          ? { jsonrpc: "2.0", id: call.id ?? null, result: "11111111111111111111111111111111" }
          : { jsonrpc: "2.0", id: call?.id ?? null, error: { code: -32601, message: "isolated RPC method blocked" } };
      };
      return Response.json(Array.isArray(payload) ? payload.map(respond) : respond(payload));
    },
  });
  await new Promise(() => {});
' >"$evidence_dir/rpc-stub.log" 2>&1 &
rpc_stub_pid="$!"

rpc_ready=0
for _ in {1..50}; do
  if python3 -c 'import socket; s=socket.create_connection(("127.0.0.1", 18999), 0.2); s.close()' \
    >/dev/null 2>&1; then
    rpc_ready=1
    break
  fi
  sleep 0.1
done
[[ "$rpc_ready" -eq 1 ]] || fail "isolated Solana RPC stub did not start"

fleet_labels=(planner revalidator executor confirmer reconciler priority-provisioner)
fleet_commands=(
  "target/release/fleet-opportunity-planner --once --poll-interval-seconds 1 --full-sweep-interval-seconds 30 --dirty-batch-size 256 --max-opportunities-per-wave 128"
  "target/release/same-mint-reserve-swap --fleet-worker revalidate --once --concurrency 16 --fused-execute-concurrency 8 --poll-interval-milliseconds 250"
  "target/release/same-mint-reserve-swap --fleet-worker execute --once --concurrency 4 --poll-interval-milliseconds 250"
  "target/release/fleet-route-confirmer --execute --once --batch-size 128 --broadcast-concurrency 16 --poll-interval-milliseconds 1000"
  "target/release/same-mint-reserve-swap --fleet-reconciler --once --concurrency 64 --batch-size 32 --poll-interval-milliseconds 250 --position-sweep-interval-seconds 300"
  "target/release/route-lookup-table-provisioner --cluster localnet --execute --max-operations 32 --concurrency 8 --rate-limit-ms 250"
)

for index in "${!fleet_labels[@]}"; do
  label="${fleet_labels[$index]}"
  command="${fleet_commands[$index]}"
  sh -c "$command" >"$evidence_dir/$label.log" 2>&1 &
  fleet_pids+=("$!")
done

# The execute lane intentionally retains its production concurrency of four.
# Draining 2,080 individually fenced terminal transitions is expected to take
# longer than the revalidator's 16-way lane, so gate the full cohort at six
# minutes rather than inflating concurrency beyond the deployed topology.
deadline=$((SECONDS + 360))
while :; do
  running=0
  for pid in "${fleet_pids[@]}"; do
    if kill -0 "$pid" >/dev/null 2>&1; then
      running=$((running + 1))
    fi
  done
  [[ "$running" -eq 0 ]] && break
  if [[ "$SECONDS" -ge "$deadline" ]]; then
    fail "$running fleet role(s) did not finish their one-shot pass within 360 seconds"
  fi
  sleep 1
done

fleet_failed=0
for index in "${!fleet_pids[@]}"; do
  pid="${fleet_pids[$index]}"
  label="${fleet_labels[$index]}"
  if ! wait "$pid"; then
    echo "FAIL: $label exited nonzero; see $evidence_dir/$label.log" >&2
    fleet_failed=1
  fi
done
[[ "$fleet_failed" -eq 0 ]] || exit 1
fleet_pids=()

for label in "${fleet_labels[@]}"; do
  if rg --quiet 'fatal|panicked|transition_failed|join_failed' "$evidence_dir/$label.log"; then
    fail "$label emitted a fatal worker condition"
  fi
done
unexpected_rpc_methods="$(rg -v '^getGenesisHash$' "$evidence_dir/rpc-stub.log" || true)"
[[ -z "$unexpected_rpc_methods" ]] || fail \
  "fleet startup attempted an unexpected RPC method"
genesis_request_count="$(rg --count '^getGenesisHash$' "$evidence_dir/rpc-stub.log" || true)"
[[ "${genesis_request_count:-0}" -ge 5 ]] || fail \
  "expected at least five localnet genesis checks, observed ${genesis_request_count:-0}"
rg --quiet '"status":"fleet_worker_healthy"' "$evidence_dir/revalidator.log" || \
  fail "revalidator did not emit its one-shot health result"
rg --quiet '"status":"fleet_worker_healthy"' "$evidence_dir/executor.log" || \
  fail "executor did not emit its one-shot health result"
rg --quiet '"status":"fleet_reconciler_healthy"' "$evidence_dir/reconciler.log" || \
  fail "reconciler did not emit its one-shot health result"
IFS='|' read -r process_load_failed process_load_leased process_load_terminal_reason <<<"$(
  psql -X -At "$database_url" -c \
    "SELECT count(*) FILTER (WHERE opportunity_state = 'failed'), count(*) FILTER (WHERE opportunity_state = 'leased'), count(*) FILTER (WHERE terminal_reason IS NOT NULL) FROM loyal_yield.rebalance_opportunities WHERE idempotency_key LIKE 'ask1973-process-load-%'"
)"
[[ "$process_load_failed" == "4160" ]] || \
  fail "expected all 4,160 process-load jobs to fail closed, observed $process_load_failed"
[[ "$process_load_leased" == "0" ]] || \
  fail "$process_load_leased process-load leases remained after worker exit"
[[ "$process_load_terminal_reason" == "4160" ]] || \
  fail "expected a terminal reason for all process-load jobs, observed $process_load_terminal_reason"
echo "PASS: all six production roles exited cleanly and real workers terminalized 4,160 fail-closed jobs"
echo

echo "== Fleet health-poll load and contention verification"
TMPDIR="$runtime_tmp_root" bash scripts/verify-fleet-health-poll-contention.sh \
  >"$evidence_dir/health-poll-contention.log" 2>&1
rg --fixed-strings --quiet "PASS: fleet health-poll contention verification" \
  "$evidence_dir/health-poll-contention.log" || \
  fail "fleet health-poll contention verifier did not report PASS"
echo "PASS: production-shaped health-poll load stayed interval-paced"
echo

echo "PASS: ASK-1973 isolated fleet E2E verification"
echo "evidence directory: $evidence_dir"
