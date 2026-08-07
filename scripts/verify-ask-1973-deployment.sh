#!/usr/bin/env bash
set -euo pipefail

# Read-only two-phase deployment verification for ASK-1973.
#
# pre-deploy:
#   - binds immutable light/heavy linux/amd64 registry images to the clean
#     Blueprint and source history;
#   - runs the complete runtime collector with a disposable migrated database;
#   - requires implementation Checks 1-7 to pass;
#   - captures a source-bound production baseline without changing production.
#
# post-deploy / verify:
#   - captures or validates fresh post-cutover Render, Neon, Timescale, and
#     finalized-chain measurements;
#   - independently recomputes implementation, deployment, and production
#     performance Checks 1-11;
#   - performs a fresh ClickStack operational-error search for all six roles;
#   - succeeds only for literal END_STATE: PASS and zero forbidden log events.
#
# The script never builds or pushes an image, deploys a service, changes a
# production database, signs a transaction, or sends a transaction.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

usage() {
  cat <<'USAGE'
Usage:
  verify-ask-1973-deployment.sh pre-deploy \
    --light-image IMAGE --heavy-image IMAGE --evidence-dir DIR \
    [--container-engine docker]

  verify-ask-1973-deployment.sh post-deploy \
    --runtime-evidence FILE --baseline FILE --cutover-at RFC3339 \
    --evidence-dir DIR

  verify-ask-1973-deployment.sh verify \
    --runtime-evidence FILE --production-evidence FILE \
    --evidence-dir DIR

All evidence paths must be outside the repository. Output files must not
already exist. Credentials are read only from the environment and must not be
supplied in command arguments. post-deploy and verify require
HYPERDX_ACCESS_KEY in addition to the production collector credentials.
USAGE
}

mode="${1:-}"
case "$mode" in
  pre-deploy | post-deploy | verify) shift ;;
  --help | -h) usage; exit 0 ;;
  "") usage >&2; fail "a mode is required" ;;
  *) usage >&2; fail "unknown mode: $mode" ;;
esac

light_image=""
heavy_image=""
runtime_evidence=""
production_evidence=""
baseline=""
cutover_at=""
evidence_dir=""
container_engine="docker"

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --light-image)
      [[ "$#" -ge 2 ]] || fail "--light-image requires a value"
      light_image="$2"
      shift 2
      ;;
    --heavy-image)
      [[ "$#" -ge 2 ]] || fail "--heavy-image requires a value"
      heavy_image="$2"
      shift 2
      ;;
    --runtime-evidence)
      [[ "$#" -ge 2 ]] || fail "--runtime-evidence requires a path"
      runtime_evidence="$2"
      shift 2
      ;;
    --production-evidence)
      [[ "$#" -ge 2 ]] || fail "--production-evidence requires a path"
      production_evidence="$2"
      shift 2
      ;;
    --baseline)
      [[ "$#" -ge 2 ]] || fail "--baseline requires a path"
      baseline="$2"
      shift 2
      ;;
    --cutover-at)
      [[ "$#" -ge 2 ]] || fail "--cutover-at requires an RFC3339 value"
      cutover_at="$2"
      shift 2
      ;;
    --evidence-dir)
      [[ "$#" -ge 2 ]] || fail "--evidence-dir requires a path"
      evidence_dir="$2"
      shift 2
      ;;
    --container-engine)
      [[ "$#" -ge 2 ]] || fail "--container-engine requires docker"
      container_engine="$2"
      shift 2
      ;;
    --help | -h)
      usage
      exit 0
      ;;
    *) fail "unknown argument: $1" ;;
  esac
done

for command_name in awk cargo git jq realpath; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done
if ! command -v shasum >/dev/null && ! command -v sha256sum >/dev/null; then
  fail "shasum or sha256sum is required"
fi

[[ -n "$evidence_dir" ]] || fail "--evidence-dir is required"
mkdir -p "$evidence_dir"
evidence_dir="$(cd "$evidence_dir" && pwd -P)"
case "$evidence_dir/" in
  "$repo_root/"*) fail "evidence directory must be outside the repository" ;;
esac

[[ -z "$(git status --porcelain --untracked-files=normal)" ]] ||
  fail "deployment evidence requires a clean checkout"
head_commit="$(git rev-parse HEAD)"

sha256_file() {
  if command -v shasum >/dev/null; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

assert_external_file() {
  local path="$1"
  local label="$2"
  [[ -f "$path" ]] || fail "$label does not exist: $path"
  local absolute
  absolute="$(realpath "$path")"
  case "$absolute" in
    "$repo_root" | "$repo_root/"*) fail "$label must be outside the repository" ;;
  esac
  printf '%s\n' "$absolute"
}

require_new_output() {
  local path="$1"
  local label="$2"
  [[ ! -e "$path" ]] || fail "$label already exists: $path"
}

