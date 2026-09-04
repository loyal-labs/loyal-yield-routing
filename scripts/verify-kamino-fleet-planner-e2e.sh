#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scratch="$(mktemp -d /tmp/kamino-fleet-planner-e2e.XXXXXX)"
data="$scratch/postgres"
socket="$scratch/socket"
port="$((59600 + RANDOM % 200))"
server_started=0

fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() {
  if [[ "$server_started" == 1 ]]; then
    pg_ctl -D "$data" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch"
}
trap cleanup EXIT

for command_name in go cargo initdb pg_ctl createdb psql jq; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

echo "== Pre-cutover planner slice: Go process only"
echo "Not invoked by this verifier: kamino-reserve-monitor and fleet-opportunity-planner"
echo "Boundary under test: existing PostgreSQL optimizer_epochs/rebalance_opportunities schema"
echo "Boundary includes durable revalidate and atomic prepared leased-execute handoff"
echo "== Rust/Go immutable market epoch parity"
"$root/scripts/verify-kamino-market-epoch-parity.sh"
echo "== Go verifier-first checks"
cd "$root/go/kamino-fleet-planner"
go test ./...
go test -race ./internal/fleet -run 'Test(Decode|Plan|EconomicKey|Snapshot|ImmutableMarketEpoch|ValidateJupiter|CrossMint|JupiterFetch|Token2022)'
echo "PASS: deterministic planner, per-mint epoch isolation, frozen KLend decoder, and adversarial slot fences"

echo "== Disposable PostgreSQL"
mkdir -p "$socket"
initdb -D "$data" -A trust --no-locale -E UTF8 >/dev/null
pg_ctl -D "$data" \
  -o "-F -k '$socket' -p $port -c listen_addresses=127.0.0.1" \
  -w start >/dev/null
server_started=1
createdb -h "$socket" -p "$port" fleet
database_url="postgresql://$(id -un)@127.0.0.1:$port/fleet"

cd "$root"
cargo build --locked \
  -p loyal-yield-orchestrator \
  --bin yield-migrations \
  --bin fleet-orchestration-verifier \
  --bin fleet-route-confirmer \
  --bin fleet-health-projector \
  --bin route-lookup-table-provisioner \
  -p loyal-fleet-worker \
  --bin same-mint-reserve-swap >/dev/null
set +e
migration_output="$(NEON_DATABASE_URL="$database_url" target/debug/yield-migrations --apply 2>&1)"
migration_status=$?
set -e
if [[ "$migration_status" -ne 0 ]]; then
  # 0071 is intentionally bound to one pre-existing production row and cannot
  # run on a blank database. Everything before it is transactional and remains
  # applied; the singleton cutover deliberately adds no planner migration.
  [[ "$migration_output" == *"Backyard Phase 1 canonical route cardinality drifted"* ]] || {
    echo "$migration_output" >&2
    fail "base migrations failed before the known production-bound activation"
  }
  [[ "$(psql "$database_url" -X -Atc "SELECT count(*) FROM pg_tables WHERE schemaname='loyal_yield' AND tablename IN ('optimizer_epochs','rebalance_opportunities','multiply_route_states')")" == "3" ]] ||
    fail "base durable fleet schema is incomplete"
fi
[[ "$(psql "$database_url" -X -Atc "SELECT to_regclass('loyal_yield.kamino_fleet_planner_owners') IS NULL")" == "t" ]] ||
  fail "cutover unexpectedly requires a planner-specific owner table"
echo "PASS: existing production queue schema is sufficient; no planner-specific migration was applied"

echo "== Confirmed RPC to durable W3 queue"
cd "$root/go/kamino-fleet-planner"
FLEET_TEST_DATABASE_URL="$database_url" go test ./internal/fleet \
  -run 'TestMarketEvidenceStoreLoadsRealMonitorIdentity|TestLoadMigratedFleetBuildsFinalizedCrossMintPolicyBindings|TestStoreIntegrationDurableHandoffWithoutPlannerMigration|TestWorkerIntegrationCutoverWithoutRustMonitorOrPlanner|TestRevalidationStoreIntegrationFusedExecuteIsAtomic' -count=1 -v

