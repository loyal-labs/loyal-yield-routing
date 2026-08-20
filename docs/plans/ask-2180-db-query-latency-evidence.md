# ASK-2180 production DB query latency evidence

All production checks in this investigation were read-only. The implementation
and latency measurements run only against disposable local PostgreSQL clusters.

## Bounded production window

- Dashboard: `Production DB Query Metrics` (`6a836c1f8d70881e14192be9`).
- Screenshot window: `2026-08-19T19:06:21Z` through
  `2026-08-19T20:06:21Z`.
- ClickStack metric: `db.client.operation.duration`.
- Sustained latency leader: `loyal-fleet-health-projector`, image
  `sha-55deae1f435b8246e029f378e03af858fe69532c`, with approximately
  `3.20 s` p95 and `4.65 s` p99 throughout the hour.
- Render logs show the same four-row source query taking `3.326-3.944 s`
  every refresh cycle:

  ```sql
  SELECT *
  FROM loyal_yield.fleet_orchestration_status
  WHERE cluster = $1
  ORDER BY opportunity_state NULLS LAST
  ```

- Live Render deployment during the window: `dep-da255ajl550s73b6uom0`,
  immutable image `light-workers:sha-55deae1f435b8246e029f378e03af858fe69532c`.
- `loyal-fleet-route-reconciler` is the separate query-volume leader by orders
  of magnitude, while its latency remains near `99 ms` p95 and `328 ms` p99.
  That volume is not the cause of the projector's sustained multi-second query.
- `loyal-balance-sweep-ata-monitor` had one isolated `9.75 s` p99 bucket near
  `2026-08-19T19:28:45Z`; it immediately returned to its normal range.
- Connection acquisition is not the main bottleneck: p95 remained near
  `98-100 ms` for most services.

## Read-only Neon plan

At `2026-08-19T20:18:20Z`, the current health snapshot reported a
`3.466748 s` source query. A read-only
`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` of that exact query took
`3,894.327 ms` and returned four rows.

The view recomputed lifetime status from approximately:

- `437,281` optimizer epochs;
- `473,207` rebalance opportunities in the measured plan; and
- `5,196` signed route submissions.

The plan read `888,267` shared blocks and spilled the final sort to temporary
storage. Its main repeated work was:

- a latest-epoch sort/unique over about `450,490` optimizer-epoch rows;
- a `1,270 ms` full scan of historical opportunities; and
- two more historical opportunity scans around `608 ms` and `573 ms`.

`pg_stat_statements` is not installed, so the bounded ClickStack histogram,
SQLx slow-query logs, and direct read-only plan are the timing sources.

## Optimization boundary

The migration keeps the projector's query and result shape intact. It adds
narrow indexes for latest-epoch lookup, current-epoch opportunities, and
lifetime state aggregates, then rewrites the view so opportunity totals are
computed once and submission lifecycle work is limited to the latest epoch.
No Render deployment, production migration, database write, or Solana action
is part of this PR.

## Isolated verifier result

On a disposable local PostgreSQL database with `400,000` optimizer epochs,
`450,000` opportunities across every durable state, and `5,000` submissions,
the exact source query produced the same canonical result before and after the
migrations. With PostgreSQL's default planner settings, the five-sample warm
median fell from `1,720.647 ms` to `75.622 ms` (`95.61%` faster). The optimized
plan naturally selected all three new indexes and contained no sequential scan
of `optimizer_epochs` or `rebalance_opportunities`.
