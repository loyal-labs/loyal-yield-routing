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

### Production completion correction (2026-07-15)

`IMPLEMENTATION: PASS` is not the goal by itself. The goal is
`END STATE: PASS`, which additionally requires repaired production ALT state,
the production cutover, complete fleet evaluation, and observed fund movement.
A locally built image, a Blueprint declaration, an alive worker, or an empty
queue cannot substitute for those outcomes.

Every active, policy-eligible vault with a fresh chain-backed balance must be
accounted for in one current optimizer epoch. “Accounted for” means a durable
opportunity/decision is progressing, the vault is already at its economically
valid winner, or a specific current exclusion such as no positive net edge,
capacity, cooldown, or policy ineligibility is recorded. `not_evaluated`, an
unclassified error, or an old serial cursor that has not reached the vault is
never an acceptable final outcome.

Historical terminal ALT operations remain immutable audit evidence. “Zero ALT
failures” means zero unresolved active failures: no current request, binding,
readiness row, opportunity, or allocator head may depend on an absent,
wrong-owner, or otherwise unusable table. Each damaged table must have a
durable repair record and successor (or an explicit no-longer-needed
resolution), and active usable prefixes on real ALTs must not be discarded
while failed suffix work is replanned.

Production movement PASS is cohort- and flow-aware. New deposits may increase
raw Main AUM during the run, so the verifier captures a baseline cohort and
separately accounts for later deposits. It must prove finalized optimizer
signatures, source/target chain deltas, and reconciler rows at or above the
confirmation slot. The baseline cohort's routeable Main balance must fall by
the confirmed net outflow, while deposits arriving after the baseline remain
visible rather than being mistaken for failed optimization.

Movement effects are route-kind specific. A `same_mint` reserve route proves
its source reserve fell and target reserve rose from the decision's pre/post
position snapshots. An `idle_vault_deposit` proves that the exact planned idle
token account fell by at least the submitted amount and that the target reserve
rose between an independently selected pre-send position snapshot and the
post-confirm snapshot; both post observations must be at or above the
confirmation slot. Every submitted row must be terminal and economically
positive after fees; every reconciled row must additionally be finalized,
successful, and individually effect-proven. Idle deposits do not enter the
reserve-to-reserve Main outflow term, but a proven idle deposit targeting Main
is an explicit Main inflow adjustment. This prevents idle-to-Main optimization
from being misreported as reserve-route outflow or unexplained balance drift.

The authoritative pre-cutover baseline freezes the complete exact set of
vault IDs that are active, policy-active, authorized for the standard delegated
policy signer, enabled for the same-mint route mode, and compatible with the
enabled stable-mint/market universe at capture time. This full cohort includes
eligible vaults with zero Main balance. Its membership never changes inside the
movement equation: final Main balance, post-baseline deposits, and confirmed
optimizer Main inflow/outflow are all restricted to those frozen IDs even if a
vault or policy is later activated, deactivated, or replaced. Vaults that first
become eligible after the baseline remain outside that aggregate equation, but
every post-cutover movement, including theirs, must still pass the individual
finality, economics, target, and chain-effect checks.

### Required production order

1. Capture the production baseline and pause the damaged legacy provisioner if
   it can create more terminal work. This initial incident snapshot is
   diagnostic only until the accepted pre-cutover baseline gate below passes.
   Do not erase or hand-edit failed rows.
2. Land a fenced repair command and additive migration. The command verifies
   finalized on-chain owner/authority/prefix state, quarantines phantom
   allocations, records successor lineage, preserves valid prefixes, and
   requeues only affected demand.
3. Publish the exact immutable light-worker image and prove its registry
   digest. Apply and checksum-verify migrations 23 through the repair migration
   before starting any worker that requires them.
4. Run the repair path and priority provisioner until active phantom
   references and unresolved terminal requests are zero. This is demand-driven
   repair, not a fleet-wide ALT pre-provisioning pass.
