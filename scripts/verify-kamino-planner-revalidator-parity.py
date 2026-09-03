#!/usr/bin/env python3
"""Strict comparator for the offline Kamino Go replacement parity artifacts."""

from __future__ import annotations

import argparse
import copy
import json
import re
import sys
from pathlib import Path
from typing import Any

REQUIRED_REVALIDATION_CASES = {
    "fresh_route_ready",
    "fresh_route_fused_execute",
    "missing_reusable_alt",
    "oversized_packet",
    "simulation_failure",
    "stale_market_epoch",
    "changed_opportunity_fence",
    "lost_lease",
}
REQUIRED_LIFECYCLE = [
    "revalidate",
    "ready",
    "leased",
    "decision_created",
    "submitted",
    "confirmed",
    "reconciled",
    "completed",
]
SHA256 = re.compile(r"^[0-9a-f]{64}$")


class ParityFailure(Exception):
    pass


def load(path: str) -> dict[str, Any]:
    value = json.loads(Path(path).read_text())
    if not isinstance(value, dict):
        raise ParityFailure(f"{path}: artifact must be a JSON object")
    return value


def require(condition: bool, message: str, issues: list[str]) -> None:
    if not condition:
        issues.append(message)


def validate_common(artifact: dict[str, Any], label: str) -> list[str]:
    issues: list[str] = []
    require(artifact.get("schemaVersion") == 1, f"{label}: schemaVersion must be 1", issues)

    fixture = artifact.get("fixture", {})
    require(isinstance(fixture.get("id"), str) and fixture.get("id"), f"{label}: fixture.id missing", issues)
    require(bool(SHA256.fullmatch(str(fixture.get("sha256", "")))), f"{label}: fixture.sha256 invalid", issues)
    require(isinstance(fixture.get("clock"), str) and fixture.get("clock"), f"{label}: fixture.clock missing", issues)

    isolation = artifact.get("isolation", {})
    for field in (
        "productionCredentialsLoaded",
        "productionDatabaseAccessed",
        "externalRpcAccessed",
        "externalHttpAccessed",
        "transactionBroadcast",
    ):
        require(isolation.get(field) is False, f"{label}: isolation.{field} must be false", issues)
    require(isolation.get("outboundNetworkAttempts") == 0, f"{label}: outbound network attempts must be zero", issues)
    require(isolation.get("databaseKind") == "disposable_postgres", f"{label}: disposable PostgreSQL evidence missing", issues)
    require(isolation.get("rpcKind") == "deterministic_loopback", f"{label}: loopback RPC evidence missing", issues)

    planner = artifact.get("planner", {})
    epoch = planner.get("marketEpoch", {})
    reserves = epoch.get("reserves")
    coverage = epoch.get("mintCoverage")
    require(isinstance(reserves, list) and len(reserves) >= 3, f"{label}: complete multi-reserve epoch missing", issues)
    require(isinstance(coverage, list) and coverage, f"{label}: mint coverage missing", issues)
    if isinstance(coverage, list):
        require(all(item.get("complete") is True for item in coverage if isinstance(item, dict)), f"{label}: incomplete mint coverage", issues)
    require(bool(SHA256.fullmatch(str(epoch.get("fingerprint", "")))), f"{label}: epoch fingerprint invalid", issues)
    require(planner.get("epochRoundTrip") is True, f"{label}: Rust ImmutableMarketEpoch round trip not proven", issues)
    require(planner.get("canonicalExecutionPlans") is True, f"{label}: canonical execution plans not proven", issues)
    require(planner.get("canonicalOpportunityIdentities") is True, f"{label}: canonical opportunity identities not proven", issues)
    opportunities = planner.get("opportunities")
    require(isinstance(opportunities, list) and opportunities, f"{label}: planner opportunities missing", issues)
    if isinstance(opportunities, list):
        for index, opportunity in enumerate(opportunities):
            if not isinstance(opportunity, dict):
                issues.append(f"{label}: planner opportunity {index} is not an object")
                continue
            for field in ("idempotencyKey", "executionPlan", "sourceApyBps", "targetApyBps", "estimatedEdgeBps"):
                require(field in opportunity, f"{label}: opportunity {index} missing {field}", issues)
            require(bool(SHA256.fullmatch(str(opportunity.get("idempotencyKey", "")))), f"{label}: opportunity {index} idempotency key invalid", issues)

    revalidator = artifact.get("revalidator", {})
    cases = revalidator.get("cases")
    require(isinstance(cases, list), f"{label}: revalidator cases missing", issues)
    names: set[str] = set()
    if isinstance(cases, list):
        for case in cases:
            if not isinstance(case, dict):
                issues.append(f"{label}: revalidator case is not an object")
                continue
            name = case.get("name")
            require(isinstance(name, str) and name not in names, f"{label}: duplicate/invalid revalidator case {name!r}", issues)
            if isinstance(name, str):
                names.add(name)
            for field in ("disposition", "queueTransition", "routeFingerprint", "requirementsFingerprint", "altAddresses", "packet", "simulation", "opportunityFence", "marketEpochFence"):
                require(field in case, f"{label}: revalidator case {name!r} missing {field}", issues)
        require(REQUIRED_REVALIDATION_CASES <= names, f"{label}: missing revalidation cases {sorted(REQUIRED_REVALIDATION_CASES - names)}", issues)
    require(revalidator.get("typedInProcessHandoff") is True, f"{label}: typed planner/revalidator handoff not proven", issues)

    lifecycle = artifact.get("lifecycle", {})
    require(lifecycle.get("states") == REQUIRED_LIFECYCLE, f"{label}: complete durable lifecycle not proven", issues)
    for field in (
        "signedWirePersistedBeforeBroadcast",
        "leaseAndConflictFencesAtomic",
        "confirmationObserved",
        "reconciliationObserved",
        "noDuplicateCapitalMovement",
    ):
        require(lifecycle.get(field) is True, f"{label}: lifecycle.{field} not proven", issues)
    return issues


