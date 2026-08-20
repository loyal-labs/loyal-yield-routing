#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

usage() {
  cat <<'EOF'
Usage: scripts/verify-rust-image-build-performance-e2e.sh [options]

Run the worker-image build in the same Rust container used by GitHub Actions,
with persistent Cargo/sccache state and an isolated target directory.

Options:
  --checkout PATH           Checkout to benchmark (default: repository root)
  --cache-root PATH         Persistent E2E cache root (required)
  --label NAME              Result/target label (required)
  --family NAME             all, laserstream-workers, light-workers, or operator-tools
                            (default: all)
  --cpus NUMBER             Docker CPU limit (default: 4)
  --memory SIZE             Docker memory limit (default: 16g)
  --compile-only            Skip runtime image packaging and probes
  --simulate-target-cache-save
                            Compress target like actions/cache before packaging
  --max-build-seconds N     Fail when compile/stage exceeds N; 0 disables
  --max-total-seconds N     Fail when the full run exceeds N; 0 disables
  --builder-image IMAGE     Prepared builder image tag
  --rebuild-builder         Rebuild the prepared builder image
  -h, --help                Show this help
EOF
}

checkout=$repo_root
cache_root=
label=
family=all
cpus=4
memory=16g
compile_only=false
simulate_target_cache_save=false
max_build_seconds=0
max_total_seconds=0
builder_image=loyal-yield-routing-rust-e2e:rust-1.89-sccache-0.17
rebuild_builder=false

while (($# > 0)); do
  case "$1" in
    --checkout)
      checkout=$2
      shift 2
      ;;
    --cache-root)
      cache_root=$2
      shift 2
      ;;
    --label)
      label=$2
      shift 2
      ;;
    --family)
      family=$2
      shift 2
      ;;
    --cpus)
      cpus=$2
      shift 2
      ;;
    --memory)
      memory=$2
      shift 2
      ;;
    --compile-only)
      compile_only=true
      shift
      ;;
    --simulate-target-cache-save)
      simulate_target_cache_save=true
      shift
      ;;
    --max-build-seconds)
      max_build_seconds=$2
      shift 2
      ;;
    --max-total-seconds)
      max_total_seconds=$2
      shift 2
      ;;
    --builder-image)
      builder_image=$2
      shift 2
      ;;
    --rebuild-builder)
      rebuild_builder=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'Unknown option: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$cache_root" || -z "$label" ]]; then
  printf '%s\n' '--cache-root and --label are required' >&2
  usage >&2
  exit 2
fi

case "$family" in
  all|laserstream-workers|light-workers|operator-tools) ;;
  *)
    printf 'Unsupported image family: %s\n' "$family" >&2
    exit 2
    ;;
esac

if [[ ! -d "$checkout" || ! -f "$checkout/Cargo.lock" ]]; then
  printf 'Checkout does not look like this repository: %s\n' "$checkout" >&2
  exit 2
fi

for command_name in docker jq sed sort; do
  if ! command -v "$command_name" >/dev/null; then
    printf 'Required command is missing: %s\n' "$command_name" >&2
    exit 2
  fi
done

checkout=$(cd "$checkout" && pwd)
mkdir -p "$cache_root"
cache_root=$(cd "$cache_root" && pwd)

safe_label=$(printf '%s' "$label" | tr -c 'A-Za-z0-9_.-' '-')
target_dir="$cache_root/targets/$safe_label"
cargo_registry_dir="$cache_root/cargo/registry"
cargo_git_dir="$cache_root/cargo/git"
sccache_dir="$cache_root/sccache"
results_root="$cache_root/results"
mkdir -p "$target_dir" "$cargo_registry_dir" "$cargo_git_dir" "$sccache_dir" "$results_root"
result_dir=$(mktemp -d "$results_root/${safe_label}.XXXXXX")

builder_dockerfile="$repo_root/scripts/ci/Dockerfile.rust-image-build-e2e"
if $rebuild_builder || ! docker image inspect "$builder_image" >/dev/null 2>&1; then
  docker build \
    --file "$builder_dockerfile" \
    --tag "$builder_image" \
    "$repo_root"
fi

started_at=$(date +%s)

