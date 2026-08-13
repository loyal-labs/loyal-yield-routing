#!/usr/bin/env bash
set -euo pipefail

# Run the complete isolated fleet verifier, then gate the policy-setup funding
# safety invariant and the measured latency comparison. The underlying verifier
# builds every production fleet entrypoint, starts disposable PostgreSQL, runs
# successful lifecycle and transaction probes, and drives production-shaped
# concurrent queue load without contacting production services.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

for command_name in jq mktemp; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

runtime_tmp_root="${FLEET_LATENCY_RUNTIME_TMPDIR:-/tmp}"
evidence_dir="${FLEET_LATENCY_EVIDENCE_DIR:-$(mktemp -d "$runtime_tmp_root/fleet-latency-speedup-e2e.XXXXXX")}"
mkdir -p "$evidence_dir"

ASK1973_RUNTIME_TMPDIR="$runtime_tmp_root" \
ASK1973_EVIDENCE_DIR="$evidence_dir" \
  bash scripts/verify-ask-1973-fleet-e2e.sh

verifier_evidence="$evidence_dir/isolated-database-verifier.json"
[[ -s "$verifier_evidence" ]] || fail "isolated fleet verifier evidence is missing"

jq -e '
  def passed($name):
    [.checks[].subchecks[]?
      | select(.name == $name and .verdict == "PASS")]
    | length == 1;
  passed("policy_setup_funding_reservation_bounds_concurrent_debits_without_global_queue_lock")
  and passed("isolated_fleet_load_measures_policy_lock_removal_and_fused_handoff_speedup")
' "$verifier_evidence" >/dev/null ||
  fail "policy reservation safety or latency speedup subcheck did not pass"

jq -e '
  [.checks[].subchecks[]?
    | select(.name == "isolated_fleet_load_measures_policy_lock_removal_and_fused_handoff_speedup")
  ][0].evidence as $load
  | $load.isolated == true
    and $load.productionMutation == false
    and $load.legacy.conflictRetries > 0
    and $load.reservationNonFused.conflictRetries == 0
    and $load.reservationFused.conflictRetries == 0
    and $load.reservationFused.reservationAdmissionP95Micros < 50000
    and $load.attribution.policySerializationP50Millis > 0
    and $load.attribution.duplicateFinalBuildP50Millis > 0
    and $load.attribution.readyToSubmittedP50SpeedupPercent >= 50
    and $load.reservationFused.readyToSubmittedP50Millis
      < $load.legacy.readyToSubmittedP50Millis
' "$verifier_evidence" >/dev/null ||
  fail "isolated load did not achieve the required measured speedup"

jq '
  [.checks[].subchecks[]?
    | select(.name == "isolated_fleet_load_measures_policy_lock_removal_and_fused_handoff_speedup")
  ][0].evidence as $load
  | {
      schemaVersion: 1,
      event: "fleet_latency_speedup_e2e",
      status: "PASS",
      isolated: true,
      productionMutation: false,
      timeScale: $load.legacy.timeScale,
      jobsPerWave: $load.legacy.jobs,
      baseline: $load.legacy,
      reservationWithoutFusedHandoff: $load.reservationNonFused,
      optimized: $load.reservationFused,
      rootCauseAttribution: $load.attribution,
      safetySubcheck: "policy_setup_funding_reservation_bounds_concurrent_debits_without_global_queue_lock",
      latencySubcheck: "isolated_fleet_load_measures_policy_lock_removal_and_fused_handoff_speedup"
    }
' "$verifier_evidence" >"$evidence_dir/fleet-latency-speedup-summary.json"

jq -r '
  "PASS: isolated eight-job fleet wave improved ready-to-submit p50 from "
  + (.baseline.readyToSubmittedP50Millis | tostring) + "ms to "
  + (.optimized.readyToSubmittedP50Millis | tostring) + "ms ("
  + (.rootCauseAttribution.readyToSubmittedP50SpeedupPercent | tostring) + "% faster)",
  "PASS: policy serialization cost "
  + (.rootCauseAttribution.policySerializationP50Millis | tostring)
  + "ms; duplicate final build cost "
  + (.rootCauseAttribution.duplicateFinalBuildP50Millis | tostring) + "ms",
  "PASS: reservation admission p95 "
  + (.optimized.reservationAdmissionP95Micros | tostring)
  + "us with zero global-policy conflict retries",
  "evidence directory: " + $evidence
' --arg evidence "$evidence_dir" "$evidence_dir/fleet-latency-speedup-summary.json"
