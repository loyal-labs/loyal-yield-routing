#!/usr/bin/env python3
"""Synthetic controls for the evidence gate; these are NOT lifecycle evidence."""
import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("verify-kamino-go-test-evidence.py")
spec = importlib.util.spec_from_file_location("evidence_gate", SCRIPT)
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)
RUN = "synthetic-control-run"


def record(lane):
    base = 100 if lane == "same-mint" else 200
    names = ["same_mint"] if lane == "same-mint" else ["withdraw", "swap", "deposit"]
    legs = [{"name": name, "opportunityId": base + i, "submissionId": base + 10 + i,
             "signature": "123456"[i + (3 if lane == "same-mint" else 0)] * 88,
             "confirmedSlot": 1000, "reconciledSlot": 1000} for i, name in enumerate(names)]
    ids = [x["submissionId"] for x in legs]
    return {"schemaVersion": 1, "runId": RUN, "lane": lane, "cluster": "local-test-" + lane,
            "epochId": base, "opportunityId": base, "decisionId": base,
            "legs": legs, "stages": [
                {"name": name, "status": "pass", "submissionIds":
                 [] if name in {"published", "revalidated"} else
                 ids[:1] if name == "ambiguous_broadcast_recovered" else ids[:]}
                for name in sorted(gate.STAGES)]}


def event(action, test=None, **kwargs):
    value = {"Package": gate.PACKAGE, "Action": action, **kwargs}
    if test is not None:
        value["Test"] = test
    return value


def stream(lane=None):
    events = []
    tests = {gate.LANES[lane]} if lane else gate.REQUIRED
    for test in sorted(tests):
        events.append(event("run", test))
        for name, owner in gate.LANES.items():
            if test == owner:
                events.append(event("output", test, Output="    harness_test.go:1: " + gate.MARKER + json.dumps(record(name)) + "\n"))
        events.append(event("pass", test))
    events.append(event("pass"))
    return events


def mutate_record(events, lane, change):
    for e in events:
        if e.get("Test") == gate.LANES[lane] and gate.MARKER in e.get("Output", ""):
            value = json.loads(e["Output"].split(gate.MARKER)[1])
            change(value)
            e["Output"] = gate.MARKER + json.dumps(value) + "\n"
            return
    raise AssertionError("fixture record missing")


