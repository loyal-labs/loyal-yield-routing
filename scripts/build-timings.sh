#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/build-timings.sh <before|after> [--all|--cargo-only|--graph-only|--images-only]

Capture reproducible Rust dependency-graph and build-timing evidence under
docs/build-timings/<label>/.

Environment overrides:
  BUILD_TIMINGS_CARGO_ARGS  Cargo selection to measure. Defaults to the main
                            production worker binary.
  BUILD_TIMINGS_TOUCH_FILE  Local source file used for the warm-change run.
  BUILD_TIMINGS_DOCKER_ARGS Extra arguments passed to docker buildx build.
EOF
}

if [[ $# -lt 1 || $# -gt 2 ]]; then
  usage >&2
  exit 2
fi

label=$1
mode=${2:---all}

if [[ "$label" != "before" && "$label" != "after" ]]; then
  usage >&2
  exit 2
fi

case "$mode" in
  --all | --cargo-only | --graph-only | --images-only) ;;
  *)
    usage >&2
    exit 2
    ;;
esac

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

output_dir="docs/build-timings/$label"
mkdir -p "$output_dir"

read -r -a cargo_selection <<<"${BUILD_TIMINGS_CARGO_ARGS:--p loyal-yield-orchestrator --bin same-mint-reserve-swap}"

if [[ -n "${BUILD_TIMINGS_TOUCH_FILE:-}" ]]; then
  touch_file=$BUILD_TIMINGS_TOUCH_FILE
elif [[ -f crates/loyal-yield-store/src/store.rs ]]; then
  touch_file=crates/loyal-yield-store/src/store.rs
else
  touch_file=crates/loyal-yield-orchestrator/src/store.rs
fi

write_metadata() {
  {
    printf 'label=%s\n' "$label"
    printf 'commit=%s\n' "$(git rev-parse HEAD)"
    printf 'rustc=%s\n' "$(rustc --version)"
    printf 'cargo=%s\n' "$(cargo --version)"
    printf 'cargo_selection='
    printf '%q ' "${cargo_selection[@]}"
    printf '\n'
    printf 'touch_file=%s\n' "$touch_file"
    printf 'captured_at_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >"$output_dir/metadata.txt"
}

capture_graph() {
  cargo tree --duplicates >"$output_dir/duplicates.txt"

  : >"$output_dir/dep-counts.txt"
  while IFS= read -r package; do
    count=$(cargo tree -p "$package" -e normal --prefix none --no-dedupe 2>/dev/null | sort -u | wc -l | tr -d ' ')
    printf '%s %s\n' "$package" "$count" >>"$output_dir/dep-counts.txt"
  done < <(cargo metadata --no-deps --format-version=1 | jq -r '.packages[].name' | sort)
}

capture_cargo() {
  local scenario=$1
  local started_at finished_at status

  started_at=$(date +%s)
  set +e
  env RUSTC_BOOTSTRAP=1 cargo -Z unstable-options build --release --locked \
    --timings=json "${cargo_selection[@]}" \
    >"$output_dir/$scenario.jsonl" \
    2>"$output_dir/$scenario.log"
  status=$?
  set -e
  finished_at=$(date +%s)

  {
    printf 'scenario=%s\n' "$scenario"
    printf 'elapsed_seconds=%s\n' "$((finished_at - started_at))"
    printf 'exit_status=%s\n' "$status"
  } >"$output_dir/$scenario.summary.txt"

  if [[ $status -ne 0 ]]; then
    printf 'Cargo timing scenario %s failed; see %s/%s.log\n' "$scenario" "$output_dir" "$scenario" >&2
    return "$status"
  fi
}

capture_cargo_scenarios() {
  cargo clean
  capture_cargo cold
  capture_cargo warm-noop

  if [[ ! -f "$touch_file" ]]; then
    printf 'Touch file does not exist: %s\n' "$touch_file" >&2
    return 1
  fi
  touch "$touch_file"
  capture_cargo warm-local-change
}

parse_buildx_steps() {
  local log_file=$1
  local output_file=$2

  awk '
    /^#[0-9]+ \[/ {
      id = $1
      sub(/^#/, "", id)
      step[id] = substr($0, index($0, "[") + 1)
      sub(/\] .*/, "]", step[id])
    }
    /^#[0-9]+ DONE / {
      id = $1
      sub(/^#/, "", id)
      duration = $3
      if (step[id] != "") {
        printf "%s %s %s\n", id, duration, step[id]
      }
    }
  ' "$log_file" >"$output_file"
}

capture_image() {
  local image=$1
  local dockerfile=$2
  local log_file="$output_dir/image-$image.log"
  local summary_file="$output_dir/image-$image.summary.txt"
  local started_at finished_at status
  local -a extra_args=()

  if [[ -n "${BUILD_TIMINGS_DOCKER_ARGS:-}" ]]; then
    read -r -a extra_args <<<"$BUILD_TIMINGS_DOCKER_ARGS"
  fi

  started_at=$(date +%s)
  set +e
  docker buildx build \
    --progress=plain \
    --file "$dockerfile" \
    --tag "loyal-yield-routing-build-timings:$label-$image" \
    --load \
    "${extra_args[@]}" \
    . >"$log_file" 2>&1
  status=$?
  set -e
  finished_at=$(date +%s)

  {
    printf 'image=%s\n' "$image"
    printf 'dockerfile=%s\n' "$dockerfile"
    printf 'elapsed_seconds=%s\n' "$((finished_at - started_at))"
    printf 'exit_status=%s\n' "$status"
  } >"$summary_file"
  parse_buildx_steps "$log_file" "$output_dir/image-$image.steps.txt"

  if [[ $status -ne 0 ]]; then
    printf 'Image timing scenario %s failed; see %s\n' "$image" "$log_file" >&2
    return "$status"
  fi
}

capture_images() {
  if ! command -v docker >/dev/null 2>&1; then
    printf 'status=unavailable\nreason=docker command not found\n' >"$output_dir/images.summary.txt"
    return 0
  fi
  if ! docker buildx version >"$output_dir/buildx-version.txt" 2>&1; then
    printf 'status=unavailable\nreason=docker buildx is not available\n' >"$output_dir/images.summary.txt"
    return 0
  fi

  printf 'status=available\n' >"$output_dir/images.summary.txt"
  capture_image light-workers Dockerfile.light-workers
  capture_image laserstream-workers Dockerfile.laserstream-workers
}

write_metadata

case "$mode" in
  --all)
    capture_graph
    capture_cargo_scenarios
    capture_images
    ;;
  --cargo-only)
    capture_cargo_scenarios
    ;;
  --graph-only)
    capture_graph
    ;;
  --images-only)
    capture_images
    ;;
esac

printf 'Build timing evidence written to %s\n' "$output_dir"
