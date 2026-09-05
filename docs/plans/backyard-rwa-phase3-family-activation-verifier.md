# Backyard RWA Phase 3: all-market support verifier-first contract

Status: implementation contract v2, revised at the operator's request following
review of v1. This explicitly replaces the five-route restriction, impossible
capacity-recovery proof and underspecified accounting. The 1/20/60 USDC caps
and existing authority boundaries remain unchanged.

This file alone defines done; the combined handoff is an implementation guide.
Documents, tests or deployment alone cannot complete the implementation goal.
Phase 2 evidence reports PASS in PR #223. Separately verify its runtime goal is
closed and its worker has no conflicting work before Phase 3 live activation;
a merged PR establishes neither runtime-goal nor lease status.

## Outcome and exact scope

Ship one deployed serialized Go implementation supporting all 11 catalogued
lanes. A lane is a market/collateral/debt tuple, not just a market family.

| Family | Collateral | Exact supported debt assets | Live proof for this goal |
| --- | --- | --- | --- |
| Prime | PRIME | USDC, PYUSD, USDS | Retain PRIME/USDC proof |
| Maple | syrupUSDC | USDC, USDG, PYUSD | Retain Maple/syrupUSDC/USDC proof |
| OnRe | ONyc | USDC, USDG, USDS | Default OnRe/ONyc/USDC |
| AUTO | AUTO | PYUSD | AUTO/AUTO/PYUSD |
| Ethena | USDe | PYUSD | Ethena/USDe/PYUSD |

All 11 lanes need working observation, decisions, construction, valuation,
exit and reconciliation in the deployed binary, with reviewed bindings. No
lane may remain disabled because implementation or proof is missing. Current
capacity, risk, policy, quote and budget checks still gate entry. Reject
unknown, duplicate or noncatalogued tuples.

Use three new family canaries, not eleven live lifecycles. The six siblings
receive batched lane-specific proof plus justified shared structural proof.
Supporting siblings does not schedule additional funded canaries. Each new
family terminates as LIVE_VALIDATED or proven CAPACITY_PENDING under R06.
Capacity-pending support is not live validation or a promise of future capacity.

## Sole verifier and baseline

```sh
op run --env-file=.env.1password -- \
  bun run --cwd tools/backyard-voltr verify:rwa-phase3-family-activation
```

Use this read-only command as the implementation feedback loop. Each condition
reports measured subclaims separately from missing evidence; a static allowlist
or passing unit test cannot establish deployed behavior. Implement measurements
alongside the affected runtime slice, rather than requiring a complete verifier
before any runtime work. Capture the complete baseline, run affected fast checks
between edits, then every condition at completion. Missing proof is FAIL or an
explicit external gate, never a skipped check or a hardcoded completion verdict.

Output schema/version, source and deployment identities, goal ID, times/slots,
R01-R08 results, exact 11-lane matrix, three canary outcomes, spent/reserved
budgets, evidence references and external gates. Distinguish controlled-state
execution, current-chain signed-unsent simulation, submission, confirmation,
finalization, reconciliation and deployed behavior. PASS is support completion
under this contract, not a claim that all lanes received live transactions.

## Standing authorization envelope

- Cluster: mainnet-beta. Signer/payer: the existing Backyard operational signer.
- Programs, authorities and destinations: existing bound Voltr vault, Loyal NAV
  adaptor, Squads account, installed catalog, catalogued Kamino/Jupiter programs
  and custody identities, existing database and Render service. Freeze exact
  non-secret identities in preflight; labels do not authorize changed addresses.
- Pre-approved: inspection, local implementation/tests, signed-unsent simulation,
  three bounded canaries, reviewed all-11-lane runtime support and eligibility
  activation, forward-rollover policy repair, committed immutable worker/adaptor
  deploys, and admin squash-merges of docs-only evidence PRs. Code PRs follow
  ordinary repository checks and merge rules; no code-check bypass is granted.
- Caps: 1 USDC-equivalent per transaction, 20 per new-family lifecycle across
  all attempts and cleanup, 60 for the goal, measured under R01. Three times 20
  equals 60: no additional shared margin exists. Unused family budget cannot
  raise another family's ceiling. No extra funded sibling or historical canary
  is implicitly authorized by support coverage.
- Expiry: actual implementation-goal close, not completion of these documents.
  Close with flat canary exposure and no unresolved submissions; stop new
  activation on expiry. Indefinite production allocation requires its own
  operating envelope outside this goal; it is not a hidden completion gate.