[[ "$(psql "$database_url" -X -Atc "SELECT count(*) > 0 AND bool_and(epoch.market_state->>'fingerprint'=epoch.epoch_key) AND bool_and((epoch.market_state->>'optimizerEpochId')::bigint>0) AND bool_and(jsonb_array_length(epoch.market_state->'reserves')>=2) AND bool_and((epoch.market_state->'mintCoverage'->0->>'complete')::boolean) AND count(*) FILTER (WHERE opportunity.opportunity_state='revalidate')>0 AND count(*) FILTER (WHERE opportunity.opportunity_state='leased' AND opportunity.lease_kind='execute' AND opportunity.execution_plan ? 'prepared_transaction')>0 FROM loyal_yield.rebalance_opportunities opportunity JOIN loyal_yield.optimizer_epochs epoch ON epoch.id=opportunity.optimizer_epoch_id")" == "t" ]] ||
  fail "durable handoff lacks typed epoch, revalidate work, or atomically prepared execute work"

echo "== Complete retained lifecycle in the same disposable PostgreSQL server"
cd "$root"
createdb -h "$socket" -p "$port" fleet_verify_lifecycle
lifecycle_database_url="postgresql://$(id -un)@127.0.0.1:$port/fleet_verify_lifecycle"
set +e
lifecycle_migration_output="$(NEON_DATABASE_URL="$lifecycle_database_url" target/debug/yield-migrations --apply 2>&1)"
lifecycle_migration_status=$?
set -e
if [[ "$lifecycle_migration_status" -ne 0 && "$lifecycle_migration_output" != *"Backyard Phase 1 canonical route cardinality drifted"* ]]; then
  echo "$lifecycle_migration_output" >&2
  fail "retained-lifecycle database migrations failed"
fi
lifecycle_artifact="$scratch/retained-lifecycle.json"
if ! target/debug/fleet-orchestration-verifier \
  --implementation \
  --json \
  --isolated-database \
  --database-url "$lifecycle_database_url" >"$lifecycle_artifact"; then
  jq '{requestedScopeStatus, firstBlockingCheck, failed: [.checks[].subchecks[]? | select(.verdict != "PASS") | {name, verdict, evidence}]}' "$lifecycle_artifact" >&2 || true
  fail "retained lifecycle verifier failed"
fi
jq -e '
  .requestedScope == "ISOLATED_DATABASE"
  and .requestedScopeStatus == "PASS"
  and .isolatedDatabase == "PASS"
  and .firstBlockingCheck == null
  and ([.checks[].subchecks[]?
    | select(.name == "signed_submission_links_decision_and_terminalizes_after_explicit_transitions" and .verdict == "PASS")]
    | length == 1)
  and ([.checks[].subchecks[]?
    | select(.name == "subscription_hint_only_accelerates_authoritative_confirmation_poll" and .verdict == "PASS")]
    | length == 1)
  and ([.checks[].subchecks[]?
    | select(.name == "reconciled_volume_counts_unique_submission_exactly_once" and .verdict == "PASS")]
    | length == 1)
' "$lifecycle_artifact" >/dev/null

for role_command in \
  "target/debug/same-mint-reserve-swap --fleet-worker execute --role-probe" \
  "target/debug/fleet-route-confirmer --role-probe" \
  "target/debug/same-mint-reserve-swap --fleet-reconciler --role-probe" \
  "target/debug/route-lookup-table-provisioner --role-probe"; do
  role_artifact="$scratch/role-$RANDOM.json"
  sh -c "$role_command" >"$role_artifact"
  jq -e '.status == "pass" and .networkAccessed == false and .databaseMutated == false and .transactionSent == false' "$role_artifact" >/dev/null
done

echo "PASS: confirmed RPC -> Go planning/revalidation -> durable revalidate and leased-execute rows"
echo "PASS: cross-mint claims use the immutable bound withdraw policy when it differs from the active base policy"
echo "PASS: blocked mint coverage does not stop healthy-mint planning and failed passes do not refresh planner health"
echo "PASS: live capacity reservations survive terminal queue state and are counted exactly once"
echo "PASS: retained executor/confirmer/reconciler lifecycle reached completed without production access"
echo "PASS: replaced Rust planner/revalidator roles were not started; retained executor, confirmer, reconciler, and ALT roles loaded without side effects"
echo "PASS: economic idempotency, active-work exclusion, restart recovery, and atomic capacity/economics fences verified"
echo "NOTE: exact route bytes and independent Rust/Go artifact parity are verified by verify-kamino-planner-revalidator-parity.sh"