build_collectors() {
  cargo build --release --locked \
    -p loyal-yield-orchestrator \
    --bin yield-migrations \
    --bin fleet-opportunity-planner \
    --bin fleet-orchestration-verifier \
    --bin fleet-orchestration-runtime-evidence \
    --bin fleet-orchestration-production-evidence \
    -p loyal-fleet-worker \
    --bin same-mint-reserve-swap
  for binary in \
    yield-migrations \
    fleet-opportunity-planner \
    fleet-orchestration-verifier \
    fleet-orchestration-runtime-evidence \
    fleet-orchestration-production-evidence \
    same-mint-reserve-swap; do
    [[ -x "target/release/$binary" ]] || fail "missing collector binary: $binary"
  done
}

report_verifier_failure() {
  local output_path="$1"
  if jq -e 'type == "object"' "$output_path" >/dev/null 2>&1; then
    jq '{status,requestedScope,requestedScopeStatus,firstBlockingCheck}' \
      "$output_path" >&2
  fi
}

verify_implementation() {
  local runtime_path="$1"
  local output_path="$2"
  if ! target/release/fleet-orchestration-verifier \
    --implementation \
    --json \
    --collect-repository-evidence \
    --repository-root "$repo_root" \
    --runtime-evidence-json "$runtime_path" \
    >"$output_path"; then
    report_verifier_failure "$output_path"
    fail "ASK-1973 implementation verifier failed"
  fi
  jq -e '
    .status == "PASS"
    and .requestedScope == "IMPLEMENTATION"
    and .requestedScopeStatus == "PASS"
    and .implementation == "PASS"
    and .firstBlockingCheck == null
    and ([.checks[].id] == [1,2,3,4,5,6,7])
    and ([.checks[].verdict == "PASS"] | all)
  ' "$output_path" >/dev/null || fail "ASK-1973 implementation Checks 1-7 did not pass"
}

verify_end_state() {
  local runtime_path="$1"
  local production_path="$2"
  local output_path="$3"
  if ! target/release/fleet-orchestration-verifier \
    --end-state \
    --json \
    --collect-repository-evidence \
    --repository-root "$repo_root" \
    --runtime-evidence-json "$runtime_path" \
    --production-evidence-json "$production_path" \
    >"$output_path"; then
    report_verifier_failure "$output_path"
    fail "ASK-1973 end-state verifier failed"
  fi
  jq -e '
    .status == "PASS"
    and .requestedScope == "END_STATE"
    and .requestedScopeStatus == "PASS"
    and .implementation == "PASS"
    and .deployment == "PASS"
    and .productionPerformance == "PASS"
    and .endState == "PASS"
    and .firstBlockingCheck == null
    and ([.checks[].id] == [1,2,3,4,5,6,7,8,9,10,11])
    and ([.checks[].verdict == "PASS"] | all)
  ' "$output_path" >/dev/null || fail "ASK-1973 end-state Checks 1-11 did not pass"
}

verify_clickstack_artifact() {
  local artifact="$1"
  local expected_cutover="$2"
  jq -e --arg head "$head_commit" --arg cutover "$expected_cutover" '
    .schemaVersion == 1
    and .event == "ask_1973_clickstack_verification"
    and .status == "PASS"
    and .productionMutation == false
    and .headCommit == $head
    and .cutoverAt == $cutover
    and .endpoint == "https://loyal-clickstack.onrender.com/api/api/v2"
    and .coverage.channel == "otlp_operational_errors_only"
    and .coverage.successfulWorkProvenByThisArtifact == false
    and .coverage.successfulWorkRequiredFromProductionEndState == true
    and ([.roles[].serviceName] == [
      "loyal-fleet-opportunity-planner",
      "loyal-fleet-route-revalidator",
      "loyal-fleet-route-executor",
      "loyal-fleet-route-confirmer",
      "loyal-fleet-route-reconciler",
      "loyal-route-lookup-table-provisioner"
    ])
    and ([.roles[] | .status == "PASS" and .forbiddenEventCount == 0 and .firstForbiddenEvent == null] | all)
  ' "$artifact" >/dev/null || fail "ClickStack fleet evidence did not pass"
}

