# Backyard Voltr orchestrator verifier

Status: approved target contract v3. This is the sole definition of done for
integrating the Backyard Voltr vault into Loyal's production Earn orchestrator.
The executable verifier still implements the earlier contract and must be
updated to this v3 contract before it can return a production PASS. This plan
supersedes the production-orchestration portions of the earlier four-market POC
verifier; that verifier remains historical proof of the on-chain Voltr/Kamino
graph and must not become a production runtime protocol.

The last v2 verifier baseline had infrastructure checks 1-5 passing and remained
`BLOCKED` at production activation. Under this changed v3 contract the current
route is `FAIL`: the live vault still has POC configuration and policies, the
TypeScript interval is stale, and the executable verifier does not yet bind the
new cap and fee fields. Historical Voltr-specific bridge/outbox code may remain
only when it is unreachable from the deployed production lane.

## Contract

**Objective:** At confirmed commitment on mainnet-beta, one Backyard Voltr USDC
route uses Loyal's existing fleet planner, opportunity lease/conflict fence,
`signed_route_submissions`, confirmer, and reconciler plus one in-process Rust
Voltr route adapter to:

1. allocate confirmed Voltr idle deposits into exactly Main, OnRe, Prime, or
   Maple;
2. restore aggregate pending-withdrawal demand before the fixed 600-second claim
   deadline;
3. evaluate ordinary allocation at least hourly and move only when the existing
   capacity-adjusted economics permit it; and
4. persist, submit, confirm, and reconcile at most one manager leg per vault at
   a time through the exact approved Squads policy, with the production policy
   ceiling `0 < amountRaw <= 200_000_000_000`; and
5. keep ordinary hourly optimization principal at or below
   `100_000_000_000` raw even though urgent restoration may use the full policy
   ceiling.

**Scope:** Production orchestration runtime, deployment, vault configuration,
Squads policy activation, and bounded confirmed-mainnet canary behavior. No new
Solana program and no Backyard frontend. The fixed activation target is a
`1_000_000_000_000` raw-USDC vault cap, a combined 500-bps performance fee,
600-second withdrawal wait, and 3,600-second normal optimization interval.
LP-token branding metadata is explicitly deferred and is not a readiness gate
for Backyard's first integration. Historical TypeScript POC code may remain
only when no deployed runtime invokes it. The activation wave updates
Voltr/policies, enables the exact route bundle, and runs the canaries.

**Verifier:** One read-only command and one structured verdict:

```sh
op run --env-file=.env.1password -- \
  cargo run -q -p loyal-yield-orchestrator \
  --bin fleet-orchestration-verifier -- \
  --backyard-voltr-end-state --json
```

The implementation adds the `--backyard-voltr-end-state` scope to the existing
fleet verifier. It discovers the sole compiled `voltr_kamino` route bundle and
its deployed binding; PASS mode accepts no caller-selected route or evidence
manifest. It may read repository/build identity, Render deployment metadata,
Neon state, and Solana RPC. It must not read a guardian key or invoke a sender.
Before the deferred Voltr activation, checks 1-5 are the infrastructure-ready
checkpoint and the overall verdict remains `BLOCKED` with resume condition
`install/approve the production bundle, enable the binding, and authorize the
smallest confirmed canaries`. This is a checkpoint in the same verifier, not a
second definition of done.

**External gates:** Voltr/admin and Squads-policy update approval, deployment
approval, an immutable worker image, funded production fee payer, and separately
authorized low-value manager plus user deposit/request/claim canaries. The
fixed fee split is `adminPerformanceFeeBps = 500` and
`managerPerformanceFeeBps = 0`; management, issuance, and redemption fees remain
zero. This routes the initial fee entitlement to the temporary manual admin
instead of requiring an additional Squads LP-fee transfer policy. The verifier
never grants transaction authority.

### Fixed activation configuration

| Setting | Exact value |
| --- | ---: |
| Vault cap | `1_000_000_000_000` raw USDC (1,000,000 USDC) |
| Squads policy amount ceiling | `200_000_000_000` raw USDC (200,000 USDC) |
| Ordinary hourly optimization principal ceiling | `100_000_000_000` raw USDC (100,000 USDC) |
| Withdrawal waiting period | `600` seconds |
| Normal optimization interval | `3_600` seconds |
| Locked-profit degradation duration | `86_400` seconds |
| Admin performance fee | `500` bps |
| Manager performance fee | `0` bps |
| Management, issuance, redemption fees | `0` bps |
| Base idle floor | `0` raw; active receipt demand is reserved dynamically |
| LP branding metadata | deferred; not a current readiness gate |

