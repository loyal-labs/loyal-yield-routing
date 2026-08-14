#!/usr/bin/env bash
set -euo pipefail

# Non-pushing linux/amd64 build and runtime probe for every ASK-1973 image.
# This is a packaging gate for binaries already staged by
# scripts/build-rust-image-binaries.sh on the requested Linux platform. It
# exercises artifact COPYs, runtime libraries, declared commands, and all six
# fleet role entrypoints.
# ASK1973_IMAGE_PLATFORM may select the native platform for a supplementary
# local smoke run; CI and the default invocation always gate linux/amd64.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

container_engine="${ASK1973_CONTAINER_ENGINE:-}"
if [[ -z "$container_engine" ]]; then
  if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
    container_engine=docker
  elif command -v podman >/dev/null && podman info >/dev/null 2>&1; then
    container_engine=podman
  else
    fail "a running Docker or Podman engine is required"
  fi
fi
[[ "$container_engine" == "docker" || "$container_engine" == "podman" ]] ||
  fail "ASK1973_CONTAINER_ENGINE must be docker or podman"
command -v "$container_engine" >/dev/null || fail "$container_engine is not installed"
command -v jq >/dev/null || fail "jq is required"

image_platform="${ASK1973_IMAGE_PLATFORM:-linux/amd64}"
[[ "$image_platform" == "linux/amd64" || "$image_platform" == "linux/arm64" ]] ||
  fail "ASK1973_IMAGE_PLATFORM must be linux/amd64 or linux/arm64"

container_command=("$container_engine")
if [[ "$container_engine" == "podman" && -n "${ASK1973_PODMAN_CONNECTION:-}" ]]; then
  container_command+=(--connection "$ASK1973_PODMAN_CONNECTION")
fi
"${container_command[@]}" info >/dev/null || fail "$container_engine engine is not reachable"

[[ -f build-artifacts/rust/balance-sweep-ata-projector ]] ||
  fail "build-artifacts/rust is missing; run the shared Rust build on $image_platform first"

runtime_tmp_root="${ASK1973_RUNTIME_TMPDIR:-/tmp}"
evidence_dir="${ASK1973_IMAGE_EVIDENCE_DIR:-$(mktemp -d "$runtime_tmp_root/ask1973-image-evidence.XXXXXX")}"
mkdir -p "$evidence_dir"
head_commit="$(git rev-parse HEAD)"
tracked_checkout_clean=true
if [[ -n "$(git status --porcelain --untracked-files=normal)" ]]; then
  tracked_checkout_clean=false
fi
revision_suffix="${head_commit:0:12}"
[[ "$tracked_checkout_clean" == true ]] || revision_suffix="$revision_suffix-dirty"
tag_suffix="$revision_suffix-${image_platform#linux/}"
light_image="localhost/loyal-ask1973-light-workers:$tag_suffix"
laserstream_image="localhost/loyal-ask1973-laserstream-workers:$tag_suffix"
operator_image="localhost/loyal-ask1973-operator-tools:$tag_suffix"

build_image() {
  local label="$1"
  local dockerfile="$2"
  local image="$3"
  echo "== Building $label $image_platform image"
  if ! "${container_command[@]}" build \
    --platform "$image_platform" \
    --file "$dockerfile" \
    --tag "$image" \
    --build-arg "LOYAL_IMAGE_VERSION=sha-$head_commit" \
    --label "org.opencontainers.image.revision=$head_commit" \
    . >"$evidence_dir/$label-build.log" 2>&1; then
    tail -80 "$evidence_dir/$label-build.log" >&2
    fail "$label image build failed"
  fi
  [[ "$("${container_command[@]}" image inspect --format '{{.Os}}/{{.Architecture}}' "$image")" == "$image_platform" ]] ||
    fail "$label image is not $image_platform"
}

probe_image_contract() {
  local label="$1"
  local image="$2"
  local expected_cmd_json="$3"
  local expected_version="$4"
  shift 4
  local inspect_file="$evidence_dir/$label-inspect.json"
  "${container_command[@]}" image inspect "$image" >"$inspect_file"
  jq -e \
    --arg revision "$head_commit" \
    --argjson expectedCmd "$expected_cmd_json" \
    --arg version "$expected_version" '
      .[0].Config.Cmd == $expectedCmd
      and .[0].Config.Labels["org.opencontainers.image.revision"] == $revision
      and ($version == "" or (.[0].Config.Env | index($version)) != null)
    ' "$inspect_file" >/dev/null || fail "$label image metadata contract failed"

  local paths="$*"
  if [[ -n "$paths" ]]; then
    "${container_command[@]}" run --rm --network=none --read-only --cap-drop=ALL \
      --security-opt=no-new-privileges --env "PROBE_PATHS=$paths" \
      --entrypoint sh "$image" -c \
      'for path in $PROBE_PATHS; do test -e "$path"; done
       case " $PROBE_PATHS " in *" /usr/local/bin/bun "*) test -x /usr/local/bin/bun;; esac
       case " $PROBE_PATHS " in *" /usr/local/bin/kamino-monitor-predeploy "*) test -x /usr/local/bin/kamino-monitor-predeploy;; esac' \
      >"$evidence_dir/$label-paths.log" 2>&1 || fail "$label runtime path probe failed"
  fi
}

