#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
observation_source="$repo_root/crates/loyal-yield-orchestrator/src/fleet_orchestration/observation.rs"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

command -v rg >/dev/null || fail "rg is required"
command -v cargo >/dev/null || fail "cargo is required"

echo "== Check the redundant correlated source-count scan is absent"
if rg --multiline --quiet \
  'FROM planning_vaults planning\s+WHERE NOT EXISTS \(\s+SELECT 1 FROM sources source\s+WHERE source\.vault_id = planning\.vault_id' \
  "$observation_source"; then
  fail "planner SQL still rescans sources once per planning vault"
fi

echo "== Check the partition remainder is derived with checked arithmetic"
rg --fixed-strings --quiet \
  'fn derive_no_positive_current_source_vault_count(' \
  "$observation_source" ||
  fail "checked source-count derivation helper is missing"

echo "== Run focused partition tests"
cargo test -p loyal-yield-orchestrator --lib \
  no_positive_current_source_vault_count_is_partition_remainder
cargo test -p loyal-yield-orchestrator --lib \
  no_positive_current_source_vault_count_rejects_invalid_partition

echo "== Check formatting and planner compilation"
bash -n "$repo_root/scripts/explain-fleet-opportunity-planner-source-count.sh"
cargo fmt --all --check
cargo check -p loyal-yield-orchestrator --bin fleet-opportunity-planner

echo "PASS: local fleet opportunity planner source-count verifier"