The 200k policy ceiling is deliberately larger than the 100k ordinary
optimization ceiling. The policy permits a fully invested 1M vault to restore
in at most five serialized legs. The controller still limits routine hourly
principal movement to 10% of the vault. Both directions remain pinned to the
same four approved Kamino graphs; a withdrawal can only return funds to the
vault's own idle account.

**Verdict:** `PASS` only when every required check below passes against current
authoritative evidence. `FAIL` identifies a false invariant. `BLOCKED`
identifies an unavailable external dependency and the exact resume condition.
Any `FAIL` wins over a `BLOCKED` result.

## Output and exit contract

```json
{
  "schemaVersion": 1,
  "verifier": "backyard-voltr-orchestrator-end-state",
  "verdict": "PASS | FAIL | BLOCKED",
  "commitment": "confirmed",
  "routeId": "...",
  "routeBundleSha256": "...",
  "sourceCommit": "...",
  "deployedImageDigest": "...",
  "observedAt": "...",
  "checks": [
    {
      "id": "...",
      "verdict": "PASS | FAIL | BLOCKED",
      "condition": "...",
      "evidence": {},
      "resumeCondition": null
    }
  ],
  "firstFailure": null,
  "blocker": null
}
```

- Exit `0`: `PASS`.
- Exit `1`: `FAIL`.
- Exit `2`: `BLOCKED`.
- Secrets, RPC URLs, database URLs, and environment values are never emitted.
- Caller-authored verdict fields, lifecycle manifests, and POC evidence JSON do
  not count as proof.

## Required checks in fail-fast order

### 1. `single_runtime_lane`

Prove that production uses one existing fleet lifecycle:

```text
opportunity -> lease/fence -> signed_route_submissions
            -> confirmer -> confirmed reconciliation
```

- The deployed execution target is Rust and has route kind `voltr_kamino`.
- Every active Voltr leg has one fleet opportunity/decision and exactly one
  `signed_route_submissions` generation. A later leg requires confirmed
  reconciliation plus a fresh observation; there is no durable sibling graph.
- The mandatory conflict key is `voltr:vault:<vault-address>`; at most one
  nonterminal submission exists for the vault.
- No deployed service invokes `tools/backyard-voltr`,
  `backyard-voltr-restoration-bridge`,
  `backyard-voltr-restoration-readback`, Phase-A/Phase-B files,
  `--enqueue-voltr-restoration-json`, or a Voltr-specific mutable outbox state
  machine.
- The existing fleet-worker binary may dispatch `voltr_kamino` to the new route
  adapter, but its direct-Kamino `same_mint` and `idle_vault_deposit` branches
  must reject Voltr opportunities. No child process or TypeScript handoff is
  permitted.
- Local source binding, immutable image digest, enabled route binding, and route
  bundle digest agree.

Historical files may remain in the tree, but reachable production wiring or a
second durable execution lifecycle is `FAIL`.

### 2. `one_leg_replan`

Invoke the real planner/domain API with independent deterministic oracles.
The small oracle preserves one-leg/replan semantics:

```text
restoration policy cap: 200,000 USDC
withdrawal demand: 120,000 USDC
idle: 0
Main available: 80,000 USDC
Prime available: 80,000 USDC
```

The only accepted sequence is:

```text
withdraw 80k
-> confirm and re-read
-> withdraw 40k
-> confirm and re-read
-> no withdrawal
```

The production-cap oracle starts with zero idle, 1,000,000 USDC of aggregate
pending demand, and enough safely redeemable liquidity across the four approved
strategies. It must produce exactly five independently confirmed and replanned
withdrawals of at most 200,000 USDC, followed by `NOOP`. It must not emit a
precomputed five-leg saga.

- Each planning cycle emits zero or one leg, never a sibling array or
  cancellation graph.
- The durable opportunity has an explicit closed operation class:
  `withdrawal_restoration`, `idle_allocation`, or `yield_optimization`.
  Restoration is admitted without fake APY, edge, annual-gain, or net-gain
  values; the existing table constraint and claim ordering are made conditional
  on that class.
- Multiple active receipts are aggregated before computing demand.
- While shortfall is positive, no deposit-allocation or normal-optimization
  task may be created or submitted.
- Duplicate observations cannot create a second active leg for the same vault
  generation.
- A new receipt scan supersedes an unsigned normal opportunity for the vault.
  A persisted or possibly broadcast signed submission is never cancelled or
  rebuilt; it is recovered first, then the vault is replanned.
- A completed leg marks the vault dirty; the next decision uses a confirmed
  observation slot at or after the previous transaction slot.

This check must falsify the historical `idleBefore + currentLegAmount`
shortfall calculation.

