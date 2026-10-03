#!/usr/bin/env python3
"""Apply the real registered Yield migrations only to allowlisted test databases."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
from urllib.parse import urlparse, urlunparse

base = os.environ.get("WORKERS_V2_FIXTURE_URL", "")
def allowlisted(url):
    parsed = urlparse(url)
    if (os.environ.get("WORKERS_V2_DISPOSABLE") != "1"
        or parsed.scheme not in ("postgres", "postgresql")
        or parsed.hostname not in ("127.0.0.1", "localhost")
        or parsed.username != "workers_v2" or parsed.password
        or parsed.path != "/workers_v2_bootstrap" or parsed.query
        or parsed.fragment or not parsed.port or parsed.port < 1024):
        raise SystemExit("Refusing fixture setup outside the allowlisted disposable local service")
    return parsed

parsed = allowlisted(base)

repo = Path(__file__).resolve().parent.parent
registry = repo / "crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs"
definition = registry.read_text().split("const MIGRATIONS:", 1)[1].split("\n];", 1)[0]
paths = re.findall(r'include_str!\(\s*"([^"]+)"\s*\)', definition)
migrations = [(registry.parent / p).resolve() for p in paths]
if not migrations or any(not p.is_file() or p.parent != repo / "crates/loyal-yield-store/migrations" for p in migrations):
    raise SystemExit("Actual Yield migration registry could not be resolved")

def execute(url, *, sql=None, file=None):
    args = ["psql", url, "-X", "-v", "ON_ERROR_STOP=1", "-q"]
    if file is not None and not re.search(r"\bCONCURRENTLY\b", file.read_text()):
        args.append("--single-transaction")
    args += ["-c", sql] if sql is not None else ["-f", str(file)]
    result = subprocess.run(args, capture_output=True, text=True)
    if result.returncode:
        raise SystemExit("Fixture SQL failed: " + result.stderr[-3000:])

urls = {}
schema = repo / "go/workers/testdata/schema"
app_schema = json.loads((schema / "manifest.json").read_text())
for entry in app_schema:
    file = schema / entry["file"]
    if file.parent != schema or hashlib.sha256(file.read_bytes()).hexdigest() != entry["sha256"]:
        raise SystemExit("Historical app schema fixture provenance drifted")
for family in ("fleet", "fleetexec", "autodeposit", "observer", "backyard", "multiply"):
    name = "fleet" if family == "fleet" else "workers_v2_" + family
    execute(base, sql='CREATE DATABASE "' + name + '"')
    url = urlunparse(parsed._replace(path="/" + name))
    for migration in migrations:
        version = int(migration.name.split("_", 1)[0])
        if version == 13:
            for entry in app_schema:
                execute(url, file=schema / entry["file"])
            # Match migration_execution_sql in the authoritative Rust runner.
            sql = migration.read_text()
            for relation in ("user_yield_positions", "user_yield_position_holding_events", "earn_deposit_onboarding_attempts"):
                cast = "'loyal_yield." + relation + "'::regclass"
                if sql.count(cast) != 1:
                    raise SystemExit("Migration 13 optional-relation execution contract drifted")
                sql = sql.replace(cast, "to_regclass('loyal_yield." + relation + "')")
            execute(url, sql=sql)
        elif version == 71:
            # Only its schema changes apply to an empty fixture. The following
            # DO block converts a hashed production singleton; no activation is
            # claimed or attempted here.
            ddl, activation = migration.read_text().split("DO $$", 1)
            if "Backyard Phase 1 canonical route cardinality drifted" not in activation:
                raise SystemExit("Production-bound activation fixture contract drifted")
            execute(url, sql=ddl)
        else:
            execute(url, file=migration)
    if family == "backyard":
        execute(url, file=schema / "backyard_route_lease.sql")
    urls[family] = url
print(json.dumps({"gate": "fixture", "verdict": "PASS", "registry": str(registry.relative_to(repo)),
                  "registered_schema_files": len(migrations), "databases": list(urls),
                  "scope": "isolated behavior schema; historical app baseline plus registered Yield schema, excludes production-bound 0071 data activation"}))
timescale = os.environ.get("WORKERS_V2_TIMESCALE_FIXTURE_URL")
timescale_url = None
if timescale:
    timescale_parsed = allowlisted(timescale)
    timescale_registry = repo / "crates/loyal-timescale-migrations/src/main.rs"
    sql = timescale_registry.read_text().split("const MIGRATIONS:", 1)[1].split("\n];", 1)[0]
    files = [(timescale_registry.parent / p).resolve()
             for p in re.findall(r'include_str!\(\s*"([^"]+)"\s*\)', sql)]
    if not files or any(not f.is_file() or f.parent != repo / "crates/loyal-timescale-migrations/migrations" for f in files):
        raise SystemExit("Actual Timescale migration registry could not be resolved")
    execute(timescale, sql='CREATE DATABASE workers_v2_timescale')
    timescale_url = urlunparse(timescale_parsed._replace(path="/workers_v2_timescale"))
    for file in files:
        execute(timescale_url, file=file)
    print(json.dumps({"gate": "timescale_fixture", "verdict": "PASS", "schema_files": len(files)}))
# The candidate watch loader has a separately classified sampled Apps/schema
# compatibility fixture. Its DROP/CREATE test never touches the registered DB.
execute(base, sql='CREATE DATABASE workers_v2_observer_watch')
watch_url = urlunparse(parsed._replace(path="/workers_v2_observer_watch"))
out = os.environ.get("GITHUB_ENV")
if out:
    with open(out, "a") as target:
        target.write("TEST_WATCH_DATABASE_URL=" + watch_url + "\n")
        for key, family in (("FLEET_TEST_DATABASE_URL", "fleet"),
                            ("FLEET_EXEC_TEST_DATABASE_URL", "fleetexec"),
                            ("AUTODEPOSIT_TEST_DATABASE_URL", "autodeposit"),
                            ("OBSERVER_TEST_DATABASE_URL", "observer"),
                            ("TEST_DATABASE_URL", "observer"),
                            ("BACKYARD_RWA_TEST_DATABASE_URL", "backyard"),
                            ("MULTIPLY_TEST_DATABASE_URL", "multiply")):
            target.write(key + "=" + urls[family] + "\n")
        if timescale_url:
            target.write("TEST_TIMESCALE_DATABASE_URL=" + timescale_url + "\n")