5. Stop and drain the executing serial monitor. Prove no serial send can race,
   then deploy the planner, revalidator, executor, confirmer, reconciler, and
   priority provisioner on the same image. Never overlap the two executors.
6. Run the production verifier repeatedly until every vault has a current
   outcome, material opportunities are draining in economic order, finalized
   signatures reconcile to the correct reserves, and the production SLOs pass.

The accepted cutover baseline in this order is captured only after the
signer-free fixed-cohort position sweep completes successfully and before the
fleet sender begins moving funds. An earlier incident or diagnostic snapshot
cannot substitute for it.

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
- The signer-free fleet reconciler owns a bounded chain-position sweep. It
  snapshots one fixed eligible-vault cohort, reuses the validated shared reserve
  catalog/runtime cache, batch-reads vault state with bounded concurrency, and
  advances the existing monotonic slot-fenced position projection. Submitted or
  ambiguous movement reconciliation always drains first; otherwise the sweep
  must finish the fleet quickly enough that planning never depends on an old
  serial cursor or stale dashboard projection. A worker restart may resume with
  a new fixed cohort, but older RPC slots can never overwrite newer rows.
- A baseline-qualifying sweep must complete in less than 600 seconds and report
  `eligible = processed = refreshed` for its frozen cohort, with `failed = 0`
  and `stale = 0`. The production position collector must then report
  `staleRowCount = 0` for the exact eligible routeable scope before the baseline
  is accepted. A partial sweep, a dynamically shrinking denominator, or stale
  rows hidden by a later policy deactivation cannot satisfy this gate.
- The periodic O(vaults) sweep is a recovery/backstop path, not the eventual
  neobank-scale ingestion architecture. As the fleet grows, live account events
  should mark deterministic vault shards dirty and multiple reconciler owners
  should partition those shards. Cross-vault RPC batching is allowed only when
  each vault's reserve/obligation/token evidence still shares one coherent
  context; otherwise preserve the one-context-per-vault safety boundary and
  scale horizontally.
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
- A first route into a missing Kamino obligation includes only the RPC-derived
  obligation rent deficit from `POLICY_KEYPAIR` to the Squads vault. Its final
  simulated atomic transaction orders protected withdraw, vault funding,
  protected obligation initialization, farm initialization, and protected
  deposit. The obligation requirement is capped at 25,000,000 lamports;
  a payer shortfall retries as `route_funding_required`, a deterministic
  simulation failure terminates, and neither is disguised as missing ALT
  coverage. Any obligation/farm setup route holds the durable
  `policy-setup-funding:<POLICY signer>` conflict through reconciliation, while
  mature routes remain independently parallel.
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
- the input position projection is chain-backed, slot-fenced, and within its
  declared freshness bound for the whole captured cohort; stale rows from a
  partial/old serial pass cannot count toward completeness;
- the fixed-cohort production sweep completed in less than 600 seconds with
  `eligible = processed = refreshed`, `failed = 0`, and `stale = 0`, and the
  immediately following exact-scope collector reported `staleRowCount = 0`;
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
- missing-obligation execution proves the exact capped rent deficit, atomic
  withdraw/fund/init/deposit order, final manifest and simulation coverage, and
  setup-funding serialization. Funding/RPC failures retry without a decision
  or send; deterministic simulation failures are terminal; only genuine
  reusable-v2 coverage gaps enter `waiting_alt`.

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

### Check 8: Production ALT damage recovery

PASS only if a finalized-RPC and production-database verifier proves:

- every reusable-v2 table that is active, warming, allocation-accepting, or
  referenced by an active/preparing binding is owned by the Solana Address
  Lookup Table program, has the standard `POLICY_KEYPAIR` authority, and its
  persisted usable prefix exactly matches the on-chain prefix;
- every absent/wrong-owner table is non-allocating and has zero active or
  preparing bindings, zero runnable operation, and zero route-readiness or
  opportunity dependency;
