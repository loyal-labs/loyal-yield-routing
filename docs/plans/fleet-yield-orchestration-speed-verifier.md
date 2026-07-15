# Fleet Yield Orchestration Speed Verifier

Use this document as the fixed PASS/FAIL verifier for replacing the serial
same-mint fleet monitor with a durable, economically prioritized orchestration
pipeline. It verifies observable behavior and performance, not whether a
particular implementation checklist was followed.

Do not mark this verifier PASS because a migration exists, workers compile, or
more tasks run concurrently. PASS means valuable funds are discovered and
moved quickly; ALT-cold work cannot delay ALT-ready work; Solana account
conflicts, retries, and fees remain bounded; and the system produces enough
evidence to show immediately when any of those properties regress.

The implementation priority is short feedback loops, throughput, and economic
value. Follow `AGENTS.md`: outside the smart-account proof surface, prefer
targeted `cargo check`, dry runs, database/RPC evidence, and existing E2E paths
over broad new Rust tests or source-string assertions.

## Required End State

The deployed architecture is:

```text
market/balance/policy event
          -> immutable optimizer epoch
          -> durable value-ranked opportunity
             -> ALT covered -> ready queue
             -> ALT missing -> waiting_alt -> priority provisioner
                                      coverage_ready outbox -> ready queue
          -> conflict-aware build/simulate/sign/send workers
          -> asynchronous confirmer
          -> monotonic slot-fenced position projection
```

The serial `vault id -> reconcile child -> route child -> next vault` loop is
not an acceptable production architecture.

Cutover is direct, not a slow per-vault canary or dual-execution migration.
Once the additive schema, immutable image, reusable-v2 ALT path, and six worker
commands pass this verifier, production replaces the executing serial monitor
as one coordinated change. The old and new execution paths must never move the
same fleet concurrently.

## Ideal Implementation Contract

### 1. Versioned observation and planning

- Market state is read once per immutable optimizer epoch. Every opportunity
  records the market epoch, source position snapshot/slot, expiry, source and
  target APY, raw amount, normalized USD notional, expected holding horizon,
  and capacity-adjusted net edge.
- Balance, market, policy, cooldown-expiry, and `coverage_ready` events wake the
  affected vault/cohort. A short recovery poll may remain, but correctness and
  latency do not depend on finishing a fleet scan. PostgreSQL notifications are
  hints only: each listener reconnects and immediately scans the durable queue.
- Scoped dirty-cohort planning may reuse a full-sweep frontier only when that
  frontier was complete, its immutable market epoch remains fresh, and its
  material economic frontier plus durable target-telemetry version still match.
  New observation timestamps or slots that do not cross a material APY,
  confidence, availability, or capacity threshold must not turn every dirty
  event into another fleet scan. A material frontier change, deferred frontier,
  oversized cohort, target-telemetry mismatch, or contention falls back to an
  authoritative full sweep.
- Shared reserve/market accounts are cached per epoch. Vault accounts are
  deduplicated and batch-read. Concurrent cache misses are singleflight/batched
  by epoch and `minContextSlot` so one worker wave does not stampede RPC.
  Normal discovery does not spawn another process or synchronously reconcile
  every vault from RPC.
- Stale epochs/opportunities are superseded before decision creation. Older
  observed slots cannot overwrite newer projected state.

### 2. Economic scheduling

Persist the inputs to, and compute the equivalent of:

```text
lost_yield_usd_per_hour =
  notional_usd * max(capacity_adjusted_net_edge_bps, 0) / 10_000 / 8_760

execution_priority =
  lost_yield_usd_per_hour * confidence / expected_service_seconds + age_boost
```

- Idle funds use source APY zero. Larger deposits are naturally prioritized
  when edges are comparable, but balance alone is not the scheduler.
- A route is eligible only when expected gain over its holding horizon exceeds
  transaction/priority fees, rounding loss, and configured safety margin.
  Economically pointless dust movements are rejected or left in a low-cost
  maintenance lane.
- Signed/submitted/ambiguous transactions awaiting reconciliation outrank new
  economic work. Tenant fairness and an age boost prevent starvation.
- Target selection is fleet- and capacity-aware. Dispatch happens in bounded
  waves; expected reserve state is updated after admitted flow and refreshed
  from chain between waves. The system does not blindly send the entire fleet
  to the current point-estimate peak.