docker run --rm \
  --cpus "$cpus" \
  --memory "$memory" \
  --volume "$checkout:/workspace" \
  --volume "$target_dir:/workspace/target" \
  --volume "$cargo_registry_dir:/usr/local/cargo/registry" \
  --volume "$cargo_git_dir:/usr/local/cargo/git" \
  --volume "$sccache_dir:/sccache" \
  --volume "$result_dir:/results" \
  --workdir /workspace \
  --env SQLX_OFFLINE=true \
  --env "E2E_FAMILY=$family" \
  --env RUSTC_WRAPPER=sccache \
  --env SCCACHE_DIR=/sccache \
  --env SCCACHE_CACHE_SIZE=20G \
  "$builder_image" \
  bash -ceu '
    sccache --start-server >/dev/null
    sccache --zero-stats >/dev/null
    python3 scripts/prepare-rust-target-cache.py restore
    bash scripts/verify-rust-image-build-once.sh
    build_started_ms=$(date +%s%3N)
    build_args=()
    if [[ "$E2E_FAMILY" != all ]]; then
      build_args=(--family "$E2E_FAMILY")
    fi
    bash scripts/build-rust-image-binaries.sh "${build_args[@]}"
    build_completed_ms=$(date +%s%3N)
    python3 scripts/prepare-rust-target-cache.py record
    expected=$(bash scripts/build-rust-image-binaries.sh --family "$E2E_FAMILY" --list-binaries | sort -u)
    actual=$(find build-artifacts/rust -maxdepth 1 -type f -executable -printf "%f\n" | sort -u)
    if [[ "$expected" != "$actual" ]]; then
      printf "Built executable inventory mismatch\nExpected:\n%s\nActual:\n%s\n" "$expected" "$actual" >&2
      exit 1
    fi
    binary_count=$(printf "%s\n" "$expected" | sed "/^$/d" | wc -l)
    sccache --show-stats --stats-format=json > /results/sccache.json
    jq -n \
      --argjson build_milliseconds "$((build_completed_ms - build_started_ms))" \
      --argjson binary_count "$binary_count" \
      "{build_milliseconds: \$build_milliseconds, build_seconds: (\$build_milliseconds / 1000), binary_count: \$binary_count}" \
      > /results/build.json
  '

build_milliseconds=$(jq -r '.build_milliseconds' "$result_dir/build.json")
build_seconds=$(jq -r '.build_seconds' "$result_dir/build.json")
binary_count=$(jq -r '.binary_count' "$result_dir/build.json")

cache_save_seconds=0
if $simulate_target_cache_save; then
  cache_save_started=$(date +%s)
  docker run --rm \
    --volume "$target_dir:/target:ro" \
    "$builder_image" \
    tar -C /target -cf - . \
    | zstd -T4 -q -o "$result_dir/target-cache.tar.zst"
  cache_save_completed=$(date +%s)
  cache_save_seconds=$((cache_save_completed - cache_save_started))
  rm -f "$result_dir/target-cache.tar.zst"
fi

