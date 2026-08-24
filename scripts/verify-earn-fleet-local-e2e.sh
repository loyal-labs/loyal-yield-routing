#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
routing_root="$(cd "$script_dir/.." && pwd)"
scratch_dir="$(mktemp -d /tmp/earn-fleet-local-e2e.XXXXXX)"
monitor_log="$scratch_dir/monitor-e2e.log"
fleet_log="$scratch_dir/fleet-e2e.log"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cleanup() {
  rm -rf "$scratch_dir"
}
trap cleanup EXIT

[[ "${1:-}" == "--app-root" && -n "${2:-}" ]] ||
  fail "usage: $0 --app-root PATH"

"$routing_root/scripts/verify-autodeposit-local-e2e.sh" --app-root "$2" 2>&1 |
  tee "$monitor_log"
"$routing_root/scripts/verify-ask-1973-fleet-e2e.sh" 2>&1 |
  tee "$fleet_log"

rg -q --fixed-strings \
  "PASS: successful durable lifecycle reached ready, signed, submitted, confirmed, reconciled, and completed" \
  "$fleet_log" || fail "fleet lifecycle did not complete a rebalance"
rg -q --fixed-strings \
  "PASS: production-shaped reconciliation load drained with zero operational alerts" \
  "$monitor_log" || fail "monitor reconciliation load did not drain alert-free"

if rg -q 'loyal\.operational_error|earn_reconciliation_job_failed|earn_reconciliation_consumer_failed' \
  "$monitor_log" "$fleet_log"; then
  fail "local monitor or fleet run emitted an operational alert"
fi

echo "PASS: isolated monitor and fleet services handled emulated load, completed rebalances, and emitted no alerts"
