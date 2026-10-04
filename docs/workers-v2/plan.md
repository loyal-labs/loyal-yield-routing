Loyal workers v2 — parallel implementation plan, 2026-10-02

Implementation is authorized on isolated rewrite branches. No main merge, deployment, live writer activation, or migration-resource changes are authorized. Root integrates and verifies; GLM Flash lanes implement scoped features. The prior [audit](audit.md) and [data/algorithm study](data-algorithms.md) supply the rationale and source evidence.

The outcome is one Go module, an observer, a retail engine, and a separately credentialed Backyard instance of the engine. Existing SSE stays initially. Existing Rust ABI/on-chain proof code and the small official KLend helper remain where exact compatibility warrants them. Multiply is a distinct family and has its own later implementation lane; it cannot disappear merely because Backyard joins the engine.

Development proceeds against saved fixtures and disposable databases while the current infrastructure migration continues. The first deliverable is complete Autodeposit progress with both applications closed. It may consume existing Rust observations, so a finished Go observer and a completed Hetzner migration are not prerequisites for developing or verifying that deliverable.

Branch and source ownership

- Routing integration branch: `codex/workers-v2`, neighboring worktree `/Users/user/loyal/.worktrees/workers-v2-integration`.
- Each implementation lane has a separate `codex/workers-v2-<lane>` branch/worktree. Only root lands reviewed changes into the integration branch. Agents never push or merge main.
- Apps changes use a separate `codex/workers-v2-app-contract` branch in Loyal Apps. Routing and Apps remain distinct repositories and release artifacts.
- Branch creation refreshes main and records the exact base. Do not derive the rewrite from the dirty shared Backyard feature checkout or copy another migration lane's uncommitted work.
- Changes made by migration threads are imported deliberately at stable checkpoints. Record the source SHA and selected changes; do not periodically merge all migration branches into the rewrite.
- Preserve existing table/column contracts through adapters first. Any required additive migration is root-owned and tested with old readers and writers. Reserve migration filenames only after checking concurrent migration work. No automatic production schema application.
- Infrastructure connection settings enter through configuration. Do not encode Render/Hetzner hostnames or assume the App, Yield, and Timescale databases have become one transactional database.

Verified local source inputs

| Input | Local source revision | Use |
| --- | --- | --- |
| Routing audit/main | `05338bbb70ce2e5287956f6e3659964e37bfa2f6` | Rust behavior/schema baseline; existing Go fleet and Backyard |
| Apps audit/main | `81b5c0256317c46e4fa9fdac49d6c0b347530b03` | Authority, API compatibility, read repairs and earnings |
| `origin/ASK-2169-go-laserstream-service` | `36a6664c125eeecd5e0ab5ea9218e72432da055d` | Review/reuse combined Go transport and handlers before building another transport |

The current LaserStream branch is more substantial than the earlier transport-only memory: its README describes a combined Go subscription, domain handlers, durable acknowledgements, and a supervised Rust `earn-domain-bridge`. Source presence and README claims do not establish a successful deployment or current test results. Review its handoff verifier and bridge boundary before adoption; replace bridge responsibilities incrementally with equivalent Go application evidence.

There is a toolchain mismatch to close in the seed step: fleet/Backyard declare Go 1.25.1; this observer branch declares Go 1.27.0; the installed toolchain reports Go 1.26.6. Do not copy a non-buildable toolchain directive into the new module. Check actual language/dependency requirements, choose a supported available toolchain, and explicitly validate any adjustment. Retain the existing pgx and Solana SDK versions unless a reviewed transport requirement forces a separately explained change. Pure feature work need not wait for transport dependency reconciliation.

Files root seeds before delegation

```text
docs/workers-v2/
  plan.md                         # milestones, lane ownership and retirement matrix
  contracts.md                    # units, authority, schemas, ordering, durable transitions
  sources.json                    # source SHAs, reused files, fixture hashes and corrections

go/workers/
  AGENTS.md                       # local implementation and verification rules
  README.md                       # concise build/run/replay/support commands
  go.mod
  go.sum
  Makefile                        # fmt, vet, test, race, build, explicit integration gate
  cmd/
    loyal-observer/main.go
    loyal-engine/main.go
    loyal-evidence/main.go
  internal/
    engine/{config,runtime,health}.go
    db/{pool,schema,lease}.go
    solana/{amount,evidence,wire}.go
    autodeposit/types.go
    observer/types.go
    fleet/types.go
    fleetexec/types.go
    backyard/types.go
  testdata/
    manifest.json
    autodeposit/
    observer/
    fleet/
    backyard/
  Dockerfile                      # local immutable build; publishing is separate
```

The seed is a small compiling foundation with reviewed types and real shared behavior. It does not pretend an unimplemented worker is ready. Entrypoints have thin `run() error` composition; missing implementations/configuration reject startup or readiness. Root owns shared files, dependency changes, entrypoint wiring, migration edits, fixture provenance and integration changes. Family type files transfer to their lane after the initial contract review.