- each historically terminal create/extend has immutable repair evidence that
  links the damaged table/operation and affected demand to a verified successor
  or an explicit no-longer-needed resolution;
- all requests failed solely because of a terminal damaged-table dependency
  have a fenced successor attempt or are satisfied; unresolved active terminal
  request count is zero;
- valid prefixes on real ALTs remain usable, and suffix retries start from the
  finalized observed prefix rather than allocating duplicate tables or
  replaying a stale extension;
- no new legacy or exact-route ALT is created, and the standard policy account
  remains within the explicit rolling lamport budget.

Historical `permanent_failure` rows are expected audit records and are not
deleted to manufacture PASS. Any live extension attempt against a phantom
table, raw operator SQL repair, missing successor lineage, or unresolved active
terminal dependency is FAIL.

### Check 9: Production migration and atomic executor cutover

PASS only if live readback proves:

- the production migration ledger contains migrations 23 through the repair
  migration with repository-matching names and checksums;
- one immutable GHCR light-worker tag/digest for the verified commit exists and
  all six production workers use it with the required commands and
  least-privilege env boundaries;
- the latest deploy for each worker is live, each process reports its durable
  recovery/health state, and no required binary or migration precondition is
  missing;
- `loyal-same-mint-yield-monitor` is suspended, scaled to zero, or otherwise
  incapable of `--execute` before the fleet executor begins sending;
- no interval exists in which both serial and fleet executors could claim/send
  production routes, and pre-cutover signed/ambiguous work is drained or
  adopted without replacement movement;
- the new provisioner is the only active ALT mutator for the cluster and uses
  the standard `POLICY_KEYPAIR` plus its explicit budget.

### Check 10: Complete fleet evaluation and economic draining

PASS only if a fresh production epoch and its durable queue prove:

- every active policy-eligible vault with a fresh chain-backed position is
  present in exactly one mutually exclusive current outcome; unaccounted and
  `not_evaluated` counts are zero;
- the epoch is complete and unexpired, the counted outcomes sum exactly to the
  eligible fleet, and no result is inherited from the old serial cursor;
- every material opportunity (at minimum the captured >= $1,000 cohort) is
  ready, leased/in flight, waiting on a named current ALT/capacity/conflict
  dependency, confirmed, or economically excluded—never silently absent;
- no lower-value nonconflicting route is submitted while a materially higher
  lost-yield-dollar opportunity is runnable and unleased; fairness aging may
  break near-ties but cannot hide the high-value cohort;
- `waiting_alt`, simulation failures, expired leases, and worker errors have
  bounded age and are decreasing or have an explicit fenced recovery action;
  no material vault remains stuck beyond ten minutes;
- aggregate outcome counts and USDC amounts are emitted every health interval
  so regressions are visible without reconstructing the fleet manually.

### Check 11: Correct production movement and reconciliation

PASS only if post-cutover production evidence proves:

- real optimizer-created routes—not a manual operator transaction—have
  finalized signatures and positive net edge after measured fees;
- each selected target was the best currently admissible safe reserve for its
  risk/mint/capacity constraints in the opportunity's immutable epoch;
- the >= $1,000/highest-lost-yield cohort begins moving before lower-value
  nonconflicting work, subject only to explicit ALT, account-conflict, or target
  capacity blockers;
- finalized RPC and chain-backed reconciler rows at or above each confirmation
  slot show the expected source decrease and target increase; projection-only
  or dashboard-only evidence is insufficient;
- source/target proof is selected by the durable route kind: reserve routes use
  reserve pre/post snapshots, while idle deposits match the exact idle token
  account and its pre-send amount to a post-confirm balance decrease plus an
  independently measured target-reserve increase. Unknown route kinds, missing
  pre-send target snapshots, or current-only target assertions fail closed;
