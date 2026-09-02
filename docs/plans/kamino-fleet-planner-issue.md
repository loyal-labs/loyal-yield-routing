# Fuse Kamino state ingestion with rebalance planning and remove the standalone planner hop

## Summary

The rebalance hot path currently crosses a database boundary between Kamino
market ingestion and opportunity planning. The Kamino monitor already has the
new reserve account state in-process, but the standalone fleet planner later
reconstructs the market snapshot from Timescale, joins it with fleet state from
Neon, decides what to move, and publishes work back to Neon for the execution
fleet.

Move market-state ownership and the decision loop into one Go process, keep a
slot-fenced confirmed protocol snapshot in memory, and trigger execution from a
durably persisted decision without reconstructing the same current market state
from the database on every update. Keep the database as the durable journal,
recovery source, audit history, and fencing mechanism rather than as the live
market-state message bus.

This issue should be described as removing the **standalone planning hop**, not
as deleting the production `loyal-fleet-route-reconciler`. The latter is a
different, downstream worker that reconciles confirmed transaction effects and
runs periodic fleet position sweeps; removing it without replacing those
responsibilities would break correctness.

## Meeting context

The meeting doodle showed:

```text
Kamino -> W1 -> DB -> W2 -> DB -> W3 -> move funds
                 notify/query

proposed:
Kamino -> W1 -----------------> W3 -> move funds
             \-> DB persistence
```

The discussion described W1 as the worker listening to Kamino, W2 as the worker
that consumes database updates and decides whether movement is needed, and W3
as the orchestrator that moves funds. The proposal was to have W1 retain the
current Kamino state in RAM, persist updates for durability/reporting, make the
decision from the in-memory state, and communicate the resulting work directly
to W3. It also suggested retaining associated account information in memory to
avoid repeated discovery RPCs.

The latency figures in the meeting were estimates, not measurements. The issue
must establish a production baseline before setting the final SLO.

## Mapping W1/W2/W3 to this repository

| Doodle | Current repository component | Responsibility |
| --- | --- | --- |
| W1 | `loyal-kamino-reserve-monitor` / `crates/kamino-reserve-monitor` | Consumes LaserStream reserve updates, decodes them, persists Timescale events, independently verifies dirty reserves at confirmed commitment, and advances the verified current-state projection. It already keeps a process-local `HashMap<Pubkey, ReserveSnapshot>`, but that map is currently used by ingestion/diffing and is not the fleet planner's authoritative cache. |
| W2 | `loyal-fleet-opportunity-planner` / `fleet-opportunity-planner` | Reads the verified Kamino market snapshot from Timescale and source/fleet state from Neon, constructs the immutable market epoch, ranks capacity-aware opportunities, and durably publishes optimizer epochs and rebalance opportunities. This is the middle decision hop crossed out in the doodle. |
| W3 | The execution side of the fleet, not one process: route revalidator, route executor, route confirmer, route reconciler, and ALT provisioner | Revalidates routes, builds/simulates/signs and durably queues exact wire bytes, broadcasts and confirms them asynchronously, reconciles exact effects, projects positions, and provisions required lookup tables. |

### Terminology warning

The meeting used uncertain terms such as “reconcile,” “projector,” and “middle
worker.” In the current code, the crossed-out W2 maps most closely to the
**opportunity planner**, not to `--fleet-reconciler`.

## Current production ordering

The deployed route is logically:

1. `loyal-kamino-reserve-monitor`
   - Receives LaserStream account updates.
   - Persists reserve events in Timescale.
   - Marks reserves dirty and performs an independent confirmed RPC refresh.
   - Advances `latest_verified_reserve_updates` only when the observed bytes and
     confirmation evidence agree.