- Stop and ask for a new signer/cluster/program/destination, authority change,
  cap increase, destructive close/rent reclaim, further scope change or contract
  weakening. Do not ask again for actions already covered by this envelope.

Log consequential actions with lane/family, operation ID, valuation inputs,
spent/reserved amounts, signature or deploy identity, commitment and reconciled
result. Never record secrets.

## Preflight: resolve feasibility before expensive implementation

Use current main as the implementation base and preserve unrelated checkout
work. Verify Phase 2 proof and runtime-goal status, deployed writer/image,
lease, unresolved operations and actual custody. Reuse the existing deployment
pipeline and journal. Record the following in the sole verifier's evidence:

- Exact lane/policy identities, token programs/extensions/decimals, custody,
  reserves, obligations, farms, lookup tables and complete entry/exit graphs.
- Credential, signer, RPC, database/migration, simulation, build/publish/deploy,
  account and funding readiness. Distinguish setup authority from lifecycle
  authority; identify any required protocol account-close behavior here.
- A bounded sample proving the available simulation mechanism can execute a
  sequential lifecycle with captured effects, including controlled capacity
  state. If unavailable, declare owner and resume condition now, not at close.
- Per-canary action-level gross USDC cost and exit-reserve estimates using R01,
  including fees and authorized setup. Shrink the amount without deleting
  lifecycle actions if needed. If no amount satisfies minimum sizes and caps,
  report the measured conflict before broadcasting; do not assume 20 fits.
- Numeric timeouts recorded before each stage: default to 30 seconds per RPC,
  3 minutes per simulation and 15 minutes per deployment observation. Read and
  record the existing worker's reconciliation window rather than inventing a
  competing transaction-expiry rule. Bound build/test commands too. A measured
  tooling requirement can justify a different finite diagnostic limit, recorded
  before retry; it cannot change acceptance or authorize a blind resend.

The Phase 2 total of 13.651629 sums selected decision amounts: its generator
omits VOLTR_RESTORE_IDLE and fees and does not normalize each asset to USDC.
Retain it as historical accounting, not a certified estimate for these caps.
The Phase 2 restore incident and its exception cannot carry into Phase 3.

### Measured setup feasibility — 2026-09-04

The immutable read-only snapshot
`docs/evidence/backyard-rwa-go/phase3/setup-feasibility-2026-09-04.json`
now includes `preflight.setupRent`. At slot 444404813, 1,400-byte policy rent
was 9,676,824 lamports, conservatively valued at 1.006738 USDC before network
fees. The existing 1,250-byte repay allocation valued at 0.907909 USDC.
These are dated observations, not evergreen prices or completed setup admission.

The exact deployed Squads binary (deployment slot 443245754; padded ELF SHA256
`1c95bd7be140589d2aec38a85d7ecfe70ec69639277f622c898f821ab1d636fa`)
was read without signing and executed locally: the same legacy, singleton-key
15-account borrow / 13-account repay policy shapes allocate 1,400 / 1,250 bytes.
See `crates/squads-test-harness/tests/rwa_policy_creation_rent.rs` and the combined
handoff for reproduction. Local rent values differ from current mainnet, so use
the live RPC rent measurement for admission. The checked-in Squads fixture is
different and allocates only 860 bytes for the borrow shape; it cannot establish
this deployed setup cost. The explicit program-bound test is not an R04 lifecycle
PASS or proof that newly compiled replacement policy semantics are correct.

Therefore the measured exact-shape borrow repair cannot pass the existing $1
transaction cap at that valuation. Shrinking canary principal cannot shrink its
account allocation. Refresh affected feasibility inputs before any setup attempt;
if it still cannot fit, a specific operator-approved envelope revision is needed.
Do not weaken constraints, silently omit setup costs or wait indefinitely for a
price change. Continue independent implementation and proof. This checkpoint
changes neither the accepted caps nor R01-R08 acceptance.

## Required conditions

### R01 — durable cap governor with reserved exit budget

R01 is the first live-money gate. Prove locally, deploy, and verify governor
identity before any Phase 3 broadcast, including setup/cleanup. Signed-unsent
simulations may precede deployment; label their signatures and verify absence
on chain. Cap-rejected live intents must not be signed or sent. The final send
boundary must reject persisted wires lacking valid authorization/reservations.

