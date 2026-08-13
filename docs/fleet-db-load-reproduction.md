# Local fleet database load reproduction

This harness measures how the production fleet health query and worker-shaped
SQL behave as durable orchestration data grows. It is intentionally isolated:
it creates a new PostgreSQL cluster on loopback, applies the repository's real
Yield migrations, clears database and Solana environment variables, and uses
synthetic chain observations only.

Run the standard evidence matrix:

```sh
bun run fleet:reproduce-db-load
```

Run a short smoke matrix:

```sh
bun run fleet:reproduce-db-load -- \
  --scales 1000,10000 \
  --duration-seconds 3 \
  --output /tmp/fleet-db-load-smoke
```

The default matrix seeds 10,000, 100,000, and 1,000,000
`rebalance_opportunities`. At every scale it also creates one decision and
signed submission per four opportunities and one outbox row per two
opportunities. The fixture has 1,000 managed vaults with current position
projections.

The concurrent phase runs these local roles:

- health pollers execute the exact `fleet_orchestration_status` query used by
  the Rust store;
- executor, confirmer, and reconciler workloads exercise their production
  queue indexes with row locking and state-check writes;
- planner workload updates the durable planning heartbeats;
- user workload updates vault observations and emits durable outbox events;
- mock-chain workload advances local position slots and balances.

Every scale records:

- `EXPLAIN (ANALYZE, BUFFERS, WAL, FORMAT JSON)` for the exact health query;
- baseline and loaded p50/p95/p99/max transaction latency;
- per-role transaction throughput;
- real row counts, relation sizes, and total database size;
- raw PostgreSQL, pgbench, and machine-readable JSON evidence.

Evidence is written beneath `artifacts/fleet-db-load/<UTC timestamp>/`. The
artifact directory is ignored because raw pgbench samples can be large.

## Safety boundary

The generated database name starts with `fleet_verify_`, the URL host is
hard-coded to `127.0.0.1`, and the harness refuses to continue unless both
guards pass. It never loads `.env` or 1Password files and unsets known
production database, RPC, Helius, and ClickStack variables.

The result is complete for the local SQL coordination path, not for external
infrastructure. It does not claim to reproduce Neon network/autoscaling
behavior or Solana validator/RPC latency.

## First full evidence run

The first controlled run completed on 2026-07-28 at 10:18:30
Asia/Yekaterinburg using PostgreSQL 17.10 on Darwin arm64. Every scale ran for
10 seconds with three concurrent health pollers plus the executor, confirmer,
reconciler, planner, user, and mock-chain roles.

| Opportunities | Total DB size | Idle health p95 | Loaded health p95 | Loaded max |
| ---: | ---: | ---: | ---: | ---: |
| 10,000 | 27.0 MiB | 13.28 ms | 45.89 ms | 76.88 ms |
| 100,000 | 236.6 MiB | 108.53 ms | 262.81 ms | 383.43 ms |
| 1,000,000 | 1,531.1 MiB | 1,079.60 ms | 2,247.75 ms | 2,247.75 ms |

The exact view query scales approximately linearly with queue history in this
fixture. Its measured `EXPLAIN ANALYZE` execution time grew from 12.76 ms to
123.28 ms to 1,255.07 ms. At 1,000,000 opportunities the plan read 279,872
shared blocks and spilled 14,707 read plus 16,427 written temporary blocks.
The view repeatedly scans durable history: opportunities feed cluster
discovery, state totals, queue totals, and current-epoch unlock metrics;
submissions feed cluster discovery, state totals, and lifecycle aggregation.

Worker-shaped throughput degraded at the same time:

| Opportunities | Executor TPS | Confirmer TPS | Reconciler TPS |
| ---: | ---: | ---: | ---: |
| 10,000 | 581.91 | 3,215.64 | 3,294.68 |
| 100,000 | 63.33 | 998.24 | 1,003.95 |
| 1,000,000 | 4.00 | 43.69 | 53.33 |

This is local evidence, not a Neon capacity number. Its actionable result is
that the current health view has an unbounded history-scan cost and amplifies
worker contention as data grows; external latency is not required to reproduce
the failure shape.
