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

workflow=.github/workflows/rust-image-build.yml
worker_entry=.github/workflows/worker-images.yml
operator_entry=.github/workflows/operator-tools-image.yml
build_script=scripts/build-rust-image-binaries.sh
crate_boundaries=docs/rust-crate-boundaries.md
worker_image_docs=docs/render-worker-images.md

require_file "$workflow"
require_file "$worker_entry"
require_file "$operator_entry"
require_file "$build_script"
require_file "$crate_boundaries"
require_file "$worker_image_docs"

require_text "$workflow" 'container: rust:1.89-bookworm' 'Rust compiles once in the Bookworm toolchain container'
require_text "$workflow" 'uses: actions/cache/restore@v4' 'Rust build restores an explicit Cargo cache'
require_text "$workflow" 'uses: actions/cache/save@v4' 'Trusted main builds save the refreshed Cargo cache'
require_text "$workflow" 'target' 'Cargo cache includes the target directory'
require_text "$workflow" 'github.event_name == '\''push'\''' 'Cargo cache save is guarded by a trusted push event'
require_text "$workflow" 'github.ref == '\''refs/heads/main'\''' 'Cargo cache save is restricted to main'
require_text "$workflow" 'uses: actions/upload-artifact@v4' 'Rust build uploads finished binaries as an artifact'
require_text "$workflow" 'if: inputs.images != '\''none'\''' 'Cache-only main builds skip the binary artifact upload'
require_text "$workflow" 'uses: actions/download-artifact@v4' 'Image jobs download finished binaries'
require_text "$workflow" 'needs: rust-build' 'Image packaging waits for the single Rust build job'
require_text "$workflow" 'bash scripts/build-rust-image-binaries.sh' 'Workflow delegates the only Rust compilation to the build script'

require_text "$worker_entry" 'uses: ./.github/workflows/rust-image-build.yml' 'Worker entry workflow delegates to the reusable build-once workflow'
require_text "$worker_entry" 'images: none' 'Main push has a compile/cache-only path with no image packaging'
require_text "$worker_entry" 'images: all' 'Pull requests verify all packaged image families after the single build'
require_text "$operator_entry" 'uses: ./.github/workflows/rust-image-build.yml' 'Operator entry workflow delegates to the reusable build-once workflow'
require_text "$operator_entry" 'images: operator-tools' 'Operator dispatch packages only operator tools'
forbid_pattern "$operator_entry" '^[[:space:]]+(pull_request|push):' 'Operator workflow has no duplicate automatic PR or push trigger'
require_text "$crate_boundaries" 'one shared Cargo invocation' 'Crate-boundary docs describe the shared Cargo build'
forbid_pattern "$crate_boundaries" 'cargo-chef|cargo chef' 'Crate-boundary docs do not prescribe the removed Docker compiler path'
require_text "$worker_image_docs" 'Dockerfiles only package that artifact' 'Worker image docs describe artifact-only Docker packaging'

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

if [[ -f "$build_script" ]]; then
  if bash -n "$build_script"; then
    pass "$build_script passes bash syntax validation"
  else
    fail "$build_script fails bash syntax validation"
  fi
fi

if cargo metadata --no-deps --format-version 1 >/tmp/loyal-rust-image-metadata.json; then
  missing_targets=0
  for binary in $(printf '%s\n' "$all_binaries" | tr ' ' '\n' | sort -u); do
    if ! jq -e --arg binary "$binary" '.packages[].targets[] | select(.name == $binary)' /tmp/loyal-rust-image-metadata.json >/dev/null; then
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
