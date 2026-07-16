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
Once the additive schema, immutable image, reusable-v2 ALT path, and durable
planner/revalidation/execution/confirmation/reconciliation/provisioning roles
pass this verifier, production replaces the executing serial monitor as one
coordinated change. Those roles may be separate processes or safely fused, but
the old and new execution paths must never move the same fleet concurrently.

### Production completion correction (2026-07-15)

`IMPLEMENTATION: PASS` is not the goal by itself. The goal is
`END STATE: PASS`, which additionally requires repaired production ALT state,
the production cutover, complete fleet evaluation, and observed fund movement.
A locally built image, a Blueprint declaration, an alive worker, or an empty
queue cannot substitute for those outcomes.

Every active, policy-eligible vault must be accounted for in exactly one
current outcome for each routeable source, whether or not its position or mint
evidence is currently fresh. “Accounted for” means a durable opportunity or
decision is progressing, the vault is already at a constrained portfolio
optimum, or a specific current blocker/exclusion such as stale position,
incomplete mint evidence, no positive net edge, capacity, cooldown, or policy
ineligibility is recorded with age and recovery action. Each executable route
is bound to a planner-created optimizer epoch that remains valid for that
route's mint; continuously updating mints do not need to share one global epoch
ID. `not_evaluated`, an unclassified error, a silently skipped mint-lifetime
candidate, or an old serial cursor that has not reached the vault is never an
acceptable final outcome.

Historical terminal ALT operations remain immutable audit evidence. “Zero ALT
failures” means zero unresolved active failures: no current request, binding,
readiness row, opportunity, or allocator head may depend on an absent,
wrong-owner, or otherwise unusable table. Each damaged table must have a
durable repair record and successor (or an explicit no-longer-needed
resolution), and active usable prefixes on real ALTs must not be discarded
while failed suffix work is replanned.

Production movement PASS is cohort- and flow-aware. A bounded verifier
observation window records its opening positions and separately accounts for
deposits, withdrawals, and optimizer movements that occur during the window.
It must prove finalized optimizer signatures, source/target chain deltas, and
reconciler rows at or above the confirmation slot. Main reduction remains a
useful incident metric, but it is not the permanent optimizer objective: an
economically correct marginal allocation may retain or add Main liquidity.
The required economic outcome is reduced constrained portfolio opportunity
gap and higher expected net yield after fees, dilution, pending flow, and
anti-churn margin.

Movement effects are route-kind specific. A `same_mint` reserve route proves
its source reserve fell and target reserve rose from the decision's pre/post
position snapshots. An `idle_vault_deposit` proves that the exact planned idle
token account fell by at least the submitted amount and that the target reserve
rose between an independently selected pre-send position snapshot and the
post-confirm snapshot; both post observations must be at or above the
confirmation slot. Every submitted row must be terminal and economically
positive after fees; every reconciled row must additionally be finalized,
  successful, and individually effect-proven once they are older than the
  declared in-flight grace period. Freshly signed/submitted work may remain
  nonterminal only while it is inside its measured SLO and has a live fenced
  owner or confirmer recovery path. Idle deposits do not enter the
reserve-to-reserve Main outflow term, but a proven idle deposit targeting Main
is an explicit Main inflow adjustment. This prevents idle-to-Main optimization
from being misreported as reserve-route outflow or unexplained balance drift.

The authoritative movement window freezes the exact vault IDs eligible at its
opening capture, including eligible zero-Main vaults, for aggregate accounting
only. Its membership never changes inside that window's equation. Vaults that
first become eligible or receive their first deposit during the window are
reported as a separate arrival cohort and must meet the same deposit-to-outcome
and individual movement checks. Capturing this verifier window is not a
precondition to sending an independently fresh, safe, high-value route.

### Required production order

1. Capture a diagnostic production snapshot and pause any damaged legacy
   mutator if it can create more terminal work. Do not erase or hand-edit
   failed rows. This snapshot starts evidence collection but does not block
   independently fresh reusable-v2 routes from moving.
2. Land a fenced repair command and additive migration. The command verifies
   finalized on-chain owner/authority/prefix state, quarantines phantom
   allocations, records successor lineage, preserves valid prefixes, and
   requeues only affected demand.
3. Publish the exact immutable light-worker and LaserStream-worker images from
   the same source commit and prove both registry digests. Apply Timescale
   migration 5 first. The Kamino monitor predeploy must be one image-contained
   executable that applies the Timescale migrations and then syncs supported
   reserves without the removal override; Render must invoke that executable
   directly rather than tokenizing a shell pipeline. The live monitor must
   establish a valid durable observation floor plus a confirmed, exact-identity,
   <= 90-second verification watermark and routeable latest-view row for every
   reserve in each mint admitted for routing. An incomplete mint is blocked and
   named without shrinking its own catalog denominator; it must not block an
   unrelated complete mint or an already safe route for that mint.
4. Apply and checksum-verify Neon migrations 23 through migration 30, including
   the commit-time opportunity and signed-handoff lifetime fences plus the
   bounded fused-queue accrual binding.
   Run the repair path and priority provisioner until active phantom references
   and unresolved terminal dependencies are zero. ALT-ready work continues
   while unrelated repair/provisioning drains. This is demand-driven repair,
   not a fleet-wide ALT pre-provisioning pass.
5. Stop and drain the executing serial monitor. Prove no serial send can race,
   then deploy the planner, revalidator, executor, confirmer, reconciler, and
   priority provisioner on the same image. Never overlap the two executors.
6. Run the production verifier repeatedly until every vault has a current
   outcome, material opportunities are draining in economic order, finalized
   signatures reconcile to the correct reserves, and the production SLOs pass.

After deployment, open a fresh bounded production verification window and run
the signer-free completeness sweep concurrently with normal routing. Per-route
source evidence must always be fresh before send. The full sweep is the final
fleet-accounting proof and recovery backstop, not a gate that delays safe
high-value movement.

## Ideal Implementation Contract

### 1. Versioned observation and planning

- Market state is read once per immutable optimizer epoch. Every opportunity
  records the market epoch, source position snapshot/slot, expiry, source and
  target APY, raw amount, normalized USD notional, expected holding horizon,
  and capacity-adjusted net edge.
- The planner is the sole production writer of durable optimizer epochs.
  Revalidators and executors load the opportunity's bound planner epoch, read a
  fresh market observation, and persist fresh revalidation evidence separately;
  they never insert or supersede optimizer epochs. Concurrent worker waves must
  not change optimizer-epoch count or IDs.
- The immutable observation may cover the complete code-owned enabled-mint
  universe, but route lifetime and material-frontier revalidation are scoped to
  the route mint. The durable optimizer row remains addressable through the
  maximum usable expiry of its complete mint members, while each opportunity
  and signing fence uses the minimum catalog/verification/economic expiry for
  its own mint. Missing or expired unrelated-mint evidence cannot stale a valid
  route. A missing source/target, insufficient route-mint lifetime, or material
  same-mint frontier change transitions the old opportunity to `stale` before
  ALT work, signing, capacity reservation, or decision creation so the planner
  can publish the current successor.
- Optimizer epoch identity is semantics-versioned. Reusing a market fingerprint
  written under older global-minimum expiry semantics must create/select a
  compatible versioned row, not collide with different immutable evidence or
  bind an opportunity to the old lifetime. The stored row expiry, envelope
  evidence, epoch key, and semantic version must agree exactly.
- Balance, market, policy, cooldown-expiry, and `coverage_ready` events wake the
  affected vault/cohort. A short recovery poll may remain, but correctness and
  latency do not depend on finishing a fleet scan. PostgreSQL notifications are
  hints only: each listener reconnects and immediately scans the durable queue.
