#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

for command_name in cargo rg; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

worker_source="$repo_root/crates/loyal-fleet-worker/src/lib.rs"
store_source="$repo_root/crates/loyal-yield-store/src/fleet_orchestration/queue.rs"

rg --quiet 'push\(format!\("policy-setup-funding:' "$worker_source" ||
  fail "same-mint worker no longer emits the policy setup funding lock"
rg --quiet 'starts_with\("policy-setup-funding:' "$store_source" ||
  fail "store validator no longer recognizes the policy setup funding lock"
rg --quiet 'policy_setup_funding_key_count > 1' "$store_source" ||
  fail "store validator no longer limits policy setup funding locks"

cd "$repo_root"

echo "== Verify production-shaped conflict ownership admission"
cargo test -p loyal-yield-store --lib conflict_ownership

echo "== Compile the planner and route worker handoff"
cargo check \
  -p loyal-yield-orchestrator --bin fleet-opportunity-planner \
  -p loyal-fleet-worker --bin same-mint-reserve-swap

echo "== Check formatting and patch hygiene"
cargo fmt --all -- --check
git diff --check

echo "PASS: ASK-2222 admits one ownership pair plus the optional policy setup funding lock"
