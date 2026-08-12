#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
fixture_source="$repo_root/scripts/fixtures/ask-1973-orchestrator-facade.rs"
output_dir="$(mktemp -d "${TMPDIR:-/tmp}/ask1973-public-api.XXXXXX")"
trap 'rm -rf "$output_dir"' EXIT

import_count="$(grep -c '^use loyal_yield_orchestrator' "$fixture_source")"
artifact="$({
  cargo build --package loyal-yield-orchestrator --lib --locked \
    --message-format=json-render-diagnostics
} | jq -sr '
  map(select(
    .reason == "compiler-artifact"
    and .target.name == "loyal_yield_orchestrator"
  ))
  | last
  | .filenames[]
  | select(endswith(".rlib"))
')"
artifact_dir="$(dirname "$artifact")"

[[ -n "$artifact" && -f "$artifact" ]] || {
  echo "FAIL: loyal-yield-orchestrator rlib was not produced" >&2
  exit 1
}

rustc --edition=2021 \
  --crate-name ask_1973_orchestrator_facade_verifier \
  --emit=metadata \
  --out-dir "$output_dir" \
  -L "dependency=$artifact_dir" \
  -L "dependency=$artifact_dir/deps" \
  --extern "loyal_yield_orchestrator=$artifact" \
  "$fixture_source"

echo "PASS: $import_count legacy loyal-yield-orchestrator facade paths compile"
