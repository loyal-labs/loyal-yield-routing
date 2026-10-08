# Fleet planner: bounded repair design review

Review date: 2026-10-03. This document proposes a shadow experiment. It does not
authorize a new live selection policy, change a financial writer, or claim global
optimality. Keep the verified full-rescore greedy planner as the runtime default.

## Recommendation

Evaluate a **two-vault target reassignment followed by complete suffix replay**.
Start with one unowned, selected same-mint reserve source and one unselected,
unblocked source competing for its target. Move the selected source to another
already-permitted same-mint target and attempt the competing source at the freed
target. Retain every originally selected vault. A later experiment can exchange
targets between two selected vaults without adding a third participant.

This repairs the measured two-vault adversary. It is not sufficient in general:
three-way assignment chains, several small moves competing with a large move,
tenant/conflict quotas, source-dependent yields, and the notional limit can require
more participants. A trial budget can also miss an improving two-vault exchange.
Report “best feasible repair found within budget,” not “locally optimal.”

Do not add this repair to an old lazy heap. Current stepwise rescoring is correct
against the independent oracle; the opportunity here is improving the objective,
not repairing another stale-priority bug. Profile and reduce the existing planner's
allocation cost before adding an optional live experiment.

## Measured evidence and objective

The independent [oracle](../../go/workers/internal/fleet/wave_oracle_test.go)
shares only the single-edge economic/eligibility primitives. It reconstructs the
entire selected prefix with wide integer sums, enumerates permitted unused-source
edges, and applies independently expressed admission/ranking rules. It does not
call production candidate, ranking, conflict, or rescore helpers.

Its objective is:

```
J(sequence) = sum(ExpectedNetGainUSDMicros of publishable admitted moves)
regret      = J(exhaustive best feasible sequence) - J(greedy sequence)
```

Each edge is repriced against the immutable outstanding commitments plus the
earlier selected prefix. These are order-sensitive, rounded marginal gains under
the current 30-day model, confidence discount, and guarded transaction cost. They
are not final portfolio NAV, realized earnings, a price/quote guarantee, or an
estimate incorporating effects absent from that model.

The measured adversary has two $10,000 source positions:

| Source | Current APY | Permitted target |
| --- | ---: | --- |
| Flexible vault 1 | 100 bps | Target A: 2,000 bps; target B: 1,900 bps |
| Constrained vault 2 | 200 bps | Target A only |

Each target has $500,000 observed supply and a $10,000 inflow frontier. Greedy
chooses vault 1 → A, producing $145.057876 marginal net gain and blocking vault 2.
Exhaustive enumeration chooses vault 1 → B and vault 2 → A, producing $274.655478.
The difference is **$129.597602, or 47.185515% of the exhaustive objective**.
The exhaustive search visits six states. Both results obey the same amount,
permission, capacity, tenant, conflict, and wave limits.

Twelve additional deterministic small fixtures visited 1,279 states and had a
maximum observed regret of 16.5751%. Ninety-six generated fixtures with up to five
vaults and three permitted targets matched the current greedy choice sequence,
also after input permutation. Named tests cover a source-dilution priority increase,
an initially ineligible edge becoming eligible, mixed mints, outstanding holds,
blocked incumbents, hidden cross-mint fee admissions, idle/reserve competition,
clock expiry, deterministic ties, and maximum selected moves. These are bounded
examples, not a fleet-wide regret distribution or a proof that greedy has a useful
approximation ratio.

## Priority ordering is a separate policy

Current ranking is economic priority, annual gain, net holding gain, principal,
observation slot, and normalized source identity, in that order. Economic priority
prices lost yield and expected service time. Tenant/conflict caps constrain resource
use; they do not establish a fair-share or starvation guarantee.

Increasing `J` can lower a chosen vault's immediate priority. In the adversary,
vault 1 must choose its lower-priority target to allow the second vault to move.
Therefore a repair cannot simultaneously preserve exact “best permitted edge at
every step” greedy parity and achieve this improvement.

For the shadow proposal:

1. Keep all originally selected vault IDs. Never evict one to improve total gain.
2. Preserve the original prefix before the earliest changed assignment.
3. Within the remaining assigned moves, choose the freshly highest-ranked feasible
   move at every replay step using the existing ranking contract.
4. Report the changed target, each incumbent's priority change, selection/order
   differences, tenant counts, extra source admitted, and objective delta.

