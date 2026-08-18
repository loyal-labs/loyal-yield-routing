#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
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

normalize_inventory() {
  tr ' ' '\n' | sed '/^$/d' | LC_ALL=C sort -u
}

require_inventory_equal() {
  local expected=$1
  local actual=$2
  local description=$3
  if [[ "$expected" == "$actual" ]]; then
    pass "$description"
  else
    fail "$description (expected: $(printf '%s' "$expected" | tr '\n' ' '); found: $(printf '%s' "$actual" | tr '\n' ' '))"
  fi
}

dockerfile_inventory() {
  local dockerfile=$1
  sed -nE 's|^COPY --chmod=0755 build-artifacts/rust/([^ ]+) /usr/local/bin/([^ ]+)$|\1 \2|p' "$dockerfile" \
    | while read -r source destination; do
        if [[ "$source" == "$destination" ]]; then
          printf '%s\n' "$source"
        fi
      done \
    | LC_ALL=C sort -u
}

workflow_probe_inventory() {
  local variable=$1
  sed -nE "s/^[[:space:]]{2}${variable}:[[:space:]]*(.*)$/\\1/p" "$workflow" | normalize_inventory
}

workflow=.github/workflows/rust-image-build.yml
worker_entry=.github/workflows/worker-images.yml
operator_entry=.github/workflows/operator-tools-image.yml
build_script=scripts/build-rust-image-binaries.sh
target_cache_script=scripts/prepare-rust-target-cache.py
target_cache_verifier=scripts/verify-rust-target-cache-freshness.sh
verifier=scripts/verify-rust-image-build-once.sh
crate_boundaries=docs/rust-crate-boundaries.md
worker_image_docs=docs/render-worker-images.md

require_file "$workflow"
require_file "$worker_entry"
require_absent "$operator_entry"
require_file "$build_script"
require_file "$target_cache_script"
require_file "$target_cache_verifier"
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

# Build contract: one matrix entry per image family compiles in parallel and
# feeds only that family's runtime image.
require_text "$workflow" 'container: rust:1.89-bookworm' 'Rust compiles in the pinned Bookworm toolchain container'
require_fixed_count "$workflow" 'fetch-depth: 0' 1 'Rust build fetches history for target-cache ancestry checks'
require_text "$workflow" 'bash scripts/verify-rust-image-build-once.sh' 'CI runs this verifier before compiling release binaries'
require_text "$workflow" 'bash scripts/build-rust-image-binaries.sh --family "${{ matrix.family }}"' 'Each matrix entry delegates its release compilation to the family-aware build script'
require_text "$workflow" 'name: Build Rust binaries (${{ matrix.family }})' 'Rust build exposes the image-family matrix entry'
require_text "$workflow" '          - laserstream-workers' 'Rust matrix includes laserstream workers'
require_text "$workflow" '          - light-workers' 'Rust matrix includes light workers'
require_text "$workflow" '          - operator-tools' 'Rust matrix includes operator tools'
require_text "$workflow" 'uses: actions/upload-artifact@v4' 'Each Rust matrix entry uploads its family artifact'
require_text "$workflow" 'name: rust-image-binaries-${{ matrix.family }}' 'Rust artifacts are isolated by image family'
forbid_pattern "$workflow" 'if:[[:space:]]*inputs\.images' 'Binary artifact upload is not conditional on an image selection'
require_fixed_count "$workflow" 'uses: actions/download-artifact@v4' 3 'All three image jobs download their family artifact'
require_text "$workflow" 'name: rust-image-binaries-laserstream-workers' 'LaserStream image downloads only its binaries'
require_text "$workflow" 'name: rust-image-binaries-light-workers' 'Light-worker image downloads only its binaries'
require_text "$workflow" 'name: rust-image-binaries-operator-tools' 'Operator image downloads only its binaries'
require_fixed_count "$workflow" 'needs: rust-build' 3 'All three image jobs wait for the parallel Rust matrix'
forbid_pattern "$workflow" 'inputs\.images|^[[:space:]]+images:' 'Reusable workflow has no image-selection control flow'