if [[ "$mode" == "pre-deploy" ]]; then
  [[ -n "$light_image" ]] || fail "pre-deploy requires --light-image"
  [[ -n "$heavy_image" ]] || fail "pre-deploy requires --heavy-image"
  [[ "$container_engine" == "docker" ]] ||
    fail "authoritative registry evidence requires --container-engine docker"
  command -v docker >/dev/null || fail "docker is required"
  [[ "$light_image" =~ ^ghcr\.io/loyal-labs/loyal-yield-routing/light-workers:sha-([0-9a-f]{40})$ ]] ||
    fail "light image must be an immutable Loyal light-workers SHA reference"
  light_commit="${BASH_REMATCH[1]}"
  [[ "$heavy_image" =~ ^ghcr\.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-([0-9a-f]{40})$ ]] ||
    fail "heavy image must be an immutable Loyal laserstream-workers SHA reference"
  heavy_commit="${BASH_REMATCH[1]}"
  [[ "$light_commit" == "$heavy_commit" ]] ||
    fail "light and heavy images must come from the same source commit"
  git merge-base --is-ancestor "$light_commit" HEAD ||
    fail "image source commit must be an ancestor of clean HEAD"
  for command_name in initdb pg_ctl createdb; do
    command -v "$command_name" >/dev/null || fail "$command_name is required"
  done

  migrations_log="$evidence_dir/pre-deploy-migrations.log"
  runtime_evidence="$evidence_dir/runtime-evidence.json"
  implementation_output="$evidence_dir/implementation-verification.json"
  baseline="$evidence_dir/production-baseline.json"
  summary="$evidence_dir/pre-deploy-summary.json"
  require_new_output "$migrations_log" "migration evidence"
  require_new_output "$runtime_evidence" "runtime evidence"
  require_new_output "$implementation_output" "implementation evidence"
  require_new_output "$baseline" "production baseline"
  require_new_output "$summary" "pre-deploy summary"

  build_collectors
  runtime_tmp_root="${ASK1973_RUNTIME_TMPDIR:-/tmp}"
  scratch_dir="$(mktemp -d "$runtime_tmp_root/ask1973-deployment-runtime.XXXXXX")"
  data_dir="$scratch_dir/postgres"
  socket_dir="$scratch_dir/socket"
  mkdir -p "$socket_dir"
  port="$((59000 + RANDOM % 500))"
  server_started=0
  cleanup() {
    if [[ "$server_started" -eq 1 ]]; then
      pg_ctl -D "$data_dir" -m immediate -w stop >/dev/null 2>&1 || true
    fi
    if [[ "$scratch_dir" == "$runtime_tmp_root/ask1973-deployment-runtime."* ]]; then
      rm -rf "$scratch_dir"
    fi
  }
  trap cleanup EXIT

  initdb -D "$data_dir" -A trust --no-locale -E UTF8 >/dev/null
  pg_ctl -D "$data_dir" \
    -o "-F -k '$socket_dir' -p $port -c listen_addresses=127.0.0.1 -c max_connections=400" \
    -w start >/dev/null
  server_started=1
  createdb -h "$socket_dir" -p "$port" fleet_verify
  database_url="postgresql://$(id -un)@127.0.0.1:$port/fleet_verify"
  NEON_DATABASE_URL="$database_url" target/release/yield-migrations --apply \
    >"$migrations_log"

  FLEET_VERIFY_DATABASE_URL="$database_url" \
    target/release/fleet-orchestration-runtime-evidence \
      --repository-root "$repo_root" \
      --image "$light_image" \
      --heavy-image "$heavy_image" \
      --container-engine "$container_engine" \
      --output "$runtime_evidence"
  jq -e --arg head "$head_commit" '
    .schemaVersion == 1
    and .headCommit == $head
    and (.runtimeSourceDigestSha256 | type == "string" and length == 64)
    and (.capturedAt | type == "string")
  ' "$runtime_evidence" >/dev/null || fail "runtime evidence is not source-bound to clean HEAD"
  verify_implementation "$runtime_evidence" "$implementation_output"

  # A pre-cutover baseline is not an end-state verdict and the collector may
  # intentionally return nonzero while still emitting a valid baseline.
  target/release/fleet-orchestration-production-evidence \
    --repository-root "$repo_root" \
    --output "$baseline" \
    --json \
    >/dev/null || [[ -s "$baseline" ]] || fail "production baseline collection failed"
  jq -e --arg head "$head_commit" '
    .schemaVersion == 1
    and .event == "fleet_orchestration_production_evidence"
    and .headCommit == $head
    and .scope.cutoverAt == null
    and .scope.baselinePathSupplied == false
    and .source.trackedWorktreeDirty == false
    and .callerVerdictsAccepted == false
  ' "$baseline" >/dev/null || fail "production baseline has the wrong source or scope contract"

  jq -n \
    --arg status "PASS" \
    --arg mode "$mode" \
    --arg headCommit "$head_commit" \
    --arg imageSourceCommit "$light_commit" \
    --arg lightImage "$light_image" \
    --arg heavyImage "$heavy_image" \
    --arg runtimeEvidence "$runtime_evidence" \
    --arg runtimeSha256 "$(sha256_file "$runtime_evidence")" \
    --arg implementationEvidence "$implementation_output" \
    --arg implementationSha256 "$(sha256_file "$implementation_output")" \
    --arg baseline "$baseline" \
    --arg baselineSha256 "$(sha256_file "$baseline")" \
    '{
      status: $status,
      mode: $mode,
      productionMutation: false,
      headCommit: $headCommit,
      imageSourceCommit: $imageSourceCommit,
      images: {light: $lightImage, heavy: $heavyImage},
      runtimeEvidence: {path: $runtimeEvidence, sha256: $runtimeSha256},
      implementationEvidence: {path: $implementationEvidence, sha256: $implementationSha256, checks: [1,2,3,4,5,6,7]},
      productionBaseline: {path: $baseline, sha256: $baselineSha256}
    }' >"$summary"
  echo "PASS: ASK-1973 pre-deploy runtime, implementation, and baseline verification"
  echo "evidence directory: $evidence_dir"
  exit 0