- Scoped dirty-cohort planning may reuse a full-sweep frontier only when that
  frontier was complete, its immutable market epoch remains fresh, and its
  route-mint material economic frontier plus durable target-telemetry version
  still match.
  New observation timestamps or slots that do not cross a material APY,
  confidence, availability, or capacity threshold must not turn every dirty
  event into another fleet scan. A material frontier change wakes the affected
  mint/cohort; a deferred frontier, oversized cohort, target-telemetry mismatch,
  or contention may fall back to an authoritative full sweep without stopping
  unrelated ready execution.
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
- A completeness-qualifying sweep must complete in less than 600 seconds and report
  `eligible = processed = refreshed` for its frozen cohort, with `failed = 0`
  and `stale = 0`. The production position collector must then report
  `staleRowCount = 0` for the exact eligible routeable scope before fleet-wide
  completeness passes. A partial sweep, a dynamically shrinking denominator,
  or stale rows hidden by a later policy deactivation cannot satisfy final
  completeness. They do not prevent a separate vault with fresh route-local
  evidence from moving.
- The periodic O(vaults) sweep is a recovery/backstop path, not the eventual
  neobank-scale ingestion architecture. As the fleet grows, live account events
  should mark deterministic vault shards dirty and multiple reconciler owners
  should partition those shards. Cross-vault RPC batching is allowed only when
  each vault's reserve/obligation/token evidence still shares one coherent
  context; otherwise preserve the one-context-per-vault safety boundary and
  scale horizontally.
- Stale epochs/opportunities are superseded before decision creation. Older
  observed slots cannot overwrite newer projected state.

#### Confirmed Kamino market-data plane

- Supported-reserve publication is compare-before-mutate. Normal startup and
  periodic refresh reject an empty response or any difference in the complete
  active `(market, liquidity_mint, reserve)` identity set. Comparison,
  deactivate/upsert, target decoding, and target loading occur in one database
  transaction under one catalog advisory lock; a partial response, shrink, or
  topology change fails before any catalog timestamp or active row changes.
  Explicit `--sync-supported-reserves` may bootstrap, add pairs, or replace the
  reserve for a retained market/mint pair, but it also rejects an empty response
  and any pair removal unless the operator additionally supplies
  `--allow-supported-reserve-removals`. That removal flag is valid only with
  explicit sync and must be absent from the standard Render predeploy.
- The normal supported-reserve refresh interval is at most 180 seconds. An
  active catalog row older than 300 seconds, future-dated, non-`kamino-api`, or
  lacking more than 60 seconds of remaining catalog lifetime is ineligible for
  epoch publication; a failed refresh never renews the old catalog.
- `kamino.reserve_current_states` is the compact current pointer and
  `kamino.reserve_confirmed_observation_floors` is the durable confirmed
  high-water record. Its positive `observation_id` is a deterministic database
  tie identity; source rank 1 is limited to `laserstream_grpc`/`websocket`, and
  rank 2 is limited to `http_snapshot`/`http_confirmed_refresh`.
  `kamino.reserve_confirmed_verifications` is its renewable confirmed
  watermark. `kamino.latest_verified_reserve_updates` is usable only when its
  reserve, event ID, account-data hash, and state `observed_at` exactly match
  the pointer and immutable tape event, with `state_slot <= verified_slot`,
  confirmed commitment, and the allowed HTTP provenance domain on both pointer
  and watermark. The pointer and watermark labels may differ; each is
  allowlisted independently rather than compared for equality. A row clears
  the durable floor without hash identity only when
  `verified_slot > floor_slot`. At the floor, admission requires a valid floor
  whose exact account-data hash still equals the HTTP-owned current pointer.
  A below-floor row or conflicting equal-slot row is never admitted or
  routeable.
- Every confirmed observation advances the durable floor monotonically,
  including valid, missing, invalid, owner/decode-mismatched HTTP accounts and
  valid or malformed confirmed stream observations. A higher slot wins. At an
  equal slot HTTP rank 2 wins over stream rank 1; an equal-rank HTTP observation
  is the latest authority, while disagreeing equal-rank stream observations
  collapse the floor to invalid/null until a higher slot or same-slot HTTP
  observation resolves it. A lagging HTTP response can therefore neither
  replace the pointer nor renew/invalidate its verification after newer
  evidence has raised the floor.
- Every durable-floor writer—including valid or malformed stream ingestion and
  batched HTTP verification—uses the same reserve-scoped advisory transaction
  lock. Batch HTTP locks are acquired in deterministic reserve order in a
  separate statement before reading the pre-update floors, so an absent floor
  row is serialized as strictly as an existing row and overlapping bootstrap
  batches cannot validate themselves against their own rewrite.
- HTTP confirmed account reads own current-pointer creation and replacement.
  A candidate must be above the durable floor, or at that floor with its exact
  valid hash, and must not trail the prior verification watermark. Stream
  observations can advance/invalidate the
  floor but cannot own the pointer or renew the HTTP verification. A hash,
  decode, owner, event, or identity mismatch invalidates the watermark before
  retry; the previous row cannot remain routeable during recovery unless a
  later read satisfies the exact at-floor or strictly-above-floor rule.
- A valid first HTTP read that conflicts with an equal-slot prior floor is
  classified `deferred` even though rank 2 durably records the candidate hash.
  The caller must not run pointer/event fallback insertion from that same read.
  This applies when a stream-created floor exists but no current pointer does.
  A second independent read with the exact now-authoritative hash may admit the
  pointer and watermark. Every valid below-prior-floor read is also deferred
  and same-read fallback is forbidden; it remains blocked until a later read is
  at or above the durable floor.
- An unchanged confirmed HTTP read advances the floor plus `verified_slot` and
  `verified_at`, but reuses the exact immutable reserve event and pointer.
  Because verification coordinates bind liveness, renewal may rotate the
  optimizer epoch, but it does not by itself force a material-frontier
  fallback.
- A planner epoch is publishable only with at least 60 seconds of remaining
  verified lifetime for every target included in a published mint. Verification
  age above 90 seconds is an operator warning and blocks that mint's admission;
  it does not globally stop unrelated complete mints. Age above 240 seconds is
  hard-expired and excluded in every path. Future-dated watermarks fail closed.
- Opportunity publication enforces that 60-second margin transactionally with
  the PostgreSQL clock, not a worker clock: the guarded INSERT requires both
  epoch and opportunity expiry to remain at least 60 seconds away, and the
  transaction repeats that assertion after any ALT consumer linkage and
  immediately before commit. A stalled linkage therefore rolls back both the
  opportunity and consumer instead of exposing nearly expired work.
  `waiting_alt` re-admission uses the same database-clock predicate in its
  mutating UPDATE; insufficient lifetime leaves the row waiting for a current
  planner epoch rather than making stale economics executable.
- Migration 5 is additive: it creates the three compact per-reserve tables,
  monotonic observation-ID sequence, and exact latest view. Bootstrap and
  periodic verification join through the indexed current pointer/event ID.
  They must not scan the reserve-update hypertable or rewrite its indexes or
  chunks.
- Production coverage and top-target evidence share one read-only,
  repeatable-read Timescale snapshot and database clock. The immutable tape
  join includes the pointer's exact `observed_at` so chunk pruning cannot
  degrade this gate into a 36GB hypertable scan.
- For each enabled mint, the denominator is every exact active safe catalog
  identity for that mint—not only the verified rows that happened to return.
  Missing, duplicate, identity-mismatched, stale, invalid, or insufficiently
  lived reserve evidence blocks that mint with named evidence; it cannot shrink
  the denominator or publish a false lower peak. A complete unrelated mint may
  still publish, but every published mint must have one exact verified row per
  catalog identity and at least one eligible target.
