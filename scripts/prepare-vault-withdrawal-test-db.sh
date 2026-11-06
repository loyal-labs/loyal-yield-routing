#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Run once per fresh disposable database. Reruns refuse any existing database;
# its owner must explicitly remove it before another bootstrap. Never reset data.
python3 - <<'PY'
import os
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import parse_qs, unquote, urlsplit


def refuse(message):
    sys.exit("withdrawal test database: " + message)


def config(variable, admin=False):
    try:
        value = os.environ[variable]
        uri = urlsplit(value)
        database = unquote(uri.path.removeprefix("/"))
        if (uri.scheme not in ("postgres", "postgresql")
                or uri.hostname != "127.0.0.1" or not uri.username
                or uri.fragment or parse_qs(uri.query, keep_blank_values=True) != {"sslmode": ["disable"]}
                or not 0 < (uri.port or 5432) <= 65535):
            raise ValueError()
        if admin and database != "postgres":
            raise ValueError()
        if not admin and not re.fullmatch(r"vault_cost_test_[a-z0-9_]{1,47}", database):
            raise ValueError()
        return (uri.hostname, str(uri.port or 5432), unquote(uri.username),
                unquote(uri.password or ""), database)
    except (KeyError, ValueError):
        refuse(variable + " must explicitly name a local disposable database with sslmode=disable")


admin = config("VAULT_CONSENT_TEST_ADMIN_DSN", admin=True)
target = config("BACKYARD_WITHDRAWAL_TEST_DSN")
if target != config("EARNMAX_WITHDRAWAL_TEST_DSN") or target[:4] != admin[:4]:
    refuse("both withdrawal DSNs must name the same database on the admin endpoint")

files = [Path(name) for name in (
    "go/workers/internal/backyard/testdata/withdrawal-cost-parent.sql",
    "migrations/yield/0113_backyard_failure_settles_by_receipt.sql",
    "migrations/yield/0139_earn_max_withdrawal_consent.sql",
    "migrations/yield/0140_earn_max_withdrawal_costs.sql",
    "migrations/yield/0141_backyard_withdrawal_cost_participants.sql",
    "migrations/yield/0142_vault_release_controls.sql",
)]
sql = b"\n".join(path.read_bytes() for path in files)


def psql(connection, statement):
    # Do not pass connection URIs/passwords as arguments or print server errors.
    env = {key: value for key, value in os.environ.items() if not key.startswith("PG")}
    env.update(zip(("PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE"), connection))
    env.update(PGSSLMODE="disable", PGCONNECT_TIMEOUT="5")
    result = subprocess.run(["psql", "-X", "-w", "-At", "-v", "ON_ERROR_STOP=1"],
                            input=statement, text=True, capture_output=True, env=env)
    if result.returncode:
        refuse("PostgreSQL command failed; no connection details were logged")
    return result.stdout.strip()


database = target[4]  # Validated SQL identifier and literal above.
existing = psql(admin, f"SELECT 1 FROM pg_database WHERE datname='{database}';")
if existing:
    refuse("database already exists; refusing to alter it (owner must explicitly remove it to rerun)")
psql(admin, f'CREATE DATABASE "{database}";')
psql(target, "BEGIN;\n" + sql.decode() + "\nCOMMIT;")

validated = psql(target, "SELECT convalidated AND "
                 "to_regclass('loyal_yield.earn_max_withdrawal_consents') IS NOT NULL AND "
                 "to_regclass('loyal_yield.backyard_withdrawal_cost_operations') IS NOT NULL AND "
                 "to_regclass('loyal_yield.backyard_withdrawal_cost_reservations') IS NOT NULL AND "
                 "to_regclass('loyal_yield.earn_max_withdrawal_claim_attempts') IS NOT NULL AND "
                 "to_regclass('loyal_yield.vault_release_controls') IS NOT NULL "
                 "FROM pg_constraint WHERE conrelid='loyal_yield.multiply_operations'::regclass "
                 "AND conname='multiply_operations_backyard_lifecycle';")
if validated != "t":
    refuse("production lifecycle constraint or withdrawal tables missing")
print("Withdrawal fixture ready: actual 0113 validated; actual 0139-0142 applied")
PY