- Target capacity is a durable, versioned admission resource. Promotion to an
  executable decision atomically reserves capacity against current target
  supply plus every active or recently landed inflow reservation. A successful
  reservation remains charged after reconciliation until target telemetry has
  crossed the confirmed movement slot; an authoritative pre-send terminal
  failure releases it. Stale fences cannot release or overwrite a newer
  reservation.
- Telemetry freshness and reservation generation are separate fences. A short
  target-local admission lock rechecks the exact supply/slot telemetry, current
  committed inflow, marginal APY, and fee economics. A sibling reservation may
  consume headroom but must not invalidate every transaction built from the
  same fresh telemetry; parallel contenders admit until the economic/capacity
  ceiling is actually reached.
- A bounded publish wave with deferred contenders schedules the next
  authoritative full sweep after one recovery poll, not the normal full-sweep
  interval. Full sweeps count every active opportunity—including
  `waiting_alt`—as already covered so cold work cannot consume each drain wave
  repeatedly. Scoped dirty/coverage passes may replace only pre-execution
  states and retain fleet-wide committed target inflow.

### 3. Durable opportunity queue

- Add a durable opportunity/job table distinct from `rebalance_decisions`,
  with explicit `waiting_alt`, `ready`, leased/in-flight, terminal, stale, and
  superseded states; lease expiry; incrementing fencing token; idempotency key;
  priority inputs; and exact snapshot/manifest evidence.
- `rebalance_decisions` remains the movement audit and one-active-movement lock.
  ALT-missing work never creates a decision. Promotion from ready opportunity
  to decision is atomic and revalidates current policy, balance, cooldown,
  market epoch, target capacity, and exact ALT coverage.
- Consumers claim work with `FOR UPDATE SKIP LOCKED`. Crash recovery cannot
  lose a job or execute the same economic action twice.
- Opportunity attempts are immutable. After a terminal outcome has proven the
  transaction had no effect, concurrent rediscovery may create exactly one next
  attempt generation with bounded retry/backoff. It must never reopen a landed
  success or a submitted/ambiguous attempt, and the prior attempt remains an
  audit row.

### 4. ALT-independent fast lane

- A route is runnable whenever its exact required addresses are within verified
  active ALT prefixes. Extending a later suffix never disables the usable
  prefix or unrelated tables.
- Opportunity-to-provisioning-request links carry current economic weight.
  Requests and operations are ordered by aggregate yield unlocked per remaining
  critical-path mutation, after already-broadcast reconciliation work.
- Normal readiness and usage writes do not take the cluster-wide
  `reusable-alt-rollout:<cluster>` advisory lock. That lock is reserved for
  pause/cutover/catalog-publication operations. Allocation uses canonical
  family/lane/table locks and optimistic epoch checks.
- Mutations remain serial per physical ALT, fenced and idempotent, while
  different ALTs can be planned and extended concurrently. The existing
  best-fit packed-vault policy remains; concurrency must not regress packing or
  create exact-route/legacy ALTs.
- Shared catalog growth has active and staging revisions. Active coverage keeps
  routing while staging warms; only staging-only routes wait.
- `POLICY_KEYPAIR` remains the reusable-v2 ALT authority and payer.
- Coverage completion commits a durable outbox row and may emit `pg_notify` as
  a low-latency hint. Workers scan durable state after LISTEN startup and retain
  a short recovery poll; notification loss cannot lose work.

### 5. Conflict-aware execution and price

- Route building/execution is reusable in-process logic used by persistent
  workers; the production fleet path does not call `Command::output()` per
  vault. RPC and database clients/pools are long-lived.
- A revalidator with an immediately available execution permit may atomically
  promote its live fence and continue with the same freshly built, ALT-checked,
  finally simulated transaction. If a semantic conflict or permit blocks that
  promotion, it publishes durable `ready` and discards the prepared bytes;
  queued work is rebuilt rather than using an aging blockhash or snapshot.
- The exact writable-account set is persisted before dispatch. At most one
  execution is in flight per vault; independent writable sets execute in
  parallel; overlapping reserve/market/fee-payer sets use bounded lanes.