Freeze an action-to-value mapping before live work. Measure actual economic
source debits using verified decimals and conservative fresh USDC valuation,
not raw-token-unit sums or assumed stablecoin pegs. A swap charges its maximum
authorized input once, not both sides or intermediate hops. Distinct deposit,
borrow, repay, withdrawal, staging and restoration actions each count even
when they recycle capital. Receipt mint/burn bookkeeping must not duplicate
the underlying asset. Include network/priority/protocol fees once, identifying
payer and valuation. Assign authorized setup costs once to a benefiting canary
or shared goal spend. Every transaction still obeys the 1 USDC ceiling.

Use maximum executable amounts and bounded fees, not requested amount alone:
Voltr's possible full-custody sweep must fit. The 1 USDC ceiling includes fees,
so an exactly 1 USDC transfer may need shrinking. Stale/missing quote, valuation
or account inputs become a typed HOLD before submission.

Persist spent value, unresolved submission reservations and exit reserves
atomically with admission under the existing lease/journal. For both family
and goal, new risk requires:

```text
spent + unresolved reservations + proposed transaction upper bound
      + conservative exit reserve after that transaction <= cap
```

Derive the exit reserve from the complete bounded repayment, interest/rounding,
withdrawal, conversion, staging, restoration and NAV/fee graph. Recovery actions
consume that reserve instead of double-counting it against themselves; they
still obey all caps. New allocation cannot spend exit reserves. Revalue before
increasing risk and unwind early as headroom shrinks. This is a conservative
estimate, not a guarantee against arbitrary price changes: an unfit exit is a
named recovery gate, never permission to bypass a cap.

Spent value is monotonic. Convert reservations to reconciled spend or release
unused amounts only with authoritative proof they cannot be spent. Ambiguous
submissions retain reservations and block conflicting work. Restart, deployment,
retry, family substitution, HOLD or returned principal cannot reset budgets.
One goal/family identity covers all attempts, not a fresh budget per new name.

Persist typed cap/exit-reserve/valuation HOLDs with proposed value, spent and
reserved totals, operation/family, inputs and exact resume condition. Behavioral
proof covers all three caps, insufficient exit reserve, restart/concurrent
admission, ambiguous submission, actual sweep versus request, and successful
reserved-budget unwind. Drive the production admission/send path; show zero
signing/sending on rejection and reservation binding on accepted wires.

#### Local bridge-admission checkpoint — 2026-09-04

The production worker now derives a cash-only bridge return estimate from its
confirmed construction snapshot: after the proposed action, include staging,
full strategy-custody restoration and a separate NAV report after every capital
mutation. Price exact compiled messages and underlying debits using fresh RPC
fees and the existing conservative token/SOL valuation. Future reports are cost
templates, not reusable signable instructions; each actual leg must be rebuilt
from fresh poststate. The exit estimate's freshness also bounds build/send.

Admission persists the measured graph, immutable current build input and both
budget sides atomically under the existing journal lease. Missing goal state
remains HOLD, not initialization. Existing startup signer validation, per-build
signer validation, caps, wire binding and final-send checks remain in place.

This initial slice covers selected-lane zero-collateral/zero-debt bridge custody only. It is
not position/swap/setup admission, global flat-queue proof, deployed governor
proof or live recovery. The sole verifier measures these local subclaims without
promoting R01 to PASS; full position exits, safe one-time budget initialization,
all-lane proof and deployment/canaries remain required.

#### Local debt-free return-admission checkpoint — 2026-09-04

Admission now also prices a complete debt-free collateral return: full Kamino
withdrawal, NAV, collateral-to-USDC conversion, NAV, staging, NAV, full restoration
and terminal NAV. The intermediate NAV and actual swap have their own production
admission paths; they do not inherit permission from a future cost template.
Entry, borrowing and debt-bearing repayment/conversion admission are still absent.

The producer verifies exit policy identities and the report ticket, reuses the
production quote/parser/compiler, retains the prospective unsigned swap input,
and values all principal transfers and message fees. Its USDC output estimate
applies the existing two-sided 100-bps valuation margin to the quote. This is a
conservative reserve estimate, not an enforceable output maximum or guarantee:
fresh actual poststate must fit the reserved full restoration, otherwise HOLD.
No new risk ceiling or authority is introduced.

Controlled-RPC tests drive admission through every return step and check durable
withdrawal reservation in disposable PostgreSQL. They do not prove real program
execution of the complete return or deployed behavior. R01 and the full goal
remain incomplete; do not use these checks as authorization to activate early.