- Stable-reserve capacity is derived from decoded raw
  `total_supply_amount`, the code-owned six-decimal stable valuation, and the
  code-owned $1 price. Reserve oracle USD estimates cannot determine capacity.
  Each reserve must satisfy
  `verified_slot - reserve_last_update_slot <= 1,500`; its economic expiry is
  `verified_at + (1,500 - lag) * 250ms` and must remain strictly more than 60
  seconds away at publication. Each mint expiry is the minimum catalog,
  verification, and economic expiry for that mint. The durable optimizer
  envelope expiry is the maximum usable expiry across complete mints, while
  its conservative global-minimum diagnostic remains visible. The configured
  verification lifetime can only tighten the 240-second hard cap, never extend
  it.

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
- The optimizer's objective is the constrained expected net yield of the whole
  fleet, not the count of vaults placed in the highest displayed APY reserve.
  A correct result may intentionally split a mint/risk cohort across reserves.
  Each admitted move applies projected source outflow and target inflow, then
  re-ranks affected candidates at their post-flow marginal APYs. The plan stops
  at a discrete fixed point where no feasible whole-vault move clears fees,
  uncertainty, safety margin, cooldown, and anti-churn hysteresis.
- Point-in-time APY is insufficient evidence for a long holding-horizon claim.
  The expected dwell/edge uses Timescale history or a documented conservative
  fallback for APY persistence and volatility. A small alternating APY lead
  must not produce `A -> B -> A` churn. A round trip inside the expected dwell
  window requires a named material exogenous change and positive cumulative
  expected gain after both moves' measured costs.
- Every decision persists its pre-flow supply/APY, projected post-flow
  supply/APY, committed same-reserve inflow/outflow, confidence/horizon, and
  predicted marginal edge. After confirmation, Neon signature/amount/slot
  evidence is joined to the first sufficiently fresh Timescale observation to
  compare predicted versus observed response. Timescale aggregate movement is
  never assumed to be caused by Loyal without that signed-flow attribution.
  Excess model error lowers confidence and wave size before more fleet capital
  is admitted.
- Target capacity is a durable, versioned admission resource. Promotion to an
  executable decision atomically reserves capacity against current target
  supply plus every active or recently landed inflow reservation and subtracts
  committed outflow when determining the projected reserve state. A successful
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
  audit row. This also applies to a pre-decision planner/executor contract
  failure: correcting the worker may create one successor generation, but must
  not rewrite the failed attempt's identity, execution plan, state, or terminal
  reason.

### 4. ALT-independent fast lane

- The reusable-ALT contract in
  `docs/plans/earn-reusable-alt-migration-verifier.md` remains a required
  dependency: one complete logical shared-market family (which may span the
  minimum necessary physical shards) serves stable market accounts, while
  vault-dependent addresses append-pack into bounded multi-vault shards only
  after genuine route demand. Stable shared addresses are not recopied into a
  per-vault family, and normal routing never creates a legacy/exact-route ALT.
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
- A first missing-coverage attempt fails before decision creation or send,
  seals exactly one idempotent typed request, and remains a named
  `waiting_alt` outcome. The priority provisioner creates/extends and verifies
  reusable-v2 coverage; the next planner/revalidation cycle retries the route.
  No missing-coverage route is silently dropped, and high-value cold demand is
  ordered by recoverable yield per remaining mutation.
- Packing efficiency is a live invariant: additional normal vaults consume
  verified headroom in existing shards before new shards are allocated, except
  for measured address/packet outliers. ALT count, addresses used/high-water,
  vaults per shard, and ALT operations per unlocked dollar are reported.
- The deployed resolver contains no legacy fallback. Imported legacy tables
  remain zero-reference retirement inventory only; every old table is
  deactivated, observes the SlotHashes cooldown, is closed, and has
  transaction-local rent-refund proof to `POLICY_KEYPAIR`. A later verifier run
  must prove that the already completed reusable-only migration and refunds
  have not regressed.
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
- Ordinary reserve yield may increase redeemable liquidity between planning and
  the fused worker's final chain read. A fused queue handoff may bind that fresh
  larger amount only for a `same_mint` reserve move, only when the increase is at
  most 100 ppm of the immutable planned amount, and only while the source
  snapshot, collateral shares, source/target/mint identity, amount semantics,
  optimizer economics, signed bytes, and execute fence remain exact. Negative
  drift, an increase above 100 ppm, an idle route, or any other identity or
  semantics change fails before decision creation. Migration 30 independently
  enforces the same bound when the decision is atomically linked to its leased
  opportunity; the worker-side allowance alone is insufficient.
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
- Keep `POLICY_KEYPAIR` as the delegated policy signer and default route fee
  payer. Optional route fee-payer sharding is not required for PASS. If it is
  enabled later, fee payers are low-balance fee-only keys with no vault or ALT
  authority, explicit budgets, and deterministic shard assignment.
  `POLICY_KEYPAIR` remains the reusable ALT authority/payer; a route fee-payer
  pool does not replace its policy signature.
- Mount signing material only where it is used. The planner uses the standard
  public policy identity, and the confirmer/reconciler consume already-signed
  evidence; none of those roles loads a private key. Revalidator/executor roles
  may load POLICY and fee-only route shards for fused execution, while the ALT
  provisioner loads POLICY only and never receives route-shard keys.
- When enabled, the fee-only pool is opt-in twice: public keys and durable limits live in
  `loyal_yield.route_fee_payer_shards`, while matching keypairs are mounted from
  `YIELD_ROUTE_FEE_PAYER_KEYPAIRS` through the standard 1Password environment.
  Missing, malformed, disabled, role-conflicting, over-budget, or out-of-range
  configuration falls through ranked rendezvous candidates and finally to
  `POLICY_KEYPAIR`; key material never enters SQL, status output, or logs.
- When enabled, a shard is eligible only for a queue-backed same-mint move whose source and
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
  `policy-setup-funding:<POLICY signer>` reservation until the funding spend is
  authoritatively confirmed or the route is authoritatively proven to have had
  no effect. Mere broadcast, submission, confirmation RPC failure, or
  ambiguous effect is not sufficient to release it.
  At confirmed reconciliation handoff, both this reservation and the bounded
  shared-write lane are released atomically while vault and real writable
  locks remain through terminal reconciliation. It must not remain a
  fleet-wide mutex through unrelated post-state projection. A durable rolling
  funding reservation and balance floor prevent overspend while multiple
  confirmed setup routes reconcile independently.
- When fee-payer sharding is enabled, the exact compiled fee and a fresh shard balance observation are admitted in
  the same SQL transaction as immutable signed bytes. A per-key row lock,
  balance floor/ceiling, per-transaction cap, and rolling spend reservation
  make concurrent budget admission deterministic. Reservation races leave the
  opportunity retryable. The payer selected during revalidation is durably
  bound to its canonical manifest fingerprint; if it becomes unhealthy, the
  opportunity returns to the short revalidation lane before a fresh ranked
  candidate or POLICY fallback publishes a new matching fingerprint. Budget
  races never create a decision-less signed route or reuse a mismatched
  manifest.
- When fee-payer sharding is enabled, reciprocal database triggers reject a fee-only key that is already a policy,
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
- The capacity input and immutable signed submission carry the exact current
  durable optimizer-epoch ID used for revalidation. Signed evidence also pins
  that epoch's fingerprint and expiry; it is a verifier failure if execution
  evaluates one full-universe epoch while persisting another.
- One opportunity attempt generation owns one immutable signed transaction and
  signature. Bounded retries may rebroadcast those exact bytes/signature while
  the blockhash is valid; `broadcast_count > 1` is not itself a duplicate. A
  newly signed or byte-distinct replacement is forbidden until expiry plus
  authoritative absence/effect checks prove it safe.
- `sendTransaction` acceptance does not occupy the executor until confirmation.
  Confirmation uses subscription plus batched status fallback.
- Every signed/submitted/ambiguous row has an explicit current owner or durable
  recovery deadline. Fresh in-flight rows are expected in a continuously moving
  fleet; rows older than their route-class SLO without confirmation,
  authoritative absence/effect proof, or a fenced retry are silent failures.
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
  outcomes; deposit-to-position, deposit-to-outcome, and deposit-to-submit
  latency; warm versus ALT-cold/setup route latency; per-reserve projected and
  observed net flow/APY; allocation opportunity gap; round trips; ALT operations
  per unlocked dollar; and fee per incremental-yield dollar.
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