- Do not turn the common `POLICY_KEYPAIR` fee payer or peak Kamino reserve into
  a fleet-wide exclusive lock. Persist full writable pubkeys as evidence, hold
  a vault-specific semantic lock for correctness, and assign every route to
  one of 64 durable shared-write lanes. This caps horizontal concurrency while
  allowing many independent vaults to progress through confirmation.
- Those 64 lanes are an admission/confirmation bound, not evidence that the
  transactions are physically independent. The scheduler and metrics must
  expose actual shared writable keys: one common route fee payer or one peak
  reserve remains an on-chain serialization ceiling even when database lanes
  differ.
- Keep `POLICY_KEYPAIR` as the delegated policy signer. If route fee-payer
  sharding is enabled, fee payers are low-balance fee-only keys with no vault or
  ALT authority, explicit budgets, and deterministic shard assignment.
  `POLICY_KEYPAIR` remains the reusable ALT authority/payer; a route fee-payer
  pool does not replace its policy signature.
- Mount signing material only where it is used. The planner uses the standard
  public policy identity, and the confirmer/reconciler consume already-signed
  evidence; none of those roles loads a private key. Revalidator/executor roles
  may load POLICY and fee-only route shards for fused execution, while the ALT
  provisioner loads POLICY only and never receives route-shard keys.
- The fee-only pool is opt-in twice: public keys and durable limits live in
  `loyal_yield.route_fee_payer_shards`, while matching keypairs are mounted from
  `YIELD_ROUTE_FEE_PAYER_KEYPAIRS` through the standard 1Password environment.
  Missing, malformed, disabled, role-conflicting, over-budget, or out-of-range
  configuration falls through ranked rendezvous candidates and finally to
  `POLICY_KEYPAIR`; key material never enters SQL, status output, or logs.
- A shard is eligible only for a queue-backed same-mint move whose source and
  target obligations and collateral-farm user states already exist. Route
  construction rejects the shard again if it discovers obligation/farm setup.
  Idle deposits, ATA/rent top-ups, setup transactions, ALT creation/extension,
  and any other account-creation work remain funded by `POLICY_KEYPAIR`.
- The exact compiled fee and a fresh shard balance observation are admitted in
  the same SQL transaction as immutable signed bytes. A per-key row lock,
  balance floor/ceiling, per-transaction cap, and rolling spend reservation
  make concurrent budget admission deterministic. Reservation races leave the
  opportunity retryable. The payer selected during revalidation is durably
  bound to its canonical manifest fingerprint; if it becomes unhealthy, the
  opportunity returns to the short revalidation lane before a fresh ranked
  candidate or POLICY fallback publishes a new matching fingerprint. Budget
  races never create a decision-less signed route or reuse a mismatched
  manifest.
- Reciprocal database triggers reject a fee-only key that is already a policy,
  delegated signer, vault key, reusable ALT family authority/payer, or physical
  ALT authority/payer, and reject later assignment of those roles to a shard.
  `fleet_worker_healthy.feePayerSharding` reports mounted/configured counts,
  role conflicts, and exact POLICY-vs-reusable-ALT authority mismatch counts.
- Compute limits come from measured route-class consumption plus bounded
  margin. Priority fees use the actual writable account set, economic tiers,
  bounded escalation, and a cap relative to expected incremental yield.
- A final transaction is recompiled and re-simulated after compute/priority
  instructions are added. Persist its compiled fee and reject it if that fee
  exceeds the durable opportunity's economic cost cap.
- Independent vault routes remain independently retryable transactions. Do not
  use multi-vault atomic transactions or Jito bundles as the fleet primitive.

### 6. Durable send, confirmation, and projection

- Before first broadcast, persist semantic operation identity, exact signed
  bytes, signature, blockhash, last-valid block height, market/snapshot/ALT
  epochs, fee payer, and executor fence.
- Retries rebroadcast identical bytes while the blockhash is valid. A newly
  signed replacement is forbidden until expiry and absence/effect checks prove
  it safe.
- `sendTransaction` acceptance does not occupy the executor until confirmation.
  Confirmation uses subscription plus batched status fallback.
- The signed handoff and first confirmer transition are set-based/atomic rather
  than a sequence of per-state database round trips. The confirmer may drain a
  backlog continuously; it waits only when no work is claimable.
- Post-confirm reads require `minContextSlot >= confirmed transaction slot`.
  Stale RPC data produces `confirmed_reconciliation_pending`, never a failed
  movement or duplicate send. Finalization/accounting proceeds asynchronously.