### R02 — exact allowlist and frozen canary queue

Resolve the exact 11 scope tuples against authoritative identities; reject
omissions, duplicates, forbidden extras and changed bindings. Default queue:
OnRe/ONyc/USDC, AUTO/AUTO/PYUSD, Ethena/USDe/PYUSD. Substitute only an in-family
catalogued lane when fresh evidence shows the default cannot safely execute
and the substitute passes binding, capacity, risk, budget and exit checks.
Freeze the reason before entry. Substitution retains the family budget and
does not remove the original lane from the 11-lane support obligation.

Advancing between flat terminal canaries is explicitly permitted. No ranking,
arbitrary caller routing or transfer of open positions between markets.

### R03 — shared runtime for all 11 lanes and existing authority

Extend the existing RuntimeRoute and production paths with reviewed catalog
bindings. Remove two-route/Maple-specific restrictions that prevent shared
operation. Keep one package/binary, writer and journal; no per-family worker
copies, generic SDK/plugin framework or second executor.

Carry mint, token program/extensions, decimals, custody, reserves/obligations,
farms, policy bindings, valuation inputs and allowed conversion edges through
observation, decisions, construction, NAV and reconciliation. Cover debt assets
USDC/PYUSD/USDG/USDS and actual SPL Token/Token-2022 behavior. Value all NAV/cap
components in USDC, including idle debt custody; never add debt-denominated
values directly to USDC balances. Exit repays debt and converts residual debt
and collateral to Voltr USDC. Binding changes alone cannot prove these paths.

Baseline: 70 logical catalog entries, 44 Kamino operations, 52 directed Jupiter
edges across 11 lanes. Reconcile against installed authority; zero new policy
semantics or net-new logical entries. Repair only by forward rollover that
preserves or tightens approved semantics/identities. Prove replacement before
retiring superseded authority. Distinguish logical entries, original seeds and
replacement accounts. Document temporary overlap; require one selected runtime
binding per logical entry and no unexplained executable authority at close.
An authority gap cannot be disguised as rollover repair.

Measured local construction checkpoint (2026-09-04): the sole verifier now
compiles the four Kamino legs for AUTO/PYUSD and Ethena/PYUSD against retained
SDK account vectors and installed-policy hashes, and exercises identity,
privilege, policy, lane, constraint and amount mutations. It reports this as
`LOCAL_UNSIGNED_CONSTRUCTION_AND_MUTATIONS_NOT_PROGRAM_EXECUTION` under R03.
Together with retained Prime/USDC and Maple/USDC, four lanes resolve locally;
the selection manifest is unchanged. This subclaim does not establish fresh
oracle/refresh correctness, program execution, complete entry/exit support or
live activation. R03 and the all-eleven R02/R04 requirements remain unchanged.

The subsequent local planner/account-decoder measurement covers separate idle
debt, bridge-USDC entry capacity, decimal-aware LTV and safe collateral release,
repayment/conversion precedence and a flat terminal drain. Its proof level is
`LOCAL_PRODUCTION_DECISIONS_AND_ACCOUNT_DECODING_NOT_EXECUTED_LIFECYCLE`.
The disposable journal witness also checks NAV/manual-recovery handling for
debt conversions. Neither observation establishes Jupiter dispatch, executable
exit-cost admission, real-program state transitions or deployed support.

Jupiter integration checkpoint: all ten unique AUTO/Ethena conversion samples
(twelve lane/edge pairs) now fit the actual Squads packet envelope. The retained
USDe -> PYUSD exit failed at 1,399 legacy bytes; v0 construction using its two
existing lookup tables reduces it to 756 bytes, byte-identical to the installed
Solana SDK. Public table accounts were read at finalized slot 444423409 and
retained in `docs/evidence/backyard-rwa-go/phase3/jupiter-lookup-accounts-2026-09-04.json`.
Runtime preparation fetches tables afresh; the common pre-signer/final-send
valuation path rejects inactive, immature, wrong-owner or changed-prefix tables.
An extension can pass only with the persisted prefix intact; it cannot rebuild
or alter a signed wire. Controlled RPC negatives and deterministic local signing
prove these local boundaries, not mainnet execution. Legacy intents retain their
JSON shape. Actual program execution, complete exit-cost admission, seven missing
runtime lane bindings and deployment remain unfinished; no caps or acceptance
conditions change.

### R04 — batched lane proof with explicit simulation boundaries

