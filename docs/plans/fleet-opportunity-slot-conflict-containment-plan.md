# Fleet Opportunity Slot Conflict Containment Plan

Step 1 of 2. This plan does not fix the planner/worker race. It removes the
race's ability to stop fleet planning. Step 2 lives in
`fleet-opportunity-vault-race-plan.md` and removes the race itself.

## Goal

A PostgreSQL `23505` on `active_rebalance_opportunity_slots_pkey` must degrade
to a per-vault deferral instead of terminating the planner process.

## Why this is separable and urgent

The conflict itself costs one vault one planning wave. The outage cost comes
entirely from error propagation:

- `publish_wave` returns any unclassified publication error
  (`crates/loyal-yield-orchestrator/src/bin/fleet-opportunity-planner.rs:440`);
- `run_full_sweep` is awaited with `?`
  (`fleet-opportunity-planner.rs:813`);
- the service loop is awaited with `?`
  (`fleet-opportunity-planner.rs:1212`).

So one contended vault exits `loyal-fleet-opportunity-planner` for the whole
cluster. There is no `23505` classification anywhere in the crate today.

This step is intentionally free of schema changes, lock-protocol changes, and
query-plan changes, so it can ship before the race fix is designed.

## Change

### 1. Typed error

Add one variant next to `OpportunityDeferredBehindLease`
(`crates/loyal-yield-orchestrator/src/lib.rs:87`):

```rust
#[error("new opportunity for vault {vault_id} is deferred behind active slot owner {slot_opportunity_id:?}")]
OpportunityDeferredBehindActiveSlot {
    vault_id: VaultId,
    slot_opportunity_id: Option<i64>,
    slot_opportunity_state: Option<String>,
    reason: &'static str,
},
```

`reason` carries a stable code: `active_slot_owner_valid` or
`active_slot_owner_unresolved`.

### 2. Classification at the insert site

In `upsert_rebalance_opportunity`
(`crates/loyal-yield-orchestrator/src/fleet_orchestration/queue.rs:1522`),
match the insert error before `?` converts it:

1. Accept only `sqlx::Error::Database` where `code() == "23505"` **and**
   `constraint() == Some("active_rebalance_opportunity_slots_pkey")`. Never
   match on message substrings. Any other database error keeps its current
   behavior.
2. Roll the transaction back explicitly. PostgreSQL leaves the transaction
   aborted after `23505`, so no further statement can run on it.
3. On a fresh pool connection, read the slot owner for the vault:

   ```sql
   SELECT slot.opportunity_id, opportunity.opportunity_state
   FROM loyal_yield.active_rebalance_opportunity_slots slot
   LEFT JOIN loyal_yield.rebalance_opportunities opportunity
     ON opportunity.id = slot.opportunity_id
   WHERE slot.vault_id = $1 AND slot.cluster = $2
   ```

4. Return `OpportunityDeferredBehindActiveSlot`. If the read fails or returns
   nothing, still return the typed error with `slot_opportunity_id: None` and
   `reason = "active_slot_owner_unresolved"`. Telemetry quality must never
   convert a contained conflict back into a fatal error.

Deliberately out of scope here: deciding whether an inconsistent slot is
database corruption. Step 2 owns that. Containment must not fail closed on a
path whose only current job is keeping the planner alive.

### 3. Wave classification

Extend `is_publish_contention`
(`fleet-opportunity-planner.rs:391`) with the new variant, and emit the
existing deferral event shape (`fleet-opportunity-planner.rs:430-438`) with
`"reason"` taken from the error instead of the hard-coded
`"unexpired_competing_lease"`.

The partition assertion at `fleet-opportunity-planner.rs:821`
(`published + deferred_contention == queue_input_count`) must keep holding;
that is the check proving the new branch does not silently drop a vault.

Telemetry fields: vault ID, slot opportunity ID, slot opportunity state,
reason code. No secrets, no signed transaction bytes.

### 4. Deferred work still drains

A contended vault stays dirty and is replanned on the next wave.
`next_full_sweep_delay` (`fleet-opportunity-planner.rs:466`) already shortens
the next sweep when `deferred_count > 0`, so no new scheduling work is needed.

## Verification

One scenario added to the existing isolated-database path
(`crates/loyal-yield-orchestrator/src/bin/fleet-orchestration-verifier.rs:7487`,
`isolated_database_evidence`). Do not add a new verifier mode; the fixture
already enforces the `fleet_verify` database-name guard
(`fleet-orchestration-verifier.rs:2346`), reads `FLEET_VERIFY_DATABASE_URL`
(`:10913`), applies the real migrations, and cleans its prefixed fixtures.

Scenario `active_slot_conflict_is_contained`:

1. Seed a managed vault, policy, optimizer epoch, and one active opportunity.
2. Insert a second competing active opportunity for the same vault through a
   direct writer that bypasses the publication path, forcing the trigger
   (`migrations/0023_value_priority_rebalance_queue.sql:518`) to raise the
   exact `23505`.
3. Assert `upsert_rebalance_opportunity` returns
   `OpportunityDeferredBehindActiveSlot`, not `StoreInvariant` and not a raw
   `sqlx` error.
4. Assert the returned slot-owner evidence names the real occupant.
5. Assert no new opportunity row and no second slot row exist for the vault.

This is a Postgres error-mapping contract that would compile while broken, so
it qualifies under the Rust test policy. No other new Rust tests.

Acceptance command:

```sh
op run --env-file=.env.1password -- sh -c 'bun run fleet:verify -- --isolated-database'
```

## Done When

- A forced slot conflict returns the typed per-vault deferral.
- `publish_wave` counts it as deferred, keeps publishing other vaults, and the
  partition assertion holds.
- The planner process does not exit on a slot conflict.
- No schema change, no migration, no change to the claim path.
- `fleet:verify --isolated-database` passes including the new scenario.

## Rollout

Planner-only change in behavior; the worker binaries are untouched. Build the
image through the `worker-images` workflow and redeploy
`loyal-fleet-opportunity-planner` only after an explicit order.