### 7. Multi-tenant scale and observability

- Workers scale horizontally by queue partition/lease rather than duplicate
  fleet scans. Tenant, risk, mint, and writable-conflict dimensions are visible
  and enforce independent quotas/budgets/fairness.
- Metrics expose, at minimum: opportunity discovery latency; queue age and
  dollars/hour by state; ALT-blocked notional and yield; time to unlock 50/90/99%
  of recoverable yield; executor/confirmation latency; writable-key conflicts;
  RPC lag/429s; blockhash expiry; stale-read retries; duplicate-prevention
  outcomes; ALT operations per unlocked dollar; and fee per incremental-yield
  dollar.
- Every worker emits a compact health/status snapshot with the durable recovery
  poll and actual health-observation interval reported separately. A regression
  is visible within one declared health-observation interval (currently one
  second); the 250ms recovery poll is not mislabeled as the emission cadence.
- Opportunity state transitions persist `state_entered_at` plus stage-specific
  timestamps such as `ready_at` and `waiting_alt_at`. Queue health reports the
  age of the current state, not merely the age of an old opportunity that has
  just become runnable.

## Required PASS/FAIL Checks

### Check 1: Repository and migration integrity

PASS only if:

- additive migrations are registered in the dedicated migration runner and
  apply/check idempotently in an isolated database;
- no plaintext secret or `.env` file is added;
- changed and untracked intended files pass trailing-whitespace inspection,
  including files that `git diff --check` cannot see;
- a non-printing high-confidence credential scan reports no private-key PEM,
  credential URL, or known live-token pattern in changed files;
- the intended diff contains no new legacy/exact-route ALT creation path;
- `cargo fmt --check`, `cargo check -p loyal-yield-orchestrator --bins`, and
  `git diff --check` pass.

Do not require a broad workspace test run.

### Check 2: Fast complete discovery

Run the production planner in non-mutating benchmark/dry-run mode against a
captured/live read-only fleet snapshot. PASS only if its machine-readable output
proves:

- every eligible current vault was considered from one non-expired epoch;
- completeness starts from the authoritative eligible-vault denominator and
  assigns every vault exactly one mutually exclusive outcome: observed
  opportunity, active queue/decision state, no positive current source,
  missing valuation, unsupported amount/market semantics, or no economic
  target. The outcome total must equal the denominator, and active outcomes
  must agree with the queue-state breakdown;
- current-fleet planning p95 is under 5 seconds;
- a 10,000-vault in-memory/captured replay completes under 10 seconds;
- output is ordered by economic priority, not vault ID;
- the reported top-value cohort contains no lower-priority job ahead of a
  higher-priority non-conflicting job;
- discovery spawns zero child route/reconcile processes.

Record hardware, fleet size, epoch, and timings with the verdict.

### Check 3: Economic behavior

Using deterministic planner inputs, PASS only if:

- increasing notional at equal edge increases priority;
- increasing net edge at equal notional increases priority;
- a smaller account with greater lost-yield rate can outrank a larger account;
- age eventually prevents starvation;
- cost/holding-horizon gating rejects negative-value and dust movements;
- capacity-aware waves stop admitting a target after marginal edge disappears.
- concurrent target admissions cannot exceed remaining capacity, and harmless
  telemetry churn keeps dirty-vault planning scoped while a material frontier
  change forces a full sweep. Reservation-generation churn alone does not.
- several same-target contenders built from one telemetry snapshot can admit
  concurrently up to the real economic ceiling; a reservation-generation
  change alone does not force every sibling to rebuild.

Prefer a verifier/dry-run fixture or SQL-backed isolated check over new unit
tests that merely restate fields.

### Check 4: ALT head-of-line isolation

Against an isolated migrated database, create ready and ALT-cold opportunities
with the same economic distribution. PASS only if:

- ready workers continue claiming all ready work while cold work remains
  `waiting_alt`;
- adding 10,000 ALT-cold jobs changes ready-claim p95 by less than 5%;
- no decision exists for `waiting_alt` work;
- satisfying coverage writes a durable wakeup and makes only affected valid jobs
  eligible immediately, without another fleet cycle;
- normal readiness writes do not acquire the global rollout lock;
- two different physical ALT lanes can progress concurrently while same-table
  mutation predecessors remain serialized and fenced.

