#!/usr/bin/env python3

"""Combine three family E2E reports into one workflow critical-path report."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys


FAMILIES = (
    "laserstream-workers",
    "light-workers",
    "operator-tools",
)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--scenario", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("reports", type=Path, nargs=3)
    args = parser.parse_args()

    reports = {}
    for path in args.reports:
        with path.open(encoding="utf-8") as handle:
            report = json.load(handle)
        family = report.get("family")
        if family not in FAMILIES or family in reports:
            raise ValueError(f"unexpected or duplicate family {family!r} in {path}")
        reports[family] = report
    if tuple(sorted(reports)) != tuple(sorted(FAMILIES)):
        raise ValueError("reports do not cover all image families")

    cpus = {report["cpus"] for report in reports.values()}
    memory = {report["memory"] for report in reports.values()}
    if len(cpus) != 1 or len(memory) != 1:
        raise ValueError("family reports used different resource limits")

    prepackage = [
        report["total_seconds"] - report["package_critical_path_seconds"]
        for report in reports.values()
    ]
    package = [report["package_critical_path_seconds"] for report in reports.values()]
    summary = {
        "scenario": args.scenario,
        "cpus": cpus.pop(),
        "memory": memory.pop(),
        "workflow_contract_passed": all(
            report.get("workflow_contract_passed") is True for report in reports.values()
        ),
        "publish_critical_path_seconds": max(prepackage) + max(package),
        "families": {
            family: {
                field: report[field]
                for field in (
                    "build_seconds",
                    "cache_save_seconds",
                    "package_critical_path_seconds",
                    "total_seconds",
                    "binary_count",
                    "probe_passed",
                )
            }
            for family, report in reports.items()
        },
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with args.output.open("w", encoding="utf-8") as handle:
        json.dump(summary, handle, indent=2, sort_keys=True)
        handle.write("\n")
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, json.JSONDecodeError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        sys.exit(1)
