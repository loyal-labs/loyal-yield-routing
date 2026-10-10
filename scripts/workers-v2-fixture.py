#!/usr/bin/env python3
"""Apply the repository Yield and Timescale migrations only to allowlisted test databases.

The Yield schema is migrated once, into a template database, by one psql
session; every family database is then a copy of that template. Each file
keeps its own transaction (psql --single-transaction semantics) unless it
uses CONCURRENTLY, exactly as the per-file runs did.
"""
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
# The same files, versions, names and sha256 ledger checksums that
# go/workers/cmd/loyal-migrate applies to production, in version order.
registry = repo / "migrations/yield"
migrations = sorted(registry.glob("*.sql"))
if not migrations:
    raise SystemExit("Yield migration directory could not be resolved")
ledger = {}
for file in migrations:
    match = re.fullmatch(r"(\d{4})_([a-z0-9_]+)\.sql", file.name)
    if not match or int(match.group(1)) in ledger:
        raise SystemExit("Unexpected Yield migration file " + file.name)
    ledger[int(match.group(1))] = (match.group(2), hashlib.sha256(file.read_bytes()).hexdigest())

def psql_quote(path):
    return "'" + str(path).replace("\\", "\\\\").replace("'", "\\'") + "'"

class Script:
    """One psql session's input, run in order."""
    def __init__(self):
        self.lines = []
    def file(self, path, sql=None):
        # A file runs as one transaction (psql --single-transaction) unless
        # it uses CONCURRENTLY; sql replaces the file's text when given.
        transactional = not re.search(r"\bCONCURRENTLY\b", path.read_text() if sql is None else sql)
        if transactional:
            self.lines.append("BEGIN;")
        self.lines.append("\\i " + psql_quote(path) if sql is None else sql)
        if transactional:
            self.lines.append("COMMIT;")
    def sql(self, statement):
        self.lines.append(statement + ";")
    def run(self, url):
        result = subprocess.run(["psql", url, "-X", "-q", "-v", "ON_ERROR_STOP=1"],
                                input="\n".join(self.lines) + "\n", capture_output=True, text=True)
        if result.returncode:
            raise SystemExit("Fixture SQL failed: " + result.stderr[-3000:])

def execute(url, sql):
    script = Script()
    script.sql(sql)
    script.run(url)

urls = {}
schema = repo / "go/workers/testdata/schema"
app_schema = json.loads((schema / "manifest.json").read_text())
for entry in app_schema:
    file = schema / entry["file"]
    if file.parent != schema or hashlib.sha256(file.read_bytes()).hexdigest() != entry["sha256"]:
        raise SystemExit("Historical app schema fixture provenance drifted")
default_families = ("fleet", "fleetexec", "autodeposit", "observer", "earn_parity", "backyard", "multiply", "lookup", "ata_projector")
families = tuple(os.environ.get("WORKERS_V2_FIXTURE_FAMILIES", ",".join(default_families)).split(","))
allowed_families = set(default_families) | {"fleet_go_same_mint", "fleet_same_mint", "fleet_go_cross_mint", "fleet_cross_mint_capture", "lookup", "ata_projector", "fleet_go_same_mint_simplify", "fleet_go_cross_mint_simplify"}
if not families or len(set(families)) != len(families) or any(f not in allowed_families for f in families):
    raise SystemExit("Fixture families must be distinct allowlisted test scopes")

def yield_schema():
    script = Script()
    for migration in migrations:
        version = int(migration.name.split("_", 1)[0])
        if version == 13:
            for entry in app_schema:
                script.file(schema / entry["file"])
            # Migration 13 guards optional app relations with eager
            # ::regclass casts that fail on a blank database; production
            # applied it over the app baseline above.
            sql = migration.read_text()
            for relation in ("user_yield_positions", "user_yield_position_holding_events", "earn_deposit_onboarding_attempts"):
                cast = "'loyal_yield." + relation + "'::regclass"
                if sql.count(cast) != 1:
                    raise SystemExit("Migration 13 optional-relation execution contract drifted")
                sql = sql.replace(cast, "to_regclass('loyal_yield." + relation + "')")
            script.file(migration, sql)
        elif version == 71:
            # Only its schema changes apply to an empty fixture. The following
            # DO block converts a hashed production singleton; no activation is
            # claimed or attempted here.
            ddl, activation = migration.read_text().split("DO $$", 1)
            if "Backyard Phase 1 canonical route cardinality drifted" not in activation:
                raise SystemExit("Production-bound activation fixture contract drifted")
            script.file(migration, ddl)
            # Do not mark the production data activation as applied.
            continue
        else:
            script.file(migration)
        # Retained runtime compatibility tools check this ledger at startup.
        # Record only completed fixture migrations with the runner's checksum.
        name, checksum = ledger[version]
        if not re.fullmatch(r"[a-z0-9_]+", name):
            raise SystemExit("Unsafe migration ledger name")
        script.sql("INSERT INTO loyal_yield.schema_migrations(version,name,checksum) "
                   f"VALUES({version},'{name}','{checksum}')")
    return script