2. `loyal-fleet-opportunity-planner`
   - Probes the compact Timescale market frontier every 5 seconds.
   - Runs an authoritative full sweep at least every 30 seconds, or immediately
     when a material frontier change is detected.
   - Uses Neon dirty-vault wakeups for scoped replanning, with durable polling as
     fallback.
   - On a planning run, reads market state from Timescale and fleet/source state
     from Neon, refreshes capacity, computes the optimizer wave, and persists an
     immutable optimizer epoch plus rebalance-opportunity rows.
3. `loyal-fleet-route-revalidator`
   - Claims `revalidate` opportunities from Neon.
   - Performs fresh route/account checks.
   - Production already has a partial fusion optimization: available permits
     use `RevalidateAndExecute`, avoiding a second revalidator-to-executor queue
     claim for those routes.
4. `loyal-fleet-route-executor`
   - Claims remaining `ready` opportunities.
   - Builds, simulates, signs, and persists exact signed submissions. It does not
     own asynchronous broadcast recovery.
5. `loyal-fleet-route-confirmer`
   - Atomically persists broadcast intent, broadcasts exact persisted bytes,
     observes signatures, and advances confirmed work to
     `reconciliation_pending`.
6. `loyal-fleet-route-reconciler`
   - Reconciles confirmed transaction effects.
   - Runs bounded periodic fleet-position sweeps so projections converge even
     without a recent route submission.
7. `loyal-route-lookup-table-provisioner`
   - Runs as a side lane for routes waiting on ALT capacity.

The current market-change path is therefore polling-based at the W1/W2
boundary. The planner does not consume the monitor's in-memory verified state;
it rebuilds the compact market snapshot through Timescale reads. Its Neon
`LISTEN/NOTIFY` channel primarily accelerates durable dirty-vault work, while
Kamino frontier changes are found by the 5-second market probe.

## Existing optimizations that must not be overlooked

This is not a greenfield rewrite:

- The Kamino monitor already keeps decoded reserve snapshots in memory.
- The planner already retains the latest optimizer epoch key and material market
  frontier in memory.
- Dirty-vault planning avoids an O(fleet) source scan when the global frontier
  can safely be reused.
- Notifications are latency hints; durable scans preserve recovery.
- Revalidation and execution are already fused opportunistically in the
  production revalidator.
- Confirmation and exact-effect reconciliation are intentionally asynchronous.

The change needs to remove the remaining process/query boundary without
regressing these properties.

## Required design preferences from the Backyard RWA Go implementation

Follow the implementation preferences documented in
`.agents/evidence/backyard-rwa-go-implementation-history.md`, not merely the
Backyard worker's final state machine:

- **Narrow first production scope.** Start with one explicitly approved route or
  cohort whose inputs, outputs, account layouts, and failure modes can be frozen
  and verified. Do not make the first Go cutover a general Earn Max rewrite.
  Expand route coverage only through separately reviewed and verified phases.
- **Verifier first.** Freeze executable invariants and adversarial scenarios
  before implementation. The verifier must try to falsify safety and recovery,
  not just demonstrate a happy path or compare log strings.
- **One concrete binary and package before abstractions.** Prefer one small Go
  binary with one internal package, direct `pgx` and Solana RPC integration, and
  exact code for the selected route scope. Avoid a broad interface hierarchy,
  general Solana SDK layer, or plugin framework until repeated production needs
  prove one is necessary.
- **Pure deterministic decisions.** Keep the decision ladder separate from I/O,
  explicit in priority order, and exhaustively fixture-tested. Invalid or
  incoherent state and unsupported layouts become durable holds or manual
  recovery, never best-effort execution.
- **Recovery before new work.** On every startup and loop iteration, resume or
  safely terminate durable nonterminal work before observing and admitting a
  new conflicting move.
- **Economic idempotency.** Derive idempotency from the economic observation,
  action, amount, reason, route/policy identity, and relevant version fences—not
  from the chain slot alone. An unchanged economic state observed at a later
  slot must not generate endless operations.
