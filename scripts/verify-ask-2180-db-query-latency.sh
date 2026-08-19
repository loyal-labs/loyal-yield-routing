#!/usr/bin/env bash
set -euo pipefail

# ASK-2180 is intentionally a local-only verifier. It never reads a
# production URL: all SQL below is sent through a private Unix socket owned by
# the temporary postmaster created by this script.
repo_root="$(cd "$(dirname "$0")/.." && pwd)"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[[ $# -eq 0 ]] || fail "this verifier does not accept a database URL"
for forbidden_env in NEON_DATABASE_URL DATABASE_URL FLEET_VERIFY_DATABASE_URL; do
  [[ -z "${!forbidden_env:-}" ]] || fail "$forbidden_env is set; refusing to run against a supplied database"
done

epochs="${ASK2180_EPOCHS:-400000}"
opportunities="${ASK2180_OPPORTUNITIES:-450000}"
submissions="${ASK2180_SUBMISSIONS:-5000}"
default_scale=1
[[ "$epochs" == 400000 && "$opportunities" == 450000 && "$submissions" == 5000 ]] || default_scale=0
[[ "$epochs" =~ ^[0-9]+$ && "$opportunities" =~ ^[0-9]+$ && "$submissions" =~ ^[0-9]+$ ]] ||
  fail "ASK2180 row-count overrides must be unsigned integers"
(( epochs >= 10 && opportunities >= 5000 && submissions >= 10 && submissions <= 5000 )) ||
  fail "row-count overrides are too small to exercise the query"

for command_name in awk cargo initdb jq pg_ctl psql rg sed; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

tmp_root="${TMPDIR:-/tmp}"
scratch_dir="$(mktemp -d "$tmp_root/ask-2180-db-verify.XXXXXX")"
data_dir="$scratch_dir/data"
socket_dir="$scratch_dir/socket"
port="$((57432 + RANDOM % 1000))"
server_started=0
database_name="ask_2180_verify"

cleanup() {
  if [[ "$server_started" -eq 1 ]]; then
    pg_ctl -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

mkdir -p "$socket_dir"
initdb -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
pg_ctl -D "$data_dir" \
  -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1 -c shared_memory_type=mmap -c dynamic_shared_memory_type=posix -c shared_buffers=128MB" \
  -w start >/dev/null
server_started=1

psql_local() {
  PGOPTIONS="${PGOPTIONS:-} -c client_min_messages=warning" psql -X --no-psqlrc --set=ON_ERROR_STOP=1 \
    --host="$socket_dir" --port="$port" --username="$(id -un)" "$database_name" "$@"
}

psql -X --no-psqlrc --set=ON_ERROR_STOP=1 \
  --host="$socket_dir" --port="$port" --username="$(id -un)" \
  --dbname=postgres --command="CREATE DATABASE $database_name" >/dev/null
echo "== Apply real Yield migrations 1 through 40"
for migration_version in $(seq 1 40); do
  migration_file="$repo_root/crates/loyal-yield-store/migrations/$(printf '%04d' "$migration_version")_*.sql"
  migration_file="$(printf '%s\n' $migration_file)"
  [[ -f "$migration_file" ]] || fail "missing migration $migration_version"
  if [[ "$migration_version" -eq 13 ]]; then
    # The production runner applies this same nullable-regclass compatibility
    # rewrite for a blank database without changing the migration checksum.
    compatible_file="$scratch_dir/migration-0013.sql"
    sed \
      -e "s/'loyal_yield\.user_yield_positions'::regclass/to_regclass('loyal_yield.user_yield_positions')/g" \
      -e "s/'loyal_yield\.user_yield_position_holding_events'::regclass/to_regclass('loyal_yield.user_yield_position_holding_events')/g" \
      -e "s/'loyal_yield\.earn_deposit_onboarding_attempts'::regclass/to_regclass('loyal_yield.earn_deposit_onboarding_attempts')/g" \
      "$migration_file" >"$compatible_file"
    if [[ "$migration_version" -eq 15 ]]; then
      psql_local --single-transaction --file="$compatible_file" >/dev/null
    else
      psql_local --file="$compatible_file" >/dev/null
    fi
  else
    if [[ "$migration_version" -eq 15 ]]; then
      psql_local --single-transaction --file="$migration_file" >/dev/null
    else
      psql_local --file="$migration_file" >/dev/null
    fi
  fi
  if (( migration_version == 1 || migration_version % 10 == 0 )); then
    echo "applied migration $migration_version"
  fi
done

echo "== Seed production-shaped fleet data"
psql_local >/dev/null <<SQL
SET session_replication_role = replica;

INSERT INTO loyal_yield.route_policies (
    settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
    threshold, last_seen_slot, last_seen_signature
)
VALUES ('ask-2180-settings', 'ask-2180-authority', 1, 'ask-2180-policy', 0,
        'ask-2180-vault-policy', 1, 1, 'ask-2180-policy-signature')
ON CONFLICT (policy_account) DO NOTHING;

INSERT INTO loyal_yield.managed_vaults (
    settings, vault_index, vault_pubkey, active_policy_id
)
SELECT 'ask-2180-settings', g, 'ask-2180-vault-' || g,
       (SELECT id FROM loyal_yield.route_policies WHERE policy_account = 'ask-2180-policy')
FROM generate_series(0, 5000) AS series(g)
ON CONFLICT (settings, vault_index, vault_pubkey) DO NOTHING;

INSERT INTO loyal_yield.fleet_planning_clusters (cluster)
VALUES ('mainnet-beta')
ON CONFLICT (cluster) DO NOTHING;

INSERT INTO loyal_yield.fleet_planning_state (
    cluster, full_sweep_started_at, full_sweep_completed_at,
    optimizer_epoch_key, optimizer_epoch_expires_at, complete_frontier,
    observed_vault_count, opportunity_count, selected_count, deferred_count
)
VALUES (
    'mainnet-beta', now() - interval '5 minutes', now() - interval '4 minutes',
    'ask-2180-epoch-' || $epochs, now() + interval '1 hour', TRUE,
    5000, $opportunities, 1000, 0
)
ON CONFLICT (cluster) DO NOTHING;

INSERT INTO loyal_yield.optimizer_epochs (
    cluster, epoch_key, market_slot, observed_at, expires_at, market_state
)
SELECT 'mainnet-beta', 'ask-2180-epoch-' || g, 100000000 + g,
       now() - (($epochs - g)::double precision * interval '1 second'),
       now() - (($epochs - g)::double precision * interval '1 second') + interval '1 hour',
       jsonb_build_object('source', 'ask-2180-local', 'sequence', g)
FROM generate_series(1, $epochs) AS series(g);

INSERT INTO loyal_yield.rebalance_decisions (
    vault_id, status, target_reserve, target_liquidity_mint,
    estimated_cost_lamports, decision_reason, idempotency_key
)
VALUES
    (1, 'confirmed', 'ask-2180-target-1', 'ask-2180-mint', 1,
     'target_supply_apy_exceeds_source', 'ask-2180-decision-1'),
    (2, 'failed', 'ask-2180-target-2', 'ask-2180-mint', 1,
     'no_value_source', 'ask-2180-decision-2');

INSERT INTO loyal_yield.rebalance_opportunities (
    cluster, idempotency_key, rediscovery_key, vault_id, optimizer_epoch_id,
    route_fingerprint, requirements_fingerprint, source_reserve, target_reserve,
    liquidity_mint, source_liquidity_mint, target_liquidity_mint, amount_raw,
    principal_usd_micros, source_apy_bps,
    target_apy_bps, estimated_edge_bps, annual_yield_gain_usd_micros,
    expected_net_gain_usd_micros, economic_priority, scheduler_priority_anchor,
    priority_version,
    opportunity_state, execution_plan, available_at, expires_at, lease_kind,
    lease_owner, lease_expires_at, decision_id, created_at, updated_at
)
SELECT
    'mainnet-beta', 'ask-2180-opportunity-' || g, 'ask-2180-rediscovery-' || g,
    CASE WHEN g > $opportunities - 5000
         THEN g - ($opportunities - 5000)
         ELSE 5000 END,
    CASE WHEN g > $opportunities - 5000 THEN $epochs
         ELSE ((g - 1) % ($epochs - 1)) + 1 END,
    'ask-2180-route-' || g, 'ask-2180-requirements-' || g,
    'ask-2180-source-' || g, 'ask-2180-target-' || g, 'ask-2180-mint',
    'ask-2180-mint', 'ask-2180-mint',
    1000 + g, 1000000 + g, 100, 500, 25, 10000 + g,
    9000 + g, 100000 + g, 100000 + g, 'ask-2180',
    CASE
      WHEN g > $opportunities - 5000 AND g <= $opportunities - 4000 THEN 'ready'
      WHEN g > $opportunities - 4000 AND g <= $opportunities - 3000 THEN 'revalidate'
      WHEN g > $opportunities - 3000 AND g <= $opportunities - 2000 THEN 'leased'
      WHEN g > $opportunities - 2000 AND g <= $opportunities - 1000 THEN 'waiting_alt'
      WHEN g = $opportunities - 999 THEN 'decision_created'
      WHEN g = $opportunities - 998 THEN 'completed'
      WHEN g % 4 = 0 THEN 'stale'
      WHEN g % 4 = 1 THEN 'superseded'
      WHEN g % 4 = 2 THEN 'failed'
      ELSE 'cancelled'
    END,
    '{}'::jsonb, now() - interval '1 hour', now() + interval '1 day',
    CASE WHEN g > $opportunities - 3000 AND g <= $opportunities - 2000
         THEN 'execute' ELSE NULL END,
    CASE WHEN g > $opportunities - 3000 AND g <= $opportunities - 2000
         THEN 'ask-2180-owner' ELSE NULL END,
    CASE WHEN g > $opportunities - 3000 AND g <= $opportunities - 2000
         THEN now() + interval '10 minutes' ELSE NULL END,
    CASE WHEN g = $opportunities - 999 THEN 1
         WHEN g = $opportunities - 998 THEN 2 ELSE NULL END,
    now() - ((g % 10000)::double precision * interval '1 second'), now()
FROM generate_series(1, $opportunities) AS series(g);

INSERT INTO loyal_yield.orchestration_outbox (
    cluster, event_kind, aggregate_kind, aggregate_id, dedupe_key, payload
)
SELECT 'mainnet-beta', 'rebalance_opportunity_changed', 'rebalance_opportunity',
       g, 'ask-2180-outbox-' || g, jsonb_build_object('id', g)
FROM generate_series(1, 1000) AS series(g);

INSERT INTO loyal_yield.signed_route_submissions (
    cluster, semantic_key, opportunity_id, decision_id, signed_transaction,
    signed_transaction_hash, message_hash, transaction_signature,
    recent_blockhash, last_valid_block_height, optimizer_epoch_id,
    alt_requirements_fingerprint, alt_selection_fingerprint, alt_mutation_epochs,
    fee_payer, compiled_fee_lamports, writable_account_keys,
    conflict_account_keys, executor_owner, executor_fencing_token,
    submission_state, submitted_at, confirmed_at, created_at, updated_at
)
SELECT
    'mainnet-beta', 'ask-2180-submission-' || g,
    $opportunities - 5000 + g,
    1, decode('01', 'hex'), 'ask-2180-tx-hash-' || g,
    'ask-2180-message-hash-' || g, 'ask-2180-signature-' || g,
    'ask-2180-blockhash-' || g, 100000000 + g, $epochs,
    'ask-2180-alt-requirements', 'ask-2180-alt-selection', '{}'::jsonb,
    'ask-2180-payer-' || g,
    5000 + g, ARRAY['ask-2180-payer-' || g, 'ask-2180-target-' || g],
    ARRAY['ask-2180-conflict-' || g, 'ask-2180-shared'], 'ask-2180-executor', g,
    CASE (g % 9)
      WHEN 0 THEN 'signed' WHEN 1 THEN 'submitted' WHEN 2 THEN 'confirmed'
      WHEN 3 THEN 'reconciliation_pending' WHEN 4 THEN 'expiry_check_pending'
      WHEN 5 THEN 'effect_ambiguous' WHEN 6 THEN 'reconciled'
      WHEN 7 THEN 'expired' ELSE 'failed'
    END,
    CASE WHEN g % 9 IN (1, 2) THEN now() - interval '30 seconds' ELSE NULL END,
    CASE WHEN g % 9 = 2 THEN now() - interval '10 seconds' ELSE NULL END,
    now() - ((g % 1000)::double precision * interval '1 second'), now()
FROM generate_series(1, $submissions) AS series(g);

SET session_replication_role = origin;

ANALYZE loyal_yield.optimizer_epochs;
ANALYZE loyal_yield.rebalance_opportunities;
ANALYZE loyal_yield.signed_route_submissions;
ANALYZE loyal_yield.orchestration_outbox;
SQL

epoch_rows="$(psql_local --tuples-only --no-align --command='SELECT count(*) FROM loyal_yield.optimizer_epochs')"
opportunity_rows="$(psql_local --tuples-only --no-align --command='SELECT count(*) FROM loyal_yield.rebalance_opportunities')"
submission_rows="$(psql_local --tuples-only --no-align --command='SELECT count(*) FROM loyal_yield.signed_route_submissions')"
echo "row counts: optimizer_epochs=$epoch_rows rebalance_opportunities=$opportunity_rows signed_route_submissions=$submission_rows"
[[ "$epoch_rows" -eq "$epochs" && "$opportunity_rows" -eq "$opportunities" && "$submission_rows" -eq "$submissions" ]] ||
  fail "seed row counts do not match requested production-shaped data"

status_query="SELECT * FROM loyal_yield.fleet_orchestration_status WHERE cluster = 'mainnet-beta' ORDER BY opportunity_state NULLS LAST"
congestion_query="WITH active_submission AS (SELECT submission.id, submission.opportunity_id, submission.fee_payer, submission.writable_account_keys FROM loyal_yield.signed_route_submissions submission WHERE submission.cluster = 'mainnet-beta' AND submission.decision_id IS NOT NULL AND submission.submission_state IN ('signed', 'submitted', 'confirmed') UNION ALL SELECT submission.id, submission.opportunity_id, submission.fee_payer, submission.writable_account_keys FROM loyal_yield.signed_route_submissions submission WHERE submission.cluster = 'mainnet-beta' AND submission.decision_id IS NOT NULL AND submission.submission_state IN ('reconciliation_pending', 'expiry_check_pending', 'effect_ambiguous')), physical_write AS (SELECT submission.id AS submission_id, writable.writable_account_key, CASE WHEN writable.writable_account_key = submission.fee_payer THEN 0 WHEN writable.writable_account_key = opportunity.target_reserve THEN 1 ELSE 2 END AS classification_rank, opportunity.principal_usd_micros, opportunity.annual_yield_gain_usd_micros FROM active_submission submission JOIN loyal_yield.rebalance_opportunities opportunity ON opportunity.id = submission.opportunity_id CROSS JOIN LATERAL unnest(submission.writable_account_keys) AS writable(writable_account_key)), congestion AS (SELECT writable_account_key, min(classification_rank) AS classification_rank, count(*)::BIGINT AS active_submission_count, COALESCE(sum(principal_usd_micros), 0)::BIGINT AS principal_usd_micros, COALESCE((sum(annual_yield_gain_usd_micros) / 8760)::BIGINT, 0)::BIGINT AS recoverable_yield_usd_micros_per_hour FROM physical_write GROUP BY writable_account_key) SELECT writable_account_key, classification_rank, active_submission_count, principal_usd_micros, recoverable_yield_usd_micros_per_hour, count(*) OVER ()::BIGINT AS total_active_physical_writable_key_count FROM congestion ORDER BY active_submission_count DESC, recoverable_yield_usd_micros_per_hour DESC, principal_usd_micros DESC, writable_account_key LIMIT 16"

optimized_explain() {
  psql_local --tuples-only --no-align \
    --command="EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) $status_query"
}

canonical_query="SELECT jsonb_agg((to_jsonb(status) - ARRAY['oldest_age_seconds','oldest_state_age_seconds','oldest_pending_submission_age_seconds','oldest_sender_state_age_seconds','oldest_confirmer_state_age_seconds','oldest_reconciler_state_age_seconds','planner_last_seen_age_seconds','full_sweep_age_seconds','latest_market_epoch_age_seconds','latest_market_epoch_expires_in_seconds','oldest_waiting_alt_state_age_seconds','oldest_ready_state_age_seconds']) ORDER BY status.opportunity_state NULLS LAST)::text FROM ($status_query) AS status"

baseline_result="$scratch_dir/baseline-result.json"
baseline_times="$scratch_dir/baseline-times"
baseline_plan="$scratch_dir/baseline-plan.json"
echo "== Baseline exact fleet health query before migrations 41-44"
psql_local --tuples-only --no-align --command="$canonical_query" >"$baseline_result"
psql_local --tuples-only --no-align --command="$congestion_query" >"$scratch_dir/baseline-congestion.txt"
psql_local --tuples-only --no-align --command="EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) $status_query" >"$baseline_plan"
for sample in 1 2 3 4 5; do
  psql_local --tuples-only --no-align --command="EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) $status_query" >"$scratch_dir/baseline-$sample.json"
  jq -r '.[0]["Execution Time"]' "$scratch_dir/baseline-$sample.json" >>"$baseline_times"
done
baseline_median="$(sort -n "$baseline_times" | awk 'NR == 3 { print }')"
baseline_plan_text="$(cat "$baseline_plan")"
[[ "$baseline_plan_text" == *'"Seq Scan"'* && "$baseline_plan_text" == *'optimizer_epochs'* && "$baseline_plan_text" == *'rebalance_opportunities'* ]] ||
  fail "baseline plan did not show both historical table scans"
echo "baseline warm execution samples (ms): $(tr '\n' ' ' <"$baseline_times")"

echo "== Apply ASK-2180 migrations 41-44"
for migration_version in 41 42 43 44; do
  migration_file="$(printf '%s\n' "$repo_root/crates/loyal-yield-store/migrations/$(printf '%04d' "$migration_version")_"*.sql)"
  [[ -f "$migration_file" ]] || fail "missing migration $migration_version"
  psql_local --file="$migration_file" >/dev/null
done
psql_local --command='VACUUM (ANALYZE) loyal_yield.optimizer_epochs' >/dev/null
psql_local --command='VACUUM (ANALYZE) loyal_yield.rebalance_opportunities' >/dev/null

echo "== Verify canonical result and optimized plan"
optimized_result="$scratch_dir/optimized-result.json"
optimized_times="$scratch_dir/optimized-times"
optimized_plan="$scratch_dir/optimized-plan.json"
psql_local --tuples-only --no-align --command="$canonical_query" >"$optimized_result"
cmp -s "$baseline_result" "$optimized_result" || fail "optimized canonical result differs from baseline"
for sample in 1 2 3 4 5; do
  optimized_explain >"$scratch_dir/optimized-$sample.json"
  jq -r '.[0]["Execution Time"]' "$scratch_dir/optimized-$sample.json" >>"$optimized_times"
done
optimized_explain >"$optimized_plan"
optimized_median="$(sort -n "$optimized_times" | awk 'NR == 3 { print }')"
optimized_plan_text="$(cat "$optimized_plan")"
for required_index in optimizer_epochs_latest_cluster_idx rebalance_opportunities_optimizer_epoch_idx rebalance_opportunities_health_aggregate_idx; do
  index_state="$(psql_local --tuples-only --no-align --command="SELECT (indisready AND indisvalid)::text FROM pg_index WHERE indexrelid = to_regclass('loyal_yield.$required_index')" | tr -d '[:space:]')"
  [[ "$index_state" == true ]] || fail "required index $required_index is not ready and valid (state=$index_state)"
done
[[ "$optimized_plan_text" == *'optimizer_epochs_latest_cluster_idx'* ]] || fail "optimized plan does not use latest-epoch index"
[[ "$optimized_plan_text" == *'rebalance_opportunities_optimizer_epoch_idx'* ]] || fail "optimized plan does not use current-epoch opportunity index"
[[ "$optimized_plan_text" == *'rebalance_opportunities_health_aggregate_idx'* ]] || fail "optimized plan does not use covering health aggregate index"
optimized_seq_scan_relations="$(jq -r '.. | objects | select(."Node Type"? == "Seq Scan") | ."Relation Name"?' "$optimized_plan")"
if printf '%s\n' "$optimized_seq_scan_relations" | rg -q '^(optimizer_epochs|rebalance_opportunities)$'; then
  fail "optimized plan still contains a historical optimizer/opportunity sequential scan"
fi

improvement="$(awk -v before="$baseline_median" -v after="$optimized_median" 'BEGIN { printf "%.2f", (before - after) * 100 / before }')"
echo "optimized warm execution samples (ms): $(tr '\n' ' ' <"$optimized_times")"
echo "medians: baseline_ms=$baseline_median optimized_ms=$optimized_median improvement_percent=$improvement"
echo "plan evidence: baseline=historical optimizer/opportunity Seq Scan; optimized=latest-epoch, current-epoch, and covering aggregate indexes with no Seq Scan"

awk -v median="$optimized_median" 'BEGIN { exit !(median < 1000) }' || fail "optimized median is not below 1000 ms"
awk -v improvement="$improvement" 'BEGIN { exit !(improvement >= 50) }' || fail "optimized median is not at least 50 percent lower"
(( default_scale == 1 )) || fail "reduced iteration overrides are diagnostic only; PASS requires production-scale defaults"

if [[ "${ASK2180_SKIP_REPO_CHECKS:-0}" != 1 ]]; then
  echo "== Full migration runner and focused repository checks"
  full_database="fleet_verify_ask_2180_full_runner"
  psql -X --no-psqlrc --set=ON_ERROR_STOP=1 --host="$socket_dir" --port="$port" \
    --username="$(id -un)" --dbname=postgres --command="CREATE DATABASE $full_database" >/dev/null
  full_url="postgresql://$(id -un)@127.0.0.1:$port/$full_database?host=$socket_dir"
  NEON_DATABASE_URL="$full_url" cargo run --quiet -p loyal-yield-orchestrator --bin yield-migrations -- --apply
  cargo test -p loyal-yield-store --lib
  FLEET_VERIFY_DATABASE_URL="$full_url" \
    cargo test -p loyal-yield-store --test fleet_health_projection_advisory_lock
  cargo check -p loyal-yield-orchestrator --bin fleet-health-projector
  cargo fmt --all -- --check
  git -C "$repo_root" diff --check
fi

echo "PASS: ASK-2180 local production-scale database query latency verifier"