For all 11 lanes prove builder bytes, discriminators, accounts/signers,
program/mint/authority, ordering, amounts, token behavior, lookup tables,
packet/compute/heap limits, policy intersection, complete exit and effects.
Batch equivalent structures while retaining each lane's bindings and positive/
negative results. Family labels alone do not establish equivalence: cover debt
token programs/extensions, decimals, farms and conversion paths explicitly.

Reject mutations of program, signer, vault, market, reserve, obligation, mint,
destination, amount, operation, order and extra instructions at their actual
enforcement owner. Reuse unchanged identity-valid Phase 2 proof; refresh only
what its invalidation keys require. No repeated historical live replay.

Where prerequisites/capacity permit, use stateful current-chain signed-unsent
sequences; independent simulations without sequential poststate do not prove a
lifecycle. For capacity-pending lanes, retain the actual current-state capacity
failure and execute the complete lifecycle in controlled state using production
builders, exact deployed program bytes, real policy enforcement and captured
effects. Record original state and every override/reason. Model necessary
lifecycle/capacity preconditions without changing programs, authority or policy
enforcement. Label it CONTROLLED_STATE; it proves neither current execution nor
future capacity. Mock success, skipped downstream actions and unsigned builder
inventories fail. Missing simulation facilities are a preflight external gate.

Sequential execution checkpoint — 2026-09-04: the Go-built Ethena deposit,
borrow, repay and withdrawal execute in one LiteSVM instance against the exact
captured deployed Squads, K-Lend and token binaries and installed policy bytes.
The finalized account batch is slot 444426663; its captured Clock reports
444426664 and is preserved, not rewritten. Adjacent writable post/pre states
match. A 100,000,000-raw USDe deposit (9 decimals) backs a 1,000-raw PYUSD borrow
(6 decimals); repayment clears the debt and withdrawal clears the obligation,
returning 99,999,999 raw collateral. The over-limit deposit mutation is rejected
by Squads with `ProgramInteractionInvalidNumericValue` (6064), before K-Lend CPI.
This establishes the local sequential mechanism and four-leg subclaim, not the
complete lifecycle. Only fee-payer/token funding was overridden; signature and
blockhash verification are disabled for these zero-signature local fixtures.
No authority, policy, program or capacity field is changed. The full bridge,
swap, return/NAV, signer, admission and controlled-capacity requirements remain.

The sole verifier runs the current Go byte comparison and actual Rust execution
when `PHASE3_KAMINO_PROBE_DIR` points to the explicit public snapshot directory.
Each subprocess is bounded (Go test timeout 30 seconds; outer execution ceilings
90/120 seconds). The retained verifier output embeds the input plan, account
snapshot, program hashes, overrides, raw pre/post captures and execution logs:
`docs/evidence/backyard-rwa-go/phase3/kamino-sequential-2026-09-04.json`.
The current local snapshot is `/private/tmp/backyard-phase3-kamino-probe.cJUj9H`.
To reproduce using it:

```sh
PHASE3_KAMINO_PROBE_DIR=/private/tmp/backyard-phase3-kamino-probe.cJUj9H \
  bun run --cwd tools/backyard-voltr verify:rwa-phase3-family-activation --offline
```

For a new snapshot, export `plan.json` with the explicitly gated
`TestExportPhase3KaminoControlledProbe` Go test, then run the read-only
`tools/backyard-voltr/src/verify/snapshot-phase3-kamino-probe.ts` under the mounted
RPC environment. Neither exporter nor capture tool signs or broadcasts. The
four-leg witness remains inside `crates/squads-test-harness`, not a second worker.

### R05 — immutable deployment and bounded sequential activation

Deploy the committed immutable image through the existing workflow and read
back source, digest, service, command and live deploy identity. Acquire the
existing fenced lease only after the previous writer cannot write. Keep that
worker lease while advancing the queue; families do not own separate leases.

Persist progress in existing state/journal. Advance only after a flat reconciled
canary or an unfunded capacity HOLD with no exposure or unresolved submission.
A partially funded attempt must unwind before advancing. Check shared custody
and every previously touched obligation at transitions so selecting another
lane cannot hide exposure. At most one canary position is active; no simultaneous
multi-family portfolio scheduler is required.

Prove a capacity-pending family does not stall the next eligible family. Shared
accounting/authority/lease faults and ambiguous submissions do stop conflicting
live work; independence cannot bypass shared safety gates.

### R06 — three canary outcomes and truthful lane readiness