- **One coherent evidence boundary.** A candidate may be triggered from the
  in-memory cache, but construction must use a coherent confirmed snapshot. If
  execution evidence is refreshed, rerun the pure decision and require the
  action and amount to remain unchanged before durable admission.
- **Exact contracts fail closed.** Validate account owners, layouts, manifest
  and policy hashes, instruction envelopes, transaction size, signed wire
  shape, and expected return/effect evidence. Unknown or changed contracts must
  block the route.
- **Single-writer fencing.** Use a production owner identity bound to the Render
  service and immutable image SHA, validate owner/token/expiry on writes, cancel
  active work on lease loss, and support bounded rolling lease handoff.
- **Bounded reads, exactly one send.** Retry only classified transient read and
  snapshot-alignment failures. Persist exact signed bytes before broadcast,
  record broadcast intent first, send with no client retries, and recover
  ambiguous outcomes by persisted signature without blind resend.
- **Adversarial pivots are expected.** Packet-size, signer propagation, account
  closure, refresh-marker, oracle, concurrent-deposit, and receipt-shape
  assumptions must be measured or disproved. Narrow the design rather than
  weakening constraints when an assumption fails.

## Proposed target architecture

### 1. One owner for the live confirmed protocol snapshot

Create or extend a Go service that owns:

- the latest **confirmed and verified** Kamino reserve state, keyed by reserve;
- the current material market frontier and optimizer epoch inputs;
- the minimum fleet/vault source state required to make a decision;
- cached static or slowly changing associated-account identities used during
  route preparation; and
- the current in-flight operation identity needed to suppress conflicting work.

Every cache update must be monotonic and slot-fenced. Keep unverified
LaserStream observations separate from the confirmed decision snapshot. A raw
stream update may schedule confirmation, but it must not become executable
market evidence merely because it is newer.

### 2. Fuse W1 ingestion and W2 planning

After a confirmed reserve update advances the in-memory snapshot:

1. Apply/coalesce the update in memory.
2. Recompute only affected candidates when the existing global-frontier fences
   permit scoped planning; otherwise run a bounded full plan.
3. For the selected narrow route scope, collect the coherent confirmed evidence
   required for admission/construction, rerun the pure decision, and fail closed
   if the candidate changed.
4. Persist the market evidence, economic idempotency identity, capacity/conflict
   reservation, decision, and execution handoff atomically enough that recovery
   cannot observe an unbound decision.
5. Wake or invoke the execution owner immediately after durable publication.

There should no longer be a standalone service that polls Timescale to discover
a market update that the ingestion owner already processed. The initial Go
owner should implement only the fixed, verifier-backed route scope; unsupported
routes remain on the existing path until separately migrated.

### 3. Keep durable execution boundaries

“Database primarily for persistence” must not mean “execute from ephemeral
memory.” Before any transaction can be signed or sent, persist the immutable
planning evidence and operation identity. Continue to use durable state for:

- restart and failover recovery;
- leases and fencing tokens;
- one-active-operation/conflict enforcement;
- target-capacity reservations and fleet-wide admission;
- exact signed wire bytes, transaction signatures, and broadcast intent;
- confirmation and exact-effect receipts;
- audit history and client/reporting projections; and
- missed-notification recovery scans.

A direct W1-to-W3 signal is a latency optimization only. W3 must be able to
recover the same work from the durable journal if the signal is lost or either
process restarts.

### 4. Cache associated accounts without weakening pre-send safety

Cache deterministic and slowly changing account identities such as obligations,
farm accounts, and derived token accounts where doing so removes repeated
account-discovery RPCs. Invalidate or refresh the cache when:

- a relevant stream update advances its slot;
- this worker's confirmed transaction changes the expected state;
- an account is missing, closed, has an unexpected owner, or fails decoding;
- simulation/revalidation reports stale evidence; or
- the worker restarts or loses its lease.