### 3. `route_and_packet_exactness`

Load one immutable production route bundle, generated at build time from the
pinned Voltr SDK and independently decoded by Rust.

- It contains exactly four strategies and eight `(strategy, operation)`
  templates for Main, OnRe, Prime, and Maple deposit/withdrawal.
- The bundle binds cluster, vault, LP mint, idle account, Squads
  settings/manager/guardian, eight policy PDAs, reserve/market/farm graphs,
  programs, deployment identities, 600-second withdrawal period, 3,600-second
  normal interval, configured idle safety buffer, exact
  `1_000_000_000_000` raw vault cap, exact 500-bps admin performance fee with
  every other owner fee zero, `200_000_000_000` raw policy ceiling,
  `100_000_000_000` raw normal-optimization ceiling, and its own digest. The
  adapter reads limits from the bundle; it does not hard-code POC limits.
- For every template, only the positive little-endian `u64` at bytes `8..16`
  may vary at runtime.
- Independently reconstruct and decode the complete v0 packet. Other than
  approved compute-budget instructions, the only top-level capital instruction
  is the exact Squads ProgramInteraction wrapper. Direct top-level K-Lend or
  Voltr manager execution is `FAIL`.
- Exact guardian, manager, policy, adaptor, vault, reserve, market, farm,
  account order/roles, instruction data, ALT, packet-size, compute, and heap
  bounds must match.
- Zero amount, `policyCap + 1`, a normal optimization above its lower software
  ceiling, wrong guardian/manager/policy/strategy/graph/program,
  extra instruction, and extra writable mutations must fail before signing.
- Current mainnet Settings, all eight policy account bytes, and program
  deployments match the bundle; no active policy in the manager namespace is
  unclassified.

### 4. `projection_and_priority`

At one confirmed observation using `minContextSlot`:

- Direct RPC idle equals `vault_idle_token_balances_current`.
- The four decoded strategy positions equal
  `vault_reserve_positions_current`; no fifth strategy exists.
- A direct scan of active Voltr withdrawal receipts equals the planner's sorted
  receipt-set fingerprint and aggregate demand.
- Every raw fixed-point receipt quote is conservatively rounded upward before
  aggregation.
- The maintained accounting identity holds:

  ```text
  totalValue = idle + Main + OnRe + Prime + Maple
  ```

- Projection freshness is at most 30 seconds.
- Log/websocket delivery is optional acceleration. With wakeups disabled, the
  periodic confirmed scan still discovers a request within 30 seconds.

Planner decisions must obey:

```text
requiredIdle = configuredSafetyBuffer + sum(active withdrawal upper bounds)
shortfall = max(0, requiredIdle - confirmedIdle)
investableIdle = max(0, confirmedIdle - requiredIdle)
```

Positive shortfall produces one liquidity-critical withdrawal. Zero shortfall
plus investable idle may produce one deposit. Only zero demand plus an elapsed
hourly cooldown may produce a normal optimization withdrawal.

The four-market allocation target comes from the existing capacity-adjusted
net-APY curves, not a new Backyard scorer. The controller fills the best
marginal capacity first and may split capital across Main, OnRe, Prime, and
Maple. Each cycle moves only one bounded surplus/deficit leg. Restoration first
prefers a source that can fill the current capped leg, minimizing transaction
count under the deadline; ties then use lowest net yield, lowest unwind cost,
and stable strategy id. A smaller source remains eligible for the final leg.

### 5. `durable_one_send`

Use the existing generic signed-submission recovery surface, not a
Voltr-specific implementation.

- Exact signed bytes, transaction/message hashes, expected signature,
  blockhash lifetime, fence, writable keys, and semantic generation are
  persisted before the network send permit.
- A disposable-store controlled-RPC probe injects failure:
  1. before persistence;
  2. after persistence and before send;
  3. after an ambiguous send response;
  4. after confirmation and before reconciliation; and
  5. after reconciliation and before the dirty wakeup.
- Actual observed network send count is at most one per signed generation.
- Restart looks up the exact expected signature and reuses persisted bytes; it
  never rebuilds or resigns an ambiguous submission.
- A stale fence loses. An ambiguous live signature freezes. A proved-expired,
  never-submitted wire becomes terminal before a new generation is allowed.

The verifier must exercise observable calls/state transitions; a source field
named `oneSendOnly` is not evidence.

### 6. `confirmed_effects`

Discover fresh production-path canaries through planner decisions and signed
submissions in Neon, then independently fetch their transactions and accounts
from RPC. A manually executed CLI transaction cannot satisfy this check.

For every sampled manager deposit/withdrawal:

