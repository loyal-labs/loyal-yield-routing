#!/usr/bin/env bash

set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

failures=0

pass() {
  printf 'PASS: %s\n' "$1"
}

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

require_file() {
  local path=$1
  if [[ -f "$path" ]]; then
    pass "$path exists"
  else
    fail "$path is missing"
  fi
}

require_absent() {
  local path=$1
  if [[ -e "$path" ]]; then
    fail "$path must be absent"
  else
    pass "$path is absent"
  fi
}

require_text() {
  local path=$1
  local text=$2
  local description=$3
  if [[ -f "$path" ]] && rg -F -q -- "$text" "$path"; then
    pass "$description"
  else
    fail "$description"
  fi
}

require_pattern() {
  local path=$1
  local pattern=$2
  local description=$3
  if [[ -f "$path" ]] && rg -q -- "$pattern" "$path"; then
    pass "$description"
  else
    fail "$description"
  fi
}

forbid_pattern() {
  local path=$1
  local pattern=$2
  local description=$3
  if [[ -f "$path" ]] && rg -q -- "$pattern" "$path"; then
    fail "$description"
  else
    pass "$description"
  fi
}

require_fixed_count() {
  local path=$1
  local text=$2
  local expected=$3
  local description=$4
  local actual=0
  if [[ -f "$path" ]]; then
    actual=$(rg -F -c -- "$text" "$path" || true)
  fi
  if [[ "$actual" == "$expected" ]]; then
    pass "$description"
  else
    fail "$description (expected $expected, found $actual)"
  fi
}

workflow=.github/workflows/rust-image-build.yml
worker_entry=.github/workflows/worker-images.yml
operator_entry=.github/workflows/operator-tools-image.yml
build_script=scripts/build-rust-image-binaries.sh
verifier=scripts/verify-rust-image-build-once.sh
crate_boundaries=docs/rust-crate-boundaries.md
worker_image_docs=docs/render-worker-images.md

require_file "$workflow"
require_file "$worker_entry"
require_absent "$operator_entry"
require_file "$build_script"
require_file "$verifier"
require_file "$crate_boundaries"
require_file "$worker_image_docs"

# Event contract: PRs verify; main builds and publishes; no manual build path remains.
require_text "$worker_entry" 'pull_request:' 'Worker images verify pull requests'
require_text "$worker_entry" 'push:' 'Worker images build trusted main pushes'
require_text "$worker_entry" 'branches:' 'Main push trigger is branch-scoped'
require_text "$worker_entry" '- main' 'Main push trigger names the main branch'
forbid_pattern "$worker_entry" '^[[:space:]]*workflow_dispatch:' 'Worker image workflow has no rebuild-capable manual trigger'
require_text "$worker_entry" 'verify-pull-request:' 'Pull requests have a dedicated verification job'
require_text "$worker_entry" 'publish-main-images:' 'Main pushes have a dedicated publication job'
require_fixed_count "$worker_entry" 'uses: ./.github/workflows/rust-image-build.yml' 2 'PR and main are the only reusable workflow callers'
require_fixed_count "$worker_entry" 'publish: false' 1 'Only the PR caller disables publication'
require_fixed_count "$worker_entry" 'publish: true' 1 'Only the main caller enables publication'
forbid_pattern "$worker_entry" '^[[:space:]]+images:' 'Entry workflow has no image-selection branch that can trigger a second build'

# Build contract: one artifact production job feeds all image families.
require_text "$workflow" 'container: rust:1.89-bookworm' 'Rust compiles in the pinned Bookworm toolchain container'
require_text "$workflow" 'bash scripts/verify-rust-image-build-once.sh' 'CI runs this verifier before compiling release binaries'
require_text "$workflow" 'bash scripts/build-rust-image-binaries.sh' 'Workflow delegates the only release compilation to the build script'
require_text "$workflow" 'uses: actions/upload-artifact@v4' 'Rust build uploads its finished binaries once'
forbid_pattern "$workflow" 'if:[[:space:]]*inputs\.images' 'Binary artifact upload is not conditional on an image selection'
require_fixed_count "$workflow" 'uses: actions/download-artifact@v4' 3 'All three image jobs download the shared binary artifact'
require_fixed_count "$workflow" 'needs: rust-build' 3 'All three image jobs depend on the single Rust build'
forbid_pattern "$workflow" 'inputs\.images|^[[:space:]]+images:' 'Reusable workflow has no image-selection control flow'