def db_url(source, name):
    return urlunparse(source._replace(path="/" + name))

# Every Yield database is a copy of this one migrated template.
template = "workers_v2_template"
execute(base, 'CREATE DATABASE "' + template + '"')
yield_schema().run(db_url(parsed, template))
def create_yield_database(name):
    execute(base, 'CREATE DATABASE "' + name + '" TEMPLATE "' + template + '"')
    return db_url(parsed, name)

for family in families:
    name = family if family in {"fleet", "fleet_go_same_mint", "fleet_same_mint", "fleet_go_cross_mint", "fleet_cross_mint_capture", "fleet_go_same_mint_simplify", "fleet_go_cross_mint_simplify"} else "workers_v2_" + family
    url = create_yield_database(name)
    if family == "backyard":
        script = Script()
        script.file(schema / "backyard_route_lease.sql")
        script.run(url)
    urls[family] = url
timescale = os.environ.get("WORKERS_V2_TIMESCALE_FIXTURE_URL")
timescale_url = None
ata_capture_url = None
if timescale:
    timescale_parsed = allowlisted(timescale)
    files = sorted((repo / "migrations/timescale").glob("*.sql"))
    if not files:
        raise SystemExit("Timescale migration directory could not be resolved")
    execute(timescale, 'CREATE DATABASE workers_v2_timescale')
    timescale_url = db_url(timescale_parsed, "workers_v2_timescale")
    script = Script()
    for file in files:
        script.file(file)
    script.run(timescale_url)
    if "ata_projector" in urls:
        # The ATA capture database holds only the split-stream migration.
        execute(timescale, 'CREATE DATABASE workers_v2_ata_capture')
        ata_capture_url = db_url(timescale_parsed, "workers_v2_ata_capture")
        stream_migration = repo / "migrations/timescale/0004_split_balance_sweep_ata_streams.sql"
        if stream_migration not in files:
            raise SystemExit("Registered ATA stream migration missing")
        script = Script()
        script.file(stream_migration)
        script.run(ata_capture_url)
    print(json.dumps({"gate": "timescale_fixture", "verdict": "PASS", "schema_files": len(files)}))
# Observer discovery reads the Yield database alone, as the Rust monitor did
# in production; its watch fixture holds only the registered Yield schema.
watch_url = None
if "observer" in urls:
    watch_url = create_yield_database("workers_v2_observer_watch")
# The C capacity handoff has its own registered database; connected SVM
# terminal-evidence databases are not reused or reset by these SQL tests.
if "fleet" in urls and "fleet_cross_mint_capture" not in urls:
    urls["fleet_cross_mint_capture"] = create_yield_database("fleet_cross_mint_capture")
print(json.dumps({"gate": "fixture", "verdict": "PASS", "registry": str(registry.relative_to(repo)),
                  "registered_schema_files": len(migrations), "databases": list(urls),
                  "watch_yield_database": bool(watch_url),
                  "scope": "isolated behavior schema; historical app baseline plus registered Yield schema, excludes production-bound 0071 data activation"}))
out = os.environ.get("GITHUB_ENV")
if out:
    with open(out, "a") as target:
        if watch_url:
            target.write("TEST_WATCH_DATABASE_URL=" + watch_url + "\n")
            target.write("READMODELS_TEST_DATABASE_URL=" + urls["observer"] + "\n")
        for key, family in (("FLEET_TEST_DATABASE_URL", "fleet"),
                            ("FLEET_EXEC_TEST_DATABASE_URL", "fleetexec"),
                            ("FLEET_TEST_CROSS_MINT_CAPTURE_DATABASE_URL", "fleet_cross_mint_capture"),
                            ("LOOKUP_TEST_DATABASE_URL", "lookup"),
                            ("ATA_PROJECTOR_TEST_DATABASE_URL", "ata_projector"),
                            ("AUTODEPOSIT_TEST_DATABASE_URL", "autodeposit"),
                            ("OBSERVER_TEST_DATABASE_URL", "observer"),
                            ("EARN_PARITY_TEST_DATABASE_URL", "earn_parity"),
                            ("TEST_DATABASE_URL", "observer"),
                            ("BACKYARD_RWA_TEST_DATABASE_URL", "backyard"),
                            ("MULTIPLY_TEST_DATABASE_URL", "multiply")):
            if family in urls:
                target.write(key + "=" + urls[family] + "\n")
        if ata_capture_url:
            target.write("ATA_CAPTURE_TEST_DATABASE_URL=" + ata_capture_url + "\n")
        if timescale_url:
            target.write("TEST_TIMESCALE_DATABASE_URL=" + timescale_url + "\n")