This preserves the priority-based scheduler **within the repaired assignment**,
not the original greedy allocation policy over every permitted edge. Any later
live use is a named selection-policy correction requiring root review. Keep the
current policy as the default and record the experimental policy/version in
research output. Do not silently relabel `priority_version`: changing allocation
policy is not necessarily changing the per-edge priority formula. Durable version
or rediscovery semantics need a separate compatibility review before live use.

## Trial algorithm

Work only on a frozen, coherent snapshot and source frontier. The baseline must
include **all selected admissions**, including unfunded cross-mint selections
that consume limits/frontier but produce no published opportunity. The current
`FleetPlan.Opportunities` alone does not contain that trace.

Keep source records immutable and represent moves by `(source index, target index)`.
Do not clone the whole fleet or regenerate JSON for every trial.

1. Obtain the normal greedy trace and baseline `J`.
2. Enumerate selected same-mint reserve anchors in normalized identity order.
   Prefer an anchor whose target blocks an otherwise valid unused source. Prefer
   unused contenders by the existing economic ranking evaluated at the frozen
   original prefix, then normalized identity. These ordering hints are not bounds
   on the attainable improvement.
3. For a pair `(anchor, contender)`, enumerate the anchor's existing permitted
   alternatives in deterministic order. Both repair participants must have the
   same liquidity mint. Amount, source, collateral evidence, policy bindings and
   authority remain unchanged. Do not invent an alternative, split an amount,
   expand a policy, or convert raw units by assuming different mints are fungible.
4. Preserve the prefix before the anchor. Replace only its target assignment and
   add the contender at the anchor's old target. Fixed suffix assignments retain
   their source and target. Add neither a third repair participant nor an unrelated
   newly eligible source during this trial.
5. Rebuild the prefix ledger from outstanding holds plus the preserved prefix.
   Replay the entire suffix, including the pair and all fixed admissions. At every
   step reprice every remaining assigned edge and choose its highest-ranked
   feasible move. Reapply all economic, amount, lifetime, wave, tenant and conflict
   checks. If any originally selected admission cannot be retained, reject the
   whole trial; partial execution of a trial is never an improvement proof.
6. Idle and cross-mint assignments are fixed for this first experiment. They still
   participate in replay and resource counting. Preserve their publication class;
   reject a trial if changed economics alter a cross-mint fee-fenced admission's
   eligibility/publication class. Idle remains shadow-only. This narrows the repair
   neighborhood without pretending those moves/holds do not exist.
7. Compare the full replay's `J` with the best complete incumbent using checked
   wide arithmetic. Accept only a strict positive delta. Break equal-gain trial
   ties by the replayed priority/normalized-identity sequence. Equal-gain swaps are
   not accepted. After an accepted repair, use its actual trace for the next pass.
8. Materialize canonical execution plans and semantic opportunity identities only
   once for the final complete trace. A different target requires a recomputed
   identity; never reuse a prior target's bytes or mutate a persisted operation.

Capacity must be replayed through the existing single-edge band model. An outflow
can change modeled yield; it is not permission to subtract that outflow from the
target's gross inflow ceiling. Outstanding committed inflows/outflows are
reserve-wide values and are counted once, not once per source/vault.

Do not add a general reserve-DAG rule. Current code rejects self-targets and
prevents a vault from moving twice; it has no universal reserve-cycle guard. The
oracle's closed-path fixture proves only that its uneconomic closing edge is
rejected. A new graph constraint would be another deliberate policy change.

## Explicit resource bounds and fallbacks

Suggested research defaults, to calibrate rather than treat as an SLO:

| Limit | Proposal |
| --- | ---: |
| Accepted-repair passes | 2 |
| Anchor candidates per pass | 8 |
| Contenders per blocked target | 8 |
| Alternatives per anchor | 4 |
| Fully attempted trials across both passes | 64 |
| Additional single-edge evaluations across both passes | 32,768 |
| Cooperative repair elapsed budget | 25 ms |

With `C` permitted edges and wave limit `K`, baseline full rescore costs `O(K*C)`
edge evaluations. A fresh-ranked replay of an assigned suffix of length `S` costs
at most `S*(S+1)/2` edge evaluations, plus ledger/count work. Bound that aggregate
with the explicit evaluation cap; do not invoke `PlanFleet` over all `C` candidates
inside every trial. A 128-move full suffix can need 8,256 evaluations for one trial,
so the budget may permit only a few early-anchor alternatives. A short suffix is
cheaper. The candidate-count caps knowingly prune potentially valuable alternatives.

