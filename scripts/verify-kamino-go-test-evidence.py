#!/usr/bin/env python3
"""Validate Go test completion AND connected lifecycle evidence, never log slogans."""
import argparse
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
    "TestRealKLendProxyCrossMintLegs",
    "TestConnectedCrossMintPreflight",
    "TestConnectedSameMintLifecycle",
}
PACKAGE = "github.com/loyal-labs/loyal-yield-routing/go/kamino-fleet-planner/internal/fleet"
LANES = {"same-mint": "TestConnectedSameMintLifecycle", "cross-mint": "TestConnectedCrossMintPreflight"}
LEGS = {"same-mint": {"same_mint"}, "cross-mint": {"withdraw", "swap", "deposit"}}
MARKER = "KAMINO_CONNECTED_EVIDENCE "
# These stages must reference every signed leg, not merely the first leg.
ALL_LEG_STAGES = {
    "signed", "confirmed", "reconciled", "expired_reconcile_lease_recovered",
    "stale_reconciler_rejected", "exact_wire_replay_no_effect",
}
STAGES = ALL_LEG_STAGES | {
    "published", "revalidated", "ambiguous_broadcast_recovered",
    "duplicate_work_rejected", "telemetry_capacity_released", "terminal_balances_verified",
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def load_json(text):
    def unique_fields(pairs):
        value = {}
        for key, item in pairs:
            require(key not in value, f"duplicate JSON field: {key}")
            value[key] = item
        return value
    return json.loads(text, object_pairs_hook=unique_fields)


def positive(value):
    return type(value) is int and value > 0


def unique_objects(items, key, expected, label):
    require(isinstance(items, list) and all(isinstance(x, dict) for x in items), f"invalid {label}")
    names = [x.get(key) for x in items]
    require(all(isinstance(n, str) for n in names), f"invalid {label} names")
    require(len(names) == len(set(names)) and set(names) == expected, f"missing, duplicate or unknown {label}")


def lifecycle(record, lane, run_id):
    require(isinstance(record, dict), "lifecycle record must be an object")
    require(set(record) == {"schemaVersion", "runId", "lane", "cluster", "epochId", "opportunityId", "decisionId", "legs", "stages"}, "unknown or missing lifecycle fields")
    require(type(record.get("schemaVersion")) is int and record["schemaVersion"] == 1, "unsupported lifecycle schema")
    require(record.get("lane") == lane and record.get("runId") == run_id, "lifecycle lane/run identity mismatch")
    require(isinstance(record.get("cluster"), str) and bool(record["cluster"].strip()), "missing cluster")
    for field in ("epochId", "opportunityId", "decisionId"):
        require(positive(record.get(field)), f"missing positive {field}")
    legs = record.get("legs")
    unique_objects(legs, "name", LEGS[lane], "legs")
    submissions, signatures = set(), set()
    for leg in legs:
        require(set(leg) == {"name", "opportunityId", "submissionId", "signature", "confirmedSlot", "reconciledSlot"}, "unknown or missing leg fields")
        for field in ("opportunityId", "submissionId", "confirmedSlot", "reconciledSlot"):
            require(positive(leg.get(field)), f"invalid leg {field}")
        require(leg["reconciledSlot"] >= leg["confirmedSlot"], "reconciliation precedes confirmation")
        signature = leg.get("signature")
        require(isinstance(signature, str) and 64 <= len(signature) <= 88 and
                all(c in "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz" for c in signature), "invalid transaction signature")
        require(leg["submissionId"] not in submissions and signature not in signatures, "duplicate signed leg")
        submissions.add(leg["submissionId"])
        signatures.add(signature)
    initial = "same_mint" if lane == "same-mint" else "withdraw"
    require(next(x for x in legs if x["name"] == initial)["opportunityId"] == record["opportunityId"], "initial opportunity mismatch")
    stages = record.get("stages")
    unique_objects(stages, "name", STAGES, "stages")
    for stage in stages:
        require(set(stage) == {"name", "status", "submissionIds"}, "unknown or missing stage fields")
        require(stage.get("status") == "pass", f"non-passing lifecycle stage: {stage['name']}")
        refs = stage.get("submissionIds")
        require(isinstance(refs, list) and all(positive(x) for x in refs), "invalid stage submission references")
        require(len(refs) == len(set(refs)) and set(refs) <= submissions, "duplicate or foreign submission reference")
        if stage["name"] in ALL_LEG_STAGES:
            require(set(refs) == submissions, f"stage does not cover every leg: {stage['name']}")
        elif stage["name"] == "ambiguous_broadcast_recovered":
            require(bool(refs), "ambiguous recovery must identify a signed submission")
        elif stage["name"] in {"published", "revalidated"}:
            require(not refs, "pre-signature stage references signed work")
        else:
            require(set(refs) == submissions, f"terminal stage does not cover every leg: {stage['name']}")
    return signatures


def verify(events, *, run_id, lane=None):
    require(isinstance(run_id, str) and bool(run_id.strip()), "fresh run ID is required")
    require(lane is None or lane in LANES, "unknown focused lane")
    require(isinstance(events, list) and all(isinstance(e, dict) for e in events), "invalid Go event stream")
    relevant = [e for e in events if e.get("Package") == PACKAGE]
    require(not any(e.get("Action") in {"skip", "fail"} for e in relevant), "fleet suite contains skipped or failed tests")
    required = REQUIRED if lane is None else {LANES[lane]}
    for test in required:
        actions = [e.get("Action") for e in relevant if e.get("Test") == test]
        require(actions.count("run") == 1 and actions.count("pass") == 1, f"required test missing or duplicated: {test}")
        require(actions.index("run") < actions.index("pass"), f"test passed before running: {test}")
    finishes = [i for i, e in enumerate(relevant) if e.get("Action") == "pass" and "Test" not in e]
    require(len(finishes) == 1 and finishes[0] == len(relevant) - 1, "fleet package missing, duplicate or premature completion")
    expected_lanes = {lane} if lane else set(LANES)
    records, signatures, active, passed = {}, {}, set(), set()
    for e in relevant:
        test, action = e.get("Test"), e.get("Action")
        if action == "run":
            active.add(test)
        if action == "pass":
            passed.add(test)
        output = e.get("Output", "")
        require(isinstance(output, str), "invalid Go output")
        if MARKER not in output:
            continue
        require(action == "output" and test in active and test not in passed, "evidence outside running test")
        require(output.count(MARKER) == 1, "multiple evidence records in output")
        record = load_json(output.split(MARKER, 1)[1].strip())
        require(isinstance(record, dict), "invalid lifecycle record")
        record_lane = record.get("lane")
        require(isinstance(record_lane, str) and record_lane in expected_lanes and LANES[record_lane] == test, "evidence emitted by wrong test/lane")
        require(record_lane not in records, "duplicate connected lifecycle evidence")
        signatures[record_lane] = lifecycle(record, record_lane, run_id)
        records[record_lane] = record
    require(set(records) == expected_lanes, "missing connected lifecycle evidence")
    if len(records) == 2:
        require(not signatures["same-mint"] & signatures["cross-mint"], "lanes reused a transaction signature")
    return records


def self_test():
    import unittest
    suite = unittest.defaultTestLoader.discover(str(Path(__file__).parent), pattern="test_verify_kamino_go_test_evidence.py")
    require(unittest.TextTestRunner(verbosity=1).run(suite).wasSuccessful(), "evidence regression tests failed")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("events", nargs="?")
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--run-id")
    parser.add_argument("--development-lane", choices=LANES)
    args = parser.parse_args()
    try:
        if args.self_test:
            require(not args.events and not args.development_lane, "self-test cannot validate artifacts")
            self_test()
        else:
            require(bool(args.events), "Go JSON event file is required")
            events = [load_json(line) for line in Path(args.events).read_text().splitlines() if line.strip()]
            records = verify(events, run_id=args.run_id, lane=args.development_lane)
            # Scratch databases/logs are disposed by the runner. Keep the bounded,
            # strictly allowlisted trace in the caller's audit log before cleanup.
            for lane in sorted(records):
                print("VERIFIED_CONNECTED_EVIDENCE " + json.dumps(records[lane], sort_keys=True, separators=(",", ":")))
            scope = f"DEVELOPMENT ONLY: {args.development_lane}" if args.development_lane else "both lanes and full required suite"
            print(f"PASS: {scope}; connected lifecycle and recovery evidence verified")
    except (OSError, ValueError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