### Check 5: Execution concurrency and crash safety

PASS only if operational verifier output proves:

- no two active jobs/decisions exist for one vault;
- non-overlapping writable sets can be leased concurrently;
- overlapping writable sets obey their lane limit;
- a killed worker's expired lease is reclaimed with a higher fence;
- common fee-payer/peak-reserve traffic occupies bounded shared-write lanes
  rather than one fleet-wide exclusive lease;
- an already persisted signed transaction is rebroadcast byte-for-byte;
- an ambiguous or stale post-confirm read cannot create a replacement movement;
- target-capacity reservations survive reconciliation until target telemetry
  crosses the landed slot, release on authoritative pre-send failure, and
  reject stale reservation fences;
- proven no-effect terminals can create exactly one next immutable attempt,
  while success and ambiguous sends cannot; shared reserve cache misses are
  singleflight rather than one RPC request per concurrent route;
- persisted fee-payer kind is immutable, and an authoritative landed failure
  retains its confirmation slot/time for coherent fee-floor accounting;
- standard `POLICY_KEYPAIR` signs policy execution and ALT mutations, while any
  distinct route fee payer has fee-only authority and budget evidence.
- every sharded route is recompiled with the shard as fee payer and
  `POLICY_KEYPAIR` as a second static signer; its final manifest, ALT coverage,
  packet size, simulation, compiled fee, and persisted hashes all describe
  that exact final transaction rather than an earlier POLICY-payer build;
- setup/idle/farm-init fixtures select POLICY, and a mature-route shard fixture
  proves exact registry/keypair matching, reciprocal authority separation,
  bounded ranked failover, low-balance limits, and one atomic immutable spend
  reservation.

### Check 6: Performance, value, and price

PASS only if a production-like replay reports:

- warm high-value opportunity: p95 submitted within 10 seconds of discovery;
- warm confirmed route: p95 confirmed within 30 seconds of discovery, excluding
  an explicitly recorded cluster outage;
- ALT backlog has less than 5% effect on warm-route p95;
- at least 90% of recoverable yield dollars/hour is submitted within 2 minutes
  and 99% within 10 minutes, subject to explicit capacity/conflict ceilings;
- fee and priority-fee spend stays below the configured fraction of expected
  incremental yield; negative-value routes are zero;
- database deadlocks and duplicate movements are zero.

### Check 7: Production wiring and short feedback loop

The required durable service roles are six distinct workers: planner,
revalidator, executor, confirmer, reconciler, and priority ALT provisioner.
The light-worker image must contain every owning binary, and the pinned Render
Blueprint must use all six rather than the serial fleet monitor for production
execution. Status queries identify a stuck market epoch, ready queue, ALT queue,
sender, confirmer, or reconciler immediately; emitted output must identify it
within one separately declared health-observation interval. The durable recovery
poll and health-observation cadence must not be conflated.

The same health snapshot must expose a bounded top set of physically shared
writable keys across active submissions, including active route count and
economic value plus fee-payer/target/other classification. Sixty-four semantic
lanes without this physical congestion evidence are insufficient.

Every PostgreSQL notification listener must reconnect with bounded backoff
after a transient failure and immediately scan durable work. Listener loss may
increase latency to the recovery poll temporarily, but it must not permanently
degrade a healthy process to polling-only mode. Outer worker-task failures and
join panics must be visible in redacted health/log evidence and release or defer
their fenced leases when safe.

The Blueprint must be pinned to a real immutable image SHA containing the new
binaries before its commands are changed. A Blueprint that still runs the
serial monitor, or commands that name binaries absent from the pinned image,
is FAIL—not a partially complete deployment.

Blueprint PASS also requires least-privilege env wiring: planner, confirmer,
and reconciler omit every signer secret; revalidator and executor receive the
standard POLICY signer plus the optional fee-only shard pool; and the ALT
provisioner receives POLICY plus an explicit rolling lamport budget but never
the route-shard pool. Recovery polls and concurrency/batch bounds must be
explicit in the commands so the feedback-loop and spend posture are reviewable
without relying on hidden binary defaults.

Actual deployment and fund movement are separate operator actions. Until run,
record `PRODUCTION PERFORMANCE: NOT RUN`; do not fabricate a PASS from local
evidence.