- Persisted bytes, expected signature, and confirmed chain transaction match.
- The readback context slot is at or after the transaction slot.
- Deposit decreases idle and increases only the selected strategy position.
- Withdrawal increases idle and decreases only the selected strategy position.
- LP supply is unchanged by manager operations.
- Token and lamport deltas form the exact allowed closed set.
- Requested amount and actual protocol redemption are tracked separately.
  Conservation uses actual idle movement and position-value movement, allowing
  only the maintained explicit protocol rounding rule.
- The submission reaches generic `reconciled` state and marks the vault dirty.

### 7. `deposit_withdrawal_and_hourly_end_state`

Require one fresh coherent production-path trace from the deployed build and
route bundle:

**Deposit allocation**

- A confirmed user deposit increases Voltr idle.
- Projection observes it within 30 seconds without waiting for the hourly tick.
- Sequential, independently replanned manager deposits allocate investable idle
  within five minutes.
- Every allocation leg is `<=200k`, uses an approved strategy/policy, and has exactly one
  reconciled generic signed submission.
- Re-observation creates no duplicate effect.

**Withdrawal restoration**

- A confirmed request produces positive initial shortfall and an exact decoded
  receipt/user/LP/deadline identity.
- Projection observes it within 30 seconds.
- Until shortfall is zero, no ordinary deposit or optimization submission
  exists.
- Every restoration leg is one independently replanned `<=200k` withdrawal from
  fresh confirmed state.
- Aggregate idle covers aggregate active demand within 300 seconds of request
  confirmation and no later than 60 seconds before the 600-second deadline.
- A separately authorized user claim succeeds at or after the deadline, closes
  the receipt, and preserves vault accounting.

**Hourly optimization**

- At least one fresh hourly evaluation exists; an economically correct `NOOP`
  is acceptable.
- No normal optimization decisions begin less than 3,600 seconds apart.
- One hourly optimization starts with at most one 100k principal slice. Its
  source withdrawal and later destination deposit are each capped at 100k; the
  two transactions are not misreported as 200k of independently optimized
  principal.
- Strategy-to-strategy movement is a confirmed withdrawal, fresh replan, then a
  later confirmed deposit through idle; it is never an atomic or preplanned
  two-leg saga.

**Cap-to-throughput**

- The configured user-accessible vault cap is exactly 1M USDC.
- A coherent same-build, same-route, zero-idle 1M-USDC restoration trace reaches
  idle coverage in at most five independently replanned 200k legs within 300
  seconds.
- The trace includes at least one injected transient failure or worker restart,
  still covers demand at least 60 seconds before the claim deadline, and fails
  if fragmented safely redeemable liquidity would require more than five legs.

## Runtime model

There is one controller and one durable transition path:

```text
confirmed Voltr snapshot
    -> next_voltr_leg(snapshot, shared market curves, last normal start)
    -> one rebalance_opportunity
    -> existing lease + conflict fence
    -> one exact Rust Voltr manager packet
    -> signed_route_submissions
    -> existing sender/confirmer
    -> Voltr effect reconciliation
    -> mark the vault dirty
    -> confirmed snapshot and replan
```

The confirmed snapshot is a value, not a second state machine. It contains:

```text
route bundle digest and observation context slot
vault total value and LP supply
idle USDC
Main/OnRe/Prime/Maple position value and safely redeemable amount
sorted active receipt generation identities and conservative quotes
aggregate required idle, shortfall, and investable idle
shared capacity-adjusted market curves and their freshness evidence
```

The only production-specific addition to the existing lifecycle is a closed
Voltr route adapter. Do not add a receipt table, restoration saga table,
Backyard scheduler, mutable file handoff, or second signed-transaction queue.
Receipts remain on-chain truth and are rescanned after restart. The opportunity
persists the exact receipt-set fingerprint and observation slot that justified
its one leg.

### Explicit opportunity semantics

The current `rebalance_opportunities` value constraint assumes every row is a
positive-APY economic rebalance. Withdrawal restoration is not one. Add one
narrow migration to the existing table and Rust types:

```text
operation_class:
  yield_optimization
  idle_allocation
  withdrawal_restoration

service_deadline_at:
  required only for withdrawal_restoration
```

- `yield_optimization` retains every existing positive edge/gain/economic
  invariant.
- `idle_allocation` prices idle at zero yield and retains the economic and
  capacity gates, but bypasses the one-hour start cooldown so a new user
  deposit is not stranded.
- `withdrawal_restoration` requires zero economic-gain fields, positive bounded
  principal, a receipt-set fingerprint, a deadline, and an exact
  strategy-to-idle route. It is ordered before unsigned allocation and
  optimization work without manufacturing a fake APY.