probe_binaries() {
  local label="$1"
  local image="$2"
  shift 2
  local binaries="$*"
  "${container_command[@]}" run --rm --network=none --read-only --cap-drop=ALL \
    --security-opt=no-new-privileges --env "PROBE_BINARIES=$binaries" \
    --entrypoint sh "$image" -c \
    'for binary in $PROBE_BINARIES; do test -x "/usr/local/bin/$binary"; done' \
    >"$evidence_dir/$label-binaries.log" 2>&1 || fail "$label runtime binary probe failed"
}

probe_role() {
  local expected_role="$1"
  local image="$2"
  local entrypoint="$3"
  shift 3
  local output_file="$evidence_dir/role-$expected_role.json"
  "${container_command[@]}" run --rm --network=none --read-only --cap-drop=ALL \
    --security-opt=no-new-privileges --entrypoint "$entrypoint" "$image" "$@" \
    >"$output_file"
  jq -e --arg role "$expected_role" '
    .schemaVersion == 1
    and .event == "fleet_worker_role_probe"
    and .status == "pass"
    and .role == $role
    and .networkAccessed == false
    and .secretsLoaded == false
    and .databaseMutated == false
    and .transactionSent == false
  ' "$output_file" >/dev/null || fail "$expected_role image role probe failed"
}

build_image light-workers Dockerfile.light-workers "$light_image"
probe_image_contract light-workers "$light_image" \
  '["/usr/local/bin/balance-sweep-ata-projector"]' \
  "LOYAL_IMAGE_VERSION=sha-$head_commit" \
  /usr/local/bin/bun /app/scripts/execute-autodeposit-policy.ts /app/node_modules
probe_binaries light-workers "$light_image" \
  balance-sweep-ata-projector \
  balance-sweep-autodeposit-trigger \
  loyal-yield-realtime \
  yield-migrations \
  same-mint-reserve-swap \
  same-mint-yield-monitor \
  fleet-opportunity-planner \
  fleet-health-projector \
  fleet-route-confirmer \
  route-lookup-table-provisioner
probe_role planner "$light_image" /usr/local/bin/fleet-opportunity-planner --role-probe
probe_role revalidator "$light_image" /usr/local/bin/same-mint-reserve-swap \
  --fleet-worker revalidate --role-probe
probe_role executor "$light_image" /usr/local/bin/same-mint-reserve-swap \
  --fleet-worker execute --role-probe
probe_role confirmer "$light_image" /usr/local/bin/fleet-route-confirmer --role-probe
probe_role reconciler "$light_image" /usr/local/bin/same-mint-reserve-swap \
  --fleet-reconciler --role-probe
probe_role priority_provisioner "$light_image" \
  /usr/local/bin/route-lookup-table-provisioner --role-probe

build_image laserstream-workers Dockerfile.laserstream-workers "$laserstream_image"
probe_image_contract laserstream-workers "$laserstream_image" \
  '["/usr/local/bin/kamino-reserve-monitor"]' \
  "LOYAL_IMAGE_VERSION=sha-$head_commit" \
  /usr/local/bin/kamino-monitor-predeploy
probe_binaries laserstream-workers "$laserstream_image" \
  kamino-reserve-monitor \
  balance-sweep-ata-monitor \
  loyal-timescale-migrations \
  yield-migrations

build_image operator-tools Dockerfile.operator-tools "$operator_image"
probe_image_contract operator-tools "$operator_image" \
  '["/usr/local/bin/fleet-orchestration-verifier", "--help"]' ""
probe_binaries operator-tools "$operator_image" \
  loyal-timescale-migrations \
  fleet-orchestration-verifier \
  fleet-orchestration-production-evidence \
  same-mint-monitor-e2e \
  route-lookup-table-shared-catalog \
  route-lookup-table-alert-monitor \
  route-lookup-table-legacy-import \
  route-lookup-table-cleanup \
  signer-balance-monitor
"${container_command[@]}" run --rm --network=none --read-only --cap-drop=ALL \
  --security-opt=no-new-privileges "$operator_image" \
  >"$evidence_dir/operator-tools-command.log" 2>&1 ||
  fail "operator-tools declared command failed"

jq -n \
  --arg headCommit "$head_commit" \
  --arg engine "$container_engine" \
  --arg engineConnection "${ASK1973_PODMAN_CONNECTION:-default}" \
  --arg platform "$image_platform" \
  --argjson trackedCheckoutClean "$tracked_checkout_clean" \
  --arg lightImage "$light_image" \
  --arg laserstreamImage "$laserstream_image" \
  --arg operatorImage "$operator_image" \
  '{
    status: "PASS",
    headCommit: $headCommit,
    platform: $platform,
    containerEngine: $engine,
    containerEngineConnection: $engineConnection,
    pushed: false,
    trackedCheckoutClean: $trackedCheckoutClean,
    authoritativeForProductionRevision: (
      $platform == "linux/amd64" and $trackedCheckoutClean
    ),
    images: {
      lightWorkers: $lightImage,
      laserstreamWorkers: $laserstreamImage,
      operatorTools: $operatorImage
    },
    fleetRoleProbeCount: 6
  }' >"$evidence_dir/summary.json"

if [[ "$image_platform" == "linux/amd64" && "$tracked_checkout_clean" == true ]]; then
  echo "PASS: ASK-1973 authoritative linux/amd64 image build and runtime verification"
else
  echo "PASS: ASK-1973 supplementary $image_platform image build and runtime smoke"
fi
echo "evidence directory: $evidence_dir"