Seed only demonstrated reuse: explicit pool/schema checks, existing fence mechanics, checked amount conversion, evidence identity, and signed-wire ownership. Family SQL and lifecycle records live with the feature. Introduce a shared custody helper only when its SQL contract has been verified against current legacy writers. A generic operation table, generic executor, plugin registry, ORM and workflow engine are unnecessary.

`contracts.md` must specify:

- Collateral versus liquidity versus USD units; mint/decimals/conversion provenance; checked SQL BIGINT boundaries; unknown values distinct from zero.
- Desired controls and revisions versus chain observations versus effective eligibility. Temporary ineligibility never silently rewrites desired user intent.
- Each field/table writer, commitment requirement, complete-versus-partial snapshot semantics, stable event identities and lifecycle ordering.
- Capture progress versus applied progress; exact coalesced-request revision acknowledgement; cross-database checkpoint boundaries.
- Family-owned operation state versus transaction-attempt state; immutable wire/hash, signed/intent/submitted distinctions, ambiguity and no-effect evidence.
- Cross-family autonomous custody ownership, including old Rust processes, user-withdrawal interaction and existing vault-index/permission boundaries. Database fences do not revoke signed Solana transactions.
- Capacity `active -> awaiting_telemetry -> released`, with the precise newer-observation rule.
- Unit, APY gap, target-status vocabulary and heap corrections as named behavior changes with separate expectations from legacy parity.

Seed source-derived bounded fixtures for normal flows, control changes, duplicate/reordered events, malformed amounts, stale owners, ambiguous sends and custody handoffs. Bring in the reproduced 60-hour coverage and changing-priority heap cases. Each fixture identifies its source/evidence and whether the expectation is legacy parity or an intentional correction. Exact instruction/message cases retain independent Rust/SVM proof where needed.

Seven logical implementation lanes; at most five active implementers

| Lane | Files owned | Concrete delivery and completion evidence |
| --- | --- | --- |
| A — Autodeposit | `internal/autodeposit/{lots,decide,schedule,store,reconcile,build,advance,worker}.go`, transferred `types.go`, feature tests | Entire target/controller family: artifact recovery, eligibility, bootstrap discovery, floor/control rebaselining, lot accounting, scheduling, destination preflight, pull, top-up, accounting and restart recovery. Preserve one-hour/coalesced deadlines and event IDs. Freeze destination before pull; retain idle custody through top-up. Demonstrate app-closed progress and restart at each changed durable boundary, including while desired enablement is false. |
| B — Observer | `internal/observer/{capture,inbox,apply,receipts,balances,reserves,policies,projections,store,worker}.go`, transferred `types.go`, feature tests | Review/adapt existing combined transport; port remaining proof/application work. Commit durable capture and cursor together; commit dedupe, financial effects and application completion together; preserve per-vault ordering and sibling rules. Cover overlap, duplicate/reordered delivery, reconnects, projection completion, revision coalescing and cross-database recovery. No signer. Reserved transport subdirectories belong to this lane if reuse requires them. |
| C — Fleet planning/admission | Reused `internal/fleet` economics/planner/wave/revalidator/store files and tests | Reuse existing Go planner. Build deterministic full-rescore reference, sparse permitted-target enumeration, coherent input and fresh locked admission. Implement affected-candidate heap updates as an explicit correction; compare against reference and bounded exhaustive oracle. Preserve fee/risk/authority/capacity/conflict constraints. No signer or broadcast client imports. |
| D — Fleet execution/recovery | `internal/fleetexec/{journal,build,advance,receipts,reconcile,lookup_tables,store,worker}.go`, transferred `types.go`, feature tests | Port existing same-mint/idle and cross-mint route lifecycles in successive increments. Own exact attempts, fresh execution checks, custody checkpoints, confirmation/reconciliation and ALT lifecycle. Keep held capacity until required telemetry. Independently prove no duplicate spending, stale-owner rejection, restart recovery and actual receipt effects. |
| E — Backyard integration | Reused `internal/backyard` controller/build/recovery/store files and tests | Move existing Go implementation into the module with minimum behavioral change. Preserve fixed manifest lane, safety/withdrawal/NAV priority, actual-effect caps and distinct recovery protocols. Replace Render-specific instance identity while retaining release identity and fencing. Demonstrate restart and ambiguity behavior under separately scoped configuration and credentials. |
| F — Read models and Apps contract | Routing `internal/observer/{telemetry,maintenance}.go` and tests; allowlisted Apps API/repository/tests in separate Apps worktree | Transfer hourly allocations/share prices, public simulation and derived health. Preserve clocks, coverage and actual sample evidence. Prepare explicit reconciliation requests and read-only GETs, then gate removal of each old repair path on replacement ownership proof. Preserve user-wallet signing, HTTP/mobile compatibility and withdrawal cleanup. Fix the unit/coverage/status defects in separately reviewable commits. Personal calculator and timezone formatting remain TypeScript initially. |
| G — Multiply controller | Later `internal/multiply/{types,observe,decide,policy,build,advance,store,worker}.go` and contract tests | Explicitly map `crates/loyal-fleet-worker/src/bin/multiply-route-worker.rs` and `src/multiply/*`. Review permission/ABI/recipe contracts, then port the bounded lifecycle into retail engine composition. Retain its existing service until exact action and recovery evidence passes. It is a distinct family from Backyard and ordinary reserve swaps. |