Store candidate references once, use a scratch ledger/count buffer for replay,
and retain only the best complete trace. Working memory should be `O(C + R + K)`
for permitted edges, relevant reserve/conflict counters and the chosen trace,
independent of trial count. Allocation measurements must verify this proposal;
the notation does not prove low transient allocation in Go.

On repair budget exhaustion, return the last complete feasible incumbent with
explicit truncation statistics; it may be the original greedy trace. On caller
cancellation, return cancellation and do not publish a fallback. Check budgets
between edge evaluations, and keep repair work synchronous with its owner. A
cooperative 25 ms limit cannot preempt a single evaluation, GC pause, or scheduler
delay. Arithmetic overflow, unknown custody/evidence, missing permission, and an
invalid initial snapshot must fail closed, never become zero capacity or free
resources. A failed trial must not mutate the incumbent or its ledger.

## Execution and source compatibility

Repair operates before publication, on unowned planning intent only. Never move,
clear or reassign an already persisted opportunity, active reservation, pending
decision, signed journal, custody checkpoint, or lookup-table usage lease. An
incumbent owned by another family remains blocked; its outstanding reservation
remains part of the frozen base frontier.

Preserve the current canonical same-mint JSON, amount semantics, policy recipes,
and per-target opportunity identity construction. Cross-mint whole-route and
one-collateral-unit recovery anchors remain untouched. No schema, dependency,
signer, broadcast, or generalized execution interface is needed for the experiment.
Wire/reader compatibility can remain unchanged; exact legacy selection parity
cannot remain unchanged when a beneficial repair is chosen.

Every published repaired route still passes normal `Store.Publish`, fresh bank
observation/policy validation, `PrepareExecution`, and locked
`CommitExecutionAdmission`. The executor still checks the opportunity/fencing
token, epoch/lifetime, aggregate capacity, conflicts, exact balance anchors,
ALT generation/mutation/member vector, and immutable signed handoff. Prediction
is not admission or financial ownership. Concurrent telemetry, another executor,
or changing custody may invalidate an entire repair after planning. Reject that
admission normally; do not weaken a fence to realize a projected gain.

This proposal adds no capacity-release route. Signed uncertainty and
`awaiting_telemetry` holds remain reserved until their family proves release.
Unsigned expired-admission recovery remains its separate existing protocol.

## Minimal implementation surfaces after review

First implement the repair as a test/replay-only experiment. No runtime feature
flag or publishing path is necessary to establish whether it improves this cohort.

| File/function | Narrow proposed change |
| --- | --- |
| `internal/fleet/revalidator.go:planFleet` | Separate selection from final JSON materialization. Return a private full admission trace plus rejections; preserve the default output exactly, including hidden fee-fenced consumption. Keep all public greedy entrypoints unchanged. |
| `internal/fleet/wave.go` | Add a compact private assigned-move/selection-state type and shared admission/ranking access needed for exact replay; keep limits and their meanings unchanged. |
| New `internal/fleet/wave_repair.go:repairWaveShadowAt` | Implement the bounded pair enumeration, scratch-ledger suffix replay, strict objective comparison, counters and cancellation. No store, RPC or signer dependency. |
| `internal/fleet/revalidator.go:canonicalExecutionPlan` and `opportunityIdentity` | Reuse unchanged after the final trace. No trial-time serialization and no copied hash/wire implementation. |
| New `internal/fleet/wave_repair_test.go` | Exercise the experiment using frozen snapshots and the independent oracle. No new DB fixture is necessary for pure search; reuse existing admission tests if live integration is later proposed. |
| `internal/fleet/wave_oracle_benchmark_test.go` | Add bounded repair benchmarks separately from the baseline; report edge evaluations, trials, truncation, elapsed time and allocations. |

Required meaningful tests before a live proposal:

- The named two-vault adversary improves to the enumerated best objective without
  dropping the original selected vault or changing its amount.
- A three-participant exchange fixture remains suboptimal with pair repair;
  demonstrate the limitation rather than enlarging the algorithm implicitly.
- Source/target interaction reprices all suffix moves and rejects a gain that
  disappears after replay. Initially ineligible permitted alternatives remain
  available; cached priority alone never accepts a trial.
- Repaired traces are independently feasible under notional, tenant, conflict,
  lifetime, publication-fee and outstanding-hold constraints, including mixed
  mints and shadow idle/reserve competition for one vault.
- Deterministic input permutation and equal-gain ties; no duplicate vault, amount
  split, invented route, dropped hidden admission, or changed immutable input.