- every active policy-eligible vault is in the denominator, including a vault
  whose position or mint evidence is stale; every executable candidate is bound
  to a non-expired planner epoch for its route mint;
- the input position projection is chain-backed, slot-fenced, and within its
  declared freshness bound for the whole captured cohort; stale rows from a
  partial/old serial pass cannot count toward completeness;
- the fixed-cohort production sweep completed in less than 600 seconds with
  `eligible = processed = refreshed`, `failed = 0`, and `stale = 0`, and the
  immediately following exact-scope collector reported `staleRowCount = 0`.
  This is required for final fleet completeness but is not a precondition for
  separately fresh high-value routes to execute while the sweep progresses;
- completeness starts from the authoritative eligible-vault denominator and
  assigns every vault exactly one mutually exclusive outcome: observed
  opportunity, active queue/decision state, stale position, incomplete
  route-mint evidence, `market_lifetime_deferred`, no positive current source,
  missing valuation, unsupported amount/market semantics, or no economic
  target. Every blocker has state age and recovery action. The outcome total
  must equal the denominator, and active outcomes must agree with the
  queue-state breakdown;
- current-fleet planning p95 is under 5 seconds;
- a 10,000-vault in-memory/captured replay completes under 10 seconds;
- output is ordered by economic priority, not vault ID;
- the reported top-value cohort contains no lower-priority job ahead of a
  higher-priority non-conflicting job;
- discovery spawns zero child route/reconcile processes.
- the planner is the only runtime optimizer-epoch writer: a concurrent
  revalidator/executor wave does not change epoch count or maximum ID, keeps the
  opportunity's bound epoch in decision/submission evidence, and records fresh
  mint-scoped revalidation separately;
- unchanged same-mint economics and unrelated-mint expiry/churn do not stale a
  route, while a material same-mint change does so before decision creation;
- a pre-existing optimizer row with the same raw market fingerprint but older
  expiry semantics cannot cause an immutable-evidence collision or be reused as
  the current semantics version.

Record hardware, fleet size, epoch, and timings with the verdict.

### Check 3: Economic behavior

Using deterministic planner inputs, PASS only if:

- increasing notional at equal edge increases priority;
- increasing net edge at equal notional increases priority;
- a smaller account with greater lost-yield rate can outrank a larger account;
- age eventually prevents starvation;
- cost/holding-horizon gating rejects negative-value and dust movements;
- capacity-aware waves stop admitting a target after marginal edge disappears;
- a deterministic two-or-more-pool declining-yield fixture routes early flow to
  the initial peak, then routes later vaults to another pool when post-flow
  marginal APYs cross, producing a split rather than sending the fleet to one
  point-estimate peak;
- source outflow and target inflow are both projected, pending/landed flow is
  counted once, and a second planning pass over the projected final state finds
  no feasible whole-vault move above the economic/hysteresis threshold. The
  result matches an independent constrained reference allocation within one
  indivisible-vault/wave tolerance;
- small alternating APY noise produces zero round trips, while a material
  persistent change eventually moves. Any `A -> B -> A` inside the expected
  dwell window is backed by a named exogenous frontier change and positive
  cumulative gain after both measured costs;
- Timescale history or the documented conservative fallback determines
  persistence/confidence rather than assuming every point APY lasts a fixed 30
  days. Predicted post-flow supply/APY is compared with an attributed post-slot
  observation, and excessive error reduces the next wave's confidence/size;
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
- after one production-sized 64-row warmup for each cohort, adding 10,000
  ALT-cold jobs changes ready-claim p95 by less than 5%;
- no decision exists for `waiting_alt` work;
- satisfying coverage writes a durable wakeup and makes only affected valid jobs
  eligible immediately, without another fleet cycle;
- one real/captured ALT-cold route proves predecision defer, one idempotent typed
  request, best-fit packed reusable-v2 allocation/extension, verified coverage,
  and successful next-cycle retry without a legacy table or silent loss;
- the complete logical shared-market family is warm and verified, normal vault
  demand reuses packed multi-vault headroom before allocating a shard, and live
  packing/operation-per-unlocked-dollar metrics stay within their declared
  bounds;
- the deployed resolver has no legacy fallback and the imported legacy fleet
  remains zero-reference, closed, and transaction-locally refunded to
  `POLICY_KEYPAIR` as already established by the reusable-ALT migration
  verifier;
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
- an already persisted signed transaction is rebroadcast byte-for-byte under
  the same opportunity generation/signature. Multiple bounded same-byte sends
  remain one semantic movement, while any second signature/replacement without
  expiry and authoritative absence/effect proof is a duplicate failure;
- an ambiguous or stale post-confirm read cannot create a replacement movement;
- target-capacity reservations survive reconciliation until target telemetry
  crosses the landed slot, release on authoritative pre-send failure, and
  reject stale reservation fences;
- the bounded accrued-amount fixture proves that a fused `same_mint` reserve
  route accepts an exact amount or a positive increase of at most 100 ppm and
  atomically binds the fresh amount into the decision and signed handoff. It
  must also prove that negative drift, more than 100 ppm, idle-route drift, or
  any source snapshot, collateral-share, route-identity, amount-semantics,
  economic, or execute-fence mismatch creates no decision or signed handoff;
  the migration-30 database trigger must reject the same invalid cases even if
  a caller bypasses worker validation;
- proven no-effect terminals can create exactly one next immutable attempt,
  while success and ambiguous sends cannot; shared reserve cache misses are
  singleflight rather than one RPC request per concurrent route;
- persisted fee-payer kind is immutable, and an authoritative landed failure
  retains its confirmation slot/time for coherent fee-floor accounting;
- standard `POLICY_KEYPAIR` signs policy execution, pays routes by default, and
  remains ALT authority/payer. If optional route-fee sharding is enabled, every
  distinct route fee payer has fee-only authority and budget evidence;
- if route-fee sharding is enabled, every sharded route is recompiled with the shard as fee payer and
  `POLICY_KEYPAIR` as a second static signer; its final manifest, ALT coverage,
  packet size, simulation, compiled fee, and persisted hashes all describe
  that exact final transaction rather than an earlier POLICY-payer build;
- setup/idle/farm-init paths select POLICY. Only when optional sharding is
  enabled must a mature-route shard fixture prove exact registry/keypair
  matching, reciprocal authority separation, bounded ranked failover,
  low-balance limits, and one atomic immutable spend reservation;
- missing-obligation execution proves the exact capped rent deficit, atomic
  withdraw/fund/init/deposit order, final manifest and simulation coverage, and
  bounded setup-funding reservation. The common funding resource is released
  after authoritative confirmation or authoritative no-effect proof and is
  not held through unrelated position projection. Ambiguous or
  non-authoritative confirmation failure retains it. Funding/RPC failures retry without a
  decision or send; deterministic simulation failures are terminal; only
  genuine reusable-v2 coverage gaps enter `waiting_alt`.

The executable verifier must additionally emit these two named subchecks from
real planner-shaped opportunity payloads rather than caller-supplied verdicts:

- `planner_executor_source_evidence_is_kind_scoped`: both source kinds retain
  generic `source_observed_slot`/`source_observed_at` in their immutable planner
  plan, but a `reserve_position` request is fenced only by its positive source
  snapshot and has all three executor `expected_idle_*` fields null. An
  `idle_vault_usdc` request has no source reserve/snapshot and maps the exact
  planned idle token account, observed slot, and observed time into those three
  fields. An idle account contaminating a reserve plan still fails closed.
- `predecision_source_contract_failure_creates_one_immutable_retry_generation`:
  reproduce the historical reserve-source terminal reason
  `same-mint reserve-position request cannot carry idle-vault evidence` before
  any decision, signed submission, unreleased capacity reservation, or conflict
  lease exists; then run two concurrent rediscoveries after the corrected
  mapping. The failed row's idempotency key, rediscovery key, generation,
  execution plan, failed state, terminal reason, and post-failure `updated_at`
  remain unchanged, while exactly one distinct successor with the same
  rediscovery key and generation `n + 1` reaches the current
  revalidation/runnable path. Reopening the failed row or retrying
  submitted/ambiguous work is FAIL.

