#!/usr/bin/env python3

"""Compare isolated worker-image rebuild reports against a fixed baseline."""

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
MAX_REGRESSION_RATIO = 1.10
MAX_CRITICAL_PATH_RATIO = 0.75


def load_report(path: Path) -> dict:
    with path.open(encoding="utf-8") as handle:
        report = json.load(handle)
    if not isinstance(report, dict):
        raise ValueError(f"{path}: expected a JSON object")
    return report


def number(report: dict, field: str, source: Path) -> float:
    value = report.get(field)
    if isinstance(value, bool) or not isinstance(value, (int, float)) or value < 0:
        raise ValueError(f"{source}: {field} must be a non-negative number")
    return float(value)


def fail(message: str, failures: list[str]) -> None:
    print(f"FAIL: {message}")
    failures.append(message)


def passed(message: str) -> None:
    print(f"PASS: {message}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("baseline", type=Path)
    parser.add_argument("candidate", type=Path)
    args = parser.parse_args()

    try:
        baseline = load_report(args.baseline)
        candidate = load_report(args.candidate)
        failures: list[str] = []

        for field in ("scenario", "cpus", "memory"):
            if candidate.get(field) == baseline.get(field):
                passed(f"candidate uses the baseline {field}: {candidate.get(field)}")
            else:
                fail(
                    f"candidate {field} {candidate.get(field)!r} does not match "
                    f"baseline {baseline.get(field)!r}",
                    failures,
                )

        for name, report in (("baseline", baseline), ("candidate", candidate)):
            families = report.get("families")
            if not isinstance(families, dict) or tuple(sorted(families)) != tuple(sorted(FAMILIES)):
                fail(f"{name} report must contain exactly {', '.join(FAMILIES)}", failures)
                continue
            passed(f"{name} report contains all three image families")
            if report.get("workflow_contract_passed") is True:
                passed(f"{name} workflow contract passed")
            else:
                fail(f"{name} workflow contract did not pass", failures)

        baseline_families = baseline.get("families", {})
        candidate_families = candidate.get("families", {})
        for family in FAMILIES:
            before = baseline_families.get(family, {})
            after = candidate_families.get(family, {})
            if not isinstance(before, dict) or not isinstance(after, dict):
                continue

            if after.get("binary_count") == before.get("binary_count") and after.get("binary_count", 0) > 0:
                passed(f"{family} preserves {after['binary_count']} packaged binaries")
            else:
                fail(f"{family} packaged binary inventory changed", failures)

            if after.get("probe_passed") is True:
                passed(f"{family} runtime probe passed")
            else:
                fail(f"{family} runtime probe did not pass", failures)

            for field in ("build_seconds", "total_seconds"):
                before_value = number(before, field, args.baseline)
                after_value = number(after, field, args.candidate)
                limit = before_value * MAX_REGRESSION_RATIO
                if after_value <= limit:
                    passed(
                        f"{family} {field} did not regress: "
                        f"{before_value:.0f}s -> {after_value:.0f}s"
                    )
                else:
                    fail(
                        f"{family} {field} regressed by more than 10%: "
                        f"{before_value:.0f}s -> {after_value:.0f}s",
                        failures,
                    )

        before_critical = number(baseline, "publish_critical_path_seconds", args.baseline)
        after_critical = number(candidate, "publish_critical_path_seconds", args.candidate)
        critical_limit = before_critical * MAX_CRITICAL_PATH_RATIO
        improvement = 0.0 if before_critical == 0 else 1 - after_critical / before_critical
        if before_critical > 0 and after_critical <= critical_limit:
            passed(
                "publish critical path improved by at least 25%: "
                f"{before_critical:.0f}s -> {after_critical:.0f}s ({improvement:.1%})"
            )
        else:
            fail(
                "publish critical path improvement is below 25%: "
                f"{before_critical:.0f}s -> {after_critical:.0f}s ({improvement:.1%})",
                failures,
            )

    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"FAIL: {error}")
        print("OVERALL: FAIL")
        return 1

    if failures:
        print(f"OVERALL: FAIL ({len(failures)} conditions failed)")
        return 1
    print("OVERALL: PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
