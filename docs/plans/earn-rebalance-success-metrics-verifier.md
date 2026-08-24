# Earn rebalance success metrics verifier

## Goal

Prove that every durable forward transition in the Earn rebalance pipeline emits
one privacy-safe OpenTelemetry success count and duration through
`loyal-observability`:

```text
balance-sweep-ata-monitor
  -> fleet-opportunity-planner
  -> fleet route revalidation
  -> fleet route execution handoff
  -> fleet-route-confirmer
  -> fleet route reconciliation
```

Run the verifier from the repository root:

```sh
bun run verify:earn-rebalance-success-metrics
```

## Required conditions

1. The package script above exists and runs the sole verifier entrypoint.
2. `loyal-observability` owns one typed, low-cardinality Earn rebalance metric
   contract with exactly these operations:
   - `ata.observation_persisted`
   - `opportunity.published`
   - `route.revalidated`
   - `route.execution_handoff_persisted`
   - `route.confirmed`
   - `route.reconciled`
3. A behavioral in-memory OpenTelemetry test proves that one success recording
   for every operation exports exactly six counter increments and six duration
   observations. Metric attributes are limited to workflow, operation, and
   outcome. Runtime wallet, vault, opportunity, route, signature, transaction,
   and error-detail values are forbidden.
4. Each production worker records its stage only after the corresponding
   durable write succeeds:
   - ATA observation persistence
   - opportunity publication
   - revalidation to `ready`
   - signed submission and decision handoff
   - confirmation to `reconciliation_pending`
   - reconciliation to `reconciled`
5. Retry, stale, skipped, waiting-ALT, deferred, failed, fenced, and generic
   task-handled outcomes do not increment a succeeded stage. A fused
   revalidation plus execution records both logical stages once.
6. The observability crate and every affected production binary compile.
7. Rust formatting and `git diff --check` pass.

## Verdict

The verifier prints one `PASS` or `FAIL` line per required condition. Overall
`PASS_EARN_REBALANCE_SUCCESS_METRICS` is allowed only when every required
condition passes. Any missing, blocked, or unexecuted required check is an
overall failure.

Live deployment, ClickStack arrival, dashboards, and alerts are post-merge
release checks. They cannot substitute for this implementation verifier.