### Check 6: Performance, value, and price

PASS only if a production-like replay reports:

- material chain-observed deposit p95 reaches a current durable outcome within
  30 seconds; a warm executable deposit is submitted within the existing
  10-second discovery gate, while an ALT-cold/setup deposit records its blocker
  and recovery owner within 30 seconds;
- warm high-value opportunity: p95 submitted within 10 seconds of discovery;
- warm confirmed route: p95 confirmed within 30 seconds of discovery, excluding
  an explicitly recorded cluster outage;
- ALT backlog has less than 5% effect on warm-route p95;
- at least 90% of recoverable yield dollars/hour is submitted within 2 minutes
  and 99% within 10 minutes, subject to explicit capacity/conflict ceilings;
- the recoverable-yield denominator is independently recomputed from all
  eligible positions and the constrained marginal reference allocation, not
  only from rows the planner chose to publish;
- the material ALT-cold and first-obligation/farm-setup cohorts meet the
  ten-minute unlock gate without a fleet-wide provisioning or setup-funding
  mutex; every miss has a named current external blocker;
- projected allocation reaches the discrete marginal fixed point, measured
  portfolio opportunity gap decreases, and round trips without a material
  frontier change are zero;
- fee and priority-fee spend stays below the configured fraction of expected
  incremental yield; negative-value routes are zero;
- database deadlocks and duplicate movements are zero.

### Check 7: Production wiring and short feedback loop

The required durable functional roles are planner, revalidator, executor,
confirmer, reconciler, and priority ALT provisioner. They may run as six
processes or use a safely fused role where leases, signer boundaries, health,
and independent scaling remain explicit. The light-worker image must contain
every owning binary used by the pinned Render Blueprint, which must replace the
serial fleet monitor for production execution. Status queries identify a stuck
market epoch, ready queue, ALT queue, sender, confirmer, or reconciler
immediately; emitted output must identify it within one separately declared
health-observation interval. The durable recovery poll and health-observation
cadence must not be conflated.

The same health snapshot must expose a bounded top set of physically shared
writable keys across active submissions, including active route count and
economic value plus fee-payer/target/other classification. A configured number
of semantic lanes without this physical congestion evidence is insufficient.

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
standard POLICY signer, with the optional fee-only shard pool absent unless the
conditional sharding checks pass; and the ALT provisioner receives POLICY plus
an explicit rolling lamport budget but never the route-shard pool. Recovery
polls and concurrency/batch bounds must be explicit in the commands so the
feedback-loop and spend posture are reviewable without relying on hidden binary
defaults.

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
- the complete logical shared-market bundle remains warm and exact, vault
  manifests use packed reusable-v2 headroom on demand, and no ordinary vault or
  route causes an exact-scope/dedicated table except a measured outlier;
- a create is not followed by extension/allocation until finalized RPC proves
  that the table exists with the ALT program owner and standard authority.
  Provisioner health proves enough unreserved POLICY balance for the admitted
  rent/fee work and raises a named funding blocker before creating phantom
  state;
- no new legacy or exact-route ALT is created, the deployed resolver has no
  legacy fallback, every imported old ALT remains zero-reference/closed with
  its recorded transaction-local refund to `POLICY_KEYPAIR`, and the standard
  policy account remains within the explicit rolling lamport budget.

Historical `permanent_failure` rows are expected audit records and are not
deleted to manufacture PASS. Any live extension attempt against a phantom
table, raw operator SQL repair, missing successor lineage, or unresolved active
terminal dependency is FAIL.

### Check 9: Production migration and atomic executor cutover

PASS only if live readback proves:

- the production migration ledger contains migrations 23 through migration 30
  with repository-matching names and checksums; migration 29's deferred
  DB-clock constraints select the base opportunity/submission row first so only
  genuine row deletion—not a missing or cross-cluster join—can take the cleanup
  path. An active opportunity must reciprocally reference an optimizer epoch in
  the same cluster. A `signed`/`submitted` handoff must reciprocally reference
  the same-cluster `decision_created` opportunity and decision ID, and its
  `optimizer_epoch_id` must equal that opportunity's same-cluster epoch. Both
  opportunity and epoch must have at least 60 seconds of DB-clock lifetime at
  commit. Any later or terminal submission state reactivation into `signed` or
  `submitted` is rejected even when `decision_id` is unchanged. Genuine row
  deletion and terminal cleanup remain legal. The ordinary already-broadcast
  `signed` -> `submitted` bookkeeping transition remains legal without
  re-running the signing-lifetime fence, including after the 60-second signing
  threshold has passed. Migration 30 permits only a `same_mint` decision's
  positive redeemable-liquidity accrual of at most 100 ppm over its leased
  opportunity amount and keeps every other leased identity/economic field
  exact; zero/negative drift, over-bound drift, another route kind, or another
  changed field must fail the atomic decision/submission link;
- Timescale migration 5 has its exact repository checksum, the heavy Kamino
  monitor (`srv-d8h4i9a8pkls73bver00` in
  `evm-d8kgt3a8qa3s7382glc0`) is a live unsuspended image worker with the exact
  Blueprint command, single image-contained migration-before-sync predeploy
  executable, plan, env-key boundary,
  and `loyal-ghcr` digest, and its LaserStream tag names the same source commit
  as the six-role light-worker tag;
- normal monitor startup and refresh prove a nonempty exact active
  `(market, liquidity_mint, reserve)` topology before atomic publication; the
  refresh interval is <= 180 seconds, active catalog age is <= 300 seconds, and
  the Render predeploy uses explicit sync without the removal override. Any
  observed removal requires a separately authorized invocation carrying both
  explicit-sync and removal flags;
- source-bound controlled-database evidence proves that partial/topology-changed
  catalog responses leave rows and timestamps untouched; every floor writer
  shares the reserve advisory lock; overlapping absent-row bootstrap writers
  serialize; and an equal-slot conflict, below-floor read, or stream-first
  no-pointer conflict cannot use same-read fallback. The first such conflict is
  deferred and revokes routability. An equal-slot/no-pointer candidate may be
  admitted only by a later independent exact read; a below-floor candidate
  remains blocked until a later read reaches or exceeds the durable floor;
- every reserve admitted for one mint's routing has exactly one current
  pointer, one valid durable observation floor, one matching confirmed
  verification, and one exact latest-view row; floor observation
  ID/source-rank/hash validity, event/hash/`observed_at`, independently
  allowlisted sources, slot, commitment, future-time, floor-admission, and
  hard-expiry error counts are zero for that mint. Each mint's catalog
  denominator is complete and cannot be reduced to the verified subset; every
  included reserve has economic slot lag <= 1,500, hard verification age <=
  240 seconds, and catalog, verification, and raw-supply-derived economic
  expiry all more than 60 seconds beyond the database capture clock. An
  incomplete mint is named and non-routeable, but it cannot block a complete
  unrelated mint or shorten that mint's opportunity lifetime;
- one immutable GHCR light-worker tag/digest for the verified commit exists and
  every deployed functional role uses it with the required commands and
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

PASS only if a fresh production observation window and its durable queues prove:

- every active policy-eligible vault/source is present in exactly one mutually
  exclusive current outcome, including explicit stale-position,
  incomplete-mint, or mint-lifetime blockers; unaccounted, silently skipped,
  and `not_evaluated` counts are zero;
- every executable route's bound planner epoch is complete and unexpired for
  its mint, the counted outcomes sum exactly to the eligible fleet, and no
  result is inherited from the old serial cursor. Different complete mints or
  continuously refreshed cohorts may use different current epoch IDs;