package_seconds=0
if ! $compile_only; then
  image_prefix=loyal-yield-routing-e2e
  revision="e2e-$safe_label"

  build_and_probe() {
    local family=$1
    local dockerfile=$2
    local probe_binaries=$3
    local image="$image_prefix/$family:$safe_label"
    local timing_file="$result_dir/package-$family.seconds"
    local package_started package_completed
    package_started=$(date +%s)
    docker buildx build \
      --file "$checkout/$dockerfile" \
      --platform linux/amd64 \
      --load \
      --provenance=false \
      --build-arg "LOYAL_IMAGE_VERSION=$revision" \
      --label "org.opencontainers.image.revision=$revision" \
      --tag "$image" \
      "$checkout"

    docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image" | grep -Fx 'linux/amd64'
    docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image" | grep -Fx "$revision"
    docker run --rm --network=none --read-only --cap-drop=ALL \
      --security-opt=no-new-privileges \
      --env "PROBE_BINARIES=$probe_binaries" \
      --entrypoint sh "$image" -c \
      'for binary in $PROBE_BINARIES; do test -x "/usr/local/bin/$binary"; done'

    case "$family" in
      laserstream-workers)
        docker run --rm --network=none --read-only --cap-drop=ALL \
          --security-opt=no-new-privileges --entrypoint sh "$image" -c \
          'test -x /usr/local/bin/kamino-monitor-predeploy'
        ;;
      light-workers)
        docker run --rm --network=none --read-only --cap-drop=ALL \
          --security-opt=no-new-privileges --entrypoint sh "$image" -c \
          'test -x /usr/local/bin/bun; test -e /app/node_modules; /usr/local/bin/bun -e "await import(\"/app/scripts/execute-autodeposit-policy.ts\")"'

        probe_role() {
          local expected_role=$1
          shift
          local output
          output=$(docker run --rm --network=none --read-only --cap-drop=ALL \
            --security-opt=no-new-privileges --entrypoint "$1" "$image" "${@:2}")
          jq -e --arg role "$expected_role" \
            '.schemaVersion == 1 and .event == "fleet_worker_role_probe" and .status == "pass" and .role == $role and .networkAccessed == false and .secretsLoaded == false and .databaseMutated == false and .transactionSent == false' \
            <<<"$output" >/dev/null
        }
        probe_role planner /usr/local/bin/fleet-opportunity-planner --role-probe
        probe_role revalidator /usr/local/bin/same-mint-reserve-swap --fleet-worker revalidate --role-probe
        probe_role executor /usr/local/bin/same-mint-reserve-swap --fleet-worker execute --role-probe
        probe_role confirmer /usr/local/bin/fleet-route-confirmer --role-probe
        probe_role reconciler /usr/local/bin/same-mint-reserve-swap --fleet-reconciler --role-probe
        probe_role priority_provisioner /usr/local/bin/route-lookup-table-provisioner --role-probe
        ;;
      operator-tools)
        docker run --rm --network=none --read-only --cap-drop=ALL \
          --security-opt=no-new-privileges "$image" >/dev/null
        ;;
    esac

    package_completed=$(date +%s)
    printf '%s\n' "$((package_completed - package_started))" >"$timing_file"
  }

  laserstream_probes=$(sed -nE 's/^[[:space:]]{2}LASERSTREAM_PROBE_BINARIES:[[:space:]]*(.*)$/\1/p' "$checkout/.github/workflows/rust-image-build.yml")
  light_probes=$(sed -nE 's/^[[:space:]]{2}LIGHT_WORKER_PROBE_BINARIES:[[:space:]]*(.*)$/\1/p' "$checkout/.github/workflows/rust-image-build.yml")
  operator_probes=$(sed -nE 's/^[[:space:]]{2}OPERATOR_TOOLS_PROBE_BINARIES:[[:space:]]*(.*)$/\1/p' "$checkout/.github/workflows/rust-image-build.yml")

  if [[ "$family" == all ]]; then
    build_and_probe laserstream-workers Dockerfile.laserstream-workers "$laserstream_probes" >"$result_dir/laserstream-workers.log" 2>&1 &
    laserstream_pid=$!
    build_and_probe light-workers Dockerfile.light-workers "$light_probes" >"$result_dir/light-workers.log" 2>&1 &
    light_pid=$!
    build_and_probe operator-tools Dockerfile.operator-tools "$operator_probes" >"$result_dir/operator-tools.log" 2>&1 &
    operator_pid=$!

    package_failed=0
    for pid in "$laserstream_pid" "$light_pid" "$operator_pid"; do
      if ! wait "$pid"; then
        package_failed=1
      fi
    done
    if ((package_failed != 0)); then
      printf 'Image packaging or runtime probe failed; logs: %s\n' "$result_dir" >&2
      exit 1
    fi
  else
    case "$family" in
      laserstream-workers)
        family_dockerfile=Dockerfile.laserstream-workers
        family_probes=$laserstream_probes
        ;;
      light-workers)
        family_dockerfile=Dockerfile.light-workers
        family_probes=$light_probes
        ;;
      operator-tools)
        family_dockerfile=Dockerfile.operator-tools
        family_probes=$operator_probes
        ;;
    esac
    if ! build_and_probe "$family" "$family_dockerfile" "$family_probes" >"$result_dir/$family.log" 2>&1; then
      printf 'Image packaging or runtime probe failed; log: %s/%s.log\n' "$result_dir" "$family" >&2
      exit 1
    fi
  fi

  package_seconds=$(cat "$result_dir"/package-*.seconds | sort -nr | head -n 1)
fi

completed_at=$(date +%s)
total_seconds=$((completed_at - started_at))

jq -n \
  --arg run_label "$label" \
  --arg family "$family" \
  --arg checkout "$checkout" \
  --arg result_dir "$result_dir" \
  --argjson cpus "$cpus" \
  --arg memory "$memory" \
  --argjson build_seconds "$build_seconds" \
  --argjson cache_save_seconds "$cache_save_seconds" \
  --argjson package_seconds "$package_seconds" \
  --argjson total_seconds "$total_seconds" \
  --argjson binary_count "$binary_count" \
  --slurpfile sccache "$result_dir/sccache.json" \
  '{
    label: $run_label,
    family: $family,
    checkout: $checkout,
    result_dir: $result_dir,
    cpus: $cpus,
    memory: $memory,
    build_seconds: $build_seconds,
    cache_save_seconds: $cache_save_seconds,
    package_critical_path_seconds: $package_seconds,
    total_seconds: $total_seconds,
    binary_count: $binary_count,
    probe_passed: true,
    workflow_contract_passed: true,
    sccache: $sccache[0]
  }' | tee "$result_dir/report.json"

if ((max_build_seconds > 0 && build_milliseconds > max_build_seconds * 1000)); then
  printf 'Build duration %ss exceeds limit %ss\n' "$build_seconds" "$max_build_seconds" >&2
  exit 1
fi
if ((max_total_seconds > 0 && total_seconds > max_total_seconds)); then
  printf 'Total duration %ss exceeds limit %ss\n' "$total_seconds" "$max_total_seconds" >&2
  exit 1
fi

printf 'OVERALL: PASS (%s)\n' "$result_dir"