- every terminal row proves reciprocal submission/opportunity/decision IDs,
  one vault and optimizer epoch, exact raw execution-plan fields, route-kind
  source-snapshot semantics, and snapshot ownership. Reconciled rows require a
  found finalized successful RPC status at exactly the persisted confirmation
  slot plus ordered submit/confirm/reconcile timestamps and post-state slots.
  Failed rows require a found finalized unsuccessful status at that exact slot.
  Expired rows require finalized-history absence, a current finalized block
  height beyond last-valid, and a persisted effect-check slot for any attempted
  broadcast. Collector verdict booleans cannot substitute for these raw facts;
- the baseline Main cohort falls by confirmed net outflow after separately
  accounting for deposits received after the baseline;
- the Main net-flow equation counts same-mint Main source moves as outflow and
  both same-mint and idle-vault Main targets as inflow; reserve and idle route
  counts remain separately visible;
- the baseline artifact contains the full frozen eligible cohort ID set,
  including eligible zero-Main vaults. The aggregate equation uses only that
  set on every term: baseline routeable Main plus post-baseline cohort deposits
  minus final cohort Main equals confirmed cohort optimizer Main net outflow
  within the declared tolerance. Final cohort Main is summed for the frozen IDs
  regardless of their later active-policy state, and the net-flow term excludes
  movements belonging to vaults that first became eligible after capture;
- all post-cutover movements remain individually verified even when their vault
  is outside the frozen aggregate cohort; excluding a newly eligible vault from
  the Main equation must never exclude its signature, economics, selected
  target, source/target delta, or reconciliation evidence from this check;
- the real deployment meets the same two-minute/ten-minute yield-unlock,
  submission, confirmation, fee, duplicate, deadlock, and negative-value gates
  from Check 6;
- unresolved terminal ALT failures, stale active decisions, ambiguous
  replacement sends, duplicate movements, and capacity oversubscriptions are
  zero at the end of the observation window.

One successful tiny route does not pass this check while material eligible
funds remain unevaluated or stuck.

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

# Final completion must consume freshly collected live measurements rather
# than operator-authored PASS booleans. These commands are implementation-owned.
op run --env-file=.env.1password -- sh -c \
  'fleet-orchestration-production-evidence --json --output <PATH>'
fleet-orchestration-verifier --end-state --json \
  --runtime-evidence-json <PATH> \
  --production-evidence-json <PATH>
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
  epochExpiresAt, oneImmutableEpoch, planningSampleEpochProofs,
  planningSampleCount,
  planningP95Milliseconds, replayVaultCount, replayMilliseconds,
  economicallyOrdered, topCohortHasNoNonconflictingPriorityInversion,
  childRouteOrReconcileProcessesSpawned

planningSampleEpochProofs[]:
  marketEpochOptimizerId, observedOpportunityEpochIds,
  selectedOpportunityEpochIds

alt:
  typedProvisionerDryRunPlans, reusableV2Plans,
  legacyOrExactRouteAltPlans, readyJobsSeeded, readyJobsClaimed, waitingAltJobs,
  waitingAltDecisions, claimLatencyGateClock,
  readyClaimBaselineP95Micros, readyClaimColdP95Micros,
  readyClaimBaselineClientP95Micros, readyClaimColdClientP95Micros,
  durableCoverageWakeupRows,
  affectedJobsPromoted, unaffectedJobsPromoted,
  additionalFleetCycleRequired,
  normalReadinessGlobalRolloutLockAcquisitions,
  independentPhysicalAltLanesProgressed,
  sameTablePredecessorViolations, staleFenceCommits

execution:
  duplicateActiveVaultMovements, nonoverlappingConcurrentLeases,
  overlappingLaneLimitViolations, physicalWritableKeyCongestionVisible,
  expiredLeaseReclaimedWithHigherFence,
  mixedRunnableAndExpiredClaimsFullAndDisjoint,
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
  lowBalanceLimitsEnforced, atomicImmutableSpendReservation,
  targetCapacityConcurrentAdmissionBounded,
  preSendTargetCapacityReleased,
  reconciledCapacityStrictTelemetryFence,
  preexistingNewerTelemetryRelease

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
  recoveryPollIntervalMilliseconds, healthObservationIntervalMilliseconds,
  stuckStageDetectionMilliseconds
