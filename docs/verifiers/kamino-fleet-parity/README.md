# Kamino planner and revalidator local verification

> **Retired.** `go/kamino-fleet-planner`, its parity scripts and the Rust
> `loyal-klend-proxy`/parity reference binaries were deleted when the planner
> moved into `go/workers/internal/fleet` (workers v2). The fixtures here remain:
> `kamino-route-v1.json` seeds the KLend byte-parity golden
> (`go/workers/testdata/klend/golden.json`), and
> `scripts/compare-fleet-decisions.sh` compares the Rust planner with
> `loyal-evidence -kind fleet-decision-parity`. The text below is historical.

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
skipped fleet tests, including the three connected Go execution tests. Missing dependencies or failed/skipped checks fail the audit rather than being
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
   The connected tests (`internal/fleet/connected_execution_test.go`) run the
   Go fleet family alone against a local LiteSVM bank (real Squads and SPL
   Token, explicit mock KLend/Jupiter SBF): Go plans and publishes, prepares
   and simulates, persists the signed wire before any send, lands it through
   `land()` while the RPC loses the response of the first executed send,
   recovers with a keyless owner and reconciles from the finalized receipt.
   `TestConnectedSameMintExecution` moves between existing obligations,
   `TestConnectedSameMintSetupExecution` moves into a market where the vault
   has no obligation and no lamports (withdraw, rent top-up, setup-policy
   `init_obligation` and deposit in one transaction), and
   `TestConnectedCrossMintExecution` lands withdraw, swap and deposit legs.
   They need `FLEET_TEST_DATABASE_URL`, `KAMINO_CONNECTED_SVM_PATH` (the
   `fleet-local-svm` example of `squads-test-harness`) and
   `MOCK_YIELD_PROTOCOLS_PROGRAM_SO` built from current source. No Rust
   process participates. The mock models no interest, oracles, farms or user
   metadata.
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

`BuildIdleDeposit` is the KLend deposit builder without a source or
withdrawal parameter. It requires an existing, PDA-bound empty/target-only
obligation and the vault's stablecoin ATA, and does not take ownership of funds.

`PlanFleet` and `Store.Publish` still reject idle diagnostic routes. Idle
publication, fresh revalidation, retained execution/reconciliation, restart
recovery and capacity release must be connected and verified before lifting
those guards. Never combine an Autodeposit subscription pull with its Kamino
deposit in one transaction.

## Remaining production gates

Require a current-source run of the connected Go execution tests. They do not close the separate executable-idle or real shared-snapshot
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