The deployed Go worker originates a fresh entry decision for each new family.
Persist wire/broadcast intent before sending; never blind-resend or replace an
ambiguous transaction. Confirmed state gates progress; finalized custody gates
terminal proof. Each new-family canary reaches exactly one accepted outcome:

- LIVE_VALIDATED: complete bounded entry/position/unwind/repayment/conversion/
  Voltr restoration/NAV lifecycle, finalized flat custody, conserved effects,
  deployment/lease provenance, zero unresolved operations and no unintended residue.
- CAPACITY_PENDING: deployed unfunded entry evaluation durably holds on fresh
  protocol capacity/utilization, without risk-increasing submission, exposure
  or unresolved work. Retain that evidence and the complete R04 controlled-state
  lifecycle. Policy/byte/budget/exit proof still passes. This completes support
  while live validation explicitly remains pending.

Capacity failure after funding requires unwind first; it cannot become an
unfunded CAPACITY_PENDING canary. Missing code, authority, accounts, funding,
credentials, quotes, deployment or tooling are FAIL/BLOCKED, not capacity.

Six siblings are SUPPORTED_READY when eligibility/proof pass, or CAPACITY_PENDING
with the same capacity-specific boundary. Distinguish retained Prime/Maple live
proof from newly performed canaries. No additional funded sibling matrix.

### R07 — withdrawal, recovery and prior-route safety

Preserve Prime/Maple, bridge, atomic one-use NAV ticket, withdrawal priority,
manual recovery, idempotency and fencing through retained identity-valid proof
and affected production-path regressions. Cover debt custody/valuation, decimals,
reserved exits and queue transitions. Withdrawal/recovery precedes new risk on
every supported lane. Demonstrate actual budgeted withdrawal admission and
execution, not just a decision-priority assertion.

### R08 — release coverage and close-out

Deployed manifest, existing partner handoff/admin read model and audit must
agree on 11-lane capability, LIVE_VALIDATED/SUPPORTED_READY/CAPACITY_PENDING
status, policy bindings, image, budgets, recovery and fixed-selection boundary.
Update the existing operational view only; consumer UI/optimizer work is excluded.

Promote budget, token/decimal/valuation, route binding, packet/policy, queue,
withdrawal and fencing checks into normal tests/CI. Keep live RPC/database/
Render reconciliation outside unit tests. Retain final output/evidence pointers
and identify promoted/retired checks. Retire the goal-only command as a future
execution gate after close, retaining its source/output for audit. Keep the
production governor active.

## Implementation sequence and rules against stalled loops

1. Capture measured verifier baseline and identify the shortest incomplete
   production path. Resolve access and demonstrate the sequential simulation
   mechanism early; do not finish unrelated abstractions before this probe.
2. Complete admission through reservation, construction, send fencing and exit
   on a representative existing lane, with decisive production-path negatives.
   In parallel where independent, reconcile each proposed binding against the
   installed policy/catalog and fresh account derivation, ownership and setup
   state. A derived address alone does not prove authorization or readiness.
   Then extend shared debt/token/valuation paths and the reviewed bindings.
3. Verify all 11 lanes in batches, repair only demonstrated policy defects and
   freeze three canaries with bounded amounts and exit reserves.
4. Deploy verified image, prove R01 at the send boundary, process the fixed
   queue. Prefer one combined release; extra deploys need a concrete defect/fix.
5. Reconcile final state, publish lane readiness and existing handoff, run every
   verifier condition and close only on PASS. Documentation, a named test or a
   deployed image alone cannot complete the implementation goal.

On failure retain exact condition/evidence, owner and next discriminating check.
Fix the falsified assumption; do not repeat unchanged expensive suites or edit
acceptance to obtain PASS. Retry capacity only after meaningful economic state
changes, not a new slot/timestamp: one entry attempt per unchanged state.
Timeouts allow diagnosis, not automatic transaction retries. Use product waiting
mechanisms when necessary; do not busy-poll a blocked family.

External gates pause dependent work only. Complete useful independent local or
safe other-family work, then report the exact resume condition. Ordinary bugs
and unfinished implementation are not external gates. Finite timeouts bound
diagnostics; they do not promise completion in a fixed number of hours.

