#!/usr/bin/env python3
"""Compare computed planner/wire artifacts, NOT simulation or lifecycle claims."""
import argparse
import copy
import json
import re
import sys
from pathlib import Path

SCOPE = "deterministic_planner_and_same_mint_wire"
SHA256 = re.compile(r"[0-9a-f]{64}")
HEX = re.compile(r"(?:[0-9a-f]{2})+")


def validate(a, implementation):
    if not isinstance(a, dict) or set(a) != {"schemaVersion", "implementation", "scope", "fixture", "opportunities", "route"}:
        raise ValueError("unexpected artifact shape; legacy lifecycle/simulation assertions are not evidence")
    if a["schemaVersion"] != 2 or a["scope"] != SCOPE or a["implementation"] != implementation:
        raise ValueError("wrong schema, scope, or implementation")
    f = a["fixture"]
    if not isinstance(f, dict) or set(f) != {"id", "sha256", "clock"} or not f["id"] or not f["clock"] or not SHA256.fullmatch(f["sha256"]):
        raise ValueError("invalid fixture binding")
    if not isinstance(a["opportunities"], list) or not a["opportunities"]:
        raise ValueError("missing computed opportunities")
    for o in a["opportunities"]:
        if set(o) != {"idempotencyKey", "executionPlan", "sourceApyBps", "targetApyBps", "estimatedEdgeBps"} or not SHA256.fullmatch(o["idempotencyKey"]) or not isinstance(o["executionPlan"], dict):
            raise ValueError("invalid computed opportunity")
        if not all(type(o[k]) is int for k in ("sourceApyBps", "targetApyBps", "estimatedEdgeBps")):
            raise ValueError("invalid economics")
    r = a["route"]
    if set(r) != {"fingerprint", "messageHex", "wireHex"} or not SHA256.fullmatch(r["fingerprint"]):
        raise ValueError("invalid route fingerprint")
    if not all(HEX.fullmatch(r[k]) for k in ("messageHex", "wireHex")):
        raise ValueError("missing exact message/wire bytes")
    if len(bytes.fromhex(r["wireHex"])) > 1232 or not r["wireHex"].endswith(r["messageHex"]):
        raise ValueError("invalid packet or message/wire binding")


def compare(reference, candidate):
    validate(reference, "rust")
    validate(candidate, "go")
    for key in ("scope", "fixture", "opportunities", "route"):
        if reference[key] != candidate[key]:
            raise ValueError(f"Rust/Go {key} differs")


def self_test():
    reference = {
        "schemaVersion": 2, "implementation": "rust", "scope": SCOPE,
        "fixture": {"id": "synthetic", "sha256": "a" * 64, "clock": "2026-01-01T00:00:00Z"},
        "opportunities": [{"idempotencyKey": "b" * 64, "executionPlan": {"amount_raw": 100}, "sourceApyBps": 100, "targetApyBps": 200, "estimatedEdgeBps": 100}],
        "route": {"fingerprint": "c" * 64, "messageHex": "aabb", "wireHex": "00aabb"},
    }
    candidate = copy.deepcopy(reference)
    candidate["implementation"] = "go"
    compare(reference, candidate)
    mutations = {
        "economics": lambda a: a["opportunities"][0].update(targetApyBps=201),
        "identity": lambda a: a["opportunities"][0].update(idempotencyKey="d" * 64),
        "plan": lambda a: a["opportunities"][0]["executionPlan"].update(amount_raw=101),
        "missing opportunity": lambda a: a.update(opportunities=[]),
        "route": lambda a: a["route"].update(fingerprint="e" * 64),
        "message": lambda a: a["route"].update(messageHex="bb"),
        "wire": lambda a: a["route"].update(wireHex="01aabb"),
        "oversize": lambda a: a["route"].update(wireHex="00" * 1233 + "aabb"),
        "fixture": lambda a: a["fixture"].update(sha256="f" * 64),
        "scope": lambda a: a.update(scope="complete_replacement"),
        "fabricated lifecycle": lambda a: a.update(lifecycle={"confirmationObserved": True}),
        "fabricated simulation": lambda a: a.update(simulation={"succeeded": True}),
        "legacy schema": lambda a: a.update(schemaVersion=1),
    }
    for name, mutate in mutations.items():
        changed = copy.deepcopy(candidate)
        mutate(changed)
        try:
            compare(reference, changed)
        except (ValueError, TypeError, KeyError):
            continue
        raise ValueError(f"negative control not detected: {name}")
    print(f"PASS: {len(mutations)} comparator mutation controls (not runtime failure tests)")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--reference")
    parser.add_argument("--candidate")
    args = parser.parse_args()
    try:
        if args.self_test:
            self_test()
        else:
            if not args.reference or not args.candidate:
                raise ValueError("--reference and --candidate are required")
            compare(json.loads(Path(args.reference).read_text()), json.loads(Path(args.candidate).read_text()))
            print("PASS: computed planner opportunities and exact same-mint message/wire bytes match Rust")
        return 0
    except (OSError, ValueError, TypeError, KeyError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
