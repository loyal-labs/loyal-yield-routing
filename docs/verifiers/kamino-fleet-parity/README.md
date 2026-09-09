# Kamino planner and revalidator local verification

The Go service is intended to replace the Rust opportunity planner and route
revalidator, not the retained executor, confirmer, reconciler, health projector,
or ALT provisioner. Local verification is necessary but is **not deployment
approval**. Follow the parallel-shadow rollout in
`go/kamino-fleet-planner/README.md` before stopping either Rust service.

## Run all local gates

```sh
scripts/verify-kamino-planner-revalidator-parity.sh --audit-current
```

The audit clears inherited credentials, disables online dependency resolution,
and sets HTTP proxies to an unavailable loopback port (local fixture endpoints
are exempt). Dependencies must already be cached. It creates disposable
PostgreSQL databases, runs Go vet and all Go tests with the race detector,
requires every named integration test to run and pass exactly once, and rejects
skipped fleet tests. Both connected lanes must also emit the structured lifecycle
and recovery evidence described below; passing test names alone are insufficient.
Missing dependencies or failed/skipped checks fail the audit rather than being
reported as successful verification. The audit also requires `cargo build-sbf`
and its cached toolchain to rebuild the mock protocol program used by LiteSVM.

The blank-database migration runner accepts only the existing documented 0071
production-bound Backyard activation failure; it validates the required fleet
schema. Production is never migrated by this verifier.

## Evidence boundaries

1. **Market epoch parity:** independently generated Go/Rust immutable epoch JSON.
2. **Go integration tests:** actual planner publication, bound cross-mint policy
   claims, ALT request/readmission, fused capacity/economics handoff, and database
   lease fences. Shadow executes under PostgreSQL `default_transaction_read_only`.
   Expiry tests run the real sweep with live/expired leases, row locks, cluster
   isolation, new-epoch recovery, and unresolved submission ownership. For the
   defensive orphan-submission cases only, test setup injects legacy/partial
   rows with transaction-local trigger bypass; sweep execution never bypasses
   triggers. These fixtures do not simulate a successful signed handoff.
   `TestConnectedCrossMintPreflight` now connects the real Go planner, publication,
   revalidator claim, TLS-local RPC/Jupiter account validation, actual Rust KLend
   proxy, and durable `ready` commit for the same opportunity. It exercises empty
   obligation vectors and complete coverage by finalized Jupiter ALTs. Exact
   simulation bytes now execute through the real Squads SBF, SPL Token and local
   Kamino/Jupiter mock SBF in LiteSVM. The narrow local protocol model uses fixed
   prices and does not implement Kamino interest/oracles or AlphaQ pricing/CPI.
   The harness invokes retained Rust execution, confirmation and reconciliation
   against the local chain. Both lanes interrupt actual signing before durable
   persistence, restart before broadcast after persistence, reject duplicate
   execution and stale owners, replay exact wires, and check final balances and
   telemetry-driven capacity release. Cross-mint covers all three signed legs,
   including initial withdrawal. Test-only interruption points are absent from
   production binaries. Real database locks hold live writes across natural
   lease expiry: same-mint preparation, cross-mint authority acquisition and
   signed insertion, and cross-mint reconciliation must roll back. The latter
   probes caught transaction-start `now()` admitting expired cross-mint writes;
   current-time guards and checked affected-row counts close that boundary.
   The terminal evidence file is emitted only after every assertion succeeds;
   the Go owner adds its own publication, revalidation and response-loss evidence.
   Both lanes still require a fresh successful full audit, not a development run.
3. **Go route negative tests:** execute validation/preparation functions with
   missing ALTs, oversized packets, simulation errors, and changed identities.
4. **Actual KLend proxy:** Go invokes the compiled, digest-verified Rust binary
   to build independent withdrawal/deposit legs for all six stable target mints.
   Both sides reject wrong-lane requests. This exercises the new cross-mint
   operation at the formerly same-mint-only boundary, not canned proxy output.
5. **Retained Rust lifecycle:** existing isolated-database verifier exercises
   durable transition methods. Eleven named cross-mint recovery checks are
   mandatory, including crash windows, source recovery, target fallback,
   ambiguous effects, admission, pause/revocation, and manual-closure fencing.
   All three custody/capacity, policy-catalog, and opt-in store tests also execute
   against disposable PostgreSQL; missing/skipped results fail the audit.
   Side-effect-free role probes load retained role boundaries. These are not
   live on-chain executor/confirmer/reconciler runs.
