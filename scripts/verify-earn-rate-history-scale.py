#!/usr/bin/env python3
"""Local-only production-migration/backfill/performance E2E on 8.12m records."""
import json
import pathlib
import subprocess
import sys
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
URL = "postgresql://postgres@127.0.0.1:55432/postgres"


def sql(query):
    result = subprocess.run(["psql", URL, "-X", "-qAt", "-v", "ON_ERROR_STOP=1"],
                            input=query, text=True, capture_output=True, timeout=180)
    if result.returncode:
        raise RuntimeError(result.stderr)
    return result.stdout.strip()


def nodes(plan):
    yield plan
    for child in plan.get("Plans", []):
        yield from nodes(child)


assert sql("SELECT to_regprocedure('public.seed_earn_history(integer)') IS NOT NULL") == "t"
assert sql("SELECT count(*) FROM kamino.reserve_updates") == "8120000"
existing = "--existing" in sys.argv
if not existing:
    assert sql("SELECT to_regclass('kamino.reserve_earn_rates') IS NULL") == "t", "Start with no projection"
query = (ROOT / "scripts/fixtures/earn-history-read.sql").read_text()
expected = (20 * 100 + 20 * 150 + 17 * 125) * ((.03 * 43199.9925 + .09 * (86400 - 43199.9925)) / 86400) / 365


def measure(label, statement):
    for run in range(3):
        # The new source must work even if NO index-only scans are available.
        prefix = "BEGIN READ ONLY; SET LOCAL jit=off; SET LOCAL enable_indexonlyscan=off; SET LOCAL statement_timeout='120s'; "
        began = time.monotonic()
        earned, days = sql(prefix + statement + " ROLLBACK;").split("|")
        elapsed = (time.monotonic() - began) * 1000
        assert int(days) == 57 and abs(float(earned) - expected) < 1e-8
        plan = json.loads(sql(prefix + "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) " + statement + " ROLLBACK;"))[0]
        scans = [n for n in nodes(plan["Plan"]) if n.get("Actual Loops", 0) > 0]
        assert not any(n["Node Type"] == "Index Only Scan" for n in scans)
        print(json.dumps({"phase": label, "run": run, "queryMs": round(elapsed), "executionMs": plan["Execution Time"], "readBlocks": plan["Plan"]["Shared Read Blocks"], "accuracy": "PASS", "indexOnlyScans": 0}), flush=True)
        if label == "compact":
            assert elapsed < 6500, "Rate query budget exceeded"


def ingest(label):
    for run in range(3):
        # Rollback both copies; future chunk creation is included in both timings.
        began = time.monotonic()
        sql("""BEGIN; SET LOCAL statement_timeout='30s';
        INSERT INTO kamino.reserve_updates(event_id,reserve,observed_at,supply_apy,reserve_last_update_stale,padding)
        SELECT 9000000+n,repeat('X',44),'2026-09-06'::timestamptz+n*interval '1 second',.04,false,repeat('x',1400)
        FROM generate_series(1,10000) n; ROLLBACK;""")
        print(json.dumps({"phase": label, "run": run, "insert10000AndRollbackMs": round((time.monotonic()-began)*1000)}), flush=True)


started = time.monotonic()
if not existing:
    measure("wide", query)
    ingest("before-trigger")
    subprocess.run(["psql", URL, "-X", "-q", "-v", "ON_ERROR_STOP=1", "-f", str(ROOT / "crates/loyal-timescale-migrations/migrations/0009_kamino_earn_rate_history.sql")], check=True, timeout=60)
    ingest("with-trigger")
    started = time.monotonic()
    result = subprocess.run(["psql", URL, "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-v", "history_start=2026-07-08T00:00:00Z", "-v", "history_end=2026-09-04T00:00:00Z", "-f", str(ROOT / "scripts/backfill-earn-rate-history.sql")], text=True, capture_output=True, timeout=900)
    if result.returncode:
        raise RuntimeError(result.stderr)
    assert result.stdout.strip().endswith("|t"), "Backfill did not finish"
assert sql("SELECT completed_until=end_at FROM kamino.reserve_earn_rate_backfills WHERE start_at='2026-07-08' AND end_at='2026-09-04'") == "t"
assert sql("SELECT count(*) FROM kamino.reserve_earn_rates") == "8120000"
# Compare every source observation to its projected payload, in bounded chunks.
for day in range(58):
    assert sql(f"""SELECT count(*) FROM kamino.reserve_updates r
      LEFT JOIN kamino.reserve_earn_rates n ON n.observed_at=r.observed_at AND n.event_id=r.event_id
        AND n.observed_at >= '2026-07-08'::timestamptz + {day}*interval '1 day'
        AND n.observed_at < '2026-07-08'::timestamptz + {day+1}*interval '1 day'
      WHERE r.observed_at >= '2026-07-08'::timestamptz + {day}*interval '1 day'
        AND r.observed_at < '2026-07-08'::timestamptz + {day+1}*interval '1 day'
        AND (n.event_id IS NULL OR (n.reserve,n.supply_apy,n.reserve_last_update_stale)
          IS DISTINCT FROM (r.reserve,r.supply_apy,r.reserve_last_update_stale))""") == "0"
print(json.dumps({"backfillAndParitySeconds": round(time.monotonic()-started), "rows": 8120000}), flush=True)
sql("ANALYZE kamino.reserve_earn_rates")
measure("compact", query.replace("kamino.reserve_updates", "kamino.reserve_earn_rates"))
print("PASS: " + ("existing projection parity, analytical earnings, no-index-only budget" if existing else "migration, full backfill/parity, live-write overhead, analytical earnings, no-index-only budget"))