# Cache contract: dependency downloads use a lockfile key; compiler outputs use sccache.
require_text "$workflow" 'uses: actions/cache/restore@v4' 'Cargo dependency state is restored explicitly'
require_text "$workflow" 'uses: actions/cache/save@v4' 'Trusted main builds save Cargo dependency state explicitly'
require_text "$workflow" "hashFiles('Cargo.lock')" 'Cargo dependency cache is keyed by the lockfile'
forbid_pattern "$workflow" 'key:.*github\.sha' 'Cargo dependency cache key is not unique per commit SHA'
forbid_pattern "$workflow" '^[[:space:]]+target[[:space:]]*$' 'Cargo target directory is not archived'
require_pattern "$workflow" 'mozilla-actions/sccache-action@v[0-9]' 'A versioned sccache action provides content-addressed compiler reuse'
require_text "$workflow" 'SCCACHE_GHA_ENABLED: "true"' 'sccache uses the GitHub Actions cache backend'
require_text "$workflow" 'RUSTC_WRAPPER: sccache' 'Rust compilation is routed through sccache'
require_text "$workflow" "github.event_name == 'push'" 'Dependency-cache writes require a trusted push event'
require_text "$workflow" "github.ref == 'refs/heads/main'" 'Dependency-cache writes are restricted to main'

# Publication contract: every main build produces all immutable image families.
require_fixed_count "$workflow" 'push: ${{ inputs.publish }}' 3 'Every image family follows the caller publication decision'
require_text "$workflow" '${{ env.REGISTRY }}/${{ env.IMAGE_NAMESPACE }}/laserstream-workers:sha-${{ github.sha }}' 'LaserStream image uses an immutable commit tag'
require_text "$workflow" '${{ env.REGISTRY }}/${{ env.IMAGE_NAMESPACE }}/light-workers:sha-${{ github.sha }}' 'Light-worker image uses an immutable commit tag'
require_text "$workflow" '${{ env.REGISTRY }}/${{ env.IMAGE_NAMESPACE }}/operator-tools:sha-${{ github.sha }}' 'Operator image uses an immutable commit tag'
forbid_pattern "$workflow" '(^|[^[:alnum:]_-])latest([^[:alnum:]_-]|$)' 'Release workflow never publishes a mutable latest tag'
forbid_pattern "$worker_entry" 'render[[:space:]]+(deploy|services)' 'Image publication does not mutate Render deployment state'
forbid_pattern "$workflow" 'render[[:space:]]+(deploy|services)' 'Reusable image build does not mutate Render deployment state'

# No other workflow or Dockerfile may reintroduce a release Cargo build.
compile_files=$(rg -l 'cargo build --release --locked' \
  "$build_script" \
  .github/workflows \
  Dockerfile.light-workers \
  Dockerfile.laserstream-workers \
  Dockerfile.operator-tools 2>/dev/null || true)
compile_count=$(printf '%s\n' "$compile_files" | sed '/^$/d' | wc -l | tr -d ' ')
if [[ "$compile_count" == 1 && "$compile_files" == "$build_script" ]]; then
  pass 'Exactly one release Cargo compilation path exists'
else
  fail "Expected the only release Cargo compilation path in $build_script; found: ${compile_files:-none}"
fi

reusable_callers=$(rg -l -F 'uses: ./.github/workflows/rust-image-build.yml' .github/workflows 2>/dev/null || true)
caller_count=$(printf '%s\n' "$reusable_callers" | sed '/^$/d' | wc -l | tr -d ' ')
if [[ "$caller_count" == 1 && "$reusable_callers" == "$worker_entry" ]]; then
  pass 'Only the automatic worker-image entry workflow can invoke the Rust image build'
else
  fail "Expected only $worker_entry to call the Rust image build; found: ${reusable_callers:-none}"
fi

