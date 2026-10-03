#!/usr/bin/env python3
"""Apply the real registered Yield migrations only to allowlisted test databases."""
import json
import os
from pathlib import Path
import re
import subprocess
from urllib.parse import urlparse, urlunparse

base = os.environ.get("WORKERS_V2_FIXTURE_URL", "")
parsed = urlparse(base)
if (os.environ.get("WORKERS_V2_DISPOSABLE") != "1"
        or parsed.scheme not in ("postgres", "postgresql")
        or parsed.hostname not in ("127.0.0.1", "localhost")
        or parsed.username != "workers_v2" or parsed.password
        or parsed.path != "/workers_v2_bootstrap" or parsed.query
        or parsed.fragment or not parsed.port or parsed.port < 1024):
    raise SystemExit("Refusing fixture setup outside the allowlisted disposable local service")

repo = Path(__file__).resolve().parent.parent
registry = repo / "crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs"
definition = registry.read_text().split("const MIGRATIONS:", 1)[1].split("\n];", 1)[0]
paths = re.findall(r'include_str!\(\s*"([^"]+)"\s*\)', definition)
migrations = [(registry.parent / p).resolve() for p in paths]
if not migrations or any(not p.is_file() or p.parent != repo / "crates/loyal-yield-store/migrations" for p in migrations):
    raise SystemExit("Actual Yield migration registry could not be resolved")

def execute(url, *, sql=None, file=None):
    args = ["psql", url, "-X", "-v", "ON_ERROR_STOP=1", "-q"]
    args += ["-c", sql] if sql is not None else ["-f", str(file)]
    result = subprocess.run(args, capture_output=True, text=True)
    if result.returncode:
        raise SystemExit("Fixture SQL failed: " + result.stderr[-3000:])

urls = {}
for family in ("fleet", "autodeposit", "observer", "backyard", "multiply"):
    name = "workers_v2_" + family
    execute(base, sql='CREATE DATABASE "' + name + '"')
    url = urlunparse(parsed._replace(path="/" + name))
    for migration in migrations:
        execute(url, file=migration)
    urls[family] = url
print(json.dumps({"gate": "fixture", "verdict": "PASS", "registry": str(registry.relative_to(repo)),
                  "migrations": len(migrations), "databases": list(urls)}))
out = os.environ.get("GITHUB_ENV")
if out:
    with open(out, "a") as target:
        for key, family in (("FLEET_TEST_DATABASE_URL", "fleet"),
                            ("AUTODEPOSIT_TEST_DATABASE_URL", "autodeposit"),
                            ("OBSERVER_TEST_DATABASE_URL", "observer"),
                            ("BACKYARD_RWA_TEST_DATABASE_URL", "backyard"),
                            ("MULTIPLY_TEST_DATABASE_URL", "multiply")):
            target.write(key + "=" + urls[family] + "\n")