# Cache contract: Cargo fingerprints come from one dependency-graph snapshot;
# sccache remains the content-addressed fallback for changed compiler outputs.
require_text "$workflow" 'uses: actions/cache/restore@v4' 'Cargo dependency state is restored explicitly'
require_text "$workflow" 'uses: actions/cache/save@v4' 'Trusted main builds save Cargo dependency state explicitly'
require_text "$workflow" "hashFiles('Cargo.lock')" 'Cargo dependency cache is keyed by the lockfile'
require_fixed_count "$workflow" 'uses: actions/cache/restore@v4' 2 'Dependency downloads and Cargo target state have separate restore steps'
require_fixed_count "$workflow" 'uses: actions/cache/save@v4' 2 'Trusted main can save dependency downloads and Cargo target state separately'
require_text "$workflow" 'RUST_TARGET_CACHE_PREFIX: rust-target-linux-amd64-rust-1.89-v2' 'Cargo target cache has an explicit generation and toolchain scope'
require_text "$workflow" 'id: cargo-target-generation' 'Cargo target cache selects a rolling generation'
require_text "$workflow" 'utc-date=$(date -u +%Y-%m-%d)' 'Cargo target cache rolls forward at most once per UTC day'
require_text "$workflow" 'id: cargo-target-cache' 'Cargo target restore exposes its exact-hit result'
require_text "$workflow" 'path: target' 'Cargo target fingerprints and outputs are restored'
require_fixed_count "$workflow" "key: \${{ env.RUST_TARGET_CACHE_PREFIX }}-\${{ matrix.family }}-\${{ hashFiles('Cargo.lock', 'rust-toolchain.toml', 'Cargo.toml', 'crates/*/Cargo.toml') }}-\${{ steps.cargo-target-generation.outputs.utc-date }}" 2 'Cargo target restore and save share one family-scoped daily key'
require_text "$workflow" "\${{ env.RUST_TARGET_CACHE_PREFIX }}-\${{ matrix.family }}-\${{ hashFiles('Cargo.lock', 'rust-toolchain.toml', 'Cargo.toml', 'crates/*/Cargo.toml') }}-" 'Cargo target restore can roll forward from the latest compatible daily snapshot'
require_text "$workflow" "rust-target-linux-amd64-rust-1.89-v1-\${{ hashFiles('Cargo.lock', 'rust-toolchain.toml', 'Cargo.toml', 'crates/*/Cargo.toml') }}" 'First family-cache run can migrate the previous trusted main snapshot'
require_text "$workflow" "steps.cargo-target-cache.outputs.cache-hit != 'true'" 'Cargo target state is saved only when the daily family snapshot is absent'
forbid_pattern "$workflow" 'key:.*github\.sha' 'Cargo caches do not create a new archive for every commit SHA'
require_text "$workflow" 'python3 scripts/prepare-rust-target-cache.py restore' 'Restored Cargo state is prepared from its trusted source revision'
require_text "$workflow" 'python3 scripts/prepare-rust-target-cache.py record' 'Successful Cargo state records its source revision before saving'
require_text "$target_cache_script" 'git_command("merge-base", "--is-ancestor"' 'Target-cache preparation rejects unrelated source revisions'
require_text "$target_cache_script" 'git("diff", "--name-only", "-z", base_revision, "HEAD", "--")' 'Target-cache preparation derives changed paths from Git'
require_text "$target_cache_script" 'safe.directory=' 'Target-cache Git commands tolerate container-mounted checkout ownership'
forbid_pattern "$workflow" 'git config .*safe\.directory' 'Workflow does not weaken global Git ownership checks'
require_pattern "$workflow" 'mozilla-actions/sccache-action@v[0-9]' 'A versioned sccache action provides content-addressed compiler reuse'
require_text "$workflow" 'SCCACHE_GHA_ENABLED: "true"' 'sccache uses the GitHub Actions cache backend'
require_text "$workflow" 'RUSTC_WRAPPER: sccache' 'Rust compilation is routed through sccache'
require_text "$workflow" "github.event_name == 'push'" 'Dependency-cache writes require a trusted push event'
require_text "$workflow" "github.ref == 'refs/heads/main'" 'Dependency-cache writes are restricted to main'
require_text "$workflow" "matrix.family == 'light-workers'" 'Only one matrix entry can save the shared dependency cache'

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
cargo_binaries=$(bash "$build_script" --family all --list-binaries | LC_ALL=C sort -u)
staged_binaries=$cargo_binaries
dockerfile_binaries=$(for dockerfile in $dockerfiles; do dockerfile_inventory "$dockerfile"; done | LC_ALL=C sort -u)

