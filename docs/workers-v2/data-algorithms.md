**Loyal worker v2: data contracts, algorithms and Go structure — 2026-10-02**

The implementation should revolve around typed custody and exposure data, small deterministic controllers, a capacity-aware admission algorithm, and incremental historical calculations. Go's useful contribution is straightforward composition, explicit error handling, cheap bounded concurrency and a fast standard toolchain. The simplification comes from clear ownership of data and transitions.

This study extends [the fresh audit](worker-v2-audit-2026-10-02.md). Source basis remains Apps `81b5c0256317c46e4fa9fdac49d6c0b347530b03` and Yield Routing `05338bbb70ce2e5287956f6e3659964e37bfa2f6`, inspected in the clean audit checkouts. Four independent reviews covered durable schemas, domain controllers, historical algorithms, and official Go practice. Production data and deployed behavior were not inspected.

**Findings that change the implementation plan**

| Finding | Evidence and scope | Consequence |
| --- | --- | --- |
| Personal earnings can interpret collateral as liquidity | The complete-snapshot writer preserves collateral in `amount_raw` and redeemable liquidity in metadata; the personal reader omits that metadata and sends raw collateral to the six-decimal liquidity calculation. This is a traced source defect when such funded snapshots reach the calculation. Production incidence and monetary impact are unmeasured. | Make collateral, redeemable liquidity and valuation separate fields/types. Correct this interpretation deliberately before treating historical output as a parity oracle. |
| Portfolio snapshots can hide APY gaps | An offline reproduction of the unchanged coverage function, with 61 hourly funded snapshots and APY samples only at hours 0 and 60, reports no gap and a maximum gap of one hour. | Track APY sample age across continuous funded exposure. Snapshot frequency must not reset the sampling clock. |
| Lazy heap invalidation can miss improved candidates | An isolated reproduction of unchanged economics/admission selects vaults `[1,3]`, although vault 2's updated priority is 7,302 versus vault 3's 7,157 after the first admission. Serialization/hash functions were stubbed; this is an admission-model reproduction, not an integrated execution test. | Updating only candidates popped from the heap is insufficient when scores can increase. Rescore affected candidates explicitly, or use a bounded full-rescore reference. |
| Desired configuration and chain facts are physically separated already | Migration 0059 renamed `active` to `desired_active` and `lifecycle_status` to `chain_status`, then removed separate config/projection tables. App repair still writes `desired_active=false` and `paused_missing_position`; that value is outside the migration's permitted chain vocabulary. | Reuse the existing target row with explicit field ownership. Derive effective eligibility and blocker reasons without changing user intent or chain status. |

Source trails:

- Units: [fleet complete publisher](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/crates/loyal-fleet-worker/src/lib.rs#L5414), [conversion metadata](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/crates/loyal-fleet-worker/src/lib.rs#L16651), [snapshot persistence](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/crates/loyal-yield-store/src/store.rs#L4239), [personal reader](https://github.com/loyal-labs/loyal-app/blob/81b5c0256317c46e4fa9fdac49d6c0b347530b03/apps/web/src/lib/yield-optimization/yield-deposit-repository.server.ts#L3013), [accrual](https://github.com/loyal-labs/loyal-app/blob/81b5c0256317c46e4fa9fdac49d6c0b347530b03/apps/web/src/lib/yield-optimization/earnings-calculator.server.ts#L737).
- Coverage: [actual function](https://github.com/loyal-labs/loyal-app/blob/81b5c0256317c46e4fa9fdac49d6c0b347530b03/apps/web/src/lib/yield-optimization/earnings-read-service.server.ts#L416); reproduction `/private/tmp/loyal-v2-coverage-repro.ts`.
- Heap: [version-on-pop logic](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/go/kamino-fleet-planner/internal/fleet/revalidator.go#L171); reproduction `/private/tmp/loyal-v2-heap-repro`, generator `/private/tmp/loyal-v2-heap-repro-generator.py`, output `result.json` inside the reproduction directory.
- Control state: [migration 0059](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/crates/loyal-yield-store/migrations/0059_autodeposit_single_target_state.sql#L7), [app field mappings](https://github.com/loyal-labs/loyal-app/blob/81b5c0256317c46e4fa9fdac49d6c0b347530b03/apps/web/src/lib/yield-optimization/yield-neon-client.server.ts#L793), [app repair mutation](https://github.com/loyal-labs/loyal-app/blob/81b5c0256317c46e4fa9fdac49d6c0b347530b03/apps/web/src/lib/yield-optimization/earn-autodeposit-repository.server.ts#L2797). Deployed constraint failures were not checked.

**The minimum useful data model**

These are semantic Go records, mostly backed by existing tables. They do not imply creating a new table for every record.

| Record | Key and contents | Owner and invariant |
| --- | --- | --- |
| Captured event | Consumer/source event identity, slot, account or transaction evidence, commitment, payload version | Observer; durable before advancing capture progress. Preserve every financial event. |
| Projection progress | Consumer application position plus pending/error backlog | Observer; distinguish captured progress from committed application. A maximum captured slot is insufficient freshness evidence. |
| Refresh request | Target, requested slot/revision, processed slot/revision, claim epoch/expiry, next due time | Current-state owner; coalesces redundant refresh demand while preserving demand that arrives during a claim. |
| Target control and observation | Desired enable/floor/limits; separate chain status, setup generation, observed policy/delegation and slot | App owns desired control; observer owns chain facts. Engine derives effective eligibility. A control edit revision, if added, must not repurpose setup generation. |
| Immutable market epoch | Verified reserve identities, amounts/rates, observation slots/times, coverage, expiration and evidence hash | Observer publishes; planners read a frozen value. Source freshness and policy generations remain explicit. |
| Vault custody snapshot | Vault identity, accounts, mints, collateral, redeemable liquidity, idle funds, debt, ordering/evidence | Owning domain; conversions bind to the observed snapshot. Current prices cannot repair historical conversion evidence. |
| Family operation | Frozen family-specific plan, ownership epoch, reservation and reconciled custody checkpoints | Engine; one autonomous custody owner per applicable vault scope. Keep existing journal adapters initially. |
| Worker attempt or external receipt | Worker wire/hash/signature/blockhash/expiry/intent versus externally signed transaction identity/effects | Engine/observer according to origin; external receipts do not require fabricated worker wire fields. |
| Capacity reservation | Reserve/mint, amount and admission evidence, movement slot, telemetry version, reservation state | Retail engine; remains held through uncertainty and, where required, until newer telemetry incorporates movement. |
| Cash-flow and exposure timeline | Verified principal changes and complete unit-tagged exposures with lifecycle/ordering evidence | Observer/read-model owner; a complete snapshot replaces the exposure set, while a partial observation is a patch. |
| Materialization head | Cash-flow/exposure/APY revisions, calculation version, dirty-from boundary, checkpoint | Projector; stale computations cannot overwrite newer results. |

In Go, use named numeric types and explicit conversions. For example:

```go
type CollateralRaw uint64
type LiquidityRaw uint64
type USDMicros int64

type Exposure struct {
    Reserve      [32]byte
    LiquidityMint [32]byte
    Collateral   CollateralRaw
    Liquidity    LiquidityRaw
    Evidence     SnapshotRef
}
```

`SnapshotRef` should identify the observation slot, evidence hash and conversion provenance. Keep mint decimals in the verified asset/catalog record. Raw liquidity and USD valuation are different quantities even when a six-decimal stablecoin makes their numbers equal. Existing PostgreSQL `BIGINT` bounds remain narrower than the full chain `uint64` domain: validate every conversion. Use checked integer arithmetic and local `big.Int` intermediates for products and aggregate quantities. Modeled rates and chart presentation may use finite floating-point values; spend amounts and receipt accounting stay integer.

Decode raw quantities into typed integer fields or decimal strings; use `json.Decoder.UseNumber` where a legacy generic JSON adapter is unavoidable. Represent missing evidence and optional limits explicitly: an unknown allowance is different from an exhausted allowance of zero. Validate actionable snapshots at IO boundaries so zero-value structs cannot accidentally authorize execution.

Keep signed wire owned by one operation; copy when accepting a caller's mutable buffer and verify its persisted hash. Publish market maps/slices once and treat them as immutable. Go does not make a map or slice immutable when it crosses a channel. The [Go memory model](https://go.dev/ref/mem) supplies synchronization rules; ownership discipline is the application design.

**Durable processing algorithm**

1. Capture the event and its required application work in the same destination transaction as capture-cursor advancement.
2. Claim bounded work, preserving per-vault ordering and the existing same-transaction sibling exception.
3. Validate the claim; apply deduplication, financial mutations, projection state and completion in one transaction. Materialization invalidation belongs with the mutation that makes it necessary.
4. For a coalesced refresh, acknowledge only the slot/revision actually processed. Newer demand stays pending.
5. On restart, replay durable work. Transient wake notifications may be lost without losing work.

The source already implements much of this in [capture](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/crates/loyal-yield-store/src/store.rs#L651), [coalesced completion](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/crates/loyal-yield-store/src/store.rs#L778) and [event application](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/crates/loyal-yield-store/src/store.rs#L1013). Dedupe identity needs signature, vault and mutation semantics, with instruction identity where the family requires it; a slot alone is not an event identity.

Use ordered batches and partial ready indexes. `FOR UPDATE SKIP LOCKED` is useful for claiming work; PostgreSQL explicitly warns that it produces an inconsistent view, so it must not construct the complete financial/market snapshot. See [PostgreSQL SELECT](https://www.postgresql.org/docs/current/sql-select.html). Construct coherent planning input with the existing repeatable-read/evidence contract, then perform short locked or serializable admission rechecks.

Timescale source reads and Yield destination commits are not one cross-database transaction. Keep a stable source identity, sufficient retention, idempotent destination writes and a destination checkpoint committed with those writes. Colocation can simplify a particular transaction boundary later, but the port should not assume it already exists.

**Autodeposit: lots, deadlines and a two-leg custody controller**

The core arithmetic is:

```text
surplus(balance) = max(balance - configured_floor, 0)
new_lot = max(surplus(after) - surplus(before), 0)
sweep = min(eligible_lots, live_surplus, period_limit, remaining_allowance)
```

External spending consumes newest open lots first; a sweep consumes oldest eligible lots first. Current scheduling coalesces lots into a batch and moves its due time to `max(previous_due, new_lot_due)`, where a new lot waits one hour. Preserve that behavior explicitly; a generic timer that executes each due lot would change it. Sustained funding can keep pushing a coalesced deadline: if v2 changes this product policy, eligible-only bounded batches preserve each lot's minimum delay without including immature funds.

Use ordered lot slices for pure calculation and indexed SQL for durable lot selection. The database is scheduling truth. A deduplicated wake set or min-heap can accelerate due work; rebuilding it after restart must be harmless. Current pure calculations are in [Autodeposit lib](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/crates/balance-sweep-autodeposit-trigger/src/lib.rs#L267), and the [batch merge](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/crates/balance-sweep-autodeposit-trigger/src/main.rs#L2083) determines actual deadline semantics.

Recovery takes precedence over fresh admission, including when desired enablement has changed. Before a pull, resolve and preflight the authorized destination and persist the immutable destination/amount plan. The pull and Kamino deposit remain separate transactions with owned idle custody between them. A later rate change cannot silently redirect that operation. Uncertain outcomes retain reservations and recovery evidence. Proven no-effect outcomes can release funds according to the existing family contract.

Business progress is roughly `reserved -> pull pending -> funds in vault -> deposit pending -> accounted`. Attempt progress is separate. Express both as typed family state rather than one overloaded status string.

**Backyard: an ordered risk and custody controller**

Current Go runtime operates on an already frozen manifest-approved lane. Its executable decision function does not select the best lane; the separate TypeScript ranking path is shadow-only. See [lane resolution and controller](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/go/backyard-rwa-worker/internal/backyardrwa/decide.go#L5).

Preserve these priorities: recover a nonterminal operation; validate coherent evidence; perform hard-LTV safety; report required post-mutation NAV; service withdrawal/deleveraging demand; report due NAV; admit bounded new exposure. The hard-LTV threshold is `min(liquidation_threshold - 1500 bps, 6000 bps)` in this reviewed runtime. Phase 2 has lane-specific behavior: a genuine withdrawal receipt drains the selected lane, and the adaptor can restore all staged custody despite a smaller outer requested amount. Action caps therefore apply to actual effects.

Return one typed action with its actual payload. Replace sentinel amounts such as `1` meaning "compute the target borrowing amount" with a distinct action such as `BorrowToTargetLTV`. Preserve fresh re-decision before journal/build and the current signed/intent/submitted recovery distinctions. Shared execution mechanics can carry exact wire and receipt evidence; family-owned logic determines expected custody effects.

**Fleet admission: a constrained optimization problem**

Current planner evaluates permitted vault-target pairs, estimates holding-period net gain, uses conservative reserve-capacity bands, and ranks by an economic priority that accounts for lost yield and estimated service time. A heap admits a bounded wave while updating reserve inflow/outflow and enforcing vault, tenant, notional and writable-conflict limits. See [economic model](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/go/kamino-fleet-planner/internal/fleet/planner.go#L24), [wave limits/ties](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/go/kamino-fleet-planner/internal/fleet/wave.go#L7), and [whole-fleet selection](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/go/kamino-fleet-planner/internal/fleet/revalidator.go#L63).

Define the objective before choosing an optimizer. Maximizing forecast net return over a horizon and maximizing economic gain per unit of service time are different objectives. Hard authority, custody, withdrawal and risk rules are feasibility gates. They must not become penalties that sufficient projected yield can outweigh.

For a fixed candidate model, a useful reference is:

```text
maximize sum(candidate_expected_net_gain * selected)
subject to:
  at most one candidate per eligible vault
  reserve capacity including unresolved/awaiting-telemetry holds
  writable-account admission limits
  tenant, wave-count, notional and fee budgets
  every selected route satisfies permission, freshness and risk constraints
```

Each `selected` is binary. `KEEP` is a valid outcome. Whole operations, fixed transaction costs, shared resources and amount-dependent yields mean a plain shortest-path or linear flow model does not capture the full problem. Google's primary examples distinguish [capacity-sized assignment using CP-SAT/MIP](https://developers.google.com/optimization/assignment/assignment_cp) from [minimum-cost flow assignment](https://developers.google.com/optimization/flow/assignment_min_cost_flow). The classification of Loyal's problem here is an inference from its actual constraints.

For a richer fleet objective, candidate gains cannot remain independent constants: selected inflows/outflows change rates for other holdings too. Evaluate post-wave whole-fleet return under the same validated utilization/rate model, or use an explicitly reviewed integer piecewise approximation. An optimum is meaningful only for that model and frozen evidence; it is not a guarantee about future realized markets.

Keep a deterministic live algorithm and build an offline oracle. Exhaustive small-cohort evaluation is enough to expose greedy gaps; a bounded CP-SAT/MIP research command can compare larger frozen cohorts without adding a live service or Go runtime solver dependency. Record solver status and bound: a merely feasible solution is not a proven optimum. Compare gain, rejected opportunities, fairness, cost sensitivity and admission latency using identical constraints.

The heap reproduction illustrates a specific improvement before adding a solver:

| Candidate | Route | Initial priority | Priority after candidate 1 |
| --- | --- | ---: | ---: |
| 1 | A -> B | 13,881 | admitted |
| 2 | B -> C | 7,012 | 7,302 |
| 3 | D -> E | 7,157 | 7,157 |

With a two-operation wave, current selection is `[1,3]`. Candidate 1's inflow dilutes B's source APY for candidate 2, improving candidate 2's edge. Its old heap key remains below candidate 3, so it is never rescored before the wave fills. A lazy heap works as a best-candidate mechanism only with valid score bounds or explicit updates when scores can improve.

For v2, index candidates by source and target reserve; after admission, rescore affected candidates and update their heap positions. First implement a full-rescore reference for bounded fixtures, then verify the indexed implementation against it. Keep deterministic tie-breaks and inject the evaluation time. Make this an intentional algorithm correction, separate from Rust/Go parity.

Let `C` be legal candidate edges and `K` the wave limit. Current source scans all reserves for every vault, so initial work includes `O(V*R)` even when legal edges are sparse. Enumerate sorted permitted targets to approach `O(C)` candidate evaluation. A fresh full-rescore reference costs `O(K*C)` evaluations; an indexed heap costs approximately `O(C + U log C)` plus `U` economic evaluations, where `U` is the actual number of affected updates. Dense shared-reserve frontiers can still make `U` large. Measure realistic batches before adding top-k pruning: pruning can discard the only feasible alternatives after contention.

Admission remains a prediction. Immediately before committing executable work, revalidate fresh policy/custody, acquire the existing database ownership/conflict/capacity fences, and recompute economics under the locked frontier. Operation completion does not automatically release capacity; preserve the [awaiting-telemetry release rule](https://github.com/loyal-labs/loyal-yield-routing/blob/05338bbb70ce2e5287956f6e3659964e37bfa2f6/crates/loyal-yield-orchestrator/src/fleet_orchestration/capacity.rs#L227).

**Historical earnings: one timeline, reusable prefix integrals**

Personal earnings already uses useful binary rate indexes and principal prefixes. It still repeatedly constructs interval boundaries across four chart ranges. Construct one verified timeline from cash flows, complete exposure changes and sampled rates, then maintain cumulative modeled earnings and cumulative principal-time:

```text
earning_rate(t) = sum(liquidity_exposure[r,t] * sampled_APY[r,t]) / seconds_per_year
earned(a,b) = cumulative_earned(b) - cumulative_earned(a)
average_principal(a,b) = (principal_time(b) - principal_time(a)) / (b-a)
```

Sort/index once, then sweep changes. Maintain the earning-rate total incrementally as individual exposures/rates change. After ordering, the work is proportional to events and affected exposures; bucket queries take two binary lookups plus endpoint interpolation. For `N` timeline boundaries and `B` buckets, aim for `O(N log N + B log N)` plus actual changed-reserve work. Keep the first confirmed-deposit boundary, position lifecycles, idle zero return, future-observation exclusion and existing timezone/DST response shapes.

Order financial facts by verified lifecycle/chain evidence. Independent producer table IDs are not interchangeable ordering keys; if principal ordering is ambiguous, preserve an explicit history failure. An application sequence is useful only when its construction preserves the required evidence.

For coverage, merge adjacent funded spans per reserve and sweep APY observations while retaining the actual last sample timestamp. Reject positive exposure occurring after that sample's permitted lifetime. Ignore holes wholly outside funded exposure. This fixes the reproduced 60-hour case and avoids the current repeated filtering/sorting. Missing conversion evidence or rate coverage should remain visible rather than becoming manufactured values.

Keep three calculations separate:

| Product | Meaning | Efficient representation |
| --- | --- | --- |
| Personal earnings | Modeled accrual over verified actual exposure | Earnings and principal-time prefixes |
| Public strategy chart | Simulated hypothetical allocation/rates | Rate-time prefixes using its own simulation rules |
| Public fleet realized APY | Chain-linked allocation-weighted share-price growth, idle at growth 1 | Canonical segment growth plus coverage prefixes |

For fleet realized APY, preserve its exact hour/allocation/expiry boundaries. Extra splits can imply extra rebalancing and change weighted compounded returns. Prefix log-growth can accelerate complete canonical segments; recompute partial endpoint segments from original weights/prices. Keep signed losses through the calculation, and preserve coverage thresholds and source clocks. See [realized calculation](https://github.com/loyal-labs/loyal-app/blob/81b5c0256317c46e4fa9fdac49d6c0b347530b03/apps/web/src/lib/yield-optimization/earn-realized-apy.shared.ts#L238).

Incremental materialization needs `(cashflow_revision, exposure_revision, APY_revision, calculator_version)` and `dirty_from`. Appends process a suffix; corrections resume from the preceding checkpoint. Publishing compares the source revision tuple so slower old calculations cannot overwrite newer ones. Rate/share-price corrections invalidate their actual interpolation neighborhood and dependent windows. Page any fanout. Hourly allocations remain forward-recorded; missing history cannot be reconstructed from today's weights.

**Go structure for fast development and support**

Use one module inside the mixed Rust/TypeScript repository, for example `go/workers`. Each executable is a thin composition root; owning feature packages contain domain logic, SQL and adapters. This follows the official [server/module layout guidance](https://go.dev/doc/modules/layout).

```text
go/workers/
  go.mod
  cmd/
    loyal-observer/main.go
    loyal-engine/main.go
    loyal-evidence/main.go
  internal/
    observer/        # admission, receipt application, projections
    engine/          # process lifetime, bounded lanes, readiness
    autodeposit/     # target, lots, scheduling, two-leg recovery, SQL
    fleet/           # economics, admission, policies, route preparation, SQL
    backyard/        # fixed-lane controller, builders, custody recovery, SQL
    solana/          # proven shared RPC, key, codec and wire primitives
    db/              # pool/schema/transaction and proven lease helpers
    earn/            # add when canonical Go timeline work is actually ported
  testdata/          # bounded replay fixtures and reviewed parity cases
```

Inside a feature, start with files such as `types.go`, `decide.go`, `store.go`, `reconcile.go`, `worker.go`, and their focused tests. File count is not abstraction count. Split packages when there is a meaningful dependency/capability boundary, not whenever another technical file category appears.

Import direction should be commands -> runtime/domain constructors -> shared primitives. Domain packages do not import engine, and fleet/Backyard do not import each other. Put composition in the commands. A tiny consumer-defined `Run(context.Context) error` interface can share process lifetime where useful; shared attempt-evidence primitives do not require a universal operation payload or protocol registry.

Return concrete constructors. Define small interfaces where a consumer actually needs a capability or fault-injection seam. Existing `MarketEpochSource` and the revalidator's bounded store interface are useful precedents. The official [interface guidance](https://go.dev/wiki/CodeReviewComments#interfaces) favors consumer-owned interfaces and concrete implementations. Pure decisions need ordinary typed parameters, not repositories or process context.

Representative APIs, in their respective feature packages:

```go
func Decide(input Snapshot, now time.Time) Decision
func ApplyWalletEvent(lots []Lot, event WalletEvent, control Control) LotChanges
func Plan(input PlanningSnapshot, limits Limits, now time.Time) (Wave, error)
func Advance(ctx context.Context, operation PersistedOperation) error
```

Keep `Advance` family-specific. Different business states and broadcast recovery rules remain explicit. Use tagged typed action payloads for known actions and validated decoders at persistence boundaries. Give blocked/deferred/unknown outcomes stable reason codes; reserve errors for failures that require operational handling. A lost per-vault lease aborts that work, while other owned vaults may continue; losing a process-wide authority invalidates the corresponding runtime.

Wrap operational errors with `%w` and branch with `errors.Is`/`errors.As`; classify at the boundary deciding recovery. Log that decision once with structured attributes instead of logging the same error at every call layer.

Retain pgx and the pinned Solana dependency while restructuring; dependency upgrades are a separate checked change. Use standard `context`, `net/http`, `errors`, `encoding/binary`, `crypto/sha256`, `log/slog` and `sync`, plus `x/sync/errgroup` for owned concurrency. Keep the small official KLend helper and generated Loyal Hub ABI surface where their exact parity is still needed. Startup should inject concrete stores/clients/signers; observer configuration never loads signing credentials.

**Concurrency and transaction ownership**

Make domain methods synchronous; their caller owns concurrency. Pass context to IO as its first argument, set operation-specific deadlines, cancel derived contexts, and keep contexts out of persisted domain records. [Go context documentation](https://pkg.go.dev/context) describes this explicit propagation contract.

The process owns a bounded set of persistent lanes: ingestion/application, recovery, fresh planning/admission, and maintenance. Recovery needs reserved capacity. Distinct limits protect stream capture from historical queries and unresolved attempts from new-plan bursts. Per-vault ownership remains enforced in PostgreSQL across process restarts and replicas; a local map mutex alone cannot provide that guarantee.

Join every goroutine before closing its dependencies. Current fleet `Run` starts revalidation goroutines and returns on cancellation without waiting; Backyard's lease refresher already cancels and joins. Adopt the latter ownership discipline. A top-level `errgroup` can own persistent lanes; ordinary per-operation failures should be persisted/retried within a lane instead of canceling the entire process. Set group limits before starting batch work and do not alter them while running; see [errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup).

Use a `run() error` function beneath `main` so resource defers run before the outer exit. Stop admitting new work, cancel and join lanes, finish bounded essential cleanup, then close pools. Persisted signed/ambiguous work remains recoverable; shutdown does not erase it.

Prepare RPC/build evidence outside long-held transactions. Recheck ownership, revisions and financial frontiers inside a short transaction and commit the exact attempt/evidence. Execute transactional writes through the transaction, not its parent pool. pgx requires explicit commit/rollback; begin-context cancellation does not automatically roll back. See [pgxpool transaction contract](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool#Pool.BeginTx). Use an appropriate bounded cleanup context when the operation's context is already canceled.

Bound read RPC retries and classify failures. Broadcast retries belong to the attempt's explicit domain protocol. A generic retry wrapper must not decide whether an ambiguous financial send is safe. Database serialization retries must reread the changed frontier rather than replay an old admission result.

**Development and support workflow**

Keep a small reproducible command surface: format, vet, offline tests, build observer/engine/evidence, and an explicitly separate disposable-database/integration gate. One module gives one dependency graph and lets package-level tests iterate quickly. Existing fixture parity and exact instruction/message checks remain useful, with reviewed intentional v2 changes separated from legacy-parity expectations.

- Fast local checks: `gofmt`, `go vet`, focused pure-decision/decoder tests and reviewed offline fixtures.
- Concurrency checks: `go test -race` exercising cancellation, lane joining, immutable publication and attempt-buffer ownership. The [race detector](https://go.dev/doc/articles/race_detector) reports exercised races, not every possible schedule.
- Timer checks: use explicit time inputs for decisions and `testing/synctest` for self-contained scheduling/cancellation tests. It is available in Go 1.25, including the existing module's language baseline. It uses fake time, but real network/database IO belongs in separate integration tests. See [synctest](https://pkg.go.dev/testing/synctest).
- Boundary checks: fuzz wire/policy/receipt decoders for hostile lengths/counts, truncation and arithmetic overflow; retain exact-byte/SVM contract checks. See [Go fuzzing](https://go.dev/doc/security/fuzz/).
- Durable checks: disposable PostgreSQL tests cover claims/revision coalescing, stale-owner rejection, attempt immutability, custody handoff, aggregate reservations and crash/restart transitions.
- Performance checks: benchmark complete realistic planning/projection batches, report allocations, and use CPU/heap/block profiles before changing structures. See [Go diagnostics](https://go.dev/doc/diagnostics).

Add a read-only evidence command that loads a bounded saved snapshot, invokes the same pure decisions as the runtime, and explains eligibility, selected actions and constraints. Structured logs should identify the operation, vault, policy/control revision, market evidence and transition with sanitized reason codes. Operational views should distinguish capture lag, application backlog, oldest unresolved attempt, capacity held awaiting telemetry and rejected admission reasons.

**Suggested first implementation work**

1. Specify units, ordering, commitment, field ownership and allowed transitions against the existing schemas. Turn the coverage and heap reproductions plus collateral/liquidity lineage into bounded regression fixtures.
2. Correct unit interpretation, continuous coverage and the target-state vocabulary as deliberately reviewed behavior changes. Determine production incidence separately through scoped read-only evidence.
3. Assemble the Go module and pure domain functions; retain existing numerical/ABI contracts. Build the full-rescore fleet reference and validate the affected-candidate heap against it.
4. Port the complete Autodeposit family with its current product timing and custody contracts; shadow and cut over ownership together. Then port observer and remaining family execution as outlined in the earlier report.
5. Move canonical exposure construction/hourly telemetry, then prefix materialization with revision-safe publication. Keep TypeScript authentication, API shapes and timezone formatting initially.
6. Evaluate economic improvement with a bounded offline oracle. Adopt a more expensive live optimizer only if measured additional gain justifies its latency, maintenance and modeling burden.

Validation completed for this study: the exact-function APY coverage reproduction was run; the extracted Go economics/admission reproduction compiled and ran without external dependencies; source lineage and schema contracts were independently checked. No repository code was changed, no production data was inspected, and no deployment or writer activation occurred.