```

The verifier recomputes completeness totals, backlog effects, and every numeric
threshold from these measurements. The wiring maps must contain exactly the six
durable roles and the six stuck stages named in Check 7; every local-container
probe must exit zero and every stuck stage must be detected within the
recorded health-observation interval. The probed image reference must exactly
equal the one immutable GHCR `light-workers:sha-<commit>` reference shared by
all six production Blueprint roles; probing an unrelated local image is FAIL. A
complete source-bound artifact can move Checks 2, 4,
5, 6, and the runtime portion of 7 to PASS; absence leaves them `NOT RUN`, and
invalid or threshold-breaking measurements produce `FAIL`.

The seven live planning latency samples may observe successively newer market
epochs; requiring the market to stop updating during measurement would test a
frozen feed, not planner correctness. Every sample must carry its market epoch
ID plus the distinct observed and selected opportunity epoch IDs, all of which
must match. The artifact records the final complete, non-expired epoch with the
p95 across all samples.

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
`NOT RUN`. Check 8 spans the repair implementation and its live active-state
proof; Check 9 is a deployment gate; Checks 10-11 require live production
optimizer and movement evidence. `--end-state` must collect or consume those
measurements and cannot hardcode deployment or production performance to
`NOT RUN`. `--implementation` succeeds only when every
required implementation subcheck passes, regardless of deployment or
production-performance `NOT RUN` state, and must not claim `END STATE: PASS`.

Do not weaken a failed condition to match the implementation. If a condition is
found to encode the wrong product or safety goal, record the correction and
reason in this document before changing implementation.

## Latest Evidence Run: 2026-07-15

This is the literal current verdict. Checks 1-7 retain the last clean
source-bound implementation artifact at checkout
`80342a3b8eeaf4dd5c2e18943c66cd8dd3fddcfd`; the production repair checkpoint
below is direct readback from migration 28 plus the fenced repair command at
checkout `f4e5d59`, and is not yet the final clean deployment artifact:

| Check | Verdict | Evidence / first missing invariant |
| --- | --- | --- |
| 1. Repository and migration integrity | FAIL | The new runner applied migrations 1-28 and passed exhaustive validation from a blank PostgreSQL database; production also completed one explicit exhaustive 1-28 audit. A checksum-current production `--apply` now takes 2.74s instead of the prior multi-minute catalog audit because it reads the ledger once and skips already-proven exhaustive validation, while the applying invocation retains a dedicated advisory-lock session and records the final validation-fence row only after validation. This check remains FAIL until the current tracked implementation is committed cleanly and the full source-bound verifier is rerun. |
| 2. Fast complete discovery | FAIL | The prior read-only planner artifact accounted for all 3,018 eligible rows in 3.320s p95 and passed ordering/replay checks, but the production evidence collector found that the Main/OnRe position projection was not fresh enough for a cutover baseline. The old serial cursor cannot prove a fresh chain-backed whole-fleet denominator. A signer-free bounded position sweep is now required; this check remains FAIL until a clean source-bound run proves the full fixed cohort was slot-fenced and fresh before the planner epoch. |
| 3. Economic behavior | PASS | Deterministic notional, edge, lost-yield, starvation, dust/cost, fee-cap, material-frontier, and concurrent target-capacity invariants passed. Three simultaneous $100 admissions against $250 of headroom admitted two and rejected only the excess contender without a telemetry-fence false rejection. |
| 4. ALT head-of-line isolation | PASS | The source-bound artifact drained all 4,096 ready jobs while leaving 10,000 `waiting_alt` jobs decision-free. PostgreSQL statement p95 was 4.821ms baseline versus 4.017ms cold (0ppm positive regression); reusable-v2-only planning, one-row durable targeted wakeup, two independent physical ALT lanes, lane-exact indexes, zero normal global-lock acquisitions, and zero stale-fence commits passed. |
| 5. Execution concurrency and crash safety | PASS | Isolated DB plus controlled-RPC evidence passed 64 nonoverlapping leases, physical writable congestion, full/disjoint mixed runnable-expired claims, higher-fence reclaim, exact-byte rebroadcast, ambiguity blocking, slot-fenced reconciliation, target-capacity admission/release fences, and zero duplicate movements/deadlocks. The standard `POLICY_KEYPAIR` signed policy execution and remained ALT authority/payer; fee-only shard identity and budget gates passed. |
| 6. Performance, value, and price | PASS | The controlled 10,000-route replay measured 2.331s warm high-value submission p95 and 23.930s confirmation p95. It submitted 100% of recoverable yield dollars/hour inside both two and ten minutes, observed a 162ppm maximum fee fraction under the 50,000ppm cap, and produced zero negative-value routes, duplicates, or deadlocks; ALT backlog changed warm p95 by 0ppm. This is implementation replay evidence, not real production movement evidence. |
| 7. Production wiring and feedback loop | FAIL | The corrected reconciler now performs a signer-free, transaction-free fixed-cohort chain sweep and interleaves one bounded 16-vault wave only after each signed-reconciliation batch, preventing both confirmation starvation and freshness starvation. Its cohort query exactly matches the planner denominator (3,015 current vaults), restarts oldest-first, validates the active catalog hash/42 reserve roles, and passed the role probe. `render.yaml` and the verifier require `--position-sweep-interval-seconds 300`; a clean source-bound image probe and measured full 3,015-vault duration are still required. |
| 8. Production ALT damage recovery | FAIL | Migration 28 and the provisioner-owned fenced repair ran under durable pause epoch 9 with finalized RPC and standard `POLICY_KEYPAIR` identity proof, sending zero transactions. The raw production collector now verifies 84/84 active or referenced ALTs at finalized RPC with zero owner, authority, or persisted-prefix mismatch; all 7 damaged phantoms are non-allocating with zero live binding/runnable/route dependency; all 107 historical terminal operations have immutable repair evidence; and all 4 real prefixes are preserved. Only 18 of 108 affected requests are currently satisfied or have a healthy successor, leaving 90 unresolved while the provisioner is suspended. Historical charged spend is about 1.313 SOL in the 24-hour window, so the explicit safe cap is 2 SOL for the remaining approximately 0.22 SOL repair. This remains FAIL until the standard payer is funded, the sole budgeted v2 provisioner drains those requests, and a fresh collector retains every zero-damage invariant. |
| 9. Production migration and atomic cutover | FAIL | Migrations 23-28 are applied and explicit production schema validation passed; migration 28 ledger readback is `reusable_alt_terminal_repair` with repository-matching checksum `890ed019ab37c0334f22cdc9a94256f3d85218c724c09669bd96595ca3006f13`. The executing fleet service is still the serial `loyal-same-mint-yield-monitor` on `light-workers:sha-1c25f69...`; none of the six optimizer services is live and the final optimized image has not been published. |
| 10. Complete fleet evaluation | FAIL | The new planner's live read-only full-fleet pass accounted for all 3,016 eligible vaults in 2.033s and found 2,540 opportunity vaults; its top cohort began at $10,104 and $7,663 and a bounded wave selected $57,582.89 notional. This is not yet a durable production epoch: the planner ran nonmutating, no six-worker fleet is live, and legacy readiness still contains incomplete/failed work. |
| 11. Correct production movement | FAIL | At 23:11 UTC, chain-backed current positions held 83,468.51 USDC in Main versus 220,294.57 in OnRe; fresh APYs were 3.5106% and 6.8297%. The old serial monitor reduced Main from the earlier 96-97k incident snapshot, but there are still no post-cutover optimizer signatures or cohort/flow-aware new-worker reconciliation because the cutover has not occurred. |

Current scoped result: `IMPLEMENTATION: FAIL`; `DEPLOYMENT: FAIL`;
`PRODUCTION PERFORMANCE: FAIL`; `END STATE: FAIL`.
The previous implementation-only goal was prematurely complete. The executable
verifier must now support live deployment/performance evidence, and the goal
stays active until the production end state passes.

## Standing Codex Goal (under 4,000 characters)

```text
Run docs/plans/fleet-yield-orchestration-speed-verifier.md adversarially against the current loyal-yield-routing checkout and production, and make END STATE: PASS without weakening any check. IMPLEMENTATION: PASS alone is insufficient. Re-run the literal verifier after every material repair, migration, deploy, or runtime finding.

