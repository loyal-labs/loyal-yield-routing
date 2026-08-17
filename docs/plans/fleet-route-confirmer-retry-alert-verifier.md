# Fleet Route Confirmer Retry Alert Verifier

Run this verifier from the repository root against the current checkout. Act as
a skeptical reviewer: report each required condition as PASS or FAIL with the
command or test evidence, then return overall `PASS` only when every required
condition passes.

## Required behavior

1. `retry_alert_state_requires_sixty_seconds_of_failures` proves a safely
   deferred failure does not request an alert initially or at 59 seconds,
   requests exactly one alert at 60 seconds, and suppresses later duplicates.
2. `retry_alert_state_ignores_idle_polls` proves an idle poll neither resets nor
   advances away the active failure streak.
3. `retry_alert_state_resets_after_successful_claimed_work` proves a successful
   non-idle poll clears the streak and a later independent streak receives its
   own full 60-second window.
4. `rpc_timeout_must_be_shorter_than_confirmation_lease` proves the intended
   10-second timeout is valid with the default 30-second lease and rejected when
   timeout is greater than or equal to the lease.
5. `exact_confirmation_defer_count_accepts_full_batch` proves an exact fenced
   deferral count is returned unchanged.
6. `exact_confirmation_defer_count_rejects_partial_batch` proves a partial
   update returns a store invariant instead of successful deferral.
7. The production loop emits
   `fleet_route_confirmer_items_deferred_after_error` only when the retry alert
   state requests the transition; a first safely deferred error cannot call
   `OperationalError::emit` directly.
8. Every `defer_claims_after_error` caller uses its returned released count for
   health accounting or intentionally discards it only when no deferred count
   is recorded. No caller increments `deferred` from the requested lease length.
9. The Solana RPC client is constructed with an explicit bounded timeout and
   startup rejects a lease that is not longer than that timeout.
10. No database migration, durable incident table, exponential backoff, new
    Render configuration, or unrelated refactor is included in the task diff.

## Commands

```sh
cargo test -p loyal-yield-orchestrator --bin fleet-route-confirmer
cargo test -p loyal-yield-store exact_confirmation_defer_count
cargo check -p loyal-yield-store
cargo check -p loyal-yield-orchestrator --bin fleet-route-confirmer
cargo fmt --check
git diff --check
```

Inspect only the task diff in:

```text
docs/plans/fleet-route-confirmer-retry-alert-plan.md
docs/plans/fleet-route-confirmer-retry-alert-verifier.md
crates/loyal-yield-orchestrator/src/bin/fleet-route-confirmer.rs
crates/loyal-yield-store/src/fleet_orchestration/queue.rs
```

Do not fail the verifier for pre-existing unrelated dirty files. Do fail it if
the task edits or depends on them.

## Verdict format

```text
1. PASS|FAIL - evidence
2. PASS|FAIL - evidence
...
10. PASS|FAIL - evidence
Overall: PASS|FAIL
```
