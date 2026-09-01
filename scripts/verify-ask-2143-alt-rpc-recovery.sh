#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source_file="$repo_root/crates/loyal-yield-orchestrator/src/bin/route-lookup-table-provisioner.rs"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

test_list="$({
  cd "$repo_root"
  cargo test -p loyal-yield-orchestrator --bin route-lookup-table-provisioner \
    alt_provisioner_read_only_rpc -- --list
})"

required_tests=(
  alt_provisioner_read_only_rpc_retries_http_500_then_recovers
  alt_provisioner_read_only_rpc_retries_request_transport_then_recovers
  alt_provisioner_read_only_rpc_does_not_retry_http_400
  alt_provisioner_read_only_rpc_backoff_is_capped
)
for required_test in "${required_tests[@]}"; do
  rg -q "$required_test" <<<"$test_list" || fail "missing verifier test $required_test"
done

(
  cd "$repo_root"
  cargo test -p loyal-yield-orchestrator --bin route-lookup-table-provisioner \
    alt_provisioner_read_only_rpc -- --nocapture
  cargo fmt --all -- --check
  cargo check -p loyal-yield-orchestrator --bin route-lookup-table-provisioner
)

function_body() {
  local function_name="$1"
  FUNCTION_NAME="$function_name" perl -0ne '
    my $name = $ENV{"FUNCTION_NAME"};
    print $& if /async fn \Q$name\E\b.*?\n}\n(?=\n(?:async )?fn |\n#\[)/s;
  ' "$source_file"
}

planning_body="$(function_body plan_next_provisioning_request)"
[[ -n "$planning_body" ]] || fail "could not inspect plan_next_provisioning_request"
planning_slot_line="$(rg -n -m1 'finalized_slot_with_retry' <<<"$planning_body" | cut -d: -f1)"
planning_families_line="$(rg -n -m1 'active_lookup_table_families' <<<"$planning_body" | cut -d: -f1)"
planning_lease_line="$(rg -n -m1 'lease_next_lookup_table_provisioning_request' <<<"$planning_body" | cut -d: -f1)"
[[ -n "$planning_slot_line" && -n "$planning_families_line" && -n "$planning_lease_line" ]] ||
  fail "planning function is missing pre-lease inputs or request lease"
(( planning_slot_line < planning_lease_line )) || fail "finalized slot is loaded after request lease"
(( planning_families_line < planning_lease_line )) || fail "ALT families are loaded after request lease"

catalog_body="$(function_body reconcile_shared_market_catalog)"
rg -q 'finalized_slot_with_retry' <<<"$catalog_body" ||
  fail "shared catalog finalized slot bypasses read-only retry"

drift_body="$(function_body report_finalized_shared_drift_if_any)"
rg -q 'finalized_accounts_with_retry' <<<"$drift_body" ||
  fail "shared account bundle bypasses read-only retry"

run_body="$(function_body run)"
if rg -U -q 'retry_read_only_rpc[\s\S]{0,240}run_operation_batch|run_operation_batch[\s\S]{0,240}retry_read_only_rpc' <<<"$run_body"; then
  fail "run_operation_batch is behind the RPC retry boundary"
fi

echo "PASS: ASK-2143 read-only RPC recovery"