- the durable planned frontier is the post-economic, post-fee work admitted
  for queue draining, and its count equals published/selected plus explicitly
  deferred work; raw rejected route alternatives cannot inflate this counter;
- every material opportunity (at minimum the captured >= $1,000 cohort) is
  ready, leased/in flight, waiting on a named current ALT/capacity/conflict
  dependency, confirmed, or economically excluded—never silently absent;
- no lower-value nonconflicting route is submitted while a materially higher
  lost-yield-dollar opportunity is runnable and unleased; fairness aging may
  break near-ties but cannot hide the high-value cohort;
- `waiting_alt`, simulation failures, expired leases, and worker errors have
  bounded age and are decreasing or have an explicit fenced recovery action;
  no material vault remains stuck beyond ten minutes;
- every material deposit first observed during the window reaches a current
  outcome within the deposit SLO; warm work submits promptly, while ALT-cold,
  setup, stale-position, or incomplete-mint work retains a named blocker,
  durable wakeup/retry path, and age. No arrival disappears because it was not
  a member of an older fixed cohort;
- selected-but-unpublished mint-lifetime work and opportunities staled during
  revalidation each receive a durable successor or named current exclusion;
- every still-economic rediscovery key with a historical attempt that failed
  before decision creation has exactly one current highest-generation successor
  (or a named current economic exclusion), while every failed row remains
  immutable; historical failure is audit evidence, not permission to leave the
  vault unevaluated;
- aggregate outcome counts and USDC amounts are emitted every health interval
  so regressions are visible without reconstructing the fleet manually.

### Check 11: Correct production movement and reconciliation

PASS only if post-cutover production evidence proves:

- real optimizer-created routes—not a manual operator transaction—have
  finalized signatures and positive net edge after measured fees;
- each selected target was the best marginal expected-net-yield placement for
  that whole vault after risk/mint constraints, projected source outflow,
  target inflow, pending/landed reservations, fees, uncertainty, and hysteresis
  in the opportunity's immutable route-mint epoch. The highest displayed APY is
  not sufficient evidence;
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
- the observation window's constrained portfolio opportunity gap falls by the
  confirmed optimizer improvement after separately accounting for deposits,
  withdrawals, and unrelated reserve flow. Main net outflow is reported as an
  incident metric but may be zero or negative when Main is part of the correct
  marginal split;
- the Main net-flow equation counts same-mint Main source moves as outflow and
  both same-mint and idle-vault Main targets as inflow; reserve and idle route
  counts remain separately visible;
- the observation-window artifact contains the full frozen opening eligible cohort ID set,
  including eligible zero-Main vaults. The aggregate equation uses only that
  set on every term: opening routeable Main plus in-window cohort deposits minus
  final cohort Main equals confirmed cohort optimizer Main net outflow within
  the declared tolerance. This proves flow accounting, not that Main must be
  emptied. Final cohort Main is summed for the frozen IDs regardless of their
  later active-policy state, and the net-flow term excludes movements belonging
  to vaults that first became eligible after capture;
- all post-cutover movements remain individually verified even when their vault
  is outside the frozen aggregate cohort; excluding a newly eligible vault from
  the Main equation must never exclude its signature, economics, selected
  target, source/target delta, or reconciliation evidence from this check;
- post-move reserve distribution, projected and observed supply/APY, and
  attributed Loyal net flow prove that the fleet approaches the independently
  recomputed discrete marginal fixed point. No feasible whole-vault move above
  the economic/hysteresis threshold remains silently absent, and unexplained
  prediction error triggers a smaller/lower-confidence next wave;
- round trips inside the expected dwell window are zero unless each carries a
  material intervening frontier change and positive cumulative expected gain
  after all associated fees;
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

# A complete implementation verdict also consumes a fresh source-bound v2
# artifact from the controlled planner/ALT/RPC/replay harnesses.
cargo run -p loyal-yield-orchestrator --bin fleet-orchestration-runtime-evidence -- \
  --image ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<COMMIT> \
  --heavy-image ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-<COMMIT> \
  --output <PATH>
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

### Runtime evidence schema v2

`--runtime-evidence-json` accepts measurements, never caller-supplied verdict
strings. The verifier rejects unknown fields, any `schemaVersion` other than 2,
an artifact older than one hour, a different checkout HEAD, or a different
SHA-256 digest of the current runtime inputs. Obtain `headCommit` and
`runtimeSourceDigestSha256` from the verifier's repository-evidence output.

The camel-case JSON object contains:

```text
schemaVersion, headCommit, runtimeSourceDigestSha256, capturedAt, hardware

discovery:
  fleetSize, eligibleCurrentVaults, accountedVaults,
  vaultOutcomesByReason, activeExclusionsByState, plannerOptimizerEpochId,
  optimizerEnvelopeExpiresAt, mintCoverageAndExpiresAt,
  optimizerEpochSemanticVersion, planningSampleEpochProofs,
  planningSampleCount,
  planningP95Milliseconds, replayVaultCount, replayMilliseconds,
  economicallyOrdered, topCohortHasNoNonconflictingPriorityInversion,
  childRouteOrReconcileProcessesSpawned,
  optimizerEpochCountBeforeWorkerWave, optimizerEpochCountAfterWorkerWave,
  optimizerEpochMaxIdBeforeWorkerWave, optimizerEpochMaxIdAfterWorkerWave,
  silentOrUnclassifiedOutcomeCount

planningSampleEpochProofs[]:
  plannerOptimizerEpochId, routeMint, routeMintExpiresAt,
  observedOpportunityEpochIds, selectedOpportunityEpochIds,
  freshRevalidationEvidencePersistedSeparately,
  unrelatedMintChurnPreservedBoundEpoch,
  materialSameMintChangeStaledBeforeDecision

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
  sameTablePredecessorViolations, staleFenceCommits,
  sharedMarketLogicalAddressCount, sharedMarketPhysicalShardCount,
  packedVaultCount, packedPhysicalShardCount, dedicatedOutlierCount,
  firstColdAttemptDecisionCount, sealedTypedRequestCount,
  coldRetrySucceeded, legacyResolverSelections,
  importedLegacyOpenCount, verifiedLegacyRefundCount

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
  sourceEvidenceContractFixtures,
  targetCapacityConcurrentAdmissionBounded,
  preSendTargetCapacityReleased,
  reconciledCapacityStrictTelemetryFence,
  preexistingNewerTelemetryRelease,
  sameGenerationSignatureCount, boundedIdenticalRebroadcastCount,
  distinctReplacementWithoutAbsenceProofCount,
  setupFundingReservationReleasedBeforeProjection

replay:
  routeSampleCount, warmHighValueSubmissionP95Milliseconds,
  warmConfirmationP95Milliseconds, explicitlyExcludedClusterOutages,
  depositToOutcomeP95Milliseconds, coldOrSetupBlockerP95Milliseconds,
  warmBaselineP95Milliseconds, warmWithAltBacklogP95Milliseconds,
  independentlyRecomputedRecoverableYieldUsdMicrosPerHour,
  submittedWithinTwoMinutesYieldPpm,
  submittedWithinTenMinutesYieldPpm,
  projectedSourceAndTargetFlowApplied,
  referenceAllocationOpportunityGapUsdMicrosPerHour,
  finalAllocationOpportunityGapUsdMicrosPerHour,
  secondPassPositiveWholeVaultMoves,
  unexplainedRoundTripsInsideDwellWindow,
  attributedPredictionErrorPpm, nextWaveSizeReducedOnExcessError,
  configuredMaxFeeFractionPpm, observedMaxFeeFractionPpm,
  negativeValueRoutes, databaseDeadlocks, duplicateMovements

wiring:
  probedContainerImageReference, localContainerImageId,
  lightRegistryIndexDigest, lightLinuxAmd64ManifestDigest,
  lightProvenanceVcsRevision, lightProvenanceVcsSource,
  probedHeavyContainerImageReference, heavyRegistryIndexDigest,
  heavyLinuxAmd64ManifestDigest, heavyProvenanceVcsRevision,
  heavyProvenanceVcsSource,
  runnableRoleProbeExitCodes,
  recoveryPollIntervalMilliseconds, healthObservationIntervalMilliseconds,
  stuckStageDetectionMilliseconds
```