def validate_candidate(artifact: dict[str, Any]) -> list[str]:
    issues = validate_common(artifact, "go")
    topology = artifact.get("topology", {})
    require(topology.get("serviceProcessCount") == 1, "go: replacement must use one service process", issues)
    require(topology.get("goOwnedRoles") == ["opportunity_planner", "route_revalidator"], "go: one service must own planner and revalidator roles", issues)
    require(topology.get("rustPlannerStarted") is False, "go: Rust planner was started", issues)
    require(topology.get("rustRevalidatorStarted") is False, "go: Rust revalidator was started", issues)
    require(topology.get("argvHandoffUsed") is False, "go: argv handoff is forbidden", issues)
    require(topology.get("childStdoutHandoffUsed") is True, "go: KLend proxy stdin/stdout boundary not proven", issues)
    require(topology.get("klendProxyOnlyChild") is True, "go: an unapproved child process was used", issues)
    proxy = artifact.get("revalidator", {}).get("klendProxy", {})
    require(artifact.get("revalidator", {}).get("childProcessesSpawned", 0) > 0, "go: KLend proxy was not invoked", issues)
    require(proxy.get("officialKlendBuilders") is True, "go: official KLend builders not proven", issues)
    require(proxy.get("transport") == "stdin_stdout_json_v1", "go: KLend proxy transport drifted", issues)
    require(proxy.get("networkAccess") is False and proxy.get("databaseAccess") is False and proxy.get("signerAccess") is False and proxy.get("broadcastCapability") is False, "go: KLend proxy is not pure and isolated", issues)
    require(bool(SHA256.fullmatch(str(proxy.get("binarySha256", "")))), "go: KLend proxy binary hash missing", issues)
    require(topology.get("durablePostgresHandoff") is True, "go: PostgreSQL handoff not proven", issues)
    require(topology.get("retainedRustRoles") == ["executor", "confirmer", "reconciler", "health_projector", "alt_provisioner"], "go: retained Rust role boundary drifted", issues)
    return issues


def comparable(artifact: dict[str, Any]) -> dict[str, Any]:
    lifecycle = artifact.get("lifecycle", {})
    return {
        "fixture": artifact.get("fixture"),
        "planner": artifact.get("planner"),
        "revalidator": {
            "typedInProcessHandoff": artifact.get("revalidator", {}).get("typedInProcessHandoff"),
            "cases": artifact.get("revalidator", {}).get("cases"),
        },
        "lifecycle": {
            key: lifecycle.get(key)
            for key in (
                "states",
                "signedWirePersistedBeforeBroadcast",
                "leaseAndConflictFencesAtomic",
                "confirmationObserved",
                "reconciliationObserved",
                "noDuplicateCapitalMovement",
                "terminalState",
            )
        },
    }


def compare(reference: dict[str, Any], candidate: dict[str, Any]) -> list[str]:
    issues = validate_common(reference, "rust") + validate_candidate(candidate)
    if comparable(reference) != comparable(candidate):
        issues.append("rust/go planner, revalidator, or lifecycle evidence differs")
    return issues


