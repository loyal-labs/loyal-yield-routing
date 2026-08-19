# ASK-2173 Earn LaserStream Reconciliation Plan

Tracking: [ASK-2173](https://linear.app/askloyal/issue/ASK-2173/add-earn-accounts-to-laserstream)

Pull requests:

- routing: [loyal-yield-routing#64](https://github.com/loyal-labs/loyal-yield-routing/pull/64)
- cron removal: [loyal-app#668](https://github.com/loyal-labs/loyal-app/pull/668)

## Goal

Make confirmed LaserStream account updates the only source that wakes Earn
reconciliation. Replace the two Loyal App cron scans without losing any of their
recovery behavior:

- policy-only onboarding recovery;
- invisible deposit recovery;
- full-withdraw cleanup recovery, including `confirm_missed` and
  `cleanup_pending`.

Neon remains the durable handoff between stream ingestion and reconciliation.
It is not a second discovery source: no polling process scans wallets or the
chain to invent work independently of a LaserStream update.

The target pipeline is:

```text
LaserStream account update
  -> atomic receipt + coalesced vault job + replay cursor
  -> existing loyal-fleet-route-reconciler process
  -> targeted transaction/account proof
  -> atomic canonical Earn writes + receipt completion + fenced job transition
```

## Current State

The current routing branch implements the producer half:

- one confirmed account subscription with separate filters for balance-sweep
  ATAs, Earn policy accounts, Earn vault accounts, Earn idle token accounts,
  and Earn obligations;
- no transaction subscription;
- dynamic watch-set replacement and bounded bootstrap work for newly watched
  vaults;
- normalization of account updates and deletion/tombstone updates;
- an atomic Neon write for idempotency receipts, a coalesced per-vault job, and
  the LaserStream replay cursor;
- strict separation between Earn updates and the balance-sweep projector.

The branch currently stops at the durable job. No Rust consumer performs the
canonical Earn reconciliation yet. This is a merge blocker, not a follow-up.

## Decisions

### LaserStream is the wake-up source

Every recovery case changes at least one account already in the subscription.
The account update carries the transaction signature, so a separate transaction
subscription would duplicate notifications and add ordering noise.

The consumer may fetch a confirmed transaction by a receipt's signature. That
is targeted proof work caused by an account update, not a second stream or a
periodic chain scan.

### Keep ingestion and reconciliation in separate execution lanes

`balance-sweep-ata-monitor` owns the LaserStream session, watch-set refresh,
normalization, receipt insertion, and replay cursor. It must not wait on
multi-call RPC proofs or canonical accounting transactions.

The consumer belongs in the already deployed `loyal-fleet-route-reconciler`
process as an independent Earn lane. That process already owns durable leasing,
fencing, retry/defer behavior, bounded RPC concurrency, and worker health
telemetry. Adding a lane does not add a Render service.

The Earn lane must have its own batch size, concurrency limit, counters, and
no-progress alert state. Earn work must never consume all route-confirmation
capacity or delay signed-route recovery.

The `loyal-yield-router` crate is not the owner: it is a Timescale client, not a
worker runtime. The `balance-sweep-ata-projector` is also not an owner: it must
never receive Earn observations or turn an Earn balance change into an
autodeposit lot.

### Canonical writes live in `loyal-yield-store`

The worker resolves chain evidence into typed outcomes. It does not scatter raw
SQL across `loyal-fleet-worker`.

`loyal-yield-store` will own:

- Earn job lease, defer, and fenced-completion methods;
- unconsumed receipt loading and completion;
- the canonical onboarding-policy transaction;
- the canonical deposit/position/holding transaction;
- the canonical cleanup transaction.

Policy and action decoding belongs in `loyal-actions`. Reusable Squads decoding
currently private to `loyal-squads-policy-monitor` should move there rather than
creating a dependency on another monitor executable.

## Safety Invariants

1. A LaserStream cursor advances only in the same transaction that durably
   records every derived receipt and wakes the affected vault job.
2. A job completes only in the same transaction that applies its canonical
   Earn writes and marks the exact consumed receipts complete.
3. Every lease transition is fenced by job id, owner, and fencing token. A
   stale worker cannot complete or defer a newer wake-up.
4. Coalescing never means dropping evidence. The consumer processes every
   unconsumed receipt for the lease, not only `latest_signature`.
5. Confirmed transaction slots are resolved from RPC and are canonical. An
   account-update slot is a trigger and ordering hint, not a substitute for the
   transaction's confirmed landing slot.
6. Full-exit cleanup writes zero state only after the independent slot-pinned
   zero proof succeeds. RPC lag, an unknown positive token account, or any
   remaining reserve/idle balance is retryable and writes no zero snapshot.
7. Canonical writes are idempotent by their existing external identities,
   including deposit signature, policy identity, and cleanup evidence.
8. Vault work is serialized. Existing current-snapshot advisory-lock behavior
   must be preserved when Rust writes tables shared with Loyal App.
9. An Earn receipt can produce a proven no-op, but it cannot be silently
   discarded because a decoder, RPC call, or invariant failed.
10. Earn updates never enter `balance_sweep_events`, balance-sweep observations,
    or autodeposit lot creation.

## Durable Queue Refinement

Migration 40 currently stores receipts, one coalesced job per environment and
vault, and one producer replay cursor. Before merge, refine it to support the
consumer contract.

Because migration 40 has not shipped, amend it in this PR. If any environment
applies it before this work is complete, freeze migration 40 and add the changes
in the next migration instead.

### Receipts

Each receipt is immutable evidence that an account update woke a vault. Keep:

- consumer name and event key;
- environment and vault identity;
- filter name and event kind;
- account pubkey;
- trigger slot;
- transaction signature when present.

Add completion metadata sufficient to select unconsumed receipts and audit the
result, such as `processed_at`, `processing_outcome`, and the completing job
attempt/fencing token. Do not replace receipts with a single latest-signature
field.

### Jobs

The job remains a coalesced wake-up, not an event payload. Leasing must use
`FOR UPDATE SKIP LOCKED` and return the live fencing token. A new receipt for a
leased or completed vault must queue the job again and invalidate the older
lease.

Required transitions:

```text
queued/retryable -> leased -> completed
                         \-> retryable with bounded backoff
                         \-> blocked with an operational error for hard invariants
```

`skipped` is valid only for a proven no-op. Missing proof, unknown metadata, or
RPC uncertainty is not a skip.

### Final transaction

RPC work happens outside a database transaction. After proof succeeds, the
store opens one short transaction that:

1. locks and verifies the live fenced lease;
2. applies ordered canonical mutations;
3. marks exactly the captured receipt keys processed;
4. completes the job if no newer receipt exists, otherwise leaves it queued;
5. commits all effects together.

If the fence changed, the transaction applies no canonical mutation. The newer
job attempt re-evaluates all still-unconsumed receipts.

## Reconciliation Flow

For each lease:

1. Load all unconsumed receipts for the vault in deterministic
   `(trigger_slot, event_key)` order.
2. Load the vault's onboarding, policy, managed-vault, position, deposit,
   withdrawal, reserve, and idle rows from Neon.
3. Deduplicate receipt signatures and fetch confirmed transactions with bounded
   concurrency. A not-yet-visible confirmed transaction is retryable.
4. Decode policy mutations and deposit token deltas from those transactions.
5. Read only the current accounts needed to prove the candidate outcome, using
   the required `minContextSlot`.
6. Build an ordered list of typed canonical mutations.
7. Commit the mutations, receipt completion, and job transition through the
   fenced store transaction.

If one coalesced lease contains more work than the configured cap, process a
deterministic prefix and leave the job queued for the remaining receipts. Never
truncate by marking the entire lease complete.

## Recovery Case Matrix

| Case | Candidate state | Required proof | Canonical result |
| --- | --- | --- | --- |
| Policy-only onboarding | `earn_deposit_onboarding_attempts` is at `route_policy_confirmed`, with no deposit signature/row | Decode all policy receipt signatures; validate program, settings, wallet signer, vault PDA, policy seeds/accounts, market, mint, and recorded signature/slot | Upsert route/setup policies and managed vault, then advance onboarding to `setup_policy_confirmed` |
| Invisible deposit | Watched vault has meaningful idle or Kamino holdings but the deposit signature has no canonical deposit row | Resolve the confirmed transaction; calculate owner token deltas; identify the reserve from transaction accounts, never from the largest current holding; validate active policy pair, market, mint, and principal | Insert idempotent deposit, create/update aggregate position, append holding event, update managed holdings, and mark onboarding complete |
| Cleanup `confirm_missed` | A full withdrawal is recorded, balances are zero, and policy accounts are already closed | Run the full inventory zero proof at `minContextSlot = withdrawal_confirmed_slot`; prove policy accounts absent; use the policy-account update signature or a bounded address-history fallback for close evidence | Deactivate policies, zero reserve/idle rows, close the active position, zero principal/current amount, deactivate managed vault, and record cleanup signature/slot |
| Cleanup `cleanup_pending` | A full withdrawal is recorded, balances are zero, but policy accounts remain open | Run the same slot-pinned full inventory zero proof; prove policies still exist | Apply the same canonical DB cleanup using the withdrawal as exit evidence; the existing refund path remains responsible for later on-chain policy rent recovery |
| Policy closure without cleanup candidate | Policy account deletion/update arrives but there is no eligible full-withdraw row | Decode and record the policy removal only; do not synthesize a position cleanup | Update canonical policy state and mark the receipt a proven no-op for Earn cleanup |
| Unrelated watched update | Receipt maps to the vault but produces no missing canonical state | Current DB/chain state is internally consistent and no recovery predicate matches | Mark receipt processed as a proven no-op |

### Multiple events in one job

A lease may contain policy creation, one or more deposits/top-ups, obligation
updates, withdrawal effects, and policy deletion. Apply recoverable historical
mutations in confirmed-slot order:

1. policy creation/setup and onboarding stages;
2. unseen deposits/top-ups, oldest first;
3. full-exit cleanup after its withdrawal and zero proof;
4. remaining policy-removal state.

This preserves deposit history even when the current on-chain state is already
closed by the time the worker runs.

## Watch-Set Lifecycle

The monitor must continue to derive addresses from durable product state:

- deterministic route/setup policy accounts from onboarding metadata;
- Earn vault accounts;
- supported-mint idle token accounts for those vaults;
- known Kamino obligations;
- balance-sweep wallet ATAs in their existing isolated filter.

Watch-set replacement must be deterministic and deduplicated. A pubkey may have
both an Earn binding and a balance-sweep binding; normalization must preserve
both without sending the Earn event into the projector.

When a newly discovered vault is added after relevant activity already landed,
enqueue one bounded bootstrap receipt/job. Live replacement retains the replay
overlap so an update during subscription replacement is either replayed or
covered by the bootstrap reconciliation.

## Worker Integration

Add an `earn_reconciliation` module to `loyal-fleet-worker` and invoke it as an
independent lane from `run_fleet_reconciler`.

The first implementation should keep the lane simple:

- separate environment variables for Earn batch size, concurrency, lease
  duration, and retry backoff, with conservative defaults;
- one shared `NeonSqlClient` and RPC runtime;
- separate `JoinSet` or bounded task set for Earn leases;
- no blocking RPC work in the main async poll loop;
- a wake-up notification channel if useful, with the existing bounded poll as
  correctness fallback;
- clean shutdown that stops leasing, waits within Render's shutdown window,
  and safely defers unfinished leases.

Do not create another binary, Render worker, generic workflow engine, or raw
event-sourcing framework.

## Observability

Emit a structured Earn-lane health record with at least:

- queued, leased, retryable, blocked, and oldest-job age;
- receipts claimed and completed;
- jobs completed, no-op, deferred, fenced, and failed;
- outcome counts for policy-only, deposit, `confirm_missed`, and
  `cleanup_pending`;
- RPC proof latency and canonical transaction latency;
- last successful progress timestamp.

Alert immediately on hard invariant failures, fenced-transition failures, and
worker termination. Alert on retryable no-progress only after a bounded elapsed
time; one transient RPC lag event must not page.

Keep existing producer telemetry for subscription filters, watch-set size,
reconnect/replay, receipt insertion, job wake-up, and replay cursor age.

## Verification Contract

Update `verification/smart-account-laserstream` so PASS requires the production
producer and the real fleet consumer. The isolated environment must use a
disposable PostgreSQL database and deterministic simulated Solana RPC/transaction
fixtures.

Required scenarios:

1. Policy-only route/setup updates produce canonical policy, managed-vault, and
   onboarding rows.
2. An invisible deposit produces exactly one deposit row, the correct principal,
   an active aggregate position, and one holding event.
3. A replay of the same deposit produces no duplicate deposit or holding event.
4. Two deposits coalesced into one vault job are both applied oldest first.
5. A policy signature followed by a later obligation signature in the same job
   does not lose the policy evidence.
6. `confirm_missed` cleanup closes and zeroes canonical state only after the
   slot-pinned zero proof and records the policy-close evidence.
7. `cleanup_pending` closes and zeroes canonical DB state while preserving the
   separate on-chain policy-refund responsibility.
8. Positive reserve, idle, or unknown token balances prevent cleanup and leave
   the receipts retryable with no zero snapshot.
9. An RPC context below `minContextSlot` is retryable and writes nothing.
10. A forced failure before the final commit leaves canonical rows, receipt
    completion, and job status unchanged.
11. A new update arriving during a lease fences the old attempt and is processed
    by a later attempt.
12. Earn fixtures create zero balance-sweep observations, events, executions,
    or lots.
13. Producer replay does not duplicate receipts/jobs and never lowers the
    LaserStream cursor.
14. Consumer restart reclaims an expired lease and converges to the same final
    database state.

Minimum focused commands after implementation:

```text
cargo fmt --check
cargo check -p loyal-yield-store
cargo check -p loyal-fleet-worker --bin same-mint-reserve-swap
cargo check -p balance-sweep-ata-monitor
bash verification/smart-account-laserstream/verify.sh \
  --routing-root <routing-worktree> \
  --app-root <app-worktree>
git diff --check
```

The verifier must assert database behavior. Source-string checks may guard
configuration shape, but they cannot substitute for running the producer,
consumer, proof fixtures, and canonical transactions.

## Implementation Sequence

### Phase 1: align the durable contract

- Refine migration 40 for receipt completion and the consumer state machine.
- Add typed lease/receipt/outcome types and store methods.
- Add store-level transactional tests for fencing, new-update races, partial
  receipt batches, and rollback.
- Update the verifier goal so the old Loyal App worker is no longer accepted.

### Phase 2: extract proof and decoding contracts

- Move reusable Squads policy decoding into `loyal-actions`.
- Add targeted confirmed-transaction and slot-pinned account proof helpers in
  the smallest suitable library module.
- Port the cron decision rules without copying cron scanning or HTTP concerns.

### Phase 3: implement canonical mutations

- Implement policy/onboarding convergence.
- Implement deposit, aggregate position, and holding-event convergence.
- Implement the complete cleanup transaction.
- Preserve existing unique keys, advisory locks, slot ordering, and idempotency
  rules shared with Loyal App.

### Phase 4: add the fleet lane

- Add independently bounded leasing and execution to
  `loyal-fleet-route-reconciler`.
- Add health telemetry, retry backoff, no-progress alerts, and shutdown
  behavior.
- Keep signed-route reconciliation behavior and capacity unchanged.

### Phase 5: make the end-to-end verifier pass

- Run all required cases through production producer and consumer code.
- Inject rollback, replay, fencing, RPC lag, positive balance, and restart
  failures.
- Prove exact canonical database outcomes and projector isolation.

### Phase 6: shadow and cut over

1. Build immutable `light-workers` and `laserstream-workers` images.
2. Apply the migration and deploy `loyal-fleet-route-reconciler` first.
3. Deploy `loyal-balance-sweep-ata-monitor-staging`, verify subscription and
   job drain, then deploy `loyal-balance-sweep-ata-monitor`.
4. Keep both Vercel crons enabled for at least one former ten-minute interval
   while comparing recovered outcomes and queue health.
5. Merge and deploy Loyal App PR #668 last.
6. Confirm both cron schedules/routes are gone and the routing backlog remains
   healthy.

Services to redeploy:

- `loyal-fleet-route-reconciler` (`light-workers` image);
- `loyal-balance-sweep-ata-monitor-staging` (`laserstream-workers` image);
- `loyal-balance-sweep-ata-monitor` (`laserstream-workers` image);
- Vercel `loyal-app`, only after routing is proven.

## Out of Scope

- A new Loyal App or Render worker.
- A transaction subscription in the balance-sweep monitor.
- Reserve-account fan-out.
- A generic blockchain event store or projection framework.
- Changes to balance-sweep lot accounting.
- Removing `loyal-squads-policy-monitor` before account-driven policy parity is
  separately proven.
- Redesigning the normal Loyal App deposit/withdraw confirmation paths.

## Done When

- The expanded isolated verifier passes every required scenario.
- All three cron recovery classes converge through the Rust consumer.
- No coalesced or replayed account update loses a transaction signature.
- Retryable and positive-balance proofs write no false zero state.
- Earn traffic has zero balance-sweep projector side effects.
- The fleet route reconciler retains its existing capacity and health behavior.
- Routing runs healthily through the shadow interval before PR #668 is merged.

