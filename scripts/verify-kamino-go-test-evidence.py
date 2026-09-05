#!/usr/bin/env python3
"""Fail closed on skipped, missing, or failed required Go integration tests."""
import json
import sys
from pathlib import Path

REQUIRED = {
    "TestMarketEvidenceStoreLoadsRealMonitorIdentity",
    "TestLoadMigratedFleetBuildsFinalizedCrossMintPolicyBindings",
    "TestStoreIntegrationDurableHandoffWithoutPlannerMigration",
    "TestWorkerIntegrationCutoverWithoutRustMonitorOrPlanner",
    "TestRevalidationStoreIntegrationFusedExecuteIsAtomic",
    "TestLoadReusableLookupTablesScopesStaleCandidatesBeforeRPC",
    "TestExpiryIntegrationRecoveryAndOwnership",
    "TestFreshPolicyWrapALTAndExactV0",
    "TestConfigRejectsShadowRevalidation",
}
PACKAGE = "github.com/loyal-labs/loyal-yield-routing/go/kamino-fleet-planner/internal/fleet"


def verify(events):
    relevant = [e for e in events if e.get("Package") == PACKAGE]
    if any(e.get("Action") in {"skip", "fail"} for e in relevant):
        raise ValueError("fleet suite contains skipped or failed tests")
    passed = {e.get("Test") for e in relevant if e.get("Action") == "pass"}
    if REQUIRED - passed:
        raise ValueError(f"required tests did not pass: {sorted(REQUIRED - passed)}")
    if not any(e.get("Action") == "pass" and "Test" not in e for e in relevant):
        raise ValueError("fleet package did not finish successfully")


def self_test():
    good = [{"Package": PACKAGE, "Test": t, "Action": "pass"} for t in sorted(REQUIRED)]
    good.append({"Package": PACKAGE, "Action": "pass"})
    verify(good)
    for bad in ([], good[1:], good[:-1], good + [{"Package": PACKAGE, "Test": "subtest", "Action": "skip"}], good + [{"Package": PACKAGE, "Action": "fail"}]):
        try:
            verify(bad)
        except ValueError:
            continue
        raise ValueError("test-evidence negative control accepted")
    print("PASS: required-test gate rejects missing, skipped, failed, and incomplete evidence")


try:
    if len(sys.argv) == 2 and sys.argv[1] == "--self-test":
        self_test()
    elif len(sys.argv) == 2:
        events = [json.loads(line) for line in Path(sys.argv[1]).read_text().splitlines() if line.strip()]
        verify(events)
        print(f"PASS: all {len(REQUIRED)} required tests executed; no fleet tests skipped")
    else:
        raise ValueError("usage: verify-kamino-go-test-evidence.py --self-test | go-test.jsonl")
except (OSError, ValueError) as error:
    print(f"FAIL: {error}", file=sys.stderr)
    sys.exit(1)