The verifier recomputes completeness totals, backlog effects, and every numeric
threshold from these measurements. The wiring maps must contain every durable
functional role and the six stuck stages named in Check 7; safely fused roles
must still expose separate leases, health, and signer boundaries. Every local-container
probe must exit zero and every stuck stage must be detected within the
recorded health-observation interval. The light and heavy references must
exactly equal the immutable GHCR Blueprint references from one commit. The
collector hashes each raw OCI index, selects exactly one linux/amd64 manifest,
and records the SLSA `vcs:revision` and `vcs:source`; Render must report those
exact platform-manifest digests. The provenance revision must equal the tag
commit and the source must be the Loyal repository. That commit may equal
checkout HEAD, or be its ancestor only when the complete diff to HEAD is
limited to `render.yaml` and this verifier document. Probing an unrelated or
mislabeled image is FAIL. A
complete source-bound artifact can move Checks 2, 4,
5, 6, and the runtime portion of 7 to PASS; absence leaves them `NOT RUN`, and
invalid or threshold-breaking measurements produce `FAIL`.

### Production evidence schema v2

The production collector emits one source-bound object and never accepts
caller verdicts. The verifier rejects unknown top-level, scope, source,
measurement, market-data-plane, Render, migration, relation, safe-target, and
deploy fields. Embedded `pass`, `matches`, and `recomputedVerdicts` values are
operator feedback only and are ignored.

The top level includes `collectionStartedAt`, `collectedAt`, and `capturedAt`.
Collection must finish within 300 seconds, the artifact must be no older than
120 seconds, and its Render, market, and queue captures must each be no more
than 90 seconds behind final `capturedAt`. Source evidence includes hashes of
the compiled collector source, checkout collector source, and executing
collector binary; the verifier recomputes the source hash and requires the
artifact to come from its current sibling collector executable.

In addition to the existing Render, Neon queue/position/movement, and ALT
repair measurements, `measurements.marketDataPlane.timescale` contains exactly:

```text
available, capturedAt, migration, relations, enabledStableMints,
activeDistinctSupportedReserveCount, activeSupportedReserveCatalogRowCount,
activeSupportedReserveIdentityFingerprint, enabledMintCoverage,
duplicateActiveSupportedReserveCount,
nonKaminoApiActiveSupportedReserveCount,
staleActiveSupportedReserveOver300SecondsCount,
oldestActiveSupportedReserveFetchedAt,
oldestActiveSupportedReserveAgeSeconds, currentPointerCoverageCount,
verificationCoverageCount, exactLatestViewCoverageCount,
eventHashObservedAtIdentityViolationCount,
verificationStateIdentityViolationCount, latestViewIdentityViolationCount,
stateSlotGreaterThanVerifiedSlotCount,
immutableTapeExactRowCardinalityViolationCount,
latestViewRowCardinalityViolationCount, nonConfirmedCommitmentCount,
observationFloorCoverageCount, observationFloorIdentityViolationCount,
observationFloorFutureObservedAtCount,
staleObservationFloorOver90SecondsCount, invalidObservationFloorStateCount,
currentStateBelowObservationFloorCount,
atOrBelowFloorExactHashAdmissionCount,
verificationAtOrBelowObservationFloorWithoutExactHashCount,
conflictingAtOrBelowFloorRoutableStateCount, nonHttpCurrentStateCount,
nonHttpVerificationSourceCount, futureCurrentStateObservedAtCount,
futureVerificationWatermarkCount,
warningOver90SecondsCount, hardExpiredOver240SecondsCount,
economicSlotLagOver1500Count, economicExpiryAtOrBelow60SecondsCount,
rawStableSupplyValuationViolationCount,
oldestVerificationAgeSeconds, coverageQueryMilliseconds,
safeTargetQueryMilliseconds, topVerifiedSafeTargets, readError, pass
```

The enabled-mint universe is code-owned and collector-local environment
variables cannot silently shrink it. Coverage and lifetime verdicts are
computed independently per mint: a missing reserve cannot be removed from that
mint's denominator, while an incomplete quiet mint cannot block a separate
complete mint. Migration evidence carries version/name plus repository and
applied checksums.
Relation evidence names the ledger, supported-reserve table, immutable tape,
current pointers, observation-ID sequence, durable observation floors,
verification watermarks, and exact latest view. Each enabled mint has exactly
one safe-target summary with its safe risk basket, reserve/market,
APY/liquidity, non-stale flag, event/hash/state coordinates, confirmed
verification coordinates, and complete floor observation ID/slot/hash/
validity/source-rank coordinates. The verifier recomputes the exact floor
admission rule instead of trusting embedded verdicts.
`enabledMintCoverage` contains exactly one entry per enabled mint with catalog,
verified, and eligible-target counts; completeness; named blockers; and the
minimum catalog/verification/economic expiry. It must prove that catalog count,
not the verified subset, is the denominator. Each safe target also carries raw
decoded supply, code-owned stable decimals/price, recomputed USD capacity,
reserve last-update slot, economic lag, and economic expiry so the verifier can
independently enforce the 1,500-slot, 240-second, and > 60-second gates.

The Render measurement additionally contains the fixed heavy environment and
monitor identity, exact live/Blueprint env-key sets, redacted scope-comparison
result, live deploy metadata, the effective supported-reserve refresh interval,
and the exact image-contained predeploy executable path. Source-bound local
evidence verifies that this executable applies migrations before reserve sync
and that the removal override is absent from normal/predeploy commands. The
measurement also contains both image tag commit suffixes and their exact
same-source comparison. It never emits
environment values. Instead, a
capture-specific nonce and per-key salted hashes let the verifier recompute the
exact local/Render data and signer scope; embedded boundary booleans are
ignored. The light environment is fixed to `evm-d8kgt4r7uimc73b1ul1g`, and
the local `POLICY_KEYPAIR` must derive the standard policy pubkey.

The seven live planning latency samples may observe successively newer planner
epochs; requiring the market to stop updating during measurement would test a
frozen feed, not planner correctness. Every selected opportunity must match its
sample's planner epoch and carry a usable route-mint expiry. Revalidators retain
that bound ID while recording fresh mint-scoped evidence separately. The
artifact records per-mint completeness/lifetime and p95 across all samples.

`measurements.portfolio` additionally contains independently recomputable raw
inputs and outputs rather than a planner-authored verdict:

```text
openingEligibleVaultIds, arrivalVaultIds, openingPositionsByReserve,
closingPositionsByReserve, externalDepositsAndWithdrawals,
pendingAndLandedOptimizerFlows, candidateWholeVaultMoves,
referenceAllocationByReserve, actualAllocationByReserve,
openingOpportunityGapUsdMicrosPerHour,
closingOpportunityGapUsdMicrosPerHour,
projectedAndObservedReserveResponses, roundTripsInsideDwellWindow,
depositToOutcomeSamples, staleOrBlockedOutcomeAges
```

Each projected/observed response binds the opportunity/decision/submission,
mint, source/target reserves, signed amount, confirmation slot, pre-flow
supply/APY, projected post-flow supply/APY, first sufficiently fresh Timescale
post-slot observation, and unrelated-flow residual. The verifier independently
recomputes the constrained marginal allocation, flow conservation, model error,
round-trip economics, and deposit/outcome SLOs.

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
local-container binary probes, structured functional-role Blueprint validation,
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

## Latest Evidence Run: 2026-07-16

This is the literal verifier verdict from facts recorded during the current
local-production iteration. It is not a fresh source-bound v2 collector
artifact and therefore cannot manufacture a PASS.

### Current local-production checkpoint

- Production migration 30 is applied and real optimizer-created movements have
  exercised the bounded accrued-amount decision/submission link.