`internal/fleetexec` is a capability boundary: the decision package can compile and be reviewed without access to signing/broadcast APIs. It consumes typed plans from `internal/fleet`; the planner does not import the executor. Both remain one retail feature composed by the engine. Root reviews additions to packages, exported surfaces and dependencies rather than letting lanes invent competing shared abstractions.

Dispatch waves and first deliverable

1. Root prepares the branch, compiling seed, schema/authority contracts and fixture manifest. Validate one bounded GLM runner launch before issuing implementation work.
2. Start A, B, C, E and F with GLM Flash, in five isolated worktrees. A begins pure lot/controller behavior and can use existing Rust observations. B first assesses/reuses transport. C establishes the reference and admission model. E reuses current Go. F inventories and stages consumers without removing active repair owners.
3. As C or E lands, use the free slot for D from the reviewed integration head. Root wires and verifies complete Autodeposit first. D proceeds after plan, custody and durable-attempt contracts are stable; it does not need to wait for the infrastructure migration.
4. F transfers scheduled recording after B's projection contract is stable. Remove app repair writers only when A/B replacement coverage is demonstrated. Domain-specific observer bridge responsibilities are retired only after equivalent application proof.
5. Start G in a released slot once shared attempt and custody mechanics have been exercised by A/D. Keep its scope bounded by the current reviewed manifest and policy recipes.
6. Integrate migration's final schema/configuration changes in a focused compatibility pass. Re-run disposable-database and old/new compatibility gates. Keep all changes on rewrite branches. Main merge and writer activation are external gates after migration readiness and explicit release authorization.

Production consolidation target is three Go worker instances plus existing SSE only after all mapped families are ported. During transition, retained legacy services and the Rust observer compatibility bridge remain explicitly visible in the responsibility matrix. No service is counted as replaced because a new binary builds.

Responsibility retirement matrix to seed

| Existing responsibility | Replacement owner |
| --- | --- |
| Kamino reserve monitor, balance-sweep ATA monitor | B observer transport/verified observations |
| ATA projector, Squads policy monitor | B observer durable application/projections |
| Autodeposit trigger and its app repair callers | A engine Autodeposit plus F explicit app controls |
| Rust opportunity planner, Go Kamino fleet planner, route revalidator | C retail planning/admission |
| Route executor, confirmer, reconciler, lookup-table provisioner | D retail execution/recovery; B confirmed external-event projection |
| Backyard worker | E separately credentialed engine instance |
| Multiply route worker | G retail Multiply family |
| Fleet health projector, app hourly recording/public simulation cron | F observer bounded maintenance |
| Realtime SSE | Retained initially |

Track staging services, alternate entrypoints, maintenance commands and app bridge writers alongside the production manifest inventory. For every deletion, record the old writer, new writer, tables/effects, acceptance evidence and unresolved work owner.

Root supervision and code quality