An action rejected by the platform remains rejected until the permitted review
resolves it. For an alleged new destination, produce the exact existing-policy
and account-identity comparison before seeking review; do not retry via another
mechanism or treat the whole implementation as blocked. Ask the operator only
if that comparison establishes a genuinely new boundary or cannot resolve it.
Reuse unaffected Phase 2 and local evidence with explicit dependency identities.
These sequencing/measurement repairs do not change v2 acceptance, the exact
11 lanes, three canaries, 1/20/60 gross caps or the standing authority envelope.

## Verdict and contract changes

- PASS: R01-R08 proven, exact 11-lane support, three accepted canary outcomes,
  current identities, flat canary custody, no unresolved submissions and caps
  respected. Report live-validated and capacity-pending counts explicitly.
- FAIL: observed false condition or missing implementation/proof, with the
  smallest useful correction and no change to acceptance semantics.
- BLOCKED: unavailable external dependency or measured infeasible envelope;
  identify owner, evidence and resume condition. Valid CAPACITY_PENDING alone
  does not block the goal.

Current chain/database/deployment state outranks saved summaries. Disclose a
measurement repair with independent evidence and preserve or strengthen meaning.
Further scope changes/weakening require the operator. Freeze v2 before runtime
implementation; do not introduce new close-out requirements midway through.

## Hard exclusions

- Consumer webapp, wallet-connect, Earn Max UI, optimizer/APY ranking.
- Moving open positions between markets, arbitrary/uncatalogued routing or a
  simultaneous multi-family allocation scheduler.
- New authority/signer/program/destination or policy semantics; registry/hooks,
  new adaptor architecture, second writer/journal or money-moving runtime.
- Destructive closes/rent reclaim and unrelated infrastructure. Identify required
  protocol-close behavior in preflight and resolve its authority before funding.
- Eleven live canaries, historical replay gates, cap resets/exceptions, simulated
  effects labeled finalized, or manual recovery relabeled reconciled.

## Appendix A — retained evidence and implementation anchors

- Phase 2: docs/plans/backyard-rwa-phase2-runtime-verifier.md and
  docs/evidence/backyard-rwa-go/phase2-runtime/, including PR #223 close-out,
  selection, rollovers, signed-unsent ladder, lifecycle and restore incident.
- Phase 1: docs/evidence/backyard-rwa-go/lifecycle-v1.json; retain adaptor byte,
  one-use NAV-ticket and guard/registry/hook retirement proof.
- Catalog: crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json.
  Manifest: docs/manifests/backyard-rwa-v1.json and its embedded Go counterpart.
- Existing package: go/backyard-rwa-worker/internal/backyardrwa/. Extend
  route_runtime.go/manifest.go for bindings; config.go/state.go/decide.go for
  decisions; store.go/execute.go for reservations/send fencing; existing
  Kamino/Jupiter/token/route observation and construction for debt-aware execution;
  nav_observe.go/reconcile.go for valuation/effects; worker.go for the fixed queue.
  These are starting points, not required new abstractions.
- Reuse r03_signed_unsent.go and tools/backyard-voltr/src/verify/rwa-phase2-runtime.ts
  where they measure v2 correctly. Extend existing tests/migrations; no parallel
  evidence store, journal or competing verifier.
- Review baseline: main 221bda0b0734dcac10a698ba7d0465ef4053420f. Historical cap
  accounting: tools/backyard-voltr/src/verify/generate-rwa-phase2-lifecycle-closeout.ts.

Revalidate retained proof when relevant deployment, program, SDK/byte, policy,
account graph or other invalidation keys change or cannot be matched. Old live
proof does not establish current capacity or a new deployment's identity.

### Implementation checkpoint — fresh Jupiter feasibility (2026-09-04)

This is a diagnostic checkpoint, not a change to R01–R08 or authorization.
`docs/evidence/backyard-rwa-go/phase3/jupiter-construction-failures-2026-09-04.json`
retains three public, unsigned construction failures: a policy-compatible
USDe/PYUSD packet exceeded the envelope with the historical lookup-table pair;
another USDe/PYUSD quote used 38 bytes rather than the installed 48-byte layout;
a 0.9-USDC entry quote used 36 bytes rather than the installed 37-byte layout.
These observations establish neither capacity absence nor a usable full exit.

The local worker now accepts bounded fresh lookup hints for the existing
Ethena collateral/debt conversion, resolves chain-owned tables, compiles only
the policy-validated instruction keys, persists their exact mappings and
revalidates them before build/send. No lookup creation/extension or policy,
signer, destination, amount or slippage expansion is implied. Historical
persisted requests retain their original lookup identities. Local tests cover
mapping substitution, invalid hints and unchanged custody enforcement; fresh
two-swap program execution remains unproven.