Do not remove mandatory fresh policy/oracle reads, coherent confirmed
observation, transaction simulation, ownership checks, or stale-state fences
merely to reduce RPC count. The Backyard worker's history contains multiple
fixes in precisely these areas; “send first and investigate failure later” is
not an acceptable replacement for safety-critical validation.

## What to reuse from `go/backyard-rwa-worker`

`backyard-rwa-worker` is a useful process-design, ownership, and recovery
reference because it was deliberately implemented as a narrow verifier-first
production worker rather than as a general optimizer. One leased Go process
owns one route's serialized lifecycle:

1. Acquire a fenced database lease.
2. Resume any durable nonterminal operation before making a new decision.
3. Observe one coherent confirmed chain snapshot.
4. Run the pure deterministic decision ladder.
5. Refresh route-specific execution evidence, rerun the decision, and require it
   not to change.
6. Persist the decision before transaction work.
7. Build, simulate, sign, persist exact wire bytes, record broadcast intent,
   submit exactly once, confirm, and reconcile independently observed effects.

Its durable lifecycle is:

```text
decided -> built -> simulated -> signed -> broadcast_intent -> submitted
        -> confirmed -> reconciling -> reconciled
```

with `failed`, `manual_recovery`, and `held` alternatives.

Recovery reuses persisted signed bytes, searches by the persisted signature
after ambiguous sends, and never blindly rebuilds or resends an ambiguous
transaction. Durable holds are valid decisions, withdrawal work preempts safe
pre-broadcast open-loop work, and idempotency follows economic state rather than
a changing slot. Mutable bridge, NAV, Kamino, policy, ticket, and custody inputs
are observed as one aligned confirmed boundary; confirmed effects are then
checked independently from canonical token balances and exact adaptor evidence.

The implementation should also retain Backyard's deliberately concrete package
style: minimal process entry point, one internal package, direct database/RPC
calls, embedded and hashed route manifests, and only the exact wire/layout code
needed by the approved first scope.

### What not to copy literally

- The Backyard worker is serialized around one fixed route; the fleet needs
  concurrency, global capacity accounting, and per-account conflict fencing.
- It still reads its nonterminal operation from the database on every tick and
  reobserves from RPC for new decisions. It is not itself an implementation of
  the proposed long-lived market cache.
- Its lease and durable operation journal are reasons to retain critical DB
  coordination, not reasons to eliminate it.

Use its “single owner plus durable checkpoints” model, not its exact polling or
single-route topology.

## Safety invariants

The implementation must preserve or strengthen all of the following:

1. Only confirmed, coherently verified, non-stale protocol state can authorize
   execution.
2. State advances monotonically by slot/version; duplicate and out-of-order
   updates are idempotent.
3. Slot advancement without an economic change does not create a new operation;
   idempotency is bound to economic observation and route/policy identity.
4. A cache-triggered candidate is rechecked against one coherent confirmed
   execution snapshot, and a changed decision is discarded rather than built.
5. A decision is durably bound to its exact market epoch, source position,
   policy/manifest hash, capacity reservation, and idempotency identity before
   transaction work.
6. A restart resumes or safely terminates nonterminal work before admitting a
   new conflicting move.
7. Unsupported layouts, ownership, policy hashes, wire envelopes, or account
   states fail closed into hold/manual recovery.
8. Ambiguous submission recovery uses persisted wire bytes and signatures and
   never blindly resends.
9. Confirmation remains asynchronous; exact post-transaction effects are
   reconciled from immutable confirmed evidence before the operation is
   terminal.
10. Fleet-wide target capacity and writable-account conflicts cannot be enforced
    only in one process's RAM unless there is provably one fenced owner for the
    entire relevant scope.
11. Lost direct notifications do not lose work; durable catch-up is
    authoritative.
12. Cache loss, corruption, stale evidence, or lease loss fails closed and
    rehydrates from confirmed chain/durable evidence.