## Evidence Commands

Use the smallest relevant set and record exact output/commit:

```sh
git status --short --branch
git diff --check
cargo fmt --check
cargo check -p loyal-yield-orchestrator --bins
bun run yield:migrate:check

# New implementation-owned commands; names may vary only if the verifier is
# updated explicitly before implementation begins.
cargo run -p loyal-yield-orchestrator --bin fleet-opportunity-planner -- \
  --once --dry-run --benchmark --json
cargo run -p loyal-yield-orchestrator --bin fleet-orchestration-verifier -- \
  --implementation --json --collect-repository-evidence

# A complete implementation verdict also consumes a fresh source-bound v1
# artifact from the controlled planner/ALT/RPC/replay harnesses.
cargo run -p loyal-yield-orchestrator --bin fleet-orchestration-verifier -- \
  --implementation --json --collect-repository-evidence \
  --runtime-evidence-json <PATH> \
  --isolated-database --database-url <ISOLATED_FLEET_VERIFY_URL>
```

### Runtime evidence schema v1

`--runtime-evidence-json` accepts measurements, never caller-supplied verdict
strings. The verifier rejects unknown fields, any `schemaVersion` other than 1,
an artifact older than one hour, a different checkout HEAD, or a different
SHA-256 digest of the current runtime inputs. Obtain `headCommit` and
`runtimeSourceDigestSha256` from the verifier's repository-evidence output.

The camel-case JSON object contains:

```text
schemaVersion, headCommit, runtimeSourceDigestSha256, capturedAt, hardware

discovery:
  fleetSize, eligibleCurrentVaults, accountedVaults,
  vaultOutcomesByReason, activeExclusionsByState, optimizerEpochId,
  epochExpiresAt, oneImmutableEpoch, planningSampleCount,
  planningP95Milliseconds, replayVaultCount, replayMilliseconds,
  economicallyOrdered, topCohortHasNoNonconflictingPriorityInversion,
  childRouteOrReconcileProcessesSpawned

alt:
  typedProvisionerDryRunPlans, reusableV2Plans,
  legacyOrExactRouteAltPlans, readyJobsSeeded, readyJobsClaimed, waitingAltJobs,
  waitingAltDecisions, readyClaimBaselineP95Micros,
  readyClaimColdP95Micros, durableCoverageWakeupRows,
  affectedJobsPromoted, unaffectedJobsPromoted,
  additionalFleetCycleRequired,
  normalReadinessGlobalRolloutLockAcquisitions,
  independentPhysicalAltLanesProgressed,
  sameTablePredecessorViolations, staleFenceCommits

execution:
  duplicateActiveVaultMovements, nonoverlappingConcurrentLeases,
  overlappingLaneLimitViolations, physicalWritableKeyCongestionVisible,
  expiredLeaseReclaimedWithHigherFence,
  fleetWideExclusiveRouteLeases, identicalByteRebroadcastAttempts,
  rebroadcastByteMismatches, replacementBeforeExpiryAndAbsenceProof,
  ambiguousOrStaleReplacementMovements, postConfirmReads,
  minContextSlotViolations, policyExecutionSignedByPolicyKeypair,
  altMutationsAuthorizedAndPaidByPolicyKeypair, shardedRouteFixtures,
  shardIsFinalFeePayer, policyIsSecondStaticSigner,
  finalManifestAndAltCoverageMatch,
  finalPacketSimulationFeeAndHashesMatch,
  setupIdleAndFarmInitUsePolicyPayer, shardRegistryKeypairMatch,
  reciprocalAuthoritySeparation, boundedRankedFailover,
  lowBalanceLimitsEnforced, atomicImmutableSpendReservation

replay:
  routeSampleCount, warmHighValueSubmissionP95Milliseconds,
  warmConfirmationP95Milliseconds, explicitlyExcludedClusterOutages,
  warmBaselineP95Milliseconds, warmWithAltBacklogP95Milliseconds,
  recoverableYieldUsdMicrosPerHour,
  submittedWithinTwoMinutesYieldPpm,
  submittedWithinTenMinutesYieldPpm,
  configuredMaxFeeFractionPpm, observedMaxFeeFractionPpm,
  negativeValueRoutes, databaseDeadlocks, duplicateMovements

wiring:
  probedContainerImageReference, localContainerImageId,
  runnableRoleProbeExitCodes,
  recoveryPollIntervalMilliseconds, stuckStageDetectionMilliseconds
```

