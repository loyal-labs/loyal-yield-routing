# ASK-2180 production DB query latency verifier

Run this verifier cold from the repository root and return one PASS or FAIL
line for every required condition, followed by an overall verdict. Treat any
missing evidence as FAIL.

## Required end state

1. `scripts/verify-ask-2180-db-query-latency.sh` creates and destroys its own
   PostgreSQL cluster under a temporary directory. It must reject a supplied
   production database URL and must not contact Render, Neon, ClickStack, or
   Solana.
2. The verifier applies the real Yield Neon migrations through migration 40,
   then generates production-shaped data at the same order of magnitude as the
   observed production tables: at least 400,000 optimizer epochs, 450,000
   rebalance opportunities across every durable state, and 5,000 signed route
   submissions. It must execute the exact query used by
   `fleet_orchestration_status_source_on_connection`, not a reduced model.
3. Before applying the ASK-2180 migration, the verifier records the canonical
   query result and at least five warm `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`
   samples. It must demonstrate both original bottlenecks: scanning historical
   optimizer epochs to find the latest cluster epoch and scanning historical
   opportunities to aggregate only the current epoch.
4. After applying the ASK-2180 migration, the exact query result must be
   canonically identical to the baseline result. The optimized plan must use
   the new latest-epoch and optimizer-epoch opportunity indexes, and must no
   longer perform either historical full scan identified above.
5. On the same isolated database, the optimized median execution time must be
   below 1,000 ms and at least 50 percent lower than the baseline median. The
   verifier must print row counts, both medians, the percentage improvement,
   and the relevant plan-node evidence.
6. The full migration runner can apply all migrations to a fresh disposable
   database, `cargo test -p loyal-yield-store --lib` passes, the focused health
   projector database contract passes, `cargo check -p loyal-yield-orchestrator
   --bin fleet-health-projector` passes, `cargo fmt --all -- --check` passes,
   and `git diff --check` passes.
7. Read-only production evidence is documented with a bounded UTC window,
   ClickStack service/version attribution, current Render image lineage, and a
   read-only Neon plan. No production mutation or deployment is part of this
   change.
8. A ready-for-review PR links ASK-2180 and explains the goal, scope,
   implementation, verification, production evidence, and post-merge migration
   and latency checks in plain human language.

## Nice to have

- The verifier supports smaller row-count overrides for local iteration while
  keeping production-scale defaults for its PASS verdict.
- The production evidence separately reports slow-per-call services and the
  highest query-volume service.

Overall verdict: PASS only when every required condition is observed directly.