def sample(implementation: str) -> dict[str, Any]:
    digest = "1" * 64
    fingerprint = "2" * 64
    key = "3" * 64
    epoch = {
        "optimizerEpochId": 7,
        "fingerprint": fingerprint,
        "catalogFingerprint": "4" * 64,
        "mintCoverage": [{"mint": "USDC", "complete": True}],
        "reserves": [{"reserve": name} for name in ("source", "target", "peer")],
    }
    case_defaults = {
        "disposition": "ready",
        "queueTransition": {"from": "revalidate", "to": "ready"},
        "routeFingerprint": "5" * 64,
        "requirementsFingerprint": "6" * 64,
        "altAddresses": ["alt"],
        "packet": {"sha256": "7" * 64, "bytes": 900},
        "simulation": {"succeeded": True, "unitsConsumed": 123},
        "opportunityFence": "current",
        "marketEpochFence": "current",
    }
    cases = [dict(case_defaults, name=name) for name in sorted(REQUIRED_REVALIDATION_CASES)]
    return {
        "schemaVersion": 1,
        "implementation": implementation,
        "fixture": {"id": "synthetic-v1", "sha256": digest, "clock": "2026-01-01T00:00:00Z"},
        "isolation": {
            "productionCredentialsLoaded": False,
            "productionDatabaseAccessed": False,
            "externalRpcAccessed": False,
            "externalHttpAccessed": False,
            "transactionBroadcast": False,
            "outboundNetworkAttempts": 0,
            "databaseKind": "disposable_postgres",
            "rpcKind": "deterministic_loopback",
        },
        "topology": {
            "serviceProcessCount": 1,
            "goOwnedRoles": ["opportunity_planner", "route_revalidator"],
            "rustPlannerStarted": False,
            "rustRevalidatorStarted": False,
            "argvHandoffUsed": False,
            "childStdoutHandoffUsed": implementation == "go",
            "klendProxyOnlyChild": implementation == "go",
            "durablePostgresHandoff": True,
            "retainedRustRoles": ["executor", "confirmer", "reconciler", "health_projector", "alt_provisioner"],
        },
        "planner": {
            "marketEpoch": epoch,
            "epochRoundTrip": True,
            "canonicalExecutionPlans": True,
            "canonicalOpportunityIdentities": True,
            "opportunities": [{"idempotencyKey": key, "executionPlan": {"kind": "same_mint"}, "sourceApyBps": 100, "targetApyBps": 200, "estimatedEdgeBps": 100}],
        },
        "revalidator": {
            "typedInProcessHandoff": True,
            "childProcessesSpawned": len(cases) if implementation == "go" else 0,
            "klendProxy": ({
                "officialKlendBuilders": True,
                "transport": "stdin_stdout_json_v1",
                "networkAccess": False,
                "databaseAccess": False,
                "signerAccess": False,
                "broadcastCapability": False,
                "binarySha256": "a" * 64,
            } if implementation == "go" else {}),
            "cases": cases,
        },
        "lifecycle": {
            "states": REQUIRED_LIFECYCLE,
            "signedWirePersistedBeforeBroadcast": True,
            "leaseAndConflictFencesAtomic": True,
            "confirmationObserved": True,
            "reconciliationObserved": True,
            "noDuplicateCapitalMovement": True,
            "terminalState": "completed",
        },
    }


def self_test() -> None:
    reference = sample("rust")
    candidate = sample("go")
    if compare(reference, candidate):
        raise ParityFailure("identical evidence did not pass")
    mutations = {
        "planner economics": lambda x: x["planner"]["opportunities"][0].update(targetApyBps=201),
        "opportunity identity": lambda x: x["planner"]["opportunities"][0].update(idempotencyKey="8" * 64),
        "epoch frontier": lambda x: x["planner"]["marketEpoch"]["reserves"].pop(),
        "route fingerprint": lambda x: x["revalidator"]["cases"][0].update(routeFingerprint="9" * 64),
        "ALT evidence": lambda x: x["revalidator"]["cases"][0].update(altAddresses=[]),
        "packet evidence": lambda x: x["revalidator"]["cases"][0]["packet"].update(bytes=901),
        "simulation evidence": lambda x: x["revalidator"]["cases"][0]["simulation"].update(unitsConsumed=124),
        "queue transition": lambda x: x["revalidator"]["cases"][0].update(queueTransition={"from": "revalidate", "to": "leased"}),
        "missing negative case": lambda x: x["revalidator"].update(cases=x["revalidator"]["cases"][1:]),
        "process topology": lambda x: x["topology"].update(serviceProcessCount=2),
        "KLend proxy isolation": lambda x: x["revalidator"]["klendProxy"].update(networkAccess=True),
        "network isolation": lambda x: x["isolation"].update(externalRpcAccessed=True, outboundNetworkAttempts=1),
        "durable lifecycle": lambda x: x["lifecycle"].update(confirmationObserved=False),
    }
    for name, mutate in mutations.items():
        changed = copy.deepcopy(candidate)
        mutate(changed)
        if not compare(reference, changed):
            raise ParityFailure(f"negative control was not detected: {name}")
    print(f"PASS: parity comparator rejected {len(mutations)} planner/revalidator/isolation negative controls")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--reference")
    parser.add_argument("--candidate")
    args = parser.parse_args()
    try:
        if args.self_test:
            self_test()
            return 0
        if not args.reference or not args.candidate:
            raise ParityFailure("--reference and --candidate are required")
        issues = compare(load(args.reference), load(args.candidate))
        if issues:
            for issue in issues:
                print(f"FAIL: {issue}", file=sys.stderr)
            return 1
        print("PASS: Go planner/revalidator with the isolated KLend proxy exactly matches Rust reference evidence")
        return 0
    except (OSError, json.JSONDecodeError, ParityFailure) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
