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
entries = re.findall(
    r'Migration\s*\{\s*version:\s*(\d+),\s*name:\s*"([^"]+)",\s*'
    r'sql:\s*include_str!\(\s*"([^"]+)"\s*\),\s*'
    r'expected_checksum:\s*(None|Some\("[0-9a-f]{64}"\)),\s*\}', definition)
if len(entries) != len(migrations):
    raise SystemExit("Actual Yield migration ledger definitions could not be resolved")
ledger = {}
for version, name, path, override in entries:
    file = (registry.parent / path).resolve()
    checksum = override[6:-2] if override.startswith('Some("') else hashlib.sha256(file.read_bytes()).hexdigest()
    ledger[int(version)] = (name, checksum)

# Production applied these outside the Yield registry: 0074-0083 through the
# deployed Backyard worker's store migrations (origin/feat/voltr-rwa-selector
# f821a78f6a) and 0085 as the separately rolled-out history index. The fixture
# applies them at their production position and records the same ledger rows
# (name, sha256 of the file) those runners wrote.
out_of_band_root = repo / "crates/loyal-yield-store/migrations"
out_of_band = {}
for version, name in ((74, "backyard_rwa_phase3_journal_actions"), (75, "backyard_rwa_setup_pre_simulation_wire"),
                      (76, "backyard_rwa_manual_recovery_latch"), (77, "backyard_rwa_manual_recovery_generation"),
                      (78, "backyard_rwa_incident_resolution"), (79, "backyard_rwa_initializer_actions"),
                      (80, "backyard_rwa_strategy_journal"), (81, "backyard_rwa_finalized_report_failure"),
                      (82, "backyard_rwa_initializer_auto_scope"), (83, "backyard_rwa_finalized_restore_failure"),
                      (85, "earn_vault_allocation_history_index")):
    if version in ledger:
        raise SystemExit("Out-of-band migration %d is now registered; drop it from the fixture list" % version)
    file = out_of_band_root / ("%04d_%s.sql" % (version, name))
    if not file.is_file():
        raise SystemExit("Deployed out-of-band migration %s is missing" % file.name)
    after = 73 if version < 84 else 84
    out_of_band.setdefault(after, []).append((version, name, file))

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
default_families = ("fleet", "fleetexec", "autodeposit", "observer", "earn_parity", "backyard", "multiply", "lookup", "ata_projector")
families = tuple(os.environ.get("WORKERS_V2_FIXTURE_FAMILIES", ",".join(default_families)).split(","))
allowed_families = set(default_families) | {"fleet_go_same_mint", "fleet_same_mint", "fleet_go_cross_mint", "fleet_cross_mint_capture", "lookup", "ata_projector", "autodeposit_intent", "fleet_go_same_mint_simplify", "fleet_go_cross_mint_simplify"}
if not families or len(set(families)) != len(families) or any(f not in allowed_families for f in families):
    raise SystemExit("Fixture families must be distinct allowlisted test scopes")
def apply_yield_schema(url):
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
            # Do not mark the production data activation as applied.
            continue
        else:
            execute(url, file=migration)
        # Retained runtime compatibility tools check this ledger at startup.
        # Record only completed fixture migrations with the runner's checksum.
        name, checksum = ledger[version]
        if not re.fullmatch(r"[a-z0-9_]+", name):
            raise SystemExit("Unsafe migration ledger name")
        execute(url, sql="INSERT INTO loyal_yield.schema_migrations(version,name,checksum) "
                f"VALUES({version},'{name}','{checksum}')")
        for extra_version, extra_name, extra_file in out_of_band.get(version, []):
            execute(url, file=extra_file)
            extra_checksum = hashlib.sha256(extra_file.read_bytes()).hexdigest()
            execute(url, sql="INSERT INTO loyal_yield.schema_migrations(version,name,checksum) "
                    f"VALUES({extra_version},'{extra_name}','{extra_checksum}')")

def apply_apps_autodeposit_schema(url):
    for entry in json.loads((schema / "apps-autodeposit-manifest.json").read_text()):
        file = schema / entry["file"]
        if file.parent != schema or hashlib.sha256(file.read_bytes()).hexdigest() != entry["sha256"]:
            raise SystemExit("Actual Apps Autodeposit fixture provenance drifted")
        sql = file.read_text()
        # App 0006 predates Yield 0059's lifecycle column rename. This empty
        # fixture applies its exact DDL; historical target-data backfill has no
        # rows to convert and is not claimed as migration acceptance.
        ddl, backfill = sql.split("INSERT INTO loyal_yield.balance_sweep_policies (", 1)
        _, tail = backfill.split("DO $$", 1)
        execute(url, sql=ddl + "DO $$" + tail)

for family in families:
    name = family if family in {"fleet", "fleet_go_same_mint", "fleet_same_mint", "fleet_go_cross_mint", "fleet_cross_mint_capture", "fleet_go_same_mint_simplify", "fleet_go_cross_mint_simplify"} else "workers_v2_" + family
    execute(base, sql='CREATE DATABASE "' + name + '"')
    url = urlunparse(parsed._replace(path="/" + name))
    apply_yield_schema(url)
    if family == "autodeposit_intent":
        apply_apps_autodeposit_schema(url)
    if family == "backyard":
        execute(url, file=schema / "backyard_route_lease.sql")
    urls[family] = url
timescale = os.environ.get("WORKERS_V2_TIMESCALE_FIXTURE_URL")
timescale_url = None
ata_capture_url = None
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
    if "ata_projector" in urls:
        execute(timescale, sql='CREATE DATABASE workers_v2_ata_capture')
        ata_capture_url = urlunparse(timescale_parsed._replace(path="/workers_v2_ata_capture"))
        stream_migration = repo / "crates/loyal-timescale-migrations/migrations/0004_split_balance_sweep_ata_streams.sql"
        if stream_migration not in files:
            raise SystemExit("Registered ATA stream migration missing")
        execute(ata_capture_url, file=stream_migration)
    print(json.dumps({"gate": "timescale_fixture", "verdict": "PASS", "schema_files": len(files)}))
# Observer discovery reads the Yield database alone, as the Rust monitor did
# in production; its watch fixture holds only the registered Yield schema.
watch_url = None
if "observer" in urls:
    execute(base, sql='CREATE DATABASE workers_v2_observer_watch')
    watch_url = urlunparse(parsed._replace(path="/workers_v2_observer_watch"))
    apply_yield_schema(watch_url)
# The C capacity handoff has its own registered database; connected SVM
# terminal-evidence databases are not reused or reset by these SQL tests.
if "fleet" in urls and "fleet_cross_mint_capture" not in urls:
    execute(base, sql='CREATE DATABASE fleet_cross_mint_capture')
    urls["fleet_cross_mint_capture"] = urlunparse(parsed._replace(path="/fleet_cross_mint_capture"))
    apply_yield_schema(urls["fleet_cross_mint_capture"])
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