Current failures are binding: migrations 23-28 and the fenced repair are now
applied, but the old ID-ordered serial monitor remains the only executor; the
six-role image is not published/deployed; the repaired reusable-v2 provisioning
queue is paused and the standard policy payer still requires an explicit safe
top-up/budget before sends; much of the fleet lacks a fresh current optimizer
epoch; and large Main balances have not yet been proven to move under the new
optimizer.

Implement a durable fenced repair path, preferably migration 28 plus provisioner-owned admin logic. From finalized RPC, distinguish absent/wrong-owner tables from real ALTs with valid prefixes. Preserve immutable failed-operation history. Quarantine phantom allocations, stop them accepting work, supersede affected preparing bindings, record repair/successor lineage, allocate fresh best-fit reusable-v2 shards, and create new attempt generations for only affected demand. Replan failed suffixes on real ALTs from their verified usable prefixes. Never repair with ad hoc production SQL, reopen a terminal row in place, create legacy/exact-route ALTs, discard a valid prefix, or use any signer other than standard POLICY_KEYPAIR for ALT authority/payer.

Extend the executable verifier with measured production evidence: finalized ALT owner/authority/prefix checks, active dependency counts, migration checksums, GHCR digest, live Render service/image/command/suspension/deploy state, complete optimizer-epoch outcomes and USDC amounts, opportunity ordering/age, signatures, fees, and chain-backed source/target reconciliation. Caller-supplied verdict booleans are invalid.