- Recovery of an already signed or ambiguous submission precedes all three
  classes. The new class never bypasses the existing lease, conflict, fee,
  signer, persistence, or one-send requirements.

One opportunity represents one manager transaction. A strategy rotation is
not a persisted two-leg saga: after the source withdrawal reconciles, idle is a
safe custody state and the next snapshot independently chooses the destination
deposit. The normal-start cooldown prevents a second optimization withdrawal;
it does not delay completing the idle deposit.

## Implementation plan

Work in this order. The first three chunks attack the packet, schema, and
state-model risks before deployment or live writes.

### Chunk 0 — exact Rust packet parity, no writes

This is the hardest dependency and the first stop condition.

1. Define one versioned, canonical route-bundle schema containing the eight
   manager instruction templates and all identities/limits in check 3.
2. Generate the bundle at build time from the pinned Voltr SDK. The generator
   may live under `tools/backyard-voltr`, but the deployed worker embeds only
   immutable bytes and never launches Bun or a TypeScript child process.
3. Add an independent Rust decoder and builder in `loyal-actions`. Runtime input
   is only `(operation, strategyId, amountRaw)`; callers cannot provide account
   metas, programs, policies, reserves, or opaque instruction bytes.
4. Reconstruct the complete Squads wrapper and v0 packet. Prove byte-for-byte
   parity with each known-good SDK template after normalizing blockhash and the
   single amount field.
5. Run read-only confirmed account/deployment checks and fresh mainnet
   simulations for the current small-cap bundle. Also compile the production
   1M-vault/200k-policy/100k-normal configuration offline, but do not claim it
   is on-chain-authorized before the Voltr/policy update.

Exit: all eight templates fit and simulate under the installed small-cap graph,
or stop with the first exact packet/policy/ALT/compute incompatibility. Do not
build queue or monitoring work around a TypeScript handoff if this fails.

Primary ownership:

- `crates/loyal-actions/src/autonomous_vaults/voltr_kamino.rs`: canonical Rust
  route/bundle types and exact manager builder;
- `tools/backyard-voltr`: offline/build-time bundle generation and historical
  mainnet diagnostics only.

### Chunk 1 — make the existing queue honest

1. Add `operation_class` and `service_deadline_at` to
   `rebalance_opportunities`; change its value constraint conditionally instead
   of inserting fake edge/yield/gain values.
2. Extend `RebalanceOpportunityInput/Record`, validation, idempotency, claim
   ordering, production evidence, and migrations in place. Add no workflow
   table.
3. Keep the existing one-active-opportunity slot per vault. An authoritative
   receipt scan may supersede an unsigned normal row atomically. It may not
   supersede a signed, submitted, confirmed, reconciliation-pending, or
   ambiguous generation.
4. Define the Voltr semantic conflict set as the exact vault key plus the
   bounded shared reserve lane selected by the packet. Persist the complete
   physical writable set separately, as the existing submission contract
   already requires.
5. Make the deterministic 120k-demand oracle produce only
   `80k -> re-read -> 40k -> re-read -> NOOP`, and make the production-cap
   oracle produce exactly five independently replanned 200k legs for a zero-idle
   1M demand.

Exit: a disposable Postgres run proves priority, duplicate suppression,
fencing, conditional economics, and fresh one-leg replanning using the generic
tables only.

Primary ownership:

- `crates/loyal-yield-store/migrations/`: one additive/conditional migration;
- `crates/loyal-yield-store/src/fleet_orchestration/queue.rs`: generic durable
  contract, with no Voltr-specific outbox SQL;
- `crates/loyal-yield-orchestrator/src/fleet_orchestration/planner.rs`: pure
  priority and deterministic oracle.

### Chunk 2 — one confirmed Voltr observation path

1. Add a small Rust Voltr observer called by the existing fleet opportunity
   planner for the single compiled route. It reads confirmed RPC with
   `minContextSlot`, decodes idle, vault totals, four positions, and every active
   receipt, and rejects a mixed/stale context.
2. Reuse the existing shared Kamino observation and capacity curves for APY and
   liquidity. Do not implement an APY client in the Backyard tool.
3. Project idle and the four positions through the existing current-state
   surfaces so their normal dirty-vault wakeups continue to work. Keep the
   receipt set in the authoritative scan/controller input; do not create a
   mutable receipt mirror table.
4. Poll the single route every ten seconds with a hard 30-second discovery SLO.
   A log, webhook, websocket, or LaserStream event may wake an immediate scan,
   but never supplies demand itself.
5. Add the read-only RPC dependency and disabled Voltr binding to the existing
   planner service. Do not deploy another monitor service for one vault.

