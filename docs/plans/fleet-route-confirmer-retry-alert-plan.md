# Fleet Route Confirmer Retry Alert Fix

## Problem

`loyal-fleet-route-confirmer` currently emits
`fleet_route_confirmer_items_deferred_after_error` after the first retryable
item failure. It also reports every requested lease as deferred without checking
how many leases the store actually released.

The production failure exposed two concrete bugs:

1. The Solana RPC client and confirmation lease both use roughly 30-second
   defaults, so an RPC timeout can consume the lease before deferral.
2. A transient retry is treated as an actionable outage before retry has had a
   chance to recover.

## Intended Behavior

- Retry a transient upstream failure without paging.
- Page immediately when safe retry cannot be proved: exact fenced deferral
  fails, an invariant fails, an on-chain effect is ambiguous, or the worker
  terminates.
- Page once when retryable confirmation work remains unable to make progress
  for 60 seconds.
- Do not let an idle poll count as recovery.
- Reset the retry alert only after a later claimed-work poll succeeds.

The alert criterion is elapsed no-progress time, not retry count. Retry count is
an implementation detail and varies with RPC latency.

## Fix

### 1. Bound RPC calls inside the lease

Construct the Solana RPC client with an explicit 10-second timeout. Keep the
existing 30-second lease and validate at startup that the RPC timeout is
strictly shorter than the lease.

Do not add lease renewal, hidden RPC retries, or deploy-time configuration for
this fix. Preserve `maxRetries: 0` for transaction broadcast; the durable queue
owns retry.

### 2. Make deferral all-or-error

`defer_signed_route_submission_lease_batch` must either release the exact batch
of live fenced leases or return the existing store-invariant error shape. Use
the same strict count check already used by confirmation batch transitions.

`defer_claims_after_error` must return only after exact release succeeds.
Callers must increment `deferred` by the returned release count, never by the
requested batch length.

This deliberately fails closed. Do not add a partial-success result type or
re-read unmatched rows in this change.

### 3. Replace first-error paging with one no-progress latch

Replace `item_errors_reported` with a small in-process state value containing:

- when the current retryable failure streak began;
- whether the 60-second alert was already emitted.

Behavior:

- safely deferred item failure: start or continue the timer, keep retrying, and
  emit only the normal structured poll health record;
- timer reaches 60 seconds: emit one
  `fleet_route_confirmer_items_deferred_after_error` operational error and mark
  it reported;
- further retryable failures: remain silent at operational-error level;
- successful poll with `claimed > 0` and `item_errors == 0`: clear the state;
- idle poll with `claimed == 0`: leave the state unchanged.

Keep the existing alert code so the deployed ClickStack rule does not require a
coordinated rename. Change its static message to describe stalled retries rather
than claiming that one item failure requires recovery.

Whole-poll errors keep their existing one-per-consecutive-outage behavior.
Fatal and invariant paths remain immediate.

## Scope

Expected code changes:

- `crates/loyal-yield-orchestrator/src/bin/fleet-route-confirmer.rs`
- `crates/loyal-yield-store/src/fleet_orchestration/queue.rs`
- focused tests/verifier only

No migration, new table, durable incident subsystem, failure taxonomy,
exponential backoff, alert-rule mutation, deployment, or production write.

Before any commit or PR, create or select a Linear issue and use its identifier
for the branch. Existing unrelated workspace changes must remain untouched.

## Verification Contract

Required observable behavior:

1. The RPC client has an explicit timeout shorter than the confirmation lease.
2. A deferral updating fewer rows than requested returns an error and is never
   reported as successful deferral.
3. One safely deferred item failure emits no operational alert.
4. Repeated failures before 60 seconds emit no operational alert.
5. The first failure observation at or after 60 seconds emits exactly one alert.
6. Continued failures emit no duplicate alert.
7. An idle poll does not reset the failure timer or alert latch.
8. A successful claimed-work poll resets the state; a later outage can alert
   once again after 60 seconds.
9. Existing fatal, poll-error, and exact-fence safety behavior still compiles
   and remains fail-closed.

The verifier must exercise state transitions with a controlled clock. Do not
wait 60 wall-clock seconds and do not verify source substrings in place of
behavior.

Minimum validation:

```text
cargo fmt --check
cargo check -p loyal-yield-store
cargo test -p loyal-yield-orchestrator --bin fleet-route-confirmer
cargo check -p loyal-yield-orchestrator --bin fleet-route-confirmer
git diff --check
```

## Done When

- The verifier passes every required condition.
- A single transient RPC failure remains visible in poll health but cannot page.
- A 60-second retry stall emits one operational error.
- Exact fenced deferral is truthful and fail-closed.
- No migration or unrelated refactor is present in the diff.
