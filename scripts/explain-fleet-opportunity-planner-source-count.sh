#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
observation_source="$repo_root/crates/loyal-yield-orchestrator/src/fleet_orchestration/observation.rs"

: "${NEON_DATABASE_URL:?NEON_DATABASE_URL is required}"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

for command_name in awk cargo jq perl psql; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

enabled_mints="${EARN_ROUTER_ENABLED_STABLE_MINTS:-EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v}"
cluster="${YIELD_ALT_CLUSTER:-mainnet-beta}"
cross_mint="${EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER:-false}"
signer="62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"

query="$({
  perl -0777 -ne '
    if (/async fn load_fleet_sources\(.*?let row(?:_result)? = crate::sqlx::query\(\s*r#"(.*?)"#,/s) {
      print $1;
    }
  ' "$observation_source"
})"

test -n "$query" || fail "could not extract load_fleet_sources SQL"

explain_json="$({
  printf '%s\n' 'BEGIN READ ONLY;'
  printf '%s\n' "SET LOCAL statement_timeout = '30s';"
  printf '%s\n' "SET LOCAL lock_timeout = '2s';"
  printf '%s\n' 'PREPARE planner_source_count(TEXT, TEXT[], TEXT, TEXT[], BIGINT, TIMESTAMPTZ, TEXT, BIGINT[], BOOLEAN) AS'
  printf '%s\n' "$query"
  printf '%s\n' ';'
  printf '%s\n' 'EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS, FORMAT JSON)'
  printf '%s\n' 'EXECUTE planner_source_count('
  printf '%s\n' "  :'signer',"
  printf '%s\n' "  string_to_array(:'enabled_mints', ',')::TEXT[],"
  printf '%s\n' "  'same_mint_kamino',"
  printf '%s\n' "  ARRAY['planned', 'simulating', 'ready', 'submitted', 'confirming']::TEXT[],"
  printf '%s\n' '  300::BIGINT,'
  printf '%s\n' '  clock_timestamp(),'
  printf '%s\n' "  :'cluster',"
  printf '%s\n' '  NULL::BIGINT[],'
  printf '%s\n' "  :'cross_mint'::BOOLEAN"
  printf '%s\n' ');'
  printf '%s\n' 'ROLLBACK;'
} | psql "$NEON_DATABASE_URL" \
  --set=ON_ERROR_STOP=1 \
  --set=signer="$signer" \
  --set=enabled_mints="$enabled_mints" \
  --set=cluster="$cluster" \
  --set=cross_mint="$cross_mint" \
  --no-psqlrc \
  --quiet \
  --tuples-only \
  --no-align)"

execution_millis="$(jq -r '.[0]["Execution Time"]' <<<"$explain_json")"
temp_read_blocks="$(jq -r '.[0].Plan["Temp Read Blocks"]' <<<"$explain_json")"
maximum_sources_scan_loops="$(
  jq -r \
    '[.. | objects | select(.["CTE Name"]? == "sources") | .["Actual Loops"]] | max // 0' \
    <<<"$explain_json"
)"

awk -v value="$execution_millis" 'BEGIN { exit !(value < 800) }' ||
  fail "execution time ${execution_millis}ms is not below 800ms"
awk -v value="$temp_read_blocks" 'BEGIN { exit !(value < 100000) }' ||
  fail "temp reads ${temp_read_blocks} blocks are not below 100000"
awk -v value="$maximum_sources_scan_loops" 'BEGIN { exit !(value <= 1) }' ||
  fail "sources CTE is still rescanned in a loop (${maximum_sources_scan_loops} loops)"

planner_json="$(
  RUST_LOG=error cargo run --quiet \
    -p loyal-yield-orchestrator \
    --bin fleet-opportunity-planner \
    -- \
    --once \
    --dry-run \
    --json
)"

jq -e '
  .mutating == false
  and .childProcessesSpawned == 0
  and .observation.completeVaultAccounting == true
' <<<"$planner_json" >/dev/null ||
  fail "live planner dry run did not preserve read-only complete-vault accounting"

eligible_vaults="$(jq -r '.observation.eligibleVaultCount' <<<"$planner_json")"
active_exclusions="$(jq -r '.observation.activeOpportunityVaultsExcluded' <<<"$planner_json")"
source_candidates="$(jq -r '.observation.sourceCandidateVaultCount' <<<"$planner_json")"
no_source_vaults="$(jq -r '.observation.noPositiveCurrentSourceVaultCount' <<<"$planner_json")"
derived_no_source_vaults="$((eligible_vaults - active_exclusions - source_candidates))"

[[ "$no_source_vaults" == "$derived_no_source_vaults" ]] ||
  fail "live no-source count ${no_source_vaults} does not match partition remainder ${derived_no_source_vaults}"

jq -n \
  --argjson executionMillis "$execution_millis" \
  --argjson tempReadBlocks "$temp_read_blocks" \
  --argjson maximumSourcesScanLoops "$maximum_sources_scan_loops" \
  --argjson eligibleVaults "$eligible_vaults" \
  --argjson activeExclusions "$active_exclusions" \
  --argjson sourceCandidates "$source_candidates" \
  --argjson noSourceVaults "$no_source_vaults" \
  '{
    verdict: "PASS",
    readOnly: true,
    executionMillis: $executionMillis,
    tempReadBlocks: $tempReadBlocks,
    maximumSourcesScanLoops: $maximumSourcesScanLoops,
    livePartition: {
      eligibleVaults: $eligibleVaults,
      activeExclusions: $activeExclusions,
      sourceCandidates: $sourceCandidates,
      noSourceVaults: $noSourceVaults
    }
  }'