fi

build_collectors
[[ -n "$runtime_evidence" ]] || fail "$mode requires --runtime-evidence"
runtime_evidence="$(assert_external_file "$runtime_evidence" "runtime evidence")"

if [[ "$mode" == "post-deploy" ]]; then
  [[ -n "$cutover_at" ]] || fail "post-deploy requires --cutover-at"
  [[ "$cutover_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]+)?Z$ ]] ||
    fail "--cutover-at must be an RFC3339 UTC timestamp ending in Z"
  [[ -n "$baseline" ]] || fail "post-deploy requires --baseline"
  baseline="$(assert_external_file "$baseline" "production baseline")"
  production_evidence="$evidence_dir/production-post-deploy.json"
  require_new_output "$production_evidence" "post-deploy production evidence"
  target/release/fleet-orchestration-production-evidence \
    --repository-root "$repo_root" \
    --cutover-at "$cutover_at" \
    --baseline "$baseline" \
    --output "$production_evidence" \
    --json \
    >/dev/null || [[ -s "$production_evidence" ]] || fail "post-deploy collection failed"
else
  [[ -n "$production_evidence" ]] || fail "verify requires --production-evidence"
  production_evidence="$(assert_external_file "$production_evidence" "production evidence")"
  cutover_at="$(jq -er '.scope.cutoverAt | select(type == "string")' "$production_evidence")" ||
    fail "production evidence has no cutover timestamp"
fi

jq -e --arg head "$head_commit" --arg cutover "$cutover_at" '
  .schemaVersion == 1
  and .event == "fleet_orchestration_production_evidence"
  and .headCommit == $head
  and .scope.cutoverAt == $cutover
  and .scope.baselinePathSupplied == true
  and .source.trackedWorktreeDirty == false
  and .callerVerdictsAccepted == false
' "$production_evidence" >/dev/null || fail "post-deploy evidence has the wrong source or scope contract"

end_state_output="$evidence_dir/end-state-verification.json"
clickstack_output="$evidence_dir/clickstack-verification.json"
summary="$evidence_dir/deployment-summary.json"
require_new_output "$end_state_output" "end-state evidence"
require_new_output "$clickstack_output" "ClickStack evidence"
require_new_output "$summary" "deployment summary"

verify_end_state "$runtime_evidence" "$production_evidence" "$end_state_output"
scripts/verify-ask-1973-clickstack.sh \
  --cutover-at "$cutover_at" \
  --expected-head "$head_commit" \
  --output "$clickstack_output"
verify_clickstack_artifact "$clickstack_output" "$cutover_at"

jq -n \
  --arg status "PASS" \
  --arg mode "$mode" \
  --arg headCommit "$head_commit" \
  --arg runtimeEvidence "$runtime_evidence" \
  --arg runtimeSha256 "$(sha256_file "$runtime_evidence")" \
  --arg productionEvidence "$production_evidence" \
  --arg productionSha256 "$(sha256_file "$production_evidence")" \
  --arg endStateEvidence "$end_state_output" \
  --arg endStateSha256 "$(sha256_file "$end_state_output")" \
  --arg clickstackEvidence "$clickstack_output" \
  --arg clickstackSha256 "$(sha256_file "$clickstack_output")" \
  '{
    status: $status,
    mode: $mode,
    productionMutation: false,
    headCommit: $headCommit,
    runtimeEvidence: {path: $runtimeEvidence, sha256: $runtimeSha256},
    productionEvidence: {path: $productionEvidence, sha256: $productionSha256},
    endStateEvidence: {path: $endStateEvidence, sha256: $endStateSha256},
    clickstackEvidence: {
      path: $clickstackEvidence,
      sha256: $clickstackSha256,
      coverage: "operational_errors_only"
    },
    checks: [1,2,3,4,5,6,7,8,9,10,11]
  }' >"$summary"

echo "PASS: ASK-1973 deployment END_STATE passed Checks 1-11 and ClickStack error gate"
echo "evidence directory: $evidence_dir"