The verifier recomputes completeness totals, backlog effects, and every numeric
threshold from these measurements. The wiring maps must contain exactly the six
durable roles and the six stuck stages named in Check 7; every local-container
probe must exit zero and every stuck stage must be detected within the recorded
recovery-poll interval. The probed image reference must exactly equal the one
immutable GHCR `light-workers:sha-<commit>` reference shared by all six
production Blueprint roles; probing an unrelated local image is FAIL. A
complete source-bound artifact can move Checks 2, 4,
5, 6, and the runtime portion of 7 to PASS; absence leaves them `NOT RUN`, and
invalid or threshold-breaking measurements produce `FAIL`.

Secret-backed read-only checks must use:

```sh
op run --env-file=.env.1password -- sh -c '<command>'
```

Do not print URLs, key material, signed transaction bytes, or environment
values. Production writes, Render changes, and transaction sends require their
own explicit authorization.

## Verdict Format

Report every required subcheck as `PASS`, `FAIL`, or `NOT RUN`, with exact
evidence and the first blocking invariant. Tag each subcheck with one scope:

- `IMPLEMENTATION`: local, isolated-DB, controlled-RPC/local-validator,
  captured/read-only fleet, or local-container evidence;
- `DEPLOYMENT`: real immutable image, live Render services/commands, applied
  production migrations, worker health, and serial-worker shutdown;
- `PRODUCTION PERFORMANCE`: real deployed fund-movement SLO evidence.

Aggregate each scope independently: any required `FAIL` makes the scope
`FAIL`; otherwise any required `NOT RUN` makes it `NOT RUN`; only all required
`PASS` evidence makes it `PASS`. A missing check is never treated as passing.
`END STATE: PASS` requires all three scopes to pass; it is `FAIL` if any scope
fails and otherwise `NOT RUN`.

Checks 1-5 are implementation gates. Check 6 requires a controlled
production-like replay for implementation and repeats the same SLOs against
real movements for production performance. Check 7 requires source-bound
local-container binary probes, structured six-service Blueprint validation,
and functional status fixtures for implementation; registry presence and live
Render/DB state are deployment evidence and never a hardcoded implementation
`NOT RUN`. `--implementation` succeeds only when every
required implementation subcheck passes, regardless of deployment or
production-performance `NOT RUN` state, and must not claim `END STATE: PASS`.

Do not weaken a failed condition to match the implementation. If a condition is
found to encode the wrong product or safety goal, record the correction and
reason in this document before changing implementation.

## Latest Evidence Run: 2026-07-15

This is the literal current verdict, not a deployment claim:

| Check | Verdict | Evidence / first missing invariant |
| --- | --- | --- |
| 1. Repository and migration integrity | PASS | The dedicated runner applied and checked migrations 1-27 in a fresh PostgreSQL database. The source-aware verifier passed the migration ledger, rolled-back reapplication, changed/untracked whitespace, non-printing credential scan, `cargo fmt --all -- --check`, orchestrator all-bin check, router check, and `git diff --check`. |
| 2. Fast complete discovery | NOT RUN | Seven deterministic 10,000-vault planning rounds completed at 59.525ms p95, remained economically ordered, and spawned zero child processes. A fresh source-bound artifact still must prove live current-fleet completeness, epoch freshness, top-cohort conflict ordering, and live planning p95. |
| 3. Economic behavior | PASS | Deterministic notional, edge, lost-yield, starvation, dust/cost, fee-cap, material-frontier, and concurrent target-capacity invariants passed. Three simultaneous $100 admissions against $250 of headroom admitted two and rejected only the excess contender without a telemetry-fence false rejection. |
| 4. ALT head-of-line isolation | NOT RUN | Every isolated database subcheck passed. With 4,096 ready jobs and 10,000 `waiting_alt` jobs, PostgreSQL statement p95 was 2.933ms baseline versus 2.722ms cold (0ppm regression); all ready work drained, cold work remained untouched, catalog predicates excluded cold/active-lease rows, and runnable index reads stayed below the derived MVCC self-churn ceiling. Source-bound reusable-v2 provisioner and independent physical-ALT lane evidence is still uncollected. |
| 5. Execution concurrency and crash safety | NOT RUN | Every isolated database subcheck passed, including mixed runnable/expired `SKIP LOCKED` claims, 64 semantic lanes, physical writable congestion, fenced reclaim, exact signed handoff, immutable retries, capacity release fences, and zero deadlocks. Controlled RPC evidence for identical-byte rebroadcast, final sharded transaction identity, real standard `POLICY_KEYPAIR` signatures, and slot-fenced reconciliation remains uncollected. |
| 6. Performance, value, and price | NOT RUN | No production-like send/confirm/yield-unlock/fee replay was authorized or run. |
| 7. Production wiring and feedback loop | FAIL | The light-worker recipe contains all owning binaries and the functional health/reconnect fixtures pass, but `render.yaml` still runs the serial five-minute executing monitor and declares none of the six durable roles. A locally built immutable candidate image, six least-privilege service declarations, serial execution removal, and bound role probes are required. |