- Evaluation/trial/pass limits, caller cancellation, overflow and failed-trial
  rollback all preserve the last complete incumbent or return the intended error.
- Exhaustive small fixtures establish `J(repaired) >= J(greedy)` and
  `J(repaired) <= J(exhaustive)` for this objective. Do not assert equality with
  exhaustive search for every fixture.
- Existing default `PlanFleet` decisions/rejections/JSON/idempotency outputs still
  pass exact greedy oracle parity after the selection/materialization extraction.

## Benchmark caveats and review provenance

The [baseline benchmark](../../go/workers/internal/fleet/wave_oracle_benchmark_test.go)
ran one iteration per case on Go 1.26.6, darwin/arm64, Apple M4 Pro, default Go
parallelism reported as `-12`. Every case selects at most eight moves; fixtures use
synthetic same-mint permitted targets, known amounts and ample capacity. These are
not production workloads, percentile latency, a live 128-move measurement, or
peak resident memory. Concurrent machine activity and GC can affect timings.

| Vaults | Permitted targets/source | Elapsed/op | Cumulative allocated bytes/op | Allocations/op |
| ---: | ---: | ---: | ---: | ---: |
| 256 | 1 | 1.493 ms | 1,688,400 | 13,935 |
| 256 | 4 | 4.474 ms | 6,657,760 | 46,820 |
| 256 | 16 | 18.631 ms | 27,486,832 | 176,570 |
| 4,096 | 1 | 19.902 ms | 29,743,048 | 209,931 |
| 4,096 | 4 | 72.256 ms | 125,521,736 | 753,655 |
| 4,096 | 16 | 300.038 ms | 514,042,024 | 2,895,542 |

The largest graph has 65,536 permitted edges. Profile CPU and allocations to find
their contributors before claiming a specific hot spot or a memory reduction.
Do not transplant an affected-candidate heap until it matches the independent
step oracle, including candidates whose priority increases or becomes eligible.
Index both source and target dependencies; dense shared-reserve inputs may still
affect most candidates. A heap optimization preserves greedy behavior and does
not solve the assignment regret above.

Reproduction commands from `go/workers`, with cached pinned dependencies:

```sh
GOTOOLCHAIN=local GOENV=off GOPROXY=off go test -race ./internal/fleet -run '^TestWaveOracle' -count=1
GOTOOLCHAIN=local GOENV=off GOPROXY=off go test ./internal/fleet -run '^$' -bench '^BenchmarkWaveOracleSparsePermittedGraph$' -benchtime=1x -count=1
GOTOOLCHAIN=local GOENV=off GOPROXY=off go vet ./internal/fleet
```

Focused oracle race passed in 1.530 s; vet passed. No DB, provider, production
capacity, or connected-chain claim is made by these experiments.

The retained routing baseline is `05338bbb70ce2e5287956f6e3659964e37bfa2f6`, recorded
in [sources.json](sources.json), with original planner files under
`go/kamino-fleet-planner/internal/fleet`. The integration checkout HEAD at review
is `5e1045f027efd9469a935f395a3ab657071a8ab0`; reviewed root corrections and these
oracle files are uncommitted, so that HEAD alone does not pin the measured/reviewed
working source. SHA-256 of the reviewed files at this review snapshot:

| File under `go/workers/internal/fleet` | SHA-256 |
| --- | --- |
| `revalidator.go` | `59de34cbefd4393da4dd8ae68ebab239ab30074834bf3d2722df570e9967dae3` |
| `wave.go` | `52f6af8abff0355ba093eb9fc0f2bfac5400a8ab4ff154c6bbb8d9a45ac449da` |
| `planner.go` | `c0e95cddfd759f41518c5a9a2b55dddafe9f68d78cda4b752357be9f96b4e809` |
| `execution_admission.go` | `3ffd9fb6b092a34f289f025dcda06d384bca11f1022da1b90e6408a93dbaad0b` |
| `revalidation_store.go` | `fc408f17050d4845262844d9d6e916716ce1bbce9ee3fe56f41a001e243162d4` |
| `wave_oracle_test.go` | `f1f9d9c2b6e2a08abf9fbae5641d38fb3e800af1cc392edf3a27fcaebce4a6ae` |
| `wave_oracle_benchmark_test.go` | `0b37cc8f8fac6f52ba7314ff5a7d7f5e7cafe1791b1dcd404f50563114fde214` |

Commit the reviewed implementation/tests before treating this working-tree
snapshot as a release reference. Retained ABI/SVM and actual admission proofs
remain separate requirements from an algorithm or benchmark result.
