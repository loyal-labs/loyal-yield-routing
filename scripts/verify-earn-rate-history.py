#!/usr/bin/env python3
"""Opt-in real-Postgres contract tests, localhost only; no production URLs."""
import pathlib
import subprocess
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
URL = "postgresql://postgres@127.0.0.1:55432/earn_history_contract"
MIGRATION = ROOT / "crates/loyal-timescale-migrations/migrations/0009_kamino_earn_rate_history.sql"


def run(query, check=True):
    result = subprocess.run(["psql", URL, "-X", "-qAt", "-v", "ON_ERROR_STOP=1"],
                            input=query, text=True, capture_output=True, timeout=30)
    if check and result.returncode:
        raise RuntimeError(result.stderr)
    return result


def value(query):
    return run(query).stdout.strip()


# An existing database is not discarded: recreate this dedicated fixture manually.
subprocess.run(["createdb", "-h", "127.0.0.1", "-p", "55432", "-U", "postgres", "earn_history_contract"], check=True)
run("""
CREATE EXTENSION IF NOT EXISTS timescaledb; CREATE SCHEMA kamino;
CREATE TABLE kamino.reserve_updates (
  observed_at timestamptz NOT NULL, event_id bigint NOT NULL, reserve text NOT NULL,
  supply_apy double precision NOT NULL, reserve_last_update_stale boolean NOT NULL
);
SELECT create_hypertable('kamino.reserve_updates','observed_at',chunk_time_interval=>interval '1 day');
INSERT INTO kamino.reserve_updates VALUES ('2026-01-01',1,'A',.03,false), ('2026-01-01 01:30Z',4,'A',.05,false);
""")
run(MIGRATION.read_text())
# Reapplication after a commit-before-ledger-write interruption preserves data.
run(MIGRATION.read_text())
assert value("SELECT count(*) FROM kamino.reserve_earn_rates") == "0"
run("SELECT kamino.backfill_reserve_earn_rates_batch('2026-01-01','2026-01-01 02:00Z')")
assert value("SELECT count(*) FROM kamino.reserve_earn_rates") == "1"
# A rollback must roll back both rows and progress, not only one of them.
before = value("SELECT completed_until FROM kamino.reserve_earn_rate_backfills")
run("BEGIN; SELECT kamino.backfill_reserve_earn_rates_batch('2026-01-01','2026-01-01 02:00Z'); ROLLBACK;")
assert value("SELECT completed_until FROM kamino.reserve_earn_rate_backfills") == before
assert value("SELECT count(*) FROM kamino.reserve_earn_rates WHERE event_id=4") == "0"
run("SELECT kamino.backfill_reserve_earn_rates_batch('2026-01-01','2026-01-01 02:00Z')")
run("SELECT kamino.backfill_reserve_earn_rates_batch('2026-01-01','2026-01-01 02:00Z')")
assert value("SELECT count(*) FROM kamino.reserve_earn_rates") == "2"
assert value("SELECT bool_and(completed_until=end_at) FROM kamino.reserve_earn_rate_backfills") == "t"
assert run("SELECT kamino.backfill_reserve_earn_rates_batch('infinity','infinity')", check=False).returncode != 0
run("""
BEGIN;
INSERT INTO kamino.reserve_updates VALUES ('2026-01-02',2,'B',.07,false);
ROLLBACK;
""")
assert value("SELECT count(*) FROM kamino.reserve_earn_rates WHERE event_id=2") == "0"
# COPY and a previously nonexistent chunk take the same synchronous trigger path.
run("COPY kamino.reserve_updates FROM STDIN;\n2026-01-02\t2\tB\t0.07\tf\n\\.\n")
assert value("SELECT supply_apy FROM kamino.reserve_earn_rates WHERE event_id=2") == "0.07"
run("UPDATE kamino.reserve_updates SET supply_apy=-1,reserve_last_update_stale=true WHERE event_id=2")
assert value("SELECT count(*) FROM kamino.reserve_earn_rates WHERE event_id=2 AND NOT reserve_last_update_stale AND supply_apy>=0 AND supply_apy<0.5") == "0"
run("UPDATE kamino.reserve_updates SET event_id=3,supply_apy=.08,reserve_last_update_stale=false WHERE event_id=2")
assert value("SELECT count(*) FROM kamino.reserve_earn_rates WHERE event_id=2") == "0"
assert value("SELECT supply_apy FROM kamino.reserve_earn_rates WHERE event_id=3") == "0.08"
run("DELETE FROM kamino.reserve_updates WHERE event_id=3")
assert value("SELECT count(*) FROM kamino.reserve_earn_rates WHERE event_id=3") == "0"
# A projection-write failure must reject/roll back the raw write too.
run("ALTER TABLE kamino.reserve_earn_rates ADD CONSTRAINT reject_probe CHECK(event_id<>999)")
assert run("INSERT INTO kamino.reserve_updates VALUES('2026-01-02',999,'X',.03,false)", check=False).returncode != 0
assert value("SELECT count(*) FROM kamino.reserve_updates WHERE event_id=999") == "0"
run("ALTER TABLE kamino.reserve_earn_rates DROP CONSTRAINT reject_probe")