Exit: independent RPC readback and planner input agree on the route bundle,
slot, idle, all four positions, receipt fingerprint, rounded-up demand, and
accounting identity.

Primary ownership:

- new `fleet_orchestration/voltr_observation.rs` owned by the orchestrator
  feature;
- `fleet-opportunity-planner.rs` only wires polling and publication;
- existing idle/position current-state tables remain projections, not truth.

### Chunk 3 — pure one-leg controller and capital splitting

Implement one pure function with no RPC, database, signer, or scheduler calls:

```text
next_voltr_leg(snapshot, market_curves, last_normal_start) ->
    RecoverExisting | WithdrawOne | DepositOne | Noop
```

It applies this fixed order:

1. recover any existing nonterminal signed generation;
2. restore positive withdrawal shortfall from the deterministic funded source;
3. deposit positive investable idle into the largest/highest-value target
   deficit;
4. after 3,600 seconds, withdraw one bounded surplus slice when the existing
   net-gain, cost, hysteresis, capacity, and risk gates approve it;
5. otherwise do nothing.

Target allocations are computed by greedily filling the existing marginal
capacity-adjusted net-APY curves. This lets the vault split capital across the
four markets instead of sending everything to whichever headline APY is
highest. The controller clips restoration and idle-allocation output by the
200k policy ceiling, normal optimization output by the lower 100k ceiling, and
all output by safely redeemable source amount, destination capacity, investable
idle, and the remaining vault cap.

Use state-derived idempotency:

```text
sha256(route bundle + vault + operation class + operation + strategy
       + amount + confirmed context slot + receipt-set fingerprint)
```

Exit: deterministic fixtures prove deposit detection, four-market splitting,
hourly cooldown, zero/over-cap rejection, duplicate scans, stale slots,
receipt aggregation, cancellation, and withdrawal priority without generating
transaction bytes.

### Chunk 4 — route dispatch through the existing worker

1. Add `voltr_kamino` dispatch inside `loyal-fleet-worker` before the existing
   direct-Kamino request path. The same deployed binary and worker services may
   execute it; the old `same_mint` and `idle_vault_deposit` builders must reject
   it.
2. Re-read the opportunity, fence, bundle digest, current Settings/policy bytes,
   route-owned prestate, 200k policy ceiling, and operation-class ceiling before
   signer use.
3. Build and simulate the exact packet in Rust, enforce packet/compute/heap/fee
   ceilings, acquire the generic semantic conflict lease, sign once, and
   atomically persist the existing decision plus `signed_route_submissions`
   row before publication to the sender.
4. Reuse the existing sender and `fleet-route-confirmer`. The adapter neither
   calls `sendTransaction` directly nor implements transport retry logic.
5. Local secret-dependent commands use only the mounted environment through
   `op run --env-file=.env.1password`; no 1Password app automation, plaintext
   env file, command argument, or logged key material is permitted.

Exit: a disposable-store/controlled-RPC probe reaches the generic signed state
for one deposit and one withdrawal, with exact bytes and zero network sends.

Primary ownership:

- new `crates/loyal-fleet-worker/src/voltr.rs`: thin packet/preflight/effect
  adapter;
- `crates/loyal-fleet-worker/src/lib.rs`: route dispatch only;
- no new worker binary or Render service.

### Chunk 5 — confirmed reconciliation and one-send recovery

1. Add `voltr_kamino` reconciliation behind the existing reconciler dispatch.
   Require successful transaction metadata plus a confirmed account read at or
   after the transaction slot.
2. Reconcile requested and actual raw amounts separately: exact idle delta,
   selected strategy value/redeemable delta, unchanged LP supply, unchanged
   other three strategies, bounded fee debit, and a closed token/lamport row
   set.
3. Re-scan receipts in the same post-effect observation. Mark the generic
   submission reconciled and the vault dirty in one fenced transition. Never
   acknowledge work from a caller-supplied readback JSON.
4. Inject crashes before persistence, after persistence, after ambiguous send,
   after confirmation, and after reconciliation. The expected signature and
   exact persisted bytes are the only recovery identity; observed send count
   remains at most one.

Exit: checks `durable_one_send` and the offline/disposable portion of
`confirmed_effects` pass without a Voltr configuration change or production
broadcast.

### Chunk 6 — remove the parallel production lane

After chunks 0-5 work through the generic lifecycle:

1. remove `--enqueue-voltr-restoration-json` from the fleet planner;
2. remove `voltr_restoration` production exports and its cross-lane SQL checks;
3. remove the restoration bridge/readback binaries from Cargo and worker-image
   build inputs;
4. leave `tools/backyard-voltr` available only for bundle generation, offline
   diagnostics, historical evidence, and user transaction builders;