require_inventory_equal "$cargo_binaries" "$staged_binaries" 'Cargo build and staged artifact inventories are exactly equal'
require_inventory_equal "$staged_binaries" "$dockerfile_binaries" 'Staged artifact and runtime Dockerfile inventories are exactly equal'

for dockerfile in $dockerfiles; do
  require_file "$dockerfile"
  rust_copy_count=$(rg -c 'build-artifacts/rust/' "$dockerfile" || true)
  parsed_copy_count=$(sed -nE 's|^COPY --chmod=0755 build-artifacts/rust/([^ ]+) /usr/local/bin/([^ ]+)$|\1 \2|p' "$dockerfile" | sed '/^$/d' | wc -l | tr -d ' ')
  if [[ "$rust_copy_count" == "$parsed_copy_count" ]]; then
    pass "$dockerfile uses the canonical artifact-copy form for every Rust binary"
  else
    fail "$dockerfile has a Rust artifact copy outside the canonical executable-copy form"
  fi

  while read -r source destination; do
    if [[ "$source" == "$destination" ]]; then
      pass "$dockerfile installs $source under the same binary name"
    else
      fail "$dockerfile renames Rust binary $source to $destination"
    fi
  done < <(sed -nE 's|^COPY --chmod=0755 build-artifacts/rust/([^ ]+) /usr/local/bin/([^ ]+)$|\1 \2|p' "$dockerfile")

  case "$dockerfile" in
    Dockerfile.laserstream-workers)
      probe_variable=LASERSTREAM_PROBE_BINARIES
      ;;
    Dockerfile.light-workers)
      probe_variable=LIGHT_WORKER_PROBE_BINARIES
      ;;
    Dockerfile.operator-tools)
      probe_variable=OPERATOR_TOOLS_PROBE_BINARIES
      ;;
    *) fail "Verifier has no binary inventory for $dockerfile"; continue ;;
  esac
  binaries=$(dockerfile_inventory "$dockerfile")
  family=${dockerfile#Dockerfile.}
  build_binaries=$(bash "$build_script" --family "$family" --list-binaries | LC_ALL=C sort -u)
  require_inventory_equal "$binaries" "$build_binaries" "$dockerfile build-family and runtime inventories are exactly equal"
  probe_binaries=$(workflow_probe_inventory "$probe_variable")
  require_inventory_equal "$binaries" "$probe_binaries" "$dockerfile runtime and probe inventories are exactly equal"
  require_fixed_count "$workflow" "PROBE_BINARIES: \${{ env.$probe_variable }}" 1 "$dockerfile probe consumes $probe_variable"
  forbid_pattern "$dockerfile" '^FROM rust:' "$dockerfile has no Rust compiler stage"
  forbid_pattern "$dockerfile" 'cargo (chef|build)' "$dockerfile performs no Rust compilation"
  forbid_pattern "$dockerfile" '/app/target' "$dockerfile does not transport Cargo target state"
done

# Documentation must describe artifact publication separately from deployment.
require_text "$crate_boundaries" 'Main pushes compile the three image-family inventories in parallel and publish immutable SHA tags.' 'Crate-boundary docs describe automatic immutable publication'
require_text "$crate_boundaries" 'Manual deployment selects an already-published immutable image tag or digest and never rebuilds Rust.' 'Crate-boundary docs separate deployment from compilation'
require_text "$worker_image_docs" 'A trusted `main` push compiles the three image-family inventories in parallel and publishes all three immutable image families.' 'Worker-image docs describe the parallel family build'
require_text "$worker_image_docs" 'Publishing these images does not deploy them.' 'Worker-image docs distinguish publication from deployment'
require_text "$worker_image_docs" 'Deployment selects an already-published immutable SHA tag or digest; it never rebuilds Rust.' 'Worker-image docs prohibit deployment-time rebuilding'

if bash -n "$build_script" && bash -n "$verifier"; then
  pass 'Build and verifier scripts pass Bash syntax validation'
else
  fail 'Build or verifier script fails Bash syntax validation'
fi

if bash "$target_cache_verifier" >/dev/null; then
  pass 'Cargo target cache freshness behavior passes its isolated verifier'
else
  fail 'Cargo target cache freshness behavior fails its isolated verifier'
fi

metadata_file=$(mktemp)
trap 'rm -f "$metadata_file"' EXIT
if cargo metadata --no-deps --format-version 1 >"$metadata_file"; then
  missing_targets=0
  for binary in $staged_binaries; do
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