Publish the exact immutable light-workers image through the worker-images workflow. Apply and verify migrations 23 through the repair migration. Run the fenced repair and drain unresolved active terminal ALT dependencies to zero. Stop/drain the executing serial monitor before the fleet executor can send, then deploy the planner, revalidator, executor, confirmer, reconciler, and priority provisioner on the same image with least-privilege envs. Never dual-execute.

Production PASS requires every active policy-eligible fresh vault in exactly one current epoch outcome; zero not_evaluated/unclassified vaults; material >=$1k/highest-lost-yield opportunities moving first unless a named current ALT/conflict/capacity blocker exists; no material vault stuck over ten minutes; and aggregate counts/USDC emitted each health interval. Do not fleet-backfill ALTs or use a slow canary.

Movement PASS requires real optimizer-created finalized signatures, positive net edge after fees, best admissible safe targets, reconciler observations at or above confirmation slots, source decreases/target increases, and baseline Main cohort reduction after separately accounting for new deposits. Meet the 2m/10m yield-unlock and 10s/30s submission/confirmation gates with zero duplicates, deadlocks, negative-value routes, ambiguous replacements, capacity oversubscription, stale active decisions, or unresolved active ALT failures.

Follow AGENTS.md: keep binaries thin, reusable logic in owning modules, and avoid broad new Rust tests. Prefer targeted cargo check, migration verification, production SQL/RPC/Render evidence, and the executable verifier. Use op run with .env.1password; never print secrets or signed bytes. Production data repair, migrations, Render changes, deployments, and optimizer sends are authorized for this goal, but remain bounded by the verifier and fail closed on uncertain state.
```