5. make source/deployment verification fail if a Render command or production
   call graph can reach Phase A, Phase B, the Voltr outbox event kind, or a Bun
   manager sender.

Do not delete historical evidence. Delete or disconnect only the mutable
production path that duplicates the generic fleet lane.

Exit: `single_runtime_lane` passes and there is one database lifecycle for all
manager transactions.

### Chunk 7 — deploy dark and close the infrastructure checkpoint

1. Extend the existing light-worker image; do not introduce a Backyard image.
2. Deploy the planner/worker/confirmer/reconciler build with the Voltr route
   binding disabled. The observer may run read-only shadow comparison, but it
   cannot publish executable Voltr opportunities or load the guardian.
3. Run the cheap checks before any network probe:

   ```sh
   cargo check -p loyal-actions -p loyal-yield-store \
     -p loyal-yield-orchestrator -p loyal-fleet-worker
   ```

4. Run the sole verifier through the mounted CLI environment:

   ```sh
   op run --env-file=.env.1password -- \
     cargo run -q -p loyal-yield-orchestrator \
     --bin fleet-orchestration-verifier -- \
     --backyard-voltr-end-state --json
   ```

At this checkpoint checks 1-5 must pass against the deployed dark build. The
overall result is intentionally `BLOCKED`, not falsely `PASS`, only because the
Voltr/policy update, enabled binding, and live canaries are deferred. Run no
broad mock-heavy suite; use the existing proof surface only for external byte,
policy, database, and recovery contracts that could compile while broken.

### Chunk 8 — Voltr activation at the fixed production cap

This chunk begins only when the separate on-chain update is approved.

1. Update the canonical TypeScript RouteSpec and maintained verifiers first:
   vault cap `1_000_000_000_000`, admin performance fee `500`, manager
   performance fee `0`, all other owner fees `0`, normal interval `3_600`, and
   policy ceiling `200_000_000_000`. Keep withdrawal wait `600` and locked-profit
   degradation `86_400`; do not change metadata in this wave.
2. Re-read the confirmed Squads Settings immediately before policy compilation.
   The last observed seed was 42, so the next eight seeds are 43-50 only if the
   live seed is still 42. Any drift requires regeneration rather than skipping
   or guessing a seed.
3. Freeze the production bundle with the unchanged 600-second withdrawal wait,
   exactly four approved strategies/eight policies, 3,600-second normal
   interval, zero base idle floor, `1_000_000_000_000` raw vault cap,
   `200_000_000_000` raw policy ceiling, and `100_000_000_000` raw normal
   optimization ceiling. Set `adminPerformanceFeeBps = 500`, every other owner
   fee to zero, and rebuild the immutable worker image around that exact digest.
4. Before any signature or send, independently decode and compare every proposed
   vault-config and policy instruction against the frozen bundle, simulate it at
   confirmed commitment, and bind the exact packet/artifact hashes into the
   one-time authorization. A source artifact cannot authorize a caller-selected
   policy payload.
5. With the route binding still disabled, update and independently read back the
   Voltr configuration. After setting the 500-bps admin performance fee and
   before public deposits, calibrate and verify the high-water mark so historical
   POC state cannot be charged as new performance.
6. Install the eight newly generated policy accounts sequentially and verify
   their full confirmed bytes after each creation. Keep the old 1-USDC policies
   until all replacements pass; then remove the old catalog so the guardian has
   one classified four-market policy surface. Regenerate the catalog,
   authorization, effective route digest, and embedded Rust bundle whenever any
   live seed, account, program, ALT, cap, or source hash differs.
7. Enable exactly one matching database/deployment binding and run one tiny
   generic-lane Main withdrawal and deposit first. Then prove each
   remaining strategy direction with bounded amounts; all use confirmed
   commitment and generic reconciliation.
8. Run a real underfunded request/restoration/claim canary. Idle must cover the
   aggregate active demand within 300 seconds and at least 60 seconds before the
   600-second claim time.
9. Prove the fixed 1M accessible cap with a controlled zero-idle restoration
   stress trace: at most five independently replanned 200k legs, including one
   injected transient failure or worker restart. If current strategy liquidity
   fragmentation cannot satisfy that bound, activation remains `BLOCKED`; do
   not silently increase concurrency or weaken one-leg reconciliation.
10. Run the same verifier to full `PASS`; no separate lifecycle manifest or
   operator-authored success JSON may substitute for deployed state and chain
   evidence.

### Chunk 9 — Backyard handoff

There are two explicit handoff states:

1. **Stable-address integration handoff:** after the vault configuration,
   high-water mark, and policy accounts are confirmed and independently read
   back, give Backyard the public vault and LP mint so frontend integration can
   begin. Label this `integration-only; public deposits not yet enabled` while
   canaries or deployment activation remain blocked.
2. **Production-ready handoff:** only after full verifier PASS, add the USDC mint
   and decimals, pinned user deposit/request/claim builders, 600-second claim
   semantics, 1M cap, 5% performance fee, status/quote contract, and verifier
   result, and explicitly authorize public deposits.

LP branding metadata is not required for either initial address handoff; the
packet labels it deferred so Backyard does not assume wallet branding is
already complete. Backyard never receives a guardian key, policy selector,
reserve graph, or manager instruction builder. The route remains disabled if
the bundle, policy catalog, deployment, or verifier digest drifts.

#### Testing-handoff verifier

The narrower stable-address handoff has one read-only command and one verdict:

```sh
op run --env-file=.env.1password -- sh -c \
  'cd tools/backyard-voltr && bun run verify:testing-handoff'
```

### Confirmed testing handoff (2026-08-21)

- Authoritative verdict: `PASS` at confirmed context slot `440856493` with
  `failedGateCount=0`, `broadcast=false`, and `signerLoaded=false`.
- Stable vault: `AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK`.
- Stable LP mint: `dbQkLsUYE7ADHHv8XEottANAa773K4xM4nyPjVdutka`.
- Vault activation signature:
  `45LNWnuXQ3QTEiV21X13smnbJ1M3bGmYeU18MASQmivZFbEdpHxHzEj3GzTbvA6KDGgzvLATAwehXhz5PodYdNM1`.
- Current 200,000-USDC policy generation is seeds `43..50`; every creation
  confirmed with exact semantic readback. The immutable seed `17..24` POC
  generation remains separately classified by exact artifact hash and creation
  signatures because Squads exposes no policy-close instruction.
- This verdict authorizes sharing the vault and LP mint with Backyard for
  integration testing. It is not the later production-readiness or deployed
  hourly-orchestration verdict.

`PASS` means only that Backyard may begin mainnet integration testing against
the published vault and LP mint. It requires confirmed readback of the exact
1M cap, five-percent admin performance fee, calibrated high-water mark, stable
admin/manager and token identities, all four initialized strategies, eight
current 200k policy accounts with exact creation origins, policy namespace
isolation, and approved executable deployments. The command loads no signer and
cannot broadcast. Missing RPC is `BLOCKED`; any checked-in or live-state mismatch
is `FAIL` with the first false gate. Metadata, deployed hourly orchestration,
the 1M restoration stress trace, and public-deposit activation are deliberately
outside this narrower verdict and remain governed by the full production
verifier above.

## Verdict classification

- Missing verifier scope/binary, route kind, Rust executor, generic submission
  integration, or a false domain invariant: `FAIL`.
- Unreachable RPC/Neon/Render/1Password environment: `BLOCKED`, with the exact
  command or access restoration needed to resume.
- Source complete but production image not yet approved/deployed: `BLOCKED`.
- Checks 1-5 passing on the deployed dark build while the explicitly deferred
  Voltr/policy update, route enablement, or canary authority is absent:
  `BLOCKED`, naming that exact activation gate. It is not a runtime failure.
- Required canary absent because its transaction was not separately authorized
  or funded: `BLOCKED`, naming the smallest required canary.
- Deployed schema, route, signer identity, policy, projection, transaction,
  accounting, priority, recovery, cadence, or SLO mismatch: `FAIL`.
- Simulation, compilation, unit tests, an RPC-returned signature, or historical
  POC evidence alone: insufficient; never `PASS`.

## Baseline and stopping rule

Run cheap local checks first and stop before network while
`single_runtime_lane`, `one_leg_replan`, or `route_and_packet_exactness` is
`FAIL`. Once they pass, run external preflight, live read-only state, disposable
recovery probes, and deploy dark. The next implementation wave first updates
the executable verifier, RouteSpec, bundle schema, and canonical policy/config
artifacts to v3. Chunk 8 then proceeds only with exact on-chain and canary
authority; a verifier result never grants that authority itself.

The first v3 result must be classified as:

```json
{
  "verdict": "FAIL",
  "firstFailure": "route_and_packet_exactness",
  "evidence": "current RouteSpec/on-chain config/policies and executable verifier do not encode the fixed v3 cap, fee, interval, and policy ceilings"
}
```

Implementation is complete only when the single command returns exit `0` and
`PASS` for every required check against the current deployed build, route,
database, and confirmed mainnet state. Do not weaken this verifier to match
partial implementation; change it only if the user changes the product outcome
or current evidence proves that the contract itself is wrong.