Current scoped result: `IMPLEMENTATION: FAIL`; `DEPLOYMENT: NOT RUN`;
`PRODUCTION PERFORMANCE: NOT RUN`; `END STATE: FAIL`.
Deployment remains `NOT RUN` in the executable verifier because registry,
Render, and production database observations are deliberately outside local
implementation evidence. No production migration, deployment, transaction
send, or fund movement occurred in this run.

## Standing Codex Goal (under 4,000 characters)

```text
Run docs/plans/fleet-yield-orchestration-speed-verifier.md adversarially against the current loyal-yield-routing checkout and make IMPLEMENTATION: PASS without weakening it. Implement the durable fleet optimizer described there, then rerun the verifier literally after each material slice.

Required end state: replace production dependence on the serial vault-id monitor with versioned market epochs, a durable economically ranked rebalance-opportunity queue, ALT-independent ready/waiting lanes, persistent in-process planner/executor/confirmer workers, slot-fenced projections, and concise operational metrics. Preserve rebalance_decisions as the one-active-movement audit/lock; ALT-missing work must not create a decision.

Prioritize lost yield dollars per hour using normalized notional, capacity-adjusted net APY edge, confidence, expected service time, holding-horizon costs, and aging/fairness. Reject negative-value/dust moves. Plan fleet-wide capacity-aware waves rather than sending every vault to the point-estimate peak.

ALT requirements: exact verified active prefixes remain usable while later suffixes extend; ready work must not wait behind cold work; remove the global reusable-alt-rollout lock from normal readiness; keep it only for cutover/pause/catalog publication; use lane/family/table locks, one fenced mutation stream per physical ALT, concurrent independent tables, economic request priority, durable coverage_ready outbox wakeups, active/staging shared catalog revisions, best-fit reusable-v2 packing, no new legacy/exact-route ALTs, and standard POLICY_KEYPAIR authority/payer.

Execution requirements: persist exact writable sets and schedule conflict-free bounded waves; keep POLICY_KEYPAIR as delegated policy signer; any route fee-payer pool is fee-only, low-balance, budgeted, and has no ALT/vault authority. Persist semantic identity and exact signed bytes before broadcast; retry identical bytes until expiry; confirm asynchronously; use transaction-slot minContextSlot for reconciliation; never duplicate movement after ambiguous sends or stale reads.

Performance/price gates: complete current-fleet planning from one fresh epoch under 5s p95; 10k replay under 10s; warm high-value submission under 10s p95 and confirmation under 30s p95; 10k ALT-cold jobs alter ready p95 by less than 5%; submit 90% of recoverable yield dollars/hour within 2m and 99% within 10m subject to explicit capacity/conflict ceilings; zero duplicate movements/deadlocks/negative-value routes; bounded fees relative to incremental yield; health evidence identifies a stuck stage within one declared health-observation interval while reporting the faster durable recovery poll separately.

Follow AGENTS.md. Keep binaries thin and reusable domain/data/integration logic in owning modules. Add no broad Rust tests or source-string assertions outside the allowed proof surface; prefer additive migration checks, targeted cargo check, deterministic dry-run/benchmark/verifier paths, isolated SQL behavior checks, and read-only live evidence. Do not expose secrets. Do not perform production writes, deploys, or sends without separate authorization. Report each verifier check PASS/FAIL/NOT RUN with exact evidence; production-only checks remain NOT RUN until authorized.
```