13. Withdrawal and safety work preempt new optimization wherever pre-broadcast
    cancellation remains safe.
14. Production health continues to expose stuck planning, signing, confirmation,
    and reconciliation stages immediately.

## Suggested implementation phases

1. **Instrument and baseline**
   - Measure update-to-confirmed-verification, verification-to-decision,
     decision-to-durable-handoff, handoff-to-sign, and sign-to-confirm latency.
   - Count Timescale/Neon reads and RPC calls per market update and per executed
     rebalance.
2. **Freeze a narrow verifier-first Go scope**
   - Select one safest route or bounded cohort; freeze its account layouts,
     policy/manifest identities, decision order, inputs, outputs, and explicit
     unsupported cases.
   - Write an executable verifier that tries to disprove coherent snapshots,
     economic idempotency, recovery-first ordering, lease fencing, exactly-one
     send, and exact-effect reconciliation before implementing the cutover.
   - Extract a pure deterministic decision function and build fixtures from the
     current Rust planner. Require field-level parity for the selected scope's
     optimizer epoch identity, eligibility, capacity selection, economics, and
     execution-plan inputs.
3. **Introduce the concrete confirmed in-memory state owner in shadow mode**
   - Prefer one Go binary and internal package with direct `pgx`/RPC integration;
     do not build a general worker framework as part of this issue.
   - Hydrate from durable state, then catch up from confirmed chain evidence.
   - Consume updates and compute decisions without publishing executable work.
   - Compare every result with the Rust planner and retain adversarial mismatch
     evidence.
4. **Publish durable decisions from the fused owner**
   - Preserve existing queue/fencing contracts initially so W3 remains
     unchanged.
   - Use direct wakeups only after durable publication.
5. **Cut over and remove W2**
   - Stop `loyal-fleet-opportunity-planner` only after parity, restart, and
     failover tests pass.
   - Keep a rollback switch that can return planning ownership to the Rust
     service without duplicate admission.
6. **Optionally fuse more of W3 later**
   - Evaluate further in-process ownership separately. Do not combine deleting
     the planner with deleting confirmer/reconciler safety boundaries unless a
     replacement proves all invariants.

## Implemented phase-one slice

The repository now contains `go/kamino-fleet-planner`, migration `0072`, and
`scripts/verify-kamino-fleet-planner-e2e.sh`. This slice is deliberately limited
to one configured vault and two distinct six-decimal USDC reserves in one
Kamino market. It polls confirmed RPC directly, owns only coherent two-reserve
snapshots in memory, plans deterministically, defaults to `shadow`, and in
explicit `publish` mode writes the existing PostgreSQL `revalidate` queue for
unchanged Rust W3 consumers. A separate heartbeat renews the fenced owner lease
even while observation or publication is running.

The disposable-PostgreSQL/RPC-stub verifier covers the frozen KLend layout,
identity and slot drift, capacity/economic no-ops, economic idempotency, lease
exclusion and stale-token rejection, restart watermarks, and confirmed RPC to
durable W3 handoff. It does not yet establish Rust/Go field-level parity from
production fixtures, consume the existing LaserStream W1 feed, choose a
production cohort, add Render deployment, measure production latency, or prove
all broader acceptance criteria below. Existing Rust services therefore remain
unchanged and deployed until those gates pass.

## Acceptance criteria

1. The production topology no longer deploys a standalone
   `loyal-fleet-opportunity-planner` for the migrated route scope.
2. A single fenced Go owner maintains the current confirmed Kamino decision
   snapshot in memory and reacts to an accepted update without waiting for the
   current 5-second Timescale market probe.
3. The hot path does not query Timescale merely to reconstruct market state that
   the same owner just confirmed. Database reads remain allowed for startup,
   catch-up, fleet state, durable admission, recovery, and auditing.
4. Every executable decision is durably persisted before W3 signs or sends, and
   a lost direct wakeup is recovered by a bounded durable scan.