- Use the existing `/Users/user/.claude/scripts/agent` GLM provider with explicit `glm-5.3-flash`, worktree cwd, per-lane label, bounded turns/timeout and explicit tool scope. This turn verified local runner configuration only; live launch/model execution remains to be checked. No silent model substitution. Disable nested agent spawning for implementation lanes so concurrency and write ownership remain visible.
- Each task names its exact allowed files, source paths, input/output contract, current milestone, required evidence and forbidden scope. Shared edits become requests to root. Worktrees prevent merge collisions but are not a security sandbox; implementation processes receive no production secrets or deployment authority.
- Require an early concrete checkpoint: exported signatures and a compiling, tested implementation increment. Broad exploration without an increment is redirected or split.
- Review small commits in dependency order. Root checks the diff, source semantics, independent fixtures and integration result; an agent's completion message is not correctness evidence. Reject generic frameworks, float spend accounting, zero-as-unknown, silent failure, fabricated rate history, and ambiguous-send retry wrappers.
- Keep IO synchronous with context/deadlines; the runtime owns bounded concurrency and joins all goroutines. Recovery has reserved capacity. Ordinary operation failures are persisted and classified rather than crashing unrelated work.
- Keep SQL beside the feature, transactions explicit and brief, network IO outside long-held locks, and serialization retries tied to reread state. Preserve old-binary/new-schema compatibility.
- Use consumer-defined small interfaces only where a real caller needs them; concrete constructors and typed family decisions elsewhere. This follows [Go layout guidance](https://go.dev/doc/modules/layout) and [interface guidance](https://go.dev/wiki/CodeReviewComments#interfaces).
- Evidence CLI uses the same decision functions as the runtime. Logs identify operation/vault, revisions, evidence and transition reasons with no secrets. Readiness reports real schema/ownership/application freshness and unresolved recovery health.

Acceptance gates

- Fast offline: formatting, vet, focused semantic fixtures and all three builds. Meaningful race/cancellation tests for exercised runtime paths; hostile decoder/arithmetic fuzz cases where boundaries change.
- Durable isolated: disposable PostgreSQL and Timescale where required, real schemas, legacy/new writer contention, stale fences, coalesced revisions, duplicate events, attempt immutability, crash/restart and capacity release. Tests do not alter migration staging or production.
- Financial contract: exact-byte Rust/Go parity and existing SVM action/receipt checks; deliberate behavior corrections use separate expectations. Modeled, simulated and realized returns retain distinct meanings.
- Shadow: saved replay first; later read-only observation with canonical writes, signing and broadcast unavailable. Compare complete decisions/constraints and record explained divergences. This task keeps even inactive code off main until migration readiness and later merge authorization.
- Ownership cutover: enumerate every legacy nonterminal operation, signed attempt, custody claim, capacity reservation and referenced ALT. Drain or adopt exact identities; fence old writers; reconcile pending requests; activate a bounded family canary. Unadopted signed uncertainty blocks new admission for the affected custody scope.
- Rollback: stop new admission while retaining recovery of possibly sent work. Rollback has an explicit owner for new journals/reservations; old binaries never blindly reinterpret new attempts. Database authority/catch-up acceptance and worker-family activation remain separate gates.

First milestone acceptance: Autodeposit setup/funding/control changes/pause/close and both transaction legs complete with web/mobile closed; crash recovery converges without duplicate spending; temporary missing-position conditions preserve desired intent; concurrent legacy fleet work cannot claim the same idle custody; app read repair deletion has direct replacement evidence.

Progress is measured by eliminated writers/services/repair callers, app-independent completion, proven restart behavior, bounded backlog/admission latency and the code that can actually be deleted. Avoid promising a percentage reduction before those deletions exist.

Implementation acceptance — 2026-10-04 UTC

The branch implementation is complete for the reviewed contracts below. Routing
is on `codex/workers-v2`; Apps is on `codex/workers-v2-app-contract`. Main,
production and migration resources remain untouched. No legacy writer or service
is counted as retired. This acceptance covers branch implementation and isolated
proof, not production activation or mature protocol execution.

Runtime source `85e92d0831d7e9aa0a66f45c54830a7ca16ab5f3` passed
[branch CI 37183509997](https://github.com/loyal-labs/loyal-yield-routing/actions/runs/37183509997).
Both jobs completed successfully: registered family databases, actual Timescale
ATA extension/hypertables, formatting, vet, package race tests and all three Go
builds; and the locked Linux image with all three roles and retained Rust tools
probed without network or credentials, read-only and as UID 65532. Financial
local-bank suites have separately enabled inputs and are not implied by a generic
CI PASS. The final acceptance commit changes documentation only; the runtime
source and executable verifier remain exactly those of the proved revision.

Apps source is `1ee615bcb22f29b04af3864cf73264a3b11eca40`. Its actual registered
intent-only SQL gate passes 14 assertions; seven GET ownership handlers pass
18 tests/132 assertions in three isolated Bun processes. Legacy load-state
34-test/78-assertion and financial/configuration 40-test/106-assertion subsets
pass. Four new tests pass standalone compatible Biome. Whole-web typechecking
has 74 diagnostics and is not a full PASS; none points to the changed paths.
Identity lookup, authenticated user-wallet POSTs and withdrawal cleanup retain
Apps ownership. No frontend build or deployment ran.

| Responsibility | Reviewed implementation and executed proof | Retained owner or later gate |
| --- | --- | --- |
| Shared runtime and Backyard | One module, borrowed pools, scoped keys and joined shutdown; separate Backyard credentials. Late retail callbacks cannot reopen readiness. Full race/build and locked Linux probes pass | No live writer ownership cutover; preserve fixed manifest, safety/withdrawal/NAV priority and existing recovery protocols |
| Fleet planning | Independent greedy oracle and fresh locked admission pass. Full-rescore decisions preserve changing shared-reserve priority; frontier refactor cuts synthetic allocation bytes by 79% | Verified greedy default; bounded assignment repair is a shadow proposal. Measured assignment regret remains explicit; no global optimum claim |
| Observer | Independent Apps UUID/Yield watch discovery; bounded startup/refresh/verification and stream gates; atomic Go ATA application and ordered source capture commit. Actual Timescale and packaging gates pass | Unsigned Rust Earn application/Claim bridge remains. Old capture producers must quiesce before scalar cursor cutover |
| Autodeposit | Desired revisions/generations, pause/resume, policy/position return, unsigned repair, creator history, scoped identity and captured-revision admission. Registered SQL/current Go pull/top-up bank passes with Apps closed; refreshed current-mock gate passes in 8.629s | Real Squads/Subscriptions/SPL with explicit KLend mock scope; migration and production cutover remain external |
| Fleet execution | Current Go C-to-D canonical vault-1 same/cross-mint lifecycles, exact-wire recovery and custody checkpoints pass. Unsigned ALT catalog/request planning, binding publication, satisfaction, expired rollback cleanup and real ALT cooldown/refund pass; full two-database/local ALT-program race gate passes in 8.638s | Exact legacy adoption/draining, custody reconciliation and writer fencing before activation; mature KLend/Jupiter execution is outside mock financial proof |
| Multiply | Exact signed receipt attribution and fenced immutable publication; full registered/current-Go local bank race passes in 28.517s, including leveraged deploy/unwind, claimable payout, lost response, keyless first-send, expiry and duplicate-packet refusal | Fixed-price protocol mocks limit the bank proof. Wallet signing/Claim admission remains App-owned; no mature KLend/Jupiter claim |
| Read models and Apps | Bounded hourly recording/public simulation, actual observation clocks and coverage; intent-only v2 floor/toggle writes and seven GET replacement-ownership proofs. Registered/Timescale gates pass | TypeScript personal calculator/timezone formatting, identity lookup and wallet POST/withdrawal cleanup remain. Existing SSE stays; no App cron or service is retired |

The final unsigned ALT increment has independent retail readiness. Actual local
PostgreSQL/ALT-program proofs cover catalog create/extend/drift rollover (2.307s),
retiring-table deactivation/cooldown/exact refund with paused keyless recovery
(1.847s), and request satisfaction plus rollback/usage/pause protection (2.089s).
Census and leasing share live economic priorities; idle loops avoid PDA probes,
and cleanup runs before planning failures. Actual C `Store.Publish` waits before
Go satisfaction and readmits the same immutable opportunity afterward (1.994s).
Waiting-state/consumer setup is a registered SQL fixture; this does not claim a
new full `CommitRevalidation` producer proof. Root's full fleet execution/ALT race
suite passes in 8.638s, followed by the final committed CI gate above.

Completion audit against contracts.md

| Contract | Acceptance evidence |
| --- | --- |
| 1 — App-independent Autodeposit | Registered controller/lots/control/creator-history tests and current Go pull/top-up local bank; exact attempts, desired pause and custody recovery remain separate from new admission. Apps v2 controls publish desired revisions rather than scheduling work |
| 2 — Durable observer | Atomic capture/job/cursor and dedupe/effects/application checks; actual COMMIT cancellation/replay; two-database ATA ordering and conflict tests; reconnect/handoff cancellation races and actual Timescale DDL |
| 3 — Deterministic planning and admission | Independent full-rescore greedy oracle, shared-reserve priority cases, fresh locked policy/custody/conflict/capacity checks and bounded exhaustive assignment study |
| 4 — Retail attempts and ALT ownership | Current Go same/cross-mint SVM lifecycles, lost-response exact-wire recovery, capacity telemetry frontier; actual ALT program planning/publication/retirement and keyless recovery |
| 5 — Backyard preservation | Reused fixed-manifest controller and family recovery code; decision precedence, actual-effect caps, immutable wire/receipt/authority tests and real database lease fencing; independently scoped engine composition |
| 6 — Distinct Multiply family | Pinned SDK recipe/envelope cases plus current Go leveraged local-bank deploy/unwind, immutable receipt/source CAS and no-effect/keyless recovery tests |
| 7 — Read models and Apps | Actual hourly/sample/coverage SQL gates, separately labeled modeled/public/realized outputs; seven GET ownership tests, actual desired-intent SQL and retained user-wallet/withdrawal contracts |
| 8 — Legacy and durable compatibility | Registered family SQL races for shared leases/custody, stale fences, immutable attempts, demand generations and newer-telemetry capacity release; additive migrations 0087–0091 remain branch-only |
| 9 — Runtime and support | Full formatting/vet/race/all-three-build verifier, cancellation/join readiness tests, runtime-function evidence CLI and locked Linux role/tool probes |

The planner review resolves the earlier acceptance question: exact greedy parity
is the selection contract. Full rescoring handles priorities that rise or become
eligible after shared-reserve changes. A dependency heap adds complexity without
improving assignment quality; it is deferred until profiling justifies it. This
is an explicit departure from the initial heap implementation proposal. The
measured two-vault assignment regret remains a policy limitation; no new live
allocation policy is introduced. Root review and native implementation lanes
completed the work after automatic approval review rejected further private
source/context transfer to Z.ai; no provider restriction was bypassed.

Before merge or activation, the release owner must accept the database/migration
handoff and deliberately reconcile its final schema/configuration with these
branches. Enumerate and drain/adopt legacy nonterminal operations, exact signed
attempts, custody/capacity claims and referenced ALTs; fence old writers and
quiesce old capture producers. Activate one bounded family canary with a named
recovery/rollback owner. Keep recovery of uncertain signed work available when
new admission is stopped. Main merge, production deployment/activation, live
proof and legacy service retirement require their own release acceptance.

Historical implementation checkpoints

All entries below record earlier executed evidence and the open work at that
time. Their pending/remaining statements are superseded by the acceptance audit
above. They remain for provenance; an older green revision never verifies later
source, and a mock or sampled fixture never becomes production proof.

Last committed routing head before the controller increment is `5e1045f0`.
Branch-only CI run 37097141373 passed its actual registered PostgreSQL/Timescale
fixtures. The separate sampled watch fixture is not complete production-schema
acceptance. Migration 0071 production-bound data activation is excluded.

The integrated controller/fleet/Multiply/read-model increment passes the offline
combined verifier. A task-owned PostgreSQL 17 service on loopback port 51913
applies the baseline 77 registered schema files and hash-pinned historical Apps schema; the retained floor enum dependency is separately verified through additive 0089 in the A fixture. Fresh CI fixtures resolve all 78 registered files and record only actually completed migrations in their ledger;
it excludes production-bound 0071 activation. Family database races passed:
Autodeposit 3.323s, fleet planning/admission 4.179s, same-mint execution 2.167s,
Multiply 2.084s and read-model SQL 1.460s. These are isolated contract fixtures,
not migration cutover or production fleet proof. Local Timescale gates remain
skipped because its extension is unavailable; branch CI runs real Timescale.
Root reused the Solana SDK and
fleet policy matcher, preserved the complete signature, fenced claim release,
re-read controls under the target lock, preflighted the official top-up before
pull, simulated exact persisted bytes before broadcast intent, and accounted
only exact receipt balances. Setup recovery now requires actual account readback
at or beyond the confirmed receipt slot. Whole-family acceptance remains open.

The latest complete Autodeposit registered race suite passes in 6.702s, including
concurrent retained Apps floor SQL versus Go control reconciliation. The native
Claim bridge rejects partial payout and requires the exact signed root-permission
Squads Transaction envelope before accounting. Its private proof still does not
assert App subject identity absent from the saved request.

The connected same-mint gate passes in 14.440s with fresh evidence run
`workers-v2-same-mint-20261004T011754Z`: Go planning/preflight to retained Rust
executor/confirmer/reconciler, real Squads SBF and explicitly mocked KLend/Jupiter.
It proves pre/post persistence crashes, ambiguous broadcast recovery, stale-owner
rejection, exact wire replay, reconciliation, terminal balances and capacity
release. It is not new Go D lifecycle proof or canonical retail index-1 proof;
the current independent Go execution gate is recorded below. Actual canonical Autodeposit
creator bytes also execute through real Squads with valid sequential policy seed.
The A fixture documents its authority-init low-rent profile and actual top-ups
before restoring default Rent; no deployed rent correctness is asserted.

The official KLend proxy built with locked offline Cargo inputs. Existing real
proxy tests pass; connected fleet SVM proof remains a separate gate. Multiply's
44 SDK instruction recipes cover farm variants and token programs, and its
actual Squads/SPL local SVM proves root-authorized claim payment and rejection of
the delegate. Neither fixture executes the mature KLend SBF.
Fleet exact arithmetic passed 501,164 fuzz cases against a wide reference.
Synthetic 4096-vault/one-target/max128 probes improved from approximately
672MB/837ms to237MB/229ms on M4Pro/Darwin/Go1.26.6; these are synthetic local
observations, not production sizing. A separate bounded eight-move, single-run
oracle benchmark measured 4096 vaults × 16 targets at 300.04ms and 514MB cumulative
allocation (2.90M allocations). A two-vault constrained-target counterexample
produced $145.057876 greedy gain versus $274.655478 exhaustive cumulative
marginal gain (47.19% regret). That objective differs from final NAV, and source
economic-priority ordering is not a global assignment solver. The algorithm
review must resolve this tradeoff explicitly before planner acceptance.

Apps financial/config tests51, repository tests16 and actual mobile GET tests2
passed in isolated test processes. New-file lint passed. Whole-web typecheck
retains its baseline62 unique diagnostics with no new diagnostic; no whole-web
PASS or zero writes across unchanged authentication helpers is claimed.

GLM initial lanes ran with provider-only credentials. Automatic approval review
rejected a later resume because private source/context would go to Z.ai;
specific source-sharing approval is pending. Native Codex review/repair continues
within the authorized local scope. A transient runner file-descriptor failure
was recorded in [the review checkpoint](review-checkpoint-2026-10-02.md); commands
subsequently resumed. All in-scope runtime/proof gaps keep the goal active.

Reviewed controller checkpoint — 2026-10-04 UTC

Current Go C admission → current Go D signing/broadcast/recovery/reconciliation
passes the connected race gate in 1.770s on canonical Earn vault index 1. It
executes through real Squads and explicitly mocked KLend, loses the successful
broadcast response, restarts without a signer, reconciles the unchanged wire
and exact balances, and releases capacity/conflicts only after fresh reserve
telemetry. The retained Rust fixture contributes catalog setup only.

The latest complete registered A race suite passes in 4.742s; G passes in
2.174s, complete D in 3.364s, and four F SQL races in 1.427s. These local registered databases
remain isolated from migration and production. The actual A pull/top-up plus
mutable-floor SVM gate passes in 1.668s; its Rust producer uses real checked-in
Squads/Subscriptions/SPL programs and an explicitly documented KLend mock.

The exact greedy frontier refactor reduces allocated bytes from 514,019,846 to
107,747,750 and allocation count from 2,895,457 to 2,305,953 for a synthetic
4096-vault/16-target/max8 fixture. Five profiled local iterations measured
276.4ms to 244.2ms. The unchanged oracle, immutable-input concurrency, output
binding isolation and permutation checks pass; timings are noisy local
observations, not a production sizing claim. The greedy assignment regret
identified above remains explicit.

The earlier cross-mint adapters and Apps watch-schema gaps are resolved by the
checkpoint below. Complete autonomous ALT provisioning/cleanup remains open. The
source-derived cross-mint activation now captures an actual capacity frontier
after finalized preparation; its new capture path still needs its dedicated
database/adversarial proof. No production worker is retired and no main merge
or activation is authorized by this branch checkpoint.

Retained cross-mint connected contract gate passes in 58.221s: actual Go
planning/preflight plus retained Rust withdrawal/swap/deposit, real Squads SBF
and explicit mock protocols. Every leg reconciles once; custody ends at zero
and capacity releases after fresh telemetry. Its disposable fixture now uses
the exact latest registered function bodies after repair of historical
overrides. This retained-executor proof does not substitute for the current Go
cross-mint runtime gate. The root adapter's captured control generation now
passes unchanged to D, which compares it to the locked live control generation.

The D fallback and control-generation increment passes the complete registered
race suite in 3.459s. A revoked then re-enabled generation cannot upgrade an
older preparation, and actual generation zero remains valid. Exactly one
fallback target uses the source ranking, current immutable market evidence,
finalized reserve/custody/history proof and atomic capacity rebind. It still
needs root market-source composition; unavailable evidence holds custody.


Reviewed cross-mint and ownership checkpoint — 2026-10-04 UTC

Branch CI [37172699857](https://github.com/loyal-labs/loyal-yield-routing/actions/runs/37172699857)
passes at `fdd6bbe1`: actual registered Yield/Apps schemas, independent UUID Apps
watch discovery, real Timescale tests, and locked Linux helper/bridge image probes.
This evidence covers that committed revision, not subsequent working changes.

The current Go C → Go D cross-mint runtime passes its connected race proof in
16.21s (package 17.594s), using canonical Earn vault index 1, real Squads, and
explicit mock KLend/Jupiter. Withdrawal loses the successful response, then a
new runtime without a signer reconciles the unchanged wire. All three exact
receipts reconcile; decision/opportunity 5 ends completed_target/completed,
custody is zero at version 3, and capacity/conflicts release only after newer
reserve telemetry. The destination receives 999999999 raw units; source
collateral leaves one rounding unit. Actual bank obligation readback and saved
receipt anchors supply the proof; D does not refresh observer-owned projections.
The fixture database `fleet_go_cross_mint` remains preserved.

External provider ALTs retain full finalized ordered vectors and actual
warm/observed slots separately from authentic managed identities. Original SDK
lookup order, combined hashes, journal reconstruction and first-send readback
are bound across C and D. Legacy managed-only hashes retain byte parity. The
existing fee-payer SQL requires managed tables: external-only preparation is
held by that source constraint, without fabricated IDs or a schema bypass.
Cross-mint rollout flags control fresh work while custody recovery remains
available. Missing continuation evidence holds custody.

Additive branch-only migration 0090 journals exact ALT packets before send and
protects unresolved ownership from retained operation resets. Registered SQL
and actual local ALT-program bank tests pass: create/extend, same-bank warming
hold, loaded v0 transfer, deactivate, actual SlotHashes aging, close/refund and
idempotent recovery. Finalized no-effect expiry requires bounded parent-block
history back to the saved original signing bank; pruned/capped history holds.
Autonomous request planning/allocation, leasing/runtime, head activation and
legacy receipt adoption remain open. Local bank tests do not model mainnet
priority fees or consensus. No connected schema migration is applied.

The acceptance audit found remaining Autodeposit ownership/liveness work.
The observer now gives the retained Earn bridge zero Autodeposit consumers and
rejects nonzero ownership configuration. The Go engine must still autonomously
project later ATA inflows, retry conclusively unsigned stranded slots, paginate
exact creator history, and own desired-control revision rebaselining. Existing
Apps floor POST still mutates scheduling state and needs an intent-only adapter.
No retained ATA projector or Apps repair is counted as retired.

Multiply's ABI/root Claim proofs remain valid but do not prove a complete worker
financial lifecycle. Exact transaction/meta receipt attribution, all signed
recovery paths, quote-context freshness, checked withdrawal conversions and
post-reconciliation strategy coherence are being completed with registered
Worker.Tick lifecycle tests. Current balance direction alone is insufficient
receipt attribution. Mature KLend SBF execution remains a separate proof limit.

The combined offline verifier passes after the cross-mint/lookup/ownership
increment, as do dedicated lookup SQL races (1.747s), C/D cross-mint races
(2.131s/6.664s), and observer ownership/namespace tests. Whole-goal acceptance
remains open; main, migration resources and production remain unchanged.

Integration progress — 2026-10-04 UTC

Both jobs in branch CI
[37173989790](https://github.com/loyal-labs/loyal-yield-routing/actions/runs/37173989790)
pass at `6b3411d1`: registered-schema behavior and the locked Linux image,
including inactive Go role probes and Rust compatibility tools. This covers the
committed checkpoint; the following working changes require another full gate.

Apps commit `1ee615bcb22f29b04af3864cf73264a3b11eca40` is pushed only to
`codex/workers-v2-app-contract`. The v2 gate now makes floor and enablement writes
intent-only, referencing migration 0091 before mutation and returning an additive
pending-reconciliation reason. Its concrete SQL test passes 14 assertions against
an independent registered Yield plus pinned historical Apps-DDL fixture. All
seven GET ownership handlers pass 18 tests/132 assertions in three isolated Bun
processes with automatic environment-file loading disabled. Legacy load-state
tests pass 34/78 assertions, and a 40-test financial/configuration subset passes
independently. Four new tests pass standalone compatible Biome; whole-web
typechecking has 74 diagnostics and is not a full PASS. Baseline web principal
rejections lack an HTTP envelope; identity lookup IO and user-signed POSTs remain
outside the autonomous-financial-write proof. No frontend build or deployment ran.

The observer's ATA capture/projector lane is integrated and joined, borrowing
the existing pools. It commits immutable events, current balance patches and the
retained stream cursor atomically, checks exact target/cluster identity, and holds
conflicting or unknown evidence. New capture serializes ID allocation through
commit. Its actual two-database race gate passes in 2.436s locally; the local
capture fixture uses ordinary PostgreSQL with exact registered DDL except the
Timescale extension/hypertable calls. Next branch CI will exercise the complete
0004 stream migration on real Timescale. Retained capture cutover still requires
quiescing in-flight old producers before accepting the scalar cursor frontier.

Autodeposit now repairs conclusively unsigned stranded work and continues bounded
creator-history pagination. Additive migration 0091 separates desired revisions,
coalesced demand generations and application acknowledgments from chain slots.
Its atomic trigger tracks control and authoritative identity changes, including
namespace and delegation timing. Fresh pull/setup publication must bind the
captured revision under the actual target lock; existing immutable signed work
keeps its custody/recovery protocol. Mainnet target/policy scope is explicit and
NULL is never inferred. Final regression/combined acceptance is still underway.

The retail ALT writer is composed with a distinct scoped manager credential,
explicit rolling budget and keyless recovery default. Its autonomous operation,
actual-bank readiness, paused recovery, packet and PDA-refresh proofs pass at
their reviewed increments. Request allocation, catalog rollover, binding
publication, retirement queues and legacy partial-projection/accounting recovery
are still being integrated; no complete replacement claim is made.

Multiply now requires exact signed transaction/meta attribution and publishes an
immutable receipt under its source operation fence. Current Go Worker.Tick local
bank tests pass fresh deposit, withdrawal, lost-send keyless recovery, persisted
unsent first-send, exact no-effect expiry, malformed receipt holds and stale
terminal-CAS rollback. They use real Squads/SPL and explicit mock yield programs.
The remaining levered borrow/swap/repay/conversion lifecycle is being exercised
with narrowly validated official instruction envelopes in that local model;
mature KLend/Jupiter behavior remains outside this proof. Whole-goal acceptance
remains open; branch work never merges main or changes production/migration state.