- Corrected local planner/worker waves through signed submission `27` produced
  approximately `$20,149.27` of reconciled movement after the epoch-writer fix,
  in addition to earlier material movements. This amount is a checkpoint from
  recorded wave queries, not a fresh aggregate collector result.
- Submission `37` safely rebroadcast the same immutable signed bytes/signature
  twice before confirmation. It created no second decision, signature, or
  economic movement. This is idempotent retry evidence; a broadcast count above
  one is not itself a duplicate.
- The observed worker-self-staling incident was concrete: routes bound to
  optimizer epoch `3308` were invalidated when concurrent workers published
  sibling epochs `3309`, `3317`, `3319`, and `3320`. The current source removes
  optimizer-epoch publication from route workers, introduces an optimizer
  envelope plus mint-scoped lifetime/frontier checks, and bounds route RPC calls.
  The combined current revision has not yet produced a clean source-bound v2
  artifact or immutable deployed image.
- The reusable-only migration and legacy ALT refunds previously passed their
  dedicated verifier, and the active v2 provisioner has since satisfied several
  material requests. A fresh finalized-RPC/current-database collector is still
  required to prove that phantom dependencies, packing, shared-family coverage,
  and legacy-refund invariants remain clean now.

These facts prove repeated real movement and one safe same-byte rebroadcast.
They do not prove the current code is deployed, every vault/deposit has a
current outcome, the fleet has reached a marginal split equilibrium, or the
production SLOs pass.

The current check table is:

| Check | Verdict | Evidence / first missing invariant |
| --- | --- | --- |
| 1. Repository and migration integrity | FAIL | The worktree contains the current uncommitted implementation and verifier changes. No clean source-bound schema-v2 artifact for the combined planner-only/mint-scoped revision has passed. |
| 2. Fast complete discovery | FAIL | Planner-only epoch publication has real incident motivation and local movement evidence, but a current artifact has not proved the all-active denominator, explicit stale/incomplete-mint outcomes, semantic-version collision handling, or a worker wave with zero epoch writes. |
| 3. Economic behavior | FAIL | Earlier capacity/dilution fixtures passed, but the required source-outflow plus target-inflow projection, independent split-equilibrium reference, historical persistence, and anti-round-trip checks are new and not run. The current fixed 30-day horizon/simple target-inflow model cannot substitute. |
| 4. ALT head-of-line isolation | NOT RUN | Prior source-bound evidence proved warm work drains beside 10,000 cold jobs. Schema-v2 demand-to-packed-retry, current shared-family/packing metrics, and legacy-refund non-regression have not been freshly collected. |
| 5. Execution concurrency and crash safety | FAIL | Repeated real routes and submission 37 prove semantic same-byte rebroadcast safety, not `broadcast_count = 1`. The combined current revision, semantic-version fence, and bounded setup-funding reservation/release path have not all passed; setup work remains a known throughput constraint. |
| 6. Performance, value, and price | FAIL | Earlier warm replay numbers remain historical evidence. Deposit-to-outcome, cold/setup unlock, independent recoverable-yield denominator, marginal fixed point, prediction error, and round-trip gates are not measured in the current revision or deployed fleet. |
| 7. Production wiring and feedback loop | FAIL | The current planner-only/mint-scoped source is not in one immutable live light-worker deployment. Local production-driving processes are not the required durable Render cutover evidence. |
| 8. Production ALT damage recovery | NOT RUN | Historical repair and reusable-only refund evidence exists, and the active provisioner satisfied material demand, but no fresh finalized collector proves zero current phantom dependency, exact prefixes, packed reuse, sufficient funding, and legacy-refund non-regression. |
| 9. Production migration and atomic cutover | FAIL | Migration 30 is live, but the current source/image, compatible Timescale migration/image, per-mint market coverage, functional worker roles, and serial-executor exclusion have not passed one fresh deployment readback. |
| 10. Complete fleet evaluation | FAIL | Multiple material routes moved, but every active vault/source and new deposit is not yet proven in one current outcome. Unaccounted, silent mint-lifetime skip, stale-position, and overdue material-blocker counts are not all proven zero. |
| 11. Correct production movement | FAIL | Real reconciled movements and safe idempotent rebroadcasts exist. The fresh observation-window flow equation, independent constrained allocation, post-move marginal split, prediction feedback, round-trip, and fleet SLO evidence are not complete. |

Current scoped result: `IMPLEMENTATION: FAIL`; `DEPLOYMENT: FAIL`;
`PRODUCTION PERFORMANCE: FAIL`; `END STATE: FAIL`.
The previous implementation-only goal was prematurely complete. The executable
verifier must now support live deployment/performance evidence, and the goal
stays active until the production end state passes.

## Standing Codex Goal (under 4,000 characters)

```text
Run docs/plans/fleet-yield-orchestration-speed-verifier.md adversarially against the current loyal-yield-routing checkout and production, and make END STATE: PASS without weakening any check. IMPLEMENTATION: PASS alone is insufficient. Re-run the literal verifier after every material repair, migration, deploy, or runtime finding.

Current failures are binding. Finish the planner-only optimizer-epoch boundary,
mint-scoped lifetime/material-frontier revalidation, and semantics-versioned
epoch identity. Prove concurrent revalidators/executors do not publish epochs,
unrelated quiet mints do not stale valid work, material same-mint changes fail
before decision creation, and older global-minimum rows cannot collide with or
masquerade as current envelope semantics.

Make fleet economics converge to the constrained marginal optimum, which may be
a split across pools. Project both source outflow and target inflow, count
pending/landed flow once, use Timescale persistence/volatility or a documented
conservative fallback, and compare predicted post-flow APY with signed,
slot-attributed post-move observations. An independent reference allocation
must find no remaining positive whole-vault move above fees, uncertainty, and
hysteresis; unexplained short-horizon round trips are zero.

Keep reusable-v2 demand driven. The logical shared-market family stays complete;
vault data append-packs into reused multi-vault shards only when a real route
needs it. Missing coverage creates one predecision request and later retry,
never a legacy/exact-route ALT or silent loss. Verify current owner/authority/
prefix/funding state, zero active phantom dependencies, packed reuse, no legacy
resolver, and the previously closed/refunded legacy fleet. Use only standard
POLICY_KEYPAIR for policy execution and ALT authority/payer.

Publish immutable compatible light/laser images and deploy every durable
functional role with least-privilege envs. Stop/drain the serial executor before
the fleet executor can send; never dual-execute. The full fleet sweep is the
final completeness/backstop proof, not a gate on independently fresh high-value
routes.

Production PASS requires every active policy-eligible vault/source and every new
deposit in exactly one current outcome, including named stale/mint/ALT/setup
blockers with age and recovery; zero silent/unclassified outcomes; no material
vault stuck over ten minutes; and higher lost-yield work moving first unless a
real capacity/conflict blocker exists. Measure warm and cold/setup feedback
loops separately.

Movement PASS requires real optimizer-created finalized signatures, positive
net value after measured fees, correct marginal targets, source/target chain
deltas, and reconciliation at or above confirmation slots. One immutable signed
transaction/signature exists per attempt generation; bounded same-byte
rebroadcasts are allowed, but distinct replacement sends require expiry plus
authoritative absence/effect proof. The observation window must show a smaller
portfolio opportunity gap and meet the 2m/10m yield-unlock plus 10s/30s warm
submission/confirmation gates with zero semantic duplicates, deadlocks,
negative-value routes, unexplained round trips, oversubscription, stale active
decisions, or unresolved active ALT failures.

Follow AGENTS.md: keep binaries thin, reusable logic in owning modules, and avoid broad new Rust tests. Prefer targeted cargo check, migration verification, production SQL/RPC/Render evidence, and the executable verifier. Use op run with .env.1password; never print secrets or signed bytes. Production data repair, migrations, Render changes, deployments, and optimizer sends are authorized for this goal, but remain bounded by the verifier and fail closed on uncertain state.
```
