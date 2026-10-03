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

Implementation checkpoint — 2026-10-02

- Runtime goal active; all work remains on rewrite branches.
- Seed commit `234a33bf` pushed to `origin/codex/workers-v2`.
- GLM Flash live launch check passed; first-wave A/B/C/E/F implementation started in isolated worktrees with provider-only credential environments and nested agents disabled.
- Offline seed gate passed: formatting, vet, race-tested imported family semantics/shared primitives and three binary builds. Runtime engine/observer entrypoints deliberately reject incomplete wiring; this is not overall implementation acceptance.
- Full durable, builder, family integration, read-model and application acceptance remain outstanding.
- Source branch reuse is selective: importing the full observer branch would discard newer main proof/schema files, so no whole-branch merge is permitted.

- Disposable PostgreSQL preparation is currently blocked by local kernel shared-memory allocation (`shmget`, ENOSPC) even with elevation. Existing processes/kernel settings were not modified. Offline implementation continues; durable acceptance cannot be claimed without a working isolated fixture.

- Branch-only PostgreSQL service schema and baseline durable fleet/Backyard lease checks passed in CI run 37092914451 at `b9ec448b`; this excludes production-bound migration 0071 data activation and connected SVM checks.
- Reviewed Backyard explicit capability injection passed offline races. Root additionally rejects injected keys whose public half does not match their seed and takes ownership of the validated key.
- Fleet correction uses sparse permitted-target enumeration and full rescore only after selected flows: O(wave limit * permitted candidates). It retains initially uneconomic candidates that can improve, existing hard limits and deterministic ordering. This is bounded greedy scheduling, not a globally optimal allocation claim.
- Autodeposit lot/claim/scheduling/request/attempt SQL increment is integrated for verification. Actual two-leg production execution and complete app-independent acceptance remain unfinished. Root added exact wire digest/packet checks, checked eligible sums, and a lease guard on reused persisted attempts.
- Observer selected Go source compiles and passes offline races on Go1.26.6 with Go1.25.1 language baseline; protobuf module split required the matching parent genproto revision. Its runtime/recovery/readiness audit continues before integration.
- Narrow Rust observer bridge source and only its required visibility/stdio/dependency changes compile against current main proof libraries; the Go-produced protobuf fixture passes. No full source-branch merge was used.

Implementation checkpoints (2026-10-02, branch only)

- Shared runtime and custody unit/wire primitives, reused fleet/Backyard Go implementations: integrated. Disposable baseline fleet/Backyard lease tests passed in run 37092914451; broader acceptance remains pending.
- Fleet planner: bounded full-rescore correction integrated, including initially ineligible candidates becoming profitable after another selected flow. No global optimality claim.
- Autodeposit lot/store/request/attempt increment integrated. Transaction controller/builder review continues in lane A; complete app-independent lifecycle acceptance is not passed.
- Observer source reused selectively from pinned ASK-2169 candidate with lifecycle corrections, capture/application distinction, scoped Rust bridge environment, and abandoned-request restart. Offline race/build/verifier passed. Real registered-schema queue/Timescale tests and the separate sampled watch compatibility fixture are being exercised by branch-only CI. Rust domain bridge remains a deployment artifact requirement.
- Fleet execution lane D is not integrated: review found unsafe send-before-intent ordering and missing durable runtime proof. Multiply lane G is implementing its distinct retail family. Routing read-model/maintenance consolidation is still outstanding.
- Apps opt-in read-only GET changes are on the separate Apps worktree, under review. Legacy remains the default. No deployment, canary, custody adoption, migration acceptance or retirement gate is passed.

Root review checkpoint (2026-10-02)

- Registered PostgreSQL and Timescale observer/Autodeposit queue-lot tests passed in branch CI run 37095615808 at dc9d9613. The watch-loader compatibility fixture is separately sampled, not a complete production-schema proof.
- Owned HTTP shutdown now has an active-request join/listener-close regression. Observer configuration rejects overflowed durations/slot counts, malformed public delegate keys, and concurrency above 64.
- Fleet-wave evidence replay requires an explicit evaluation clock; the saved dilution correction chooses vaults 1,2 rather than the stale-priority 1,3. Exact proportional arithmetic passed 501,164 fuzz executions against a wide integer reference.
- Synthetic one-shot scale probes on Apple M4 Pro, Darwin arm64, Go1.26.6: 4096 vaults/one permitted target each/max128 admitted originally allocated about672MB in837ms; cached conflicts plus checked arithmetic fast paths measured about237MB in229ms. These are local synthetic observations, not production sizing or a globally optimal allocation proof. Full rescoring remains a bounded baseline with visible allocation cost.
- Fleet execution and Autodeposit controller are still under financial review; signature absence is not balance-effect proof. Main/prod/migration resources remain untouched. Resuming an external GLM lane was rejected by automatic approval review pending explicit Z.ai source-sharing approval; local review continues.