6. **Squads/LiteSVM:** rebuild the mock SBF, then run the existing generalized
   cross-mint policy test that creates policies, reads them back, executes swaps,
   and rejects adversarial mutations. Require its named non-skipped PASS.
7. **Schema-v2 deterministic artifact parity:** independent Rust planner,
   official KLend/Squads builders and Solana compiler versus Go planner/proxy
   preparation. Compare actual opportunity plans/keys, route fingerprint, and
   complete unsigned message/wire bytes. No database or RPC is used by these
   two artifact producers. Go preparation uses a simulation stub solely to
   obtain compiled bytes; no simulation result is emitted as evidence.

The old schema-v1 artifacts claimed negative outcomes and lifecycle success
using literals and an unrelated table of state names. Those claims and the
fake lifecycle table have been removed. The comparator rejects legacy schemas,
extra lifecycle/simulation assertions, missing outputs, and differing bytes.

## Individual commands

```sh
scripts/verify-kamino-fleet-planner-e2e.sh
scripts/verify-kamino-market-epoch-parity.sh
scripts/verify-kamino-route-parity.sh
scripts/verify-kamino-planner-revalidator-parity.sh --self-test
python3 scripts/verify-kamino-go-test-evidence.py --self-test
scripts/verify-kamino-planner-revalidator-parity.sh --compare rust.json go.json
```

Comparator mutation controls test the comparator, not runtime failure recovery.
The separate test-evidence self-test exercises missing, skipped, failed,
incomplete, duplicated, stale-run, wrong-lane and partially covered recovery
records. Neither synthetic control is a substitute for the integration suite.

## Focused development loop (not audit evidence)

```sh
scripts/verify-kamino-fleet-planner-e2e.sh --development-lane same-mint
scripts/verify-kamino-fleet-planner-e2e.sh --development-lane cross-mint
# Optional: explicitly select the existing loyal_fleet_worker libtest executable.
# Other Rust executables and mock SBF are reused at their usual local paths.
scripts/verify-kamino-fleet-planner-e2e.sh --development-lane cross-mint \
  --reuse-builds /absolute/path/to/target/debug/deps/loyal_fleet_worker-HASH
```

Focused mode still isolates credentials, migrates disposable PostgreSQL, runs
one exact Go lane with `-race -count=1`, and enforces every stage for that lane.
It omits market parity, broad Go vet/unit/integration checks and the separate
retained lifecycle/store/role probes. Reuse skips **all Rust/SBF builds** and can
execute stale binaries; it is only for iteration, never freshness or release
proof. No dependency downloads are enabled. Missing binaries/SBF still fail.
`--reuse-builds` without `--development-lane` is rejected before doing work.
No options always runs the full current-source build path (Cargo may use its
normal dependency cache); the mandatory `--audit-current` parent invokes this
path without development flags. Cached development success cannot substitute
for rerunning that audit after edits.

## Connected lifecycle evidence contract (version 1)

The runner generates `KAMINO_CONNECTED_RUN_ID` after environment isolation. Each
owning Go test must emit exactly one single-line JSON record via
`t.Logf("KAMINO_CONNECTED_EVIDENCE %s", jsonBytes)` **after all real assertions**
and before the test passes. The checker consumes `go test -json` output, not
arbitrary stderr or component-verifier PASS statements. Long `test2json` output
is reassembled per owning test; truncated or foreign-test fragments fail closed.
Owners:

- `same-mint`: `TestConnectedSameMintLifecycle`
- `cross-mint`: `TestConnectedCrossMintPreflight`

Root fields (no extras): `schemaVersion` (integer `1`), `runId` (the runner value),
`lane`, `cluster` (nonempty), `epochId`, `opportunityId` (original publication),
`decisionId` (positive integer DB identities), `legs`, `stages`.

