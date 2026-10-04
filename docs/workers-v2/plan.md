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

Implementation status — 2026-10-03

The runtime goal remains active. Routing is on `codex/workers-v2`; Apps is on
`codex/workers-v2-app-contract`. Main, production and migration resources remain
untouched. No legacy writer or service is counted as retired.

| Responsibility | Reviewed branch state | Required remaining acceptance |
| --- | --- | --- |
| Shared runtime and Backyard | Retail A/C/D/G composed with borrowed pools, scoped keys, explicit active mode and joined shutdown; Backyard remains separate | Linux helper/bridge image and real schema CI pass at fdd6bbe1; full ownership acceptance remains; per-family fresh-cycle readiness is implemented and race tested |
| Fleet planning | Independent greedy oracle matches 96 generated fixtures; wide-reference arithmetic and unsigned admission recovery pass | Assignment regret requires review; allocation frontier refactor preserves the full-rescore oracle and cuts synthetic allocation bytes by 79%; no global optimum claim |
| Observer | Selected transport composed; capture/application, coalescing, abandonment and lifecycle corrections verified against registered PostgreSQL/Timescale | Actual independent Apps UUID/Yield watch schema, domain readiness and packaged Rust bridge pass; ATA projection ownership remains |
| Autodeposit | Two-leg controller, destination setup/readback, control reconciliation and immutable accounting integrated; registered PostgreSQL race suite passes | Complete app-closed/observer ownership acceptance; actual Go pull and official top-up execute through real Squads, Subscriptions and SPL Token with an explicit KLend mock, including immutable-floor admission rejection |
| Fleet execution | Same-mint fused fresh admission, exact-wire recovery, real bank/receipt reconciliation and ALT selection fencing integrated; registered PostgreSQL race suite passes | Current Go cross-mint composition/fallback/full provider snapshots pass; autonomous ALT provisioning/cleanup remains; source activation/first-send validator and recovery-first runtime are implemented; independent current Go C-to-D canonical vault-1 connected lifecycle passes |
| Multiply | Source-derived planner/math/wire/policy/quote/recovery integrated; registered PostgreSQL race suite and independent SDK/root-claim SVM proof pass | Full lifecycle coverage and packaging; external Claim source bridge compatibility and runtime health now tested; no full KLend SBF execution claim |
| Read models | Go maintenance integrated, fixed mainnet catalog/namespace, price clocks, allocation coverage and modeled-return labels; four registered PostgreSQL SQL tests pass | Current increment real Timescale CI and complete consumer ownership before retiring App maintenance |

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
