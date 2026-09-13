# Compact Kamino rate history

## Scope

Migration 0009 adds `kamino.reserve_earn_rates`, a small synchronous projection
of `reserve_updates`: observation time, stable event ID, reserve, rate and stale
flag. It preserves observations, not daily samples or precomputed earnings.
The separate `reserve_apy_backfill` source is unchanged.

**This is database infrastructure, not the completed personal-earnings fix.**
No app reader is switched, no cached estimate is invalidated and no public
benchmark meaning changes in this PR. The app follow-up must account for
cash flows/reserve switches, verify history coverage before using this source,
replace old cached estimates and keep figures labelled as estimates. Actual
on-chain interest and the reported user's identity remain separate verification.

The existing wide-history index can find observation locations but must often
read large monitoring rows for rates. The new table keeps those heap reads
small even when index-only scans are unavailable.

## Consistency and failure behavior

- An AFTER trigger mirrors INSERT (including COPY), UPDATE and DELETE in the
  same database transaction. A failed projection write rejects the raw write
  too; existing ingest retry handling must surface this failure, not swallow it.
- The key is `(observed_at, event_id)`, using the source's sequence-assigned event
  identity. Invalid/stale rates are retained; eligibility remains a read concern.
- The trigger runs as its trusted migration owner with a fixed search path and
  fully qualified relations; no new write permissions are required for ingestion
  roles. Direct PUBLIC execution is revoked.
- Existing history is not copied during migration/startup. The operator backfill
  copies one hour per transaction, ordered by reserve to improve locality, and
  commits its checkpoint atomically. Exact-window retries are idempotent.
- Backfill source row locks prevent concurrent corrections/deletes being lost
  or resurrected. New inserts do not wait on those historical row locks and are
  captured by the trigger, including observations with late timestamps.
- TRUNCATE and Timescale `drop_chunks` do not fire row DELETE triggers. Retention
  is deliberately not configured here. Any destructive maintenance must treat
  both histories and the coverage ledger together; never claim coverage after
  independently truncating/dropping projected history. Raw and projected rows
  must not be independently edited outside these maintenance procedures.

## Local verification

All commands below use a disposable local container; never redirect the
verifiers to production. They refuse remote URL overrides. Allow about 80 GiB
free container storage and 8 GiB RAM.

```sh
podman run -d --name earn-history-local \
  -p 127.0.0.1:55432:5432 -e POSTGRES_HOST_AUTH_METHOD=trust \
  docker.io/timescale/timescaledb:2.24.0-pg17 \
  -c shared_buffers=256MB -c jit=off
python3 scripts/verify-earn-rate-history.py
psql postgresql://postgres@127.0.0.1:55432/postgres -X -v ON_ERROR_STOP=1 \
  -f scripts/fixtures/earn-history-scale.sql
psql postgresql://postgres@127.0.0.1:55432/postgres -X -v ON_ERROR_STOP=1 \
  -c 'CALL public.seed_earn_history(58)'
python3 scripts/verify-earn-rate-history-scale.py
# Repeat full row parity and performance checks after an existing backfill:
python3 scripts/verify-earn-rate-history-scale.py --existing
```

Contract verification creates a separate `earn_history_contract` database and
refuses to discard an existing database. It checks backfill retry/rollback,
COPY/new chunks, corrections, deletes, atomic failure, and actual concurrent
backfill/update/delete races. Scale verification applies the production
migration and backfill script, compares **every** projected row with its source,
and checks a 57-day earnings query against an independent analytical answer
with deposits, withdrawal and reserve switches. Both paths use real PostgreSQL,
not mocked SQL calls.

### Recorded evidence

Production comparison point: a historical chunk with 138,893 rows, 940 MB total,
and sampled logical row width 5,436 bytes. Local synthetic fixture: 58 daily
chunks, 140,000 rows/day, **8,120,000 rows**, four reserves and about 5.5 KiB logical
rows with out-of-line payload. TimescaleDB 2.24.0 matches the queried production
extension version. This approximates the relevant history's scale and layout,
not the entire production database or its hardware/cache/load.

- Actual migration registry: migrations 1–9 `--apply`, then `--check`, passed on
  an empty local database; `cargo check -p loyal-timescale-migrations` passed.
- Contract verifier: PASS, including waiting concurrent writers and rollback.
- Scale backfill: every source observation matched its projection; all 8.12m
  rows present and the explicit history-window checkpoint complete.
- Wide query: 12.27 / 11.20 / 11.10 seconds. Final compact query: **3.68 / 3.50 /
  3.39 seconds**, accurate in every run, with **zero index-only scans**. Final
  table sizes: wide 55 GB, compact 2,562 MB including indexes.
- 10,000 inserts plus rollback and future-chunk creation: 179–199 ms before the
  trigger, 548–677 ms with it. This is measurable ingest overhead, not free;
  monitor production writer throughput/latency before enabling app reads.
- The separate unshipped loyal-app prototype was also replayed against this
  database with 6,670 snapshots and all chart ranges. Numerical earnings passed,
  but its more complex full calculation **did not consistently meet its 6.5s
  database budget** (latest first rate read 7.33s; earlier attempts 7–8.3s).
  Earlier 3.4s prototype results used a simpler experimental table/backfill and
  are not the final integration result. Do not claim the app issue is fixed or
  enable that reader based solely on this database PR.

## Operator rollout / rollback

1. Obtain explicit approval for production writes. This PR has not been applied
   to production. Review extra disk/index/WAL capacity, ingestion headroom and
   which role owns the trigger. Apply through the normal Timescale migration
   runner; installation takes brief DDL locks and fails on a five-second lock
   acquisition timeout instead of waiting indefinitely.
2. Watch reserve-ingest errors/lag, rebalance and autodeposit health immediately
   after installation. Projection failures are intentionally critical and can
   reject ingestion. Stop rollout if throughput or retries regress.
3. Backfill a fixed approved UTC range, with the 1Password-injected Timescale URL:
   `psql -X -v ON_ERROR_STOP=1 -v history_start=<UTC> -v history_end=<UTC> -f scripts/backfill-earn-rate-history.sql`.
   Supply the connection via the normal injected environment/psql connection;
   never save credentials to files. Do not use `--single-transaction` or an outer
   BEGIN. The script sets two-second lock and ten-second statement limits. On
   failure stop, inspect ingest/locks and rerun the **same** bounds to resume.
4. Verify checkpoints and bounded source/projection parity, including late
   observations; grant the app's existing read-only role SELECT according to
   normal database role policy. A completed window certifies only that explicit
   source range, not arbitrary earlier history. Do not enable app reads until
   the follow-up proves complete coverage through the requested end and passes
   the full application budget. No background worker or timeout increase is
   introduced by this PR.
5. If ingestion is affected, an operator can drop `sync_reserve_earn_rate` on
   `kamino.reserve_updates` under a short lock timeout. Mark the projection as
   unusable and reset/replay affected backfill checkpoints before re-enabling;
   dropping the trigger breaks ongoing completeness. Leave the tables in place
   for investigation. Never silently fall back to an incomplete projection.