run("DELETE FROM kamino.reserve_updates WHERE event_id=4")
# Backfill holds source row locks until commit. Concurrent correction/deletion
# must wait and then win over the copy, rather than being silently resurrected.
for mutation in ("UPDATE kamino.reserve_updates SET supply_apy=.12 WHERE event_id=1", "DELETE FROM kamino.reserve_updates WHERE event_id=1"):
    run("TRUNCATE kamino.reserve_earn_rates; DELETE FROM kamino.reserve_earn_rate_backfills")
    holder = subprocess.Popen(["psql", URL, "-X", "-qAt", "-v", "ON_ERROR_STOP=1"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    writer = None
    try:
        holder.stdin.write("SET application_name='earn-backfill-lock-test'; BEGIN; SELECT kamino.backfill_reserve_earn_rates_batch('2026-01-01','2026-01-01 00:05Z');\n")
        holder.stdin.flush()
        deadline = time.monotonic() + 10
        while value("SELECT count(*) FROM pg_stat_activity WHERE application_name='earn-backfill-lock-test' AND state='idle in transaction'") != "1":
            assert time.monotonic() < deadline, "Backfill did not acquire locks"
            time.sleep(.05)
        writer = subprocess.Popen(["psql", URL, "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-c", "SET application_name='earn-concurrent-test'; SET statement_timeout='15s'; " + mutation], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        deadline = time.monotonic() + 10
        while value("SELECT count(*) FROM pg_stat_activity WHERE application_name='earn-concurrent-test' AND wait_event_type='Lock'") != "1":
            assert writer.poll() is None and time.monotonic() < deadline, "Concurrent mutation was not blocked"
            time.sleep(.05)
        holder.stdin.write("COMMIT;\n"); holder.stdin.close()
        assert holder.wait(timeout=10) == 0
        assert writer.wait(timeout=10) == 0
        assert value("SELECT NOT EXISTS (SELECT observed_at,event_id,reserve,supply_apy,reserve_last_update_stale FROM kamino.reserve_updates EXCEPT SELECT * FROM kamino.reserve_earn_rates) AND NOT EXISTS (SELECT * FROM kamino.reserve_earn_rates EXCEPT SELECT observed_at,event_id,reserve,supply_apy,reserve_last_update_stale FROM kamino.reserve_updates)") == "t"
    finally:
        if holder.poll() is None: holder.kill(); holder.wait()
        if writer is not None and writer.poll() is None: writer.kill(); writer.wait()
print("PASS: backfill resume/rollback, live COPY, correction/delete, atomic write failure, concurrent mutation")