Each leg has exactly `name`, `opportunityId`, `submissionId`, `signature`,
`confirmedSlot`, `reconciledSlot`. IDs/slots are positive integers; signatures
are base58 strings of length 64–88. Reconciled slot cannot precede confirmed
slot. Same-mint requires one `same_mint` leg; cross-mint requires `withdraw`,
`swap`, `deposit`. The initial leg references the original opportunity; later
cross-mint legs may have different opportunity IDs. Submission IDs and signatures
are unique within a lane; signatures must not be reused between lanes. DB IDs
may overlap between independently migrated databases.

Each stage has exactly `name`, `status` (literal `pass`), `submissionIds` (unique
IDs referencing that record's legs). Exactly one of **every** stage is required:

| Stage | Required submission references |
| --- | --- |
| `published`, `revalidated` | Empty (pre-signature stages) |
| `signed`, `confirmed`, `reconciled` | Every leg |
| `expired_reconcile_lease_recovered` | Every leg |
| `stale_reconciler_rejected` | Every leg |
| `exact_wire_replay_no_effect` | Every leg |
| `pre_persistence_crash_recovered` | Every leg |
| `post_persistence_crash_recovered` | Every leg |
| `expiry_during_signed_write_rejected` | Every leg |
| `ambiguous_broadcast_recovered` | At least one actually affected leg |
| `duplicate_work_rejected` | Every leg |
| `telemetry_capacity_released` | Every leg |
| `terminal_balances_verified` | Every leg |

Stage lists are summaries of completed assertions, not instructions to fabricate
literal success. Recover expired leases through real retained paths; prove stale
owners cannot commit and exact persisted wire replay does not change balances,
receipts or transaction count. Ambiguous broadcast must actually be injected,
then resolved through durable ownership and authoritative confirmation. Duplicate
work prevention must cover the movement, not just a second revalidator poll.
Terminal checks must compare actual token/collateral balances and durable states,
assert no idle custody/unresolved submissions or account conflicts, and verify
capacity release only after observed post-execution telemetry. The emitting
harness must enforce these semantic assertions: the Python checker validates
provenance, identity, completeness and references, not the truth of arbitrary
JSON or source-code freshness. Both parts are mandatory.

The checker rejects missing/unknown/duplicate legs or stages, any skipped/failed
fleet test or stage, duplicate test completions, missing package completion,
evidence outside the owning running test, reused signatures and mismatched run
IDs. Full mode cannot accept single-lane output. Successful records are printed
as `VERIFIED_CONNECTED_EVIDENCE` so the audit log retains the identity/signature
trace after scratch cleanup. Do not put accounts, wire bytes or secrets in these
records. Standalone validation requires `--run-id RUN_ID`; optional
`--development-lane LANE` explicitly narrows scope and labels its result.

## Broader shared-input decision diagnostic

Run `bash scripts/compare-fleet-decisions.sh /absolute/new-output-directory`.
This clears inherited credentials, disables dependency downloads and sends proxy
traffic to a closed loopback port. It runs Rust's production capacity-curve
builder, core wave planner and publication fee guard, and Go's shared wave engine
through `PlanFleetShadowAt` (an explicit offline clock), on
one SHA-256-bound input file. Outputs and a detailed report remain in the new
private directory. Exit 1 means disagreement; tooling failures also fail closed.

The 154 deterministic scenarios include profitable/unprofitable same-mint and
cross-mint moves, all six same-mint and 30 directed stablecoin pairs, fee/notional
thresholds, committed inflows/outflows, shared capacity, alternative targets,
large reserves, scheduling limits, and 32 seeded repricing cases. The 25 added
idle scenarios cover all six mints, joint idle/reserve capacity, alternative
fallback, competing sources of one vault, and rejection of cross-mint idle routes. Comparison is
exact, including selected order, route, amount, source/target APY, edge, net gain,
priority and fee cap. Five comparator controls reject omissions, stale fixtures,
invalid fields, changed decisions, and fabricated idle reserve/source yield.

**Current diagnostic result: 154/154 match, without waived differences. Handover remains blocked by the separate gates below.** Input
SHA-256: `55d6bb4bd714f0a2a38de6ba3131620e2510594c7ffbc5d0585ac9383f2a92f9`.
The preceding 129-case fixture passed with SHA-256
`c155e6a6eaf3659a3dbb87bbd53f773d7eeeb7195e2fd5e21e1316f0a08f9cbf`;
those reserve scenarios remain, rather than being replaced by the idle cases.

The five original disagreements are now regression cases: both planners select
three moves when an alternative target remains after the first fills, admit the
$5M move into a $1B reserve's $20M frontier, and stop the shared-tenant wave at
64 moves. Go retains every eligible target, reprices stale heap candidates after
capacity changes, and applies Rust's admission limits and deterministic ordering.
Fee-envelope publication rejection happens after wave admission, without freeing
that wave's capacity or bypassing its limits. Focused Go tests isolate tenant,
conflict, count and exact-notional limits, loader-order independence, and the
large-reserve frontier boundary.

`--audit-current` now **requires** this comparison as well as the fixed wire and
connected lifecycle gates. Any disagreement still fails; there is no allowlist.
The fixed wire fixture now gives Go only reserve-b as an allowed target, matching
the Rust reference's candidate list. It previously gave Go an extra reserve-c;
the old missing-fallback bug concealed those unequal inputs. The separate shared
fixture retains both alternatives and requires the third move.

Both producers assume policy-eligible, six-decimal stablecoin reserve or idle sources
with finalized capabilities already admitted. The small Rust adapter normalizes
source amounts and recovery anchors; it does not run the SQL observer. It uses
current-source default wave limits, not downloaded production environment
settings. This is not a replay of a live shared database/RPC snapshot, does not
compare rejection explanations or policy admission, and does not include fresh
executable quotes or durable idle ownership. Passing every case still does not establish
complete observer/revalidator or production compatibility.

## Idle development boundary

Shadow cycles now use the same wave to allocate reserve and idle candidates,
with one selection per vault and shared target capacity. Idle has zero source
yield, no reserve outflow, and absent source/collateral fields in the diagnostic
plan. The loader retains finalized policy/signer, cooldown, active-work and
Autodeposit fences. Logs retain independent candidate counts and additionally
report `jointAllocationChecked`, `jointSelectedIdleCount`, and
`jointSelectedReserveCount`; `executableIdleEnabled` remains false.

`KLendProxy.BuildIdleDeposit` invokes the official Rust KLend deposit builder
without a source or withdrawal parameter. It requires an existing, PDA-bound
empty/target-only obligation and the vault's stablecoin ATA. It does not create
missing obligations/ATAs or take ownership of funds. The required
`TestRealKLendProxyIdleDeposit` exercises all six mints through the compiled
proxy, exact deposit bytes, every returned account/privilege/byte mutation,
invalid footprints/PDAs/amounts, duplicate JSON fields and withdrawal injection.
`PrepareIdleDepositRoute` compiles a deposit-only Squads payload, applies its
compute budget, requires reusable ALT coverage and exact simulation, and cannot
use the withdrawal policy index. The required
`TestConnectedSameMintLifecycle/idle-deposit-signed-wire` subtest starts a separate
SVM with initial USDC idle funds and an existing target obligation. It signs and
executes the exact Go-produced bytes through Squads/SPL and mock Kamino, rejects
wrong policy indexes, leaves the reserve source unchanged, and checks that exact
wire replay retains one successful execution and no second transfer.

This is **local signed-wire proof, not durable idle lifecycle/recovery proof**;
it does not add a third connected lifecycle record or prove real KLend execution.

`PlanFleet` and `Store.Publish` still reject idle diagnostic routes. Idle
publication, fresh revalidation, retained execution/reconciliation, restart
recovery and capacity release must be connected and verified before lifting
those guards. Never combine an Autodeposit subscription pull with its Kamino
deposit in one transaction.

## Remaining production gates

Require a current-source audit with both connected records before accepting the
local handoff. It does not close the separate executable-idle or real shared-snapshot
Rust/Go observer/revalidator parity gaps. Joint idle/reserve allocation now has
normalized-input comparison coverage, but not connected idle lifecycle evidence.
Even a completed connected run uses mock Kamino/Jupiter programs and fixed
local liquidity/prices, not production KLend interest/oracle behavior, Jupiter
routing/price discovery/AlphaQ CPI, validator consensus or real RPC finality and
fault distributions. Signatures and local SPL/Squads execution are real; these
protocol and network models are not production economics or reliability proof.
Live Jupiter/RPC behavior, real-protocol route compatibility, throughput,
production alert delivery, and safe parallel-shadow cutover remain deployment
gates. A local PASS must not be described as proof that both Rust services can
be stopped.
