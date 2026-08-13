# Fleet control-loop simplification verifier

Run this verifier adversarially. The overall verdict is `PASS` only when every
required section passes without production access, production keys, or a
deployment.

## Required 1: one bounded health read path

- A migration creates one cluster-keyed current health snapshot containing the
  complete serialized `FleetOrchestrationStatus` result, refresh timestamp,
  source watermark, refresh duration, and owner/fence evidence.
- The expensive `loyal_yield.fleet_orchestration_status` history aggregation
  remains available only as the projector's source/analytics view.
- Worker health emission reads only the snapshot row. It never falls back to
  the history view when the snapshot is missing, stale, or malformed.
- Missing/stale/malformed snapshots produce an explicit degraded-health event
  and do not stop or delay queue claiming/execution.
- A refresh computes the source result and active writable-key congestion once,
  stores them atomically, and a fresh cached read is semantically identical to
  that source result.

## Required 2: single refresh owner and bounded freshness

- A dedicated `fleet-health-projector` binary owns refreshes. Production fleet
  workers do not refresh the snapshot.
- A PostgreSQL advisory lock or equivalent durable fence prevents two
  projectors from refreshing one cluster concurrently.
- The projector publishes compact one-line JSON including cluster, duration,
  row count, source watermark, and next refresh time.
- Snapshot maximum age and refresh interval are explicit and validated. The
  worker reports stale data as degraded instead of presenting it as current.
- `render.yaml` defines exactly one pinned lightweight worker for the projector;
  no image is built, pushed, or deployed by this task.

## Required 3: coalesced planning wakeups and compact logs

- Dirty-vault state remains durable and authoritative. PostgreSQL `NOTIFY`
  remains only a lossy wakeup hint.
- Repeated updates to an already-dirty `(cluster, vault_id)` row do not emit a
  new notification. A newly inserted dirty row does emit one.
- Generation, reasons, maximum observed slot, and earliest availability still
  merge correctly under repeated updates and leased-row races.
- The production planner command uses compact JSON.
- Under the isolated 10k opportunity / high-frequency mock-chain scenario, the
  planner remains alive and emits no more than 2,000 physical log lines in 20
  seconds. No synthetic log activity may be labelled worker progress.

## Required 4: measured hot-path improvement

Run the isolated component lab with 1,000,000 historical opportunities, ten
health clients, and at least 15 seconds of concurrent load after a successful
snapshot refresh.

- Cached health p95 is at most 50 ms.
- Executor-shaped throughput is at least 100 TPS.
- The hot health workload causes no PostgreSQL temp-file spill.
- No deadlocks, workload errors, or process exits occur.
- The evidence separately reports projector refresh duration and hot snapshot
  read latency; it must not hide source-refresh cost inside the cached number.
- A direct-source control remains in evidence so the report cannot claim that
  historical analytics became cheap.

Run a one-versus-ten health-client comparison at 100k history:

- Ten cached readers retain at least 80% of the one-reader executor-shaped TPS.
- Ten-reader cached health p95 remains at most 50 ms.

## Required 5: route correctness and RPC budget visibility

- The LiteSVM verifier still passes.
- The fresh-validator Main-to-Prime full-chain verifier still passes with
  exactly one signature, a reconciled terminal state, and a no-op rerun.
- The stateful runner accepts explicit RPC latency, jitter, and fault controls
  and records per-source/per-method counts.
- A successful 80-120 ms delayed-RPC run has zero production-process RPC errors
  and reports continuously refreshed simulated market input truthfully.
- Evidence reports production RPC method budgets; no claim is made that a
  process-local cache can remove identity calls across separate CLI processes.

## Required 6: automated negative controls and regressions

`bun run verify:fleet-control-loop-simplification` must pass positive fixtures
and reject at least these mutations:

1. worker SQL or Rust directly reads the expensive source view;
2. stale snapshot is labelled healthy;
3. missing snapshot triggers a source-view fallback;
4. two refresh owners are admitted for one cluster;
5. cached payload differs from the direct source result;
6. repeated dirty-row update emits another notification;
7. synthetic SQL/RPC/log work is attributed to a real worker;
8. performance evidence omits source refresh cost;
9. route evidence has duplicate signatures or a mutating rerun.

Also run:

```sh
cargo fmt --all -- --check
cargo test -p loyal-yield-store --lib
cargo test -p loyal-yield-orchestrator --bin fleet-health-projector
cargo test -p loyal-fleet-worker --lib
bun run verify:fleet-local-load-lab
bun run verify:fleet-litesvm-e2e
bun run verify:fleet-local-chain-e2e
git diff --check
```

No attributable file may contain Cyrillic. Evidence must contain no private-key
array, production endpoint, or production database value.

## Verdict

```text
BOUNDED_HEALTH_READ: PASS | FAIL
SINGLE_REFRESH_OWNER: PASS | FAIL
COALESCED_WAKEUPS_AND_LOGS: PASS | FAIL
MEASURED_LOAD_IMPROVEMENT: PASS | FAIL
ROUTE_AND_RPC_REGRESSION: PASS | FAIL
ADVERSARIAL_CONTROLS: PASS | FAIL
FLEET_CONTROL_LOOP_SIMPLIFICATION: PASS | FAIL
```
