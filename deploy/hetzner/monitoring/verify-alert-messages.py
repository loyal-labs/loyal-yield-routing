#!/usr/bin/env python3
"""Validate operator message coverage and rendering. Requires PyYAML and amtool."""
import copy
import json
from html.parser import HTMLParser
from pathlib import Path
import re
import subprocess
import tempfile
import yaml

root = Path(__file__).resolve().parent
rules = [r for g in yaml.safe_load((root / "loyal.rules.yml").read_text())["groups"] for r in g["rules"]]
config = yaml.safe_load((root / "alertmanager.yml").read_text())
cases = json.loads((root / "telegram-message-cases.json").read_text())
assert len(cases) == len(rules), "Every rule variant needs a render case"
required = {"summary", "trigger", "impact", "action", "recovery"}
for rule, case in zip(rules, cases):
    assert case["labels"]["alertname"] == rule["alert"]
    assert required <= rule["annotations"].keys()
    assert all(rule["annotations"][key].strip() for key in required)
# Exercise every rendered failure-code contract from promtool fixtures too.
for group in yaml.safe_load((root / "loyal.rules.test.yml").read_text())["tests"]:
    for test in group.get("alert_rule_test", []):
        if test["alertname"] == "LoyalFamilyFailing":
            cases.extend({"labels": {"alertname": test["alertname"], **a["exp_labels"]}, "annotations": a["exp_annotations"]} for a in test.get("exp_alerts", []))
assert {"code", "lane", "instance", "payer"} <= set(config["route"]["group_by"])
message = next(r for r in config["receivers"] if r["name"] == "telegram")["telegram_configs"][0]["message"]
assert "GeneratorURL" not in message and "ExternalURL" not in message
alerts = []
for case in cases:
    for status in ("firing", "resolved"):
        alerts.append({"status": status, "labels": case["labels"], "annotations": case["annotations"], "generatorURL": "http://DO_NOT_RENDER.invalid/graph"})
hostile = copy.deepcopy(alerts[0])
hostile["labels"]["code"] = '<unsafe attr="x">&'
hostile["labels"]["arbitrary"] = "DO_NOT_RENDER_LABEL"
hostile["annotations"]["summary"] = '<unsafe attr="x">&'
alerts.append(hostile)
# Unknown future rules get explicit uncertainty, not a false recovery claim.
alerts.append({"status": "firing", "labels": {"alertname": "FutureRule", "family": "backyard", "severity": "page"}, "annotations": {"summary": "Future condition"}})
with tempfile.TemporaryDirectory(prefix="loyal-alert-render-") as work:
    work = Path(work)
    (work / "message.tmpl").write_text('{{ define "review.message" }}' + message + '{{ end }}')
    (work / "data.json").write_text(json.dumps({"status": "firing", "receiver": "telegram", "alerts": alerts, "groupLabels": {}, "commonLabels": {}, "commonAnnotations": {}, "externalURL": "http://DO_NOT_RENDER.invalid"}))
    result = subprocess.run(["amtool", "template", "render", "--template.glob=" + str(work / "message.tmpl"), '--template.text={{ template "review.message" . }}', "--template.data=" + str(work / "data.json")], capture_output=True, text=True, timeout=30)
    assert result.returncode == 0, result.stderr
text = result.stdout
messages = [m for m in re.split(r"(?=<b>(?:FIRING|RESOLVED) · )", text) if m.strip()]
assert len(messages) == len(alerts)
for alert, rendered in zip(alerts, messages):
    assert len(rendered) < 2000, "Keep room for firing/resolved transition pairs under Telegram's 4096 limit"
    if alert["status"] == "firing":
        assert "Signal:" in rendered and "Impact:" in rendered and "Next:" in rendered
    else:
        assert "Verify:" in rendered and "no longer firing" in rendered
assert "DO_NOT_RENDER" not in text
assert '<unsafe' not in text and "&lt;unsafe" in text
assert "Impact is unclassified" in messages[-1]
class SafeHTML(HTMLParser):
    def handle_starttag(self, tag, attrs):
        assert tag in ("b", "a"), tag
        if tag == "a":
            assert dict(attrs).get("href", "").startswith("https://github.com/loyal-labs/loyal-yield-routing/blob/")
SafeHTML().feed(text)
print(f"PASS: {len(rules)} rule variants, {len(alerts)} firing/resolved/hostile render cases; escaped and bounded messages")