class EvidenceTests(unittest.TestCase):
    def reject(self, events, **kwargs):
        with self.assertRaises(ValueError):
            gate.verify(events, run_id=RUN, **kwargs)

    def test_complete_and_focused(self):
        gate.verify(stream(), run_id=RUN)
        for lane in gate.LANES:
            gate.verify(stream(lane), run_id=RUN, lane=lane)
            self.reject(stream(lane))  # Development never qualifies as the full gate.
            self.reject(stream(), lane=lane)

    def test_missing_skipped_failed_duplicate_tests(self):
        good = stream()
        self.reject([])
        self.reject(good[:-1])
        self.reject(good + [event("pass")])
        for test in gate.REQUIRED:
            with self.subTest(test=test):
                self.reject([e for e in good if e.get("Test") != test])
                for action in ("skip", "fail", "pass", "run"):
                    self.reject(good[:-1] + [event(action, test)] + good[-1:])
        self.reject(good[:-1] + [event("skip", "Unknown/subtest")] + good[-1:])
        self.reject(good[:-1] + [event("fail")] + good[-1:])

    def test_missing_and_duplicate_records(self):
        for lane in gate.LANES:
            good = stream()
            index = next(i for i, e in enumerate(good) if e.get("Test") == gate.LANES[lane] and gate.MARKER in e.get("Output", ""))
            self.reject(good[:index] + good[index + 1:])
            self.reject(good[:index] + [good[index]] + good[index:])
            bad = copy.deepcopy(good)
            bad[index]["Package"] = "foreign/package"
            self.reject(bad)
            bad = copy.deepcopy(good)
            bad[index]["Test"] = gate.LANES[lane] + "/subtest"
            self.reject(bad)
            # A record after the owning test has passed is not execution evidence.
            self.reject(good[:index] + [good[index + 1], good[index]] + good[index + 2:])

    def test_every_stage_required_and_non_skippable(self):
        for lane in gate.LANES:
            for name in gate.STAGES:
                for mode in ("missing", "duplicate", "skip", "fail"):
                    with self.subTest(lane=lane, stage=name, mode=mode):
                        def change(r):
                            stage = next(s for s in r["stages"] if s["name"] == name)
                            if mode == "missing":
                                r["stages"].remove(stage)
                            elif mode == "duplicate":
                                r["stages"].append(copy.deepcopy(stage))
                            else:
                                stage["status"] = mode
                        bad = stream()
                        mutate_record(bad, lane, change)
                        self.reject(bad)

    def test_trace_identity_and_leg_mutations(self):
        changes = [
            lambda r: r.update(runId="stale-run"),
            lambda r: r.update(schemaVersion=2),
            lambda r: r.update(schemaVersion=True),
            lambda r: r.update(cluster=""),
            lambda r: r.update(lane="unknown"),
            lambda r: r.update(opportunityId=99999),
            lambda r: r.update(epochId=0),
            lambda r: r.update(decisionId=True),
            lambda r: r["legs"].pop(),
            lambda r: r["legs"].append(copy.deepcopy(r["legs"][0])),
            lambda r: r["legs"][0].update(signature="placeholder"),
            lambda r: r["legs"][0].update(reconciledSlot=999),
            lambda r: r["legs"][0].update(submissionId=0),
            lambda r: r["stages"].append({"name": "unknown"}),
        ]
        for lane in gate.LANES:
            for change in changes:
                bad = stream()
                mutate_record(bad, lane, change)
                self.reject(bad)
        for field in ("signature", "submissionId"):
            bad = stream()
            mutate_record(bad, "cross-mint", lambda r: r["legs"][1].update({field: r["legs"][0][field]}))
            self.reject(bad)
        bad = stream()
        mutate_record(bad, "same-mint", lambda r: r["legs"][0].update(signature=record("cross-mint")["legs"][0]["signature"]))
        self.reject(bad)
        with self.assertRaises(ValueError):
            gate.verify(stream(), run_id="")

    def test_recovery_covers_all_legs_and_known_submissions(self):
        for name in gate.STAGES:
            for refs in ([999999], [210, 210], [True]):
                bad = stream()
                mutate_record(bad, "cross-mint", lambda r: next(s for s in r["stages"] if s["name"] == name).update(submissionIds=refs))
                self.reject(bad)
            if name not in {"published", "revalidated"}:
                bad = stream()
                mutate_record(bad, "cross-mint", lambda r: next(s for s in r["stages"] if s["name"] == name).update(submissionIds=[]))
                self.reject(bad)
            if name in gate.ALL_LEG_STAGES:
                bad = stream()
                mutate_record(bad, "cross-mint", lambda r: next(s for s in r["stages"] if s["name"] == name).update(submissionIds=[210]))
                self.reject(bad)

    def test_malformed_and_incomplete_streams(self):
        self.reject([None])
        self.reject([event("pass"), *stream()[:-1]])
        for payload in ("{", "[]", "null", '{"lane":"same-mint",' + json.dumps(record("same-mint"))[1:],
                        json.dumps(record("same-mint")) + gate.MARKER):
            bad = stream()
            e = next(e for e in bad if gate.MARKER in e.get("Output", ""))
            e["Output"] = gate.MARKER + payload
            self.reject(bad)
        good = stream()
        self.reject([e for e in good if e.get("Action") != "run"])

    def test_cli_requires_run_id_and_preserves_verified_trace(self):
        with tempfile.TemporaryDirectory() as directory:
            artifact = Path(directory) / "go.jsonl"
            artifact.write_text("\n".join(json.dumps(e) for e in stream()))
            result = subprocess.run(["python3", str(SCRIPT), str(artifact), "--run-id", RUN], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.count("VERIFIED_CONNECTED_EVIDENCE "), 2)
            self.assertIn("both lanes and full required suite", result.stdout)
            for args in ([], ["--run-id", "stale"], ["--run-id", RUN, "--development-lane", "same-mint"]):
                result = subprocess.run(["python3", str(SCRIPT), str(artifact), *args], capture_output=True, text=True)
                self.assertEqual(result.returncode, 1)
                self.assertNotIn("VERIFIED_CONNECTED_EVIDENCE", result.stdout)
            artifact.write_text("not JSON\n")
            result = subprocess.run(["python3", str(SCRIPT), str(artifact), "--run-id", RUN], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)

    def test_unknown_fields_are_not_forwarded_to_audit_log(self):
        for change in (lambda r: r.update(unexpected="untrusted"),
                       lambda r: r["legs"][0].update(unexpected="untrusted"),
                       lambda r: r["stages"][0].update(unexpected="untrusted")):
            bad = stream()
            mutate_record(bad, "same-mint", change)
            self.reject(bad)

    def test_runner_rejects_cache_outside_development_before_work(self):
        runner = SCRIPT.with_name("verify-kamino-fleet-planner-e2e.sh")
        for args in (["--reuse-builds", "/unused"], ["--development-lane", "invalid"],
                     ["--development-lane"], ["--development-lane", ""],
                     ["--reuse-builds"], ["--reuse-builds", ""], ["--unknown"]):
            result = subprocess.run(["bash", str(runner), *args], capture_output=True, text=True)
            self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
            self.assertNotIn("Disposable PostgreSQL", result.stdout)
        result = subprocess.run(["bash", str(runner), "--help"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0)
        self.assertIn("Development", result.stdout)


if __name__ == "__main__":
    unittest.main()