The sole verifier now measures the two-swap deployed-program experiment when
`PHASE3_JUPITER_PROBE_DIR` points to a complete public snapshot. Missing input,
construction failure or a compiled-only test cannot pass that measurement.
Even a successful two-swap experiment is only an existing R04 subclaim, never
a full lending lifecycle or mainnet/signature proof.

Next critical path: resolve the quote-dependent policy layouts through the
existing forward-repair workflow while preserving exact custody/economic
constraints; prove the sequential bridge/swap/lending/return mechanics; then
finish entry/debt-bearing admission and remaining lane/queue implementation.
Do not poll indefinitely for a favorable quote, treat catalog coverage as
executable coverage, or raise setup/transaction caps without authorization.

V2 repair follow-up: fresh public quotes at slot 444445591 have 40- and
42-byte instructions but identical economic offsets (input 9, output 17,
slippage 25, two zero u16 fee fields at 27 and 29). The shared TypeScript
header/constraint compiler now handles that fixed prefix instead of reading
legacy tail offsets; tests preserve all 52 existing legacy constraints and
reject amount, fee-byte and custody mutations. The API's V2 request and
positive-slippage feature are documented in the
[Jupiter guide](https://developers.jup.ag/docs/guides/how-to-build-a-custom-swap-with-metis)
and [official release notes](https://github.com/jup-ag/jupiter-swap-api/releases).

The two candidate physical replacements retain their unaffected sibling edges.
Real local PolicyCreate execution under the captured deployed Squads binary
measured packets of 1,072 / 1,111 bytes and allocations of 1,383 / 1,458 bytes.
These are ephemeral-Settings local creation measurements, not mainnet rent or
installation proof. Candidate constraints and public quote bytes are retained
in `phase3/jupiter-v2-repair-candidates-2026-09-04.json` and
`phase3/jupiter-v2-public-quotes-2026-09-04.json` under the evidence directory.
The sole verifier reruns compiler/negative and real PolicyCreate checks when
`SQUADS_SMART_ACCOUNT_PROGRAM_SO` names the reviewed binary. The Go manifest
still selects installed legacy policies. The subsequent checkpoint below proves
candidate execution only; current setup cost, forward installation/readback and
runtime activation remain unproven.

V2 sequential follow-up (same acceptance and authorization): the two candidate
policies create on cloned finalized Settings at fresh local seeds 140/141.
USDC -> USDe through Manifest and USDe -> PYUSD through Whirlpool/Token-2022
execute sequentially against captured deployed binaries at slot 444449068.
Actual packets are 652/627 bytes and compute is 105214/184383. The first swap
produces 100010665 raw USDe; the second spends its quoted minimum 99510612,
produces 99483 raw PYUSD and leaves 500053 raw USDe. This is intentionally
nonterminal two-swap proof, not flat custody or a complete lending lifecycle.
Fourteen amount/slippage/fee/destination mutations reject before Jupiter CPI
and leave source/destination custody unchanged.

The Go client/compiler now supports explicitly bound V2 fixed-prefix layouts,
preserves all economic fields and exact authority/custody/token roles, and
validates fresh lookup mappings. Conditional test-only candidate bindings
produce byte-identical SDK wires; the installed catalog is unchanged and
rejects these uninstalled V2 instructions. The sole verifier measures this
distinction rather than inferring it from test names.

Set `PHASE3_JUPITER_CANDIDATE_PROBE_DIR` to the explicit public snapshot directory
to rerun candidate PolicyCreate, swaps, negatives and current-Go parity. Output
embeds the plan, account/program identities, all overrides and raw execution
evidence in `docs/evidence/backyard-rwa-go/phase3/jupiter-v2-sequential-2026-09-04.json.gz`
(lossless gzip JSON; `gzip -dc` reads it without extracting a second copy).
The installed-policy witness remains separate; neither can make R04 PASS without
the complete lifecycle, signer/admission and eleven-lane coverage.

The snapshot records preexisting shared custody of 214898 raw PYUSD (0.214898
PYUSD), explicitly zeroed only for local isolation. Before any canary, attribute
and reconcile that balance under the existing flat-transition condition; do not
call production flat or infer authority to drain it. Continue full-lifecycle
mechanics and entry/debt-bearing admission; no need to repeat this resolved
two-swap experiment without a relevant implementation or identity change.