require_text "$build_script" 'BASH_SOURCE[0]' 'Build script resolves the checkout without invoking Git'
forbid_pattern "$build_script" 'git rev-parse' 'Container build does not depend on Git checkout ownership'

# Runtime-image inventory remains complete and compiler-free.
dockerfiles='Dockerfile.laserstream-workers Dockerfile.light-workers Dockerfile.operator-tools'
laserstream_binaries='kamino-reserve-monitor balance-sweep-ata-monitor loyal-timescale-migrations yield-migrations'
light_binaries='balance-sweep-ata-projector balance-sweep-autodeposit-trigger loyal-yield-realtime yield-migrations same-mint-reserve-swap same-mint-yield-monitor fleet-opportunity-planner fleet-health-projector fleet-route-confirmer route-lookup-table-provisioner'
operator_binaries='loyal-timescale-migrations fleet-orchestration-verifier fleet-orchestration-production-evidence same-mint-monitor-e2e route-lookup-table-shared-catalog route-lookup-table-alert-monitor route-lookup-table-legacy-import route-lookup-table-cleanup signer-balance-monitor'
all_binaries="$laserstream_binaries $light_binaries $operator_binaries"

for dockerfile in $dockerfiles; do
  case "$dockerfile" in
    Dockerfile.laserstream-workers) binaries=$laserstream_binaries ;;
    Dockerfile.light-workers) binaries=$light_binaries ;;
    Dockerfile.operator-tools) binaries=$operator_binaries ;;
    *) fail "Verifier has no binary inventory for $dockerfile"; continue ;;
  esac
  require_file "$dockerfile"
  forbid_pattern "$dockerfile" '^FROM rust:' "$dockerfile has no Rust compiler stage"
  forbid_pattern "$dockerfile" 'cargo (chef|build)' "$dockerfile performs no Rust compilation"
  forbid_pattern "$dockerfile" '/app/target' "$dockerfile does not transport Cargo target state"
  for binary in $binaries; do
    require_text "$dockerfile" "COPY --chmod=0755 build-artifacts/rust/$binary /usr/local/bin/$binary" "$dockerfile restores executable mode for $binary from the job artifact"
    require_text "$build_script" "stage_binary $binary" "$build_script stages $binary"
  done
done

# Documentation must describe artifact publication separately from deployment.
require_text "$crate_boundaries" 'Main pushes compile once, package all three image families, and publish immutable SHA tags.' 'Crate-boundary docs describe automatic immutable publication'
require_text "$crate_boundaries" 'Manual deployment selects an already-published immutable image tag or digest and never rebuilds Rust.' 'Crate-boundary docs separate deployment from compilation'
require_text "$worker_image_docs" 'A trusted `main` push compiles the shared Rust artifact once and publishes all three immutable image families.' 'Worker-image docs describe the single trusted build'
require_text "$worker_image_docs" 'Publishing these images does not deploy them.' 'Worker-image docs distinguish publication from deployment'
require_text "$worker_image_docs" 'Deployment selects an already-published immutable SHA tag or digest; it never rebuilds Rust.' 'Worker-image docs prohibit deployment-time rebuilding'

if bash -n "$build_script" && bash -n "$verifier"; then
  pass 'Build and verifier scripts pass Bash syntax validation'
else
  fail 'Build or verifier script fails Bash syntax validation'
fi

metadata_file=$(mktemp)
trap 'rm -f "$metadata_file"' EXIT
if cargo metadata --no-deps --format-version 1 >"$metadata_file"; then
  missing_targets=0
  for binary in $(printf '%s\n' "$all_binaries" | tr ' ' '\n' | sort -u); do
    if ! jq -e --arg binary "$binary" '.packages[].targets[] | select(.name == $binary)' "$metadata_file" >/dev/null; then
      fail "Cargo metadata has no target named $binary"
      missing_targets=1
    fi
  done
  if [[ "$missing_targets" == 0 ]]; then
    pass 'Every packaged binary is a real Cargo target'
  fi
else
  fail 'cargo metadata could not load the workspace'
fi

if [[ "$failures" == 0 ]]; then
  printf 'OVERALL: PASS\n'
  exit 0
fi

printf 'OVERALL: FAIL (%s required checks failed)\n' "$failures" >&2
exit 1