5. The first production cutover is limited to an explicitly approved fixed
   route or bounded cohort. Its verifier proves parity for eligibility,
   optimizer/capacity selection, economics, fee budget, expiry, route plan,
   stale-market, durable-hold, and no-op behavior. Same-mint, cross-mint, and
   Voltr coverage expands only in separately verified phases.
6. Executable tests cover duplicate, out-of-order, missing, malformed, and stale
   stream updates; a later slot with unchanged economics; confirmed-verification
   lag; changed decisions after refreshed evidence; unsupported layouts/hashes;
   DB unavailability; process crash before and after decision persistence; lost
   wakeups; lease loss/handoff; and cache rehydration.
7. The verifier proves recovery-first ordering, exact persisted wire reuse,
   broadcast intent before exactly one send, ambiguous-send recovery,
   asynchronous confirmation, immutable exact-effect reconciliation, withdrawal
   preemption, and periodic position convergence.
8. Account caching measurably reduces repeated discovery RPCs without removing
   simulation or mandatory fresh policy/oracle/state checks.
9. Production evidence reports lower p50/p95 verification-to-durable-decision
   latency and fewer database/RPC round trips than the baseline. Final numeric
   thresholds are set from phase-1 measurements rather than the meeting's
   estimates.
10. Render worker health and stuck-stage alerting are updated for the new owner,
    and the rollback procedure is documented and exercised.

## Non-goals

- Deleting Timescale/Neon persistence or audit history.
- Treating unverified LaserStream state as confirmed execution evidence.
- Removing durable queue recovery, leases, capacity reservations, or conflict
  fences.
- Removing transaction simulation and safety-critical fresh reads.
- Deleting `loyal-fleet-route-reconciler` without implementing equivalent
  confirmed-effect reconciliation and periodic position convergence.
- Requiring the client-facing realtime service to move in the same cutover.
- Building a general Go orchestration framework, broad Solana SDK abstraction,
  or all-route optimizer before the first fixed route/cohort is live and
  verified.
- Using chain slot alone as operation idempotency.

## Open design questions

1. What is the narrowest economically useful and safest first route/cohort for
   the verifier-first Go cutover?
2. Is the first cutover only W1+W2 fusion while retaining the current durable W3
   queue, or should one concrete Go process own planning through signed handoff?
3. Which fleet/vault source fields can be maintained safely from streams, and
   which must still be read from Neon or confirmed RPC at decision time?
4. Is there one active owner for the complete fleet, or is state partitioned by
   vault/reserve? The answer determines where capacity and conflict fences must
   remain durable.
5. What is the authoritative startup hydration source for each cache category,
   and how is the hydration slot fenced against concurrent stream updates?
6. Which direct W1-to-W3 mechanism is justified? In-process invocation is
   simplest when fused; an inter-process RPC adds availability and replay
   complexity and must remain subordinate to the durable journal.
7. Does the existing Rust planner remain as a read-only verifier after cutover,
   and for how long?

## Relevant code and deployment references

- `.agents/evidence/backyard-rwa-go-implementation-history.md`
- `render.yaml`
- `crates/kamino-reserve-monitor/src/main.rs`
- `crates/loyal-yield-router/src/timescale/mod.rs`
- `crates/loyal-yield-orchestrator/src/bin/fleet-opportunity-planner.rs`
- `crates/loyal-yield-orchestrator/src/fleet_orchestration/observation.rs`
- `crates/loyal-fleet-worker/src/lib.rs`
- `crates/loyal-yield-orchestrator/src/bin/fleet-route-confirmer.rs`
- `crates/loyal-yield-store/src/fleet_orchestration/queue.rs`
- `go/backyard-rwa-worker/internal/backyardrwa/worker.go`
- `go/backyard-rwa-worker/internal/backyardrwa/lifecycle.go`
- `go/backyard-rwa-worker/internal/backyardrwa/store.go`
