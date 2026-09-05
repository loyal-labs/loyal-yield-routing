# Backyard RWA Phase 3: all-market support verifier-first contract

Status: implementation contract v2, revised at the operator's request following
review of v1. This explicitly replaces the five-route restriction, impossible
capacity-recovery proof and underspecified accounting. The 1/20/60 USDC caps
and existing authority boundaries remain unchanged.

## Current recovery checkpoint — 2026-09-05

The former `/private/tmp/loyal-backyard-phase3.3EQhbn` checkout and disposable
probe/database directories are missing. The cause is unverified. The surviving
branch commit is `e8732c3800034d12b6b6d7b2eb0900a39d28fc58`; subsequent source
changes were recovered from successful recorded patches into the persistent,
isolated `/Users/user/loyal/loyal-yield-routing/.phase3-recovery` worktree. The
dirty shared checkout was not overwritten. Rejected patches were not replayed.

Historical checkpoint statements below describe their original runs. They are
not evidence that missing artifacts still exist or that interrupted tests passed.
The interrupted final race/TypeScript run has no recovered completion result.
The new offline verifier checkpoint `phase3/worktree-recovery-2026-09-05.json.gz`
reports R01–R08 FAIL; its gzip SHA256 is
`aaf951ecc48862da5d6250a490771792dbac77a6df4af1ba8969ffd13367b4bb`.
No production signing, broadcast, migration or deployment occurred in recovery.

Recovered source compiles. Pinned TypeScript checking and the 12 verifier tests
pass. A fresh disposable-PostgreSQL admission/send/setup journal run passes.
The targeted setup/payment/catalog/journal race run also passes (15.787s).
The Prime lookup fixture was refreshed at finalized slot 444643583 with all
eight lookup tables and seven matching policy hashes; its packet tests pass.
That fixture explicitly identifies itself as a fresh capture, not the lost one.
The historical return-quote fixture, V2 return candidates, later generated
checkpoints and local real-program probe snapshots remain unrecovered; do not
fabricate them or weaken their checks. The full Go suite is not yet green.

The implementation critical path remains: finish setup build/simulation/send and
expiry recovery; resolve the five rejected/unbound lane bindings using the
permitted authority-review path; integrate the complete worker lifecycle and
serialized family queue; verify all eleven lanes; then deploy immutably and
prove the three accepted canary outcomes. Setup still HOLDs before signing/send.
The known rejected historical-return verifier change remains unapplied.

The durable Codex goal was resumed after recovery and now reports `active`.
Its objective still names the missing temporary checkout; the persistent
recovery worktree above is its working copy. The goal API cannot edit that
objective. This location repair changes no scope, caps or acceptance criteria.

## Contract authority

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
At this checkpoint entry, borrowing and debt-bearing admission were absent;
the funded-payoff extension below supersedes that limitation for its exact shape.

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

Debt-residue extension: debt-free withdrawal and subsequent NAV/conversion
admission now include both full collateral-to-USDC and idle-debt-to-USDC swaps,
a NAV after each, then aggregate staging/restoration. Each quoted exit is retained
inside the existing operation authorization; none is reused as a later current
wire. The full restoration is priced against combined conservative outputs, not
each asset in isolation. Actual debt-residue swaps receive their own admission.
Outstanding obligation debt still rejects; this does not supply repayment or
interest-through-execution-horizon admission. Tests cover non-peg PYUSD valuation,
combined-output cap rejection, changed custody/remaining-debt rejection and
durable two-quote persistence without signing/sending.

Finite repayment follow-up — 2026-09-05: production construction now retains a
finite decision limit separately from the currently observed debt floor. The
repayment-only effects graph reserves the full wire maximum, preserves the actual
token program (including PYUSD Token-2022), and reconciles equal source debit and
reserve credit within the bounds using transaction-scoped balances. Neither a
successful token transfer nor the maximum request asserts zero remaining debt.
No automatic interest buffer or new spending ceiling is introduced.

The deployed-program witness requests 1010 raw PYUSD against 1000 owed, consumes
1000 and leaves zero obligation debt. Requesting 999 instead rejects with KLend
6092 `NetValueRemainingTooSmall` and leaves debt/custody unchanged. The current Go
compiler reproduces both exact local wires; its reconciliation accepts the clipped
transfer and rejects the failed partial transfer. This is a controlled-state
feasibility result, not a live receipt or a guarantee at future execution slots.
Do not rely on tiny partial repayments or residual-debt retries to finish canaries.
Interest-through-horizon sizing, a complete funded payoff/release reservation and
fresh terminal obligation observation remain required for debt-bearing admission.
The sole verifier retains these subclaims in
`docs/evidence/backyard-rwa-go/phase3/repayment-bounds-2026-09-05.json.gz`;
all full R01-R08 conditions remain incomplete.

#### Funded-payoff admission — 2026-09-05

The production observer now sizes a complete repayment when the intended full
payoff is funded. It reads actual reserve accrual basis, timestamp, curve and host
rate; preserves unrounded debt; and computes an upward-rounded compound upper
estimate for 60 seconds (seconds-based reserves) or the existing 32-slot window
(legacy reserves). These are implementation estimation windows, not new money
limits or an indefinite guarantee. Build and final-send revalidation recompute
the bound from chain Clock and reject an insufficient persisted wire or changed
custody. Existing budget/quote freshness and all caps still apply.

The installed SDK 7.3.9 ignores the newer accrual-basis/timestamp fields. The
[official reserve implementation](https://github.com/Kamino-Finance/klend/blob/master/programs/klend/src/state/reserve.rs)
and [LastUpdate layout](https://github.com/Kamino-Finance/klend/blob/master/programs/klend/src/state/last_update.rs)
describe those fields. Captured deployed-program execution independently confirms
the relevant seconds-based behavior: a 1001-unit repayment consumes 1001 and
clears debt after a 60-second/32-slot clock advance. Go reproduces the wire and
checks the estimate against the actual token and obligation poststate.

Funded payoff admission now reserves the maximum repayment and all 11 remaining
steps: NAV, full collateral withdrawal, NAV, collateral conversion, NAV, maximum
debt-residue conversion, NAV, staging, NAV, full restoration and NAV. The residue
estimate uses the minimum possible repayment, avoiding an understated return.
The actual post-payoff NAV also receives fresh full-return admission; subsequent
withdrawal/conversions are rebuilt rather than promoted from cost templates.
Disposable PostgreSQL proves this binds to the existing recovery reservation,
persists interest/withdrawal/quote evidence, and does not sign or send on rejection.

Funded-payoff checkpoint evidence:
`docs/evidence/backyard-rwa-go/phase3/funded-payoff-admission-2026-09-05.json.gz`.
This is not full-lifecycle, live, deployment or all-lane proof. Collateral
release/conversion when repayment cash is insufficient, entry/borrowing,
setup/budget initialization and remaining lane/queue implementation still need
their complete admission and execution proof. No condition or cap is weakened.

#### Interest-aware drain selection and open-debt release — 2026-09-05

The non-USDC observer now carries the same current interest-window payoff bound
used by the builder, includes it in economic observation identity, and persists
it in the route projection. A normal drain with partial cash or principal-only
cash obtains the missing funding rather than repeatedly choosing a repayment
that cannot meet the full-payoff check. The existing emergency-LTV repayment
precedence is unchanged; this does not assert that every partial repayment is
executable or admit a previously unsupported action.

The sole verifier now compiles an immutable open-debt withdrawal sidecar and
executes it against the retained deployed Ethena programs and installed policy.
From the actual post-borrow state, withdrawing 20,000,000 receipt units releases
21,585,834 USDe raw units while debt remains open. Go reproduces the wire,
reserve redemption, economic debit and exact custody reconciliation. A full
withdrawal request from the same state is rejected by KLend `WithdrawTooLarge`
(6011), with no custody or position change. Neither branch signs or broadcasts.

Open-debt release checkpoint:
`docs/evidence/backyard-rwa-go/phase3/open-debt-release-2026-09-05.json.gz`.
This proves the release mechanic only, not the planner-selected release size or
a sequential release -> funding swap -> payoff -> complete return. That linked
path and its admission remain the next implementation gap; all R01–R08 retain
their full acceptance criteria, and local evidence cannot make them PASS.

#### Funding-swap and NAV admission — 2026-09-05

The production worker now admits a full idle-collateral-to-debt funding swap,
the NAV immediately before it, and the NAV immediately after it. The reservation
covers the payoff, remaining collateral withdrawal, both residue conversions,
full bridge return and every intervening NAV. Their respective future-step
counts are 13, 14 and 12. Future packets remain cost templates; only the actual
current request is persisted for signing.

Funding must cover debt through all intervening steps: swap/NAV/payoff uses
three interest windows, NAV/swap/NAV/payoff four, and NAV/payoff two. This does
not extend current-wire freshness beyond 32 slots. The final-send path refreshes
actual custody and the funding bound. It derives a conservative minimum from
the legacy instruction's quoted output and slippage, rejects overstated JSON
thresholds, and never relies on optimistic output to establish repayment cash.
[Jupiter's quote documentation](https://developers.jup.ag/docs/swap/v1/get-quote)
distinguishes quoted output from the slippage-adjusted minimum.

Controlled transport tests exercise the actual compilers, pricing and admission
functions, including versioned lookup-table messages, insufficient multi-step
interest coverage, changed signed-input custody and over-cap full returns.
Disposable PostgreSQL proves the funding producer persists the complete return
against an existing recovery reservation, rejects unreserved exposure, and keeps
all wires unsigned/unsent during these tests.

Funding-return checkpoint:
`docs/evidence/backyard-rwa-go/phase3/funding-return-admission-2026-09-05.json.gz`.
Release-before-funding admission and sequential release/swap/payoff/return
execution remain incomplete. The USDC-to-debt funding variant, entry/borrowing,
setup/initialization, all-lane queue and deployed/live proof also remain open.
No cap, authorization boundary or R01–R08 verdict meaning changes here.

#### Production-sized release and complete return admission — 2026-09-05

The worker now sizes repayment collateral against the five-step interest window
(release, NAV, funding swap, NAV, payoff) and the existing unwind LTV. It converts
the liquidity allowance through the reserve's unrounded exchange rate. Build
and final-send checks reject an unsafe retained amount, changed release effects
or changed idle debt cash; the funding precondition is part of persisted intent.

The deployed-program probe now executes this production-sized request alongside
the retained small positive and oversized negative. It releases 90,592,198
receipt units for exactly 97,775,409 USDe raw units, retaining 2,061,157 receipts
and open debt in the controlled Ethena fixture. Go independently reproduces the
amount, token reconciliation and post-release reserve liquidity/receipt supply.
This proves the cost estimator's immediate reserve projection, not a future
price guarantee or live withdrawal.

The existing funding-return producer now also admits this current release,
reserving all 15 remaining steps through full return. It checks that the
slippage-adjusted funding minimum covers payoff before release can build, and
prices the remaining collateral using the proven post-release pool transition.
Projected balances never replace RPC state or the persisted current wire.
Disposable PostgreSQL exercises this through the existing recovery reservation:
unreserved exposure rejects; admitted release retains the funding/payoff/return
templates and remains unsigned/unsent in the test. Current freshness stays at
32 slots, not the five-step interest horizon.

Latest sole-verifier checkpoint:
`docs/evidence/backyard-rwa-go/phase3/release-return-admission-2026-09-05.json.gz`.
Sequential release/funding/payoff/full-return execution is still required.
USDC-to-debt funding, entry/borrowing and setup/budget initialization remain
unadmitted, and all-lane queue, deployed/live and final custody proof remain
unfinished. R01–R08 cannot pass on this local checkpoint alone.

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

Debt-interest measurement repair: stored obligation debt was previously used
without applying the reserve/obligation cumulative-borrow-rate ratio. The worker
now uses the unrounded stored Fraction, all four rate limbs, KLend's scaled
integer division and a final raw-unit ceiling in position observation, LTV,
repayment selection and NAV. The installed SDK decodes seven stored units at
rates 1/1.25 as 8.75, hence nine raw units; controlled production-path checks
now enforce that result and reject missing/regressed/overflowing rates. This
matches [KLend interest accrual](https://github.com/Kamino-Finance/klend/blob/master/programs/klend/src/state/obligation.rs)
and the locally installed SDK byte layout. It measures debt at the captured
reserve refresh, not unaccrued interest after that refresh or a future full-payoff
guarantee. Those bounds remain required for debt-bearing admission; no contract
condition, live policy or accounting ceiling changes.

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

#### Complete-return policy-layout checkpoint — 2026-09-05

Public unsigned V1 sizing quotes at slots 444478869–444478870 expose a
previously unproven return boundary. Current production Go validation measures:

| Edge | Installed amount/slippage offsets | Observed offsets | Result |
| --- | --- | --- | --- |
| USDe -> PYUSD | 29 / 45 | 19 / 35 | Reject |
| PYUSD -> USDC | 18 / 34 | 18 / 34 | Accept layout only |
| USDe -> USDC | 17 / 33 | 18 / 34 | Reject |

All three have the installed discriminator and correct custody roles. A matching
discriminator is therefore insufficient. Input samples are the retained release
amount 97,775,409 USDe raw, its new quote minimum 97,257 PYUSD raw minus a
hypothetical 1,001-raw payoff, and a conservative 2,224,590-raw remaining USDe
sample. They exclude preexisting debt custody and are not linked executions,
fresh lending observations, capacity proof or an executable return reservation.
Raw responses are retained in `phase3/return-quote-feasibility-2026-09-05.json`.

The V2 repair candidate now also covers USDe -> USDC, preserving its packed
USDe -> USDS sibling exactly. Superset artifacts
`phase3/jupiter-v2-return-repair-candidates-2026-09-05.json` and
`phase3/jupiter-v2-return-public-quotes-2026-09-05.json` retain the first two
candidates/quotes unchanged and add the return sample at slot 444479270.
They deliberately do not represent a coherent chain snapshot. The original
two-swap evidence remains valid for its limited claim.

The current TypeScript compiler rejects economic and custody mutations for all
three samples, preserving all 52 original legacy constraints. Actual local
PolicyCreate through the captured deployed Squads binary accepts all three
complete replacement groups: packets 1,072 / 1,111 / 1,072 bytes, allocations
1,383 / 1,458 / 1,383 bytes. These are ephemeral-Settings local measurements,
not installation, present mainnet rent feasibility, or third-swap execution.
No installed binding, authority, cap or live state changed.

The sole verifier now retains exact current-Go compatibility diagnostics and
checks the three-group compiler/creation proof. Its focused offline checkpoint
is `phase3/return-policy-compatibility-2026-09-05.json.gz`; unchanged DB and
sequential execution evidence stays in the earlier checkpoints above rather
than being relabelled as newly observed.

Outstanding verifier edit: this checkpoint's implementation also contains an
uncommitted extra R04 acceptance check requiring these dated sizing quotes to
pass. That check was introduced during this checkpoint, not in the accepted v2
contract. The attempted correction to leave it diagnostic-only was rejected by
platform review, including after showing the original Git diff. Do not bypass
that rejection or treat the accidental check as user-approved contract scope;
obtain explicit approval for that exact correction. Existing full-lifecycle,
installed-authority, cap and final-custody requirements remain unchanged.

Next implementation dependency: include the third return repair in the existing
forward-roll workflow, then prove linked release/funding/payoff/full return
against a coherent snapshot. Do not try indefinitely to obtain old legacy
offsets, infer capacity failure from this layout mismatch, or install candidates
before current authority, setup cost and final-send admission are satisfied.

#### USDC-funded payoff admission — 2026-09-05

The existing funding-return producer now also admits the planner's
`withdrawal_usdc_repayment_buffer` action. It reads USDC source custody in the
same batch as debt/reserves/Clock, checks the wire-enforced minimum against the
interest horizon, and rechecks persisted funding at final send. The current
swap reserves 13 future steps; a preceding NAV reserves 14. Spent USDC is removed
only from the cost-only return projection, preventing double-counting during
full staging/restoration. Original observations and current wires stay intact.
A funded NAV does not swap remaining USDC again merely because it exists.

The Go race suite passes. Controlled-input tests cover source-custody drift,
underfunding, over-cap return and complete return accounting; disposable
PostgreSQL additionally proves the real producer rejects unreserved exposure,
persists the selected USDC source and full payoff/return reservation, and
authorizes the matching unsigned build. No signer or broadcast is used.

Current sole-verifier checkpoint:
`phase3/usdc-funding-return-admission-2026-09-05.json.gz`. This extends R01 local
admission evidence, not linked execution or deployment proof. Entry/borrowing,
setup/budget initialization, complete lifecycle, remaining lane/queue and live
proof are still missing. The platform-blocked verifier correction above remains
unchanged and still awaits explicit approval; this implementation does not
bypass it or alter caps, installed bindings or authority.

#### Executed return conversions — 2026-09-05

At finalized snapshot slot 444485232, the captured deployed Squads/Jupiter/
Manifest/AlphaQ/token programs execute both return conversions sequentially:
2,224,590 USDe raw -> 2,224 USDC raw, then 96,256 PYUSD raw -> 96,272 USDC raw.
Both controlled source custodies end at zero and the same destination ends at
98,496 USDC raw. Compute is 105,699 / 121,463 units. Eleven economic/custody
mutations reject before Jupiter CPI without changing source/destination funds.

The USDe return uses one V2 candidate created only on cloned Settings; the PYUSD
return uses its exact installed legacy policy. Go reproduces both executed SDK
messages with the candidate binding confined to the test. The probe initially
forced v0 encoding; the production-matching legacy-first messages both fit at
895 / 860 bytes. No runtime lookup permission was broadened to satisfy parity.
The original capture was reused immutably for this encoding correction.

Run the sole verifier with `PHASE3_JUPITER_RETURN_PROBE_DIR` pointing to
`/private/tmp/backyard-phase3-jupiter-probe.xpg0aH`. Retained checkpoint:
`phase3/return-conversions-execution-2026-09-05.json.gz`. The verifier requires
exact source sweeps, continuous destination balances, full negative coverage,
program/policy/plan identities and current-Go parity; a claimed flat flag alone
cannot pass the subclaim. Existing forward-swap proof is rerun because the same
local runner/compiler-comparison code changed.

This is still controlled post-payoff sizing input, not proof that lending
produced those balances. It neither restores Voltr nor proves live/flat global
custody, installed candidate authority, signatures or cap admission. The real
214,898-raw PYUSD prebalance is retained explicitly as a local override, not
drained or attributed. Next connect release, funding, payoff, these conversions
and bridge/NAV in one coherent execution; do not repeat the resolved isolated
return experiment without an affected dependency change.

#### Linked lending and both returns — 2026-09-05

The existing local runner now executes four exact Go lending messages followed
by both return conversions on one finalized 56-account snapshot (slot 444491195)
and eight captured deployed programs. Deposit creates 92,650,599 receipt units;
borrow creates 1,000 PYUSD raw debt; repayment clears that debt; withdrawal
returns 99,999,999 USDe raw and leaves zero obligation receipts/debt. The actual
withdrawal output converts to 99,984 USDC raw, and the actual remaining 2,000
PYUSD raw converts to 2,000 USDC raw. Terminal local custody is 101,984 USDC raw,
zero USDe and zero PYUSD. No account/balance reset occurs between the six legs.

The linked mode reuses `prepare-phase3-jupiter-v2-probe.ts --lending-return`,
with a current-Go `kamino-plan.json` export in the same directory. It unions
account addresses before the single finalized capture, not historical snapshots.
Quoted return amounts remain explicit sizing assumptions: the runner fails if
actual lending output differs rather than modifying custody or quote economics.
The verifier compares every before/after account set, checks account digests,
decodes raw terminal obligation/custody, and checks all six wires against current
Go compilation. Existing return-policy negative tests also run on this poststate.

Configure `PHASE3_LINKED_LENDING_RETURN_PROBE_DIR` with
`/private/tmp/backyard-phase3-jupiter-probe.KT0qLr`; the sole-verifier checkpoint
for this slice is `phase3/linked-lending-return-2026-09-05.json.gz`.

This closes the disconnected-lending/return proof gap for the controlled funded
round trip only. Initial 100,000,000 USDe raw and 2,000 PYUSD raw are explicit
local overrides; the unchanged-clock repayment uses the existing four-leg probe,
not the runtime's accrued-payoff admission path. USDe return still uses a cloned
candidate; no installed binding changes. Bridge entry/restoration, release and
funding on this continuous state, runtime admission across entry/borrow/setup,
all-lane and live proofs remain required. The pending historical-quote verifier
correction remains untouched; no cap, authority or deployment change occurred.

#### Production initial-swap admission — 2026-09-05

The worker now routes initial USDC/collateral swaps to an entry producer instead
of the return-only producer. For initialized, position-free custody it reserves
the immediate complete reverse conversion, each required NAV, staging and full
Voltr restoration. Spent USDC is removed only from the cost projection; unspent
USDC remains included. The actual entry wire and observation stay unchanged.
Prospective output uses the existing two-sided estimate, not an enforceable
Jupiter maximum; actual poststate still requires fresh admission before the next
transaction, and excess output never authorizes a partial/truncated exit.

The persisted entry intent carries `entryReturnReserved`. Admission, pre-sign
valuation and persisted-input final-send valuation check current source,
collateral/debt custody and the empty obligation in one fresh account batch.
Changed custody/position or mismatched minimum-output effects produce typed
HOLD. This flag neither installs authority nor authorizes later deposits/borrows.

The real disposable PostgreSQL producer test proves an entry cannot adopt
unreserved bridge custody. With an existing reserve, it extends the reserve as
entry spending within the unchanged caps, rather than misclassifying entry as
recovery constrained to the cheaper cash-only exit. Existing recovery rules stay
unchanged. The test persists the exact unsigned current build and all seven
future return steps, then authorizes that build without signer or send access.
Controlled tests also cover partial entry, family-cap rejection and changed
custody at the actual persisted-input final-send gate. The Go race suite passes.

Sole-verifier checkpoint: `phase3/entry-swap-admission-2026-09-05.json.gz`.
R01 now distinguishes this initial conversion from still-missing collateral
deposit, borrowing, setup and one-time budget initialization admission. This is
local production-code/journal proof, not an executed admitted lifecycle or
deployment. All remaining R01-R08 requirements and pending approval boundaries
remain unchanged.

#### Deposit rounding falsifier and runtime correction — 2026-09-05

Before adding deposit admission, two cloned-program probes on the existing
linked snapshot exposed a deterministic reconciliation defect. A deposit request
of 1,000,000 USDe raw actually transfers 999,999 and mints 926,505 receipt units;
99,999,999 raw transfers exactly and mints 92,650,598 receipts. The old exact
transfer expectation rejects the first successful transaction. Separately, the
100,000,000-raw linked deposit mints 20 fewer receipts than the unrefreshed
pre-transaction exchange rate predicts. The transaction accrues reserve interest
before computing receipt issuance; a stale-rate exact projection is invalid.

Production deposit observation now produces explicit finite deposit-transfer
bounds. Reconciliation checks conserved source/destination movement within those
bounds; it does not infer receipt issuance, full custody consumption or terminal
position state. The allowance bounds less-than-one-receipt rounding using the
entire net reserve compounded at its maximum configured rate through the existing
60-second/32-slot window (conservative versus interest on only borrowed assets).
Build and persisted-input final-send valuation refresh the bound and custody;
changed conditions produce HOLD. Economic cap debit remains the full wire
request, not the smaller rounded transfer. Repayment semantics remain separate.

The Go comparison reproduces both mutated deposit wires, checks captured token
debits and minted supply, reconstructs the refreshed exchange-rate equation from
actual poststate, accepts the genuine rounded transfer and rejects the former
exact-debit negative control. It also rejects conserved transfers below the
finite minimum. Controlled tests reject changed custody/rounding window and
malformed or mixed deposit/repayment effects. The Go race suite passes.

Retained current checkpoint: `phase3/deposit-rounding-compact-2026-09-05.json.gz`,
using `PHASE3_LINKED_LENDING_RETURN_PROBE_DIR` with the existing
`/private/tmp/backyard-phase3-jupiter-probe.KT0qLr` snapshot. The extra probes use
isolated clones; they do not alter the six-leg continuous execution.

The initial `phase3/deposit-rounding-2026-09-05.json.gz` checkpoint records a
combined Go race-check timeout while parsing duplicated immutable program bytes,
not a program-execution failure. Transition captures previously repeated the
LiteSVM executable-account representation, producing 599,811,905 bytes of JSON.
Those binaries remain independently hash-checked and retained in the snapshot;
transition captures now include every non-executable account, reducing the same
execution report to 2,584,227 bytes. The verifier derives the exact state-address
set from the snapshot and rejects omitted state. No assertion, race check or
timeout was removed or increased to make the check pass.

This fixes a production reconciliation prerequisite, not deposit admission.
The deposit return producer must still reserve withdrawal plus any rounded
collateral residue, conversions and bridge/NAV; borrowing/setup/initialization
and the other outstanding goal requirements remain unproven. No policy binding,
cap, authority, live custody or pending verifier-approval boundary changed.

### Initial deposit admission and remainder return (2026-09-05)

The next local runtime slice wires initial deposits into production admission.
An unsigned simulation of the exact current deposit (no overrides or replacement
blockhash) supplies refreshed receipt issuance and conserved collateral movement.
Only exit costing uses that poststate: full withdrawal, existing/rounded collateral
remainder, conversion, restoration and every intervening NAV. The durable current
input remains the original deposit. Subsequent NAV and withdrawal reobserve real
custody; final-send rejects changed source custody, debt cash or initial position.
Admission requires an existing family exit reserve and extends it within the
unchanged caps; it cannot adopt unreserved collateral.

Controlled RPC/quote tests cover the full nine-step reserve and failed, stale,
incomplete or inconsistent projections. The real disposable-Postgres production
test rejects unreserved deposit custody, persists the complete reserve and exact
current input without signing, and authorizes the bounded build. These are local
admission proofs, not deployed-program simulation or live canary proof. The
retained linked deployed-program rounding witness remains separate.

Next implementation gaps are borrowing/leveraged redeposit admission and
setup/budget initialization, followed by complete all-lane execution, immutable
deployment and family canaries. The existing binding-review and verifier-gate
approval boundaries remain unchanged. The goal is active; R01-R08 are not PASS.

### Borrow origination-fee accounting (2026-09-05)

Borrow admission review found another actual protocol mismatch: KLend's exact
borrow amount is received liquidity, not gross reserve debit or added debt.
Isolated clones of the retained deployed-program snapshot prove that a 1,000-raw
PYUSD receive debits 1,001 or 1,004 raw when the origination-fee configuration is
changed to exercise minimum-fee and nearest-integer rounding. The existing fee
receiver gets the difference and obligation debt includes it. Only that fee field
is overridden locally; these probes do not feed or reset the linked lifecycle.

Production observation now records exact source, vault and fee-receiver effects.
Cap valuation includes the fee; build and persisted-input final-send revalidate
the fee configuration, referrer assumption and custody. Receipt reconciliation
uses the existing three-account conservation checks. Go agrees with actual token
and raw-debt changes; the prior two-account expectation fails the same receipts.
A production-builder negative test rejects a receive whose zero-origination-fee
control passes even with valuation margins and network fee, while its actual
gross debit crosses the unchanged cap, before database or signer access.

This establishes borrowing economics, not complete borrow admission. Its reserved
unwind must still fund origination fee and interest, including cases where the
post-deposit collateral remainder is too small to quote on its own. No installed
binding, policy, authority or live account changed. Current checkpoint:
`phase3/borrow-fees-strict-negative-2026-09-05.json.gz` (the earlier
`phase3/borrow-fees-2026-09-05.json.gz` predates the explicit zero-fee control).

### Initial borrowing return reservation (2026-09-05)

Initial borrowing is now wired to the existing production admission and journal.
The shared unsigned entry simulation captures the exact borrowing poststate;
the validator binds token effects, unchanged collateral, refreshed reserves and
fee-inclusive debt. It accepts the retained deployed-program fee probes. No
simulation overrides or future instructions become the current persisted input.

For insufficient cash, the cost plan combines a safe collateral release with
the existing remainder, validates the funding quote's enforceable minimum, then
reserves full payoff, remaining receipt withdrawal, both residue conversions,
restoration and NAV. The 17-step future graph needs seven interest windows from
borrow through payoff; the current wire remains limited to 32 slots and its
original 60-second entry window. Already-funded borrowing uses an 11-step return
without unnecessary release/funding. No cap is raised.

Controlled tests reject failed/inconsistent projections, insufficient quote
funding, changed custody/position, rate increases and expired interest windows.
Disposable-Postgres tests reject borrowing against an unreserved position and
persist the full return with the original unsigned borrowing input before build
authorization. The local race suite passes. These proofs establish admission
costing, not a sequentially executed return or live canary.

The next required integration is post-borrow NAV and release/funding dispatch:
the current worker can still select an inadequate dust-only funding conversion.
That continuation and leveraged redeposit must be completed before claiming
R01 or executing a new borrowing canary. Setup, binding review, all-lane proof,
deployment and canaries remain outstanding. Checkpoint:
`phase3/borrow-admission-2026-09-05.json.gz`.

### Post-borrow funding continuation (2026-09-05)

The dust-only planning/admission gap identified at the preceding checkpoint is
now addressed locally. The existing same-batch NAV supplies idle collateral's
rounded-down USDC value; observation identity and persisted projection retain
that value. Funding selection compares it (or bridge USDC) with the debt
shortfall using the existing two-sided price margin and wide arithmetic, not
equal raw units or an assumed stablecoin peg. Missing/zero value never makes
dust adequate. This estimate selects a path; only the executable quote minimum
can establish funding sufficiency.

Post-borrow NAV can reserve release, NAV, combined collateral funding, NAV,
payoff and the complete remaining return: 16 future steps, six interest windows
from current NAV through payoff. The actual release accepts a nonempty buffer,
preserves its exact prebalance, and prices conversion of released liquidity plus
the remainder. Sufficient bridge USDC can fund repayment while retaining the
collateral remainder for final withdrawal/conversion. Already-funded debt does
not trigger another swap merely because collateral or USDC remains. No cap,
installed binding, authority or current-wire freshness limit changes.

The sole verifier now includes controlled production decision/admission tests
for these paths, underfunded quotes, changed custody and valuation overflow.
The disposable-Postgres test also persists the NAV's release/funding reservation
and authorizes only its original current input, with no signed wire or send.
This is local planning, admission and persistence proof, not linked program
execution of the continuation. Checkpoint:
`phase3/funding-continuation-2026-09-05.json.gz`.

Remaining critical path: leveraged swap/redeposit admission, setup and one-time
budget initialization, exact installed-binding review, complete linked worker
execution, all-lane evidence, immutable deployment and the required canaries.
R01–R08 remain unproven overall; a passing local continuation does not authorize
a borrowing canary. The pending historical-quote gate decision is unchanged.

### Leveraged swap/redeposit admission and rounding progression (2026-09-05)

The worker now routes the borrowed-debt conversion and debt-bearing redeposit
through production admission, sharing the borrowing exit estimator. Each validates
its exact unsigned simulation and reserves payoff funding, remaining collateral,
all residue conversions and bridge/NAV return. Current inputs and original
snapshots remain separate from projected poststate. Existing-position entry must
already have an exit reserve; it can extend that reserve only within the existing
caps. Final-send checks reject changed custody/position and expired debt windows.
Controlled tests and real disposable-Postgres persistence/build authorization pass.

The planner also uses the builder's same reserve-derived minimum deposit input.
An initial deposit remainder can no longer prevent borrowing, and a redeposit
remainder no longer restarts redeposit. These balances remain included in exit
custody; the rule neither discards dust nor imposes an arbitrary token threshold.

The verifier exports the current Go redeposit wire and executes it on an isolated
clone of the retained debt-bearing Ethena post-borrow state. Explicit local
overrides set collateral custody to 1,000,000 raw and debt cash to zero; this is
not linked borrowed-funds conversion proof. Actual deposit debit is 999,999 raw,
receipts increase 92,650,599 -> 93,577,104, and debt stays unchanged. The current
Go validator accepts the captured poststate and rejects a receipt-removal negative
control. The original six linked lending/return legs remain unchanged and pass.
Checkpoint: `phase3/leverage-redeposit-2026-09-05.json.gz`.

Still required: complete linked worker execution (including leveraged swap,
redeposit and return), setup/one-time budget initialization, exact installed-binding
review, all-lane proof, immutable deployment and required canaries. Local admission
and the isolated redeposit witness are not R01/R04 completion. Caps, authority,
hard exclusions and the pending historical-quote gate remain unchanged.

### Explicit one-time budget initialization (2026-09-05)

The production binary now provides `--initialize-phase3-budget`. It uses only
`NEON_DATABASE_URL` and the exact existing `BACKYARD_RWA_ROUTE_KEY`, acquires the
existing lease without preemption, and atomically creates fixed zero family
counters plus an initialization marker in the existing route row. It preserves
unrelated state and advances the row generation once. No signer, RPC, route
activation, operation insertion or broadcast is involved; the result explicitly
says `BOOKKEEPING_NOT_ACTIVATION`. The command has a 15-second operation bound
and a separate three-second lease-release bound. Ordinary worker startup and
admission never invoke it implicitly.

An existing valid budget is returned unchanged, including closed status, spent
amounts, exit reserves and unresolved reservations. Partial/malformed state,
prior Phase 3 journal authorization, untagged signed/submission history on a
new-family lane, active journal work or unresolved capital recovery prevents
first creation. The exact retained Phase 2 recovery exception is not broadened.
A never-submitted missing-budget HOLD does not prevent first creation. This is
not protection against an administrator deleting both the state and its journal;
such deletion remains outside the workflow, not a supported reset operation.

Disposable-Postgres behavioral coverage exercises concurrent creation, restart,
closed/spent/reserved-state preservation, corrupt or missing halves, historical
submissions, active legacy/current statuses and lease fencing. The sole verifier
includes that coverage and the public command's fixed-route/config/error-redaction
checks. This local proof does not initialize the production database or establish
R01 completion. Checkpoint: `phase3/budget-initialization-2026-09-05.json.gz`.

Before live activation, still verify Phase 2 goal closure, finalized flat custody,
actual production budget state, complete setup/admission feasibility, approved
installed bindings and deployed governor identity. Remaining delivery work is
complete linked worker execution, all-lane support/proof, setup, immutable
deployment and required canaries. Accepted caps, pending review decisions and
R01–R08 acceptance are unchanged.

### Reviewed Prime sibling construction and canonical key encoding (2026-09-05)

Fresh finalized binding review at slot 444525169, Settings seed 139, establishes
exact custody/obligation ownership and all four installed Kamino account vectors
for Prime/PRIME/PYUSD and Prime/PRIME/USDS. Neither requires farm substitution.
The scoped local registration was accepted after that comparison; the other five
previously rejected bindings remain untouched. These two existing catalog lanes
now resolve through the shared runtime construction paths. The selected production
manifest and three-family canary budget have not changed; no extra Prime canary
is authorized by this support work.

Their eight additional logical swap edges reuse existing installed policy bytes.
At finalized slot 444526815 all seven involved swap-policy accounts matched the
retained hashes; eight lookup tables were captured in
`phase3/prime-sibling-lookup-review-2026-09-05.json`. Prime packets use the existing
fresh-hint versioned-message path with exact instruction-key matching, table
ownership/activation validation and preserved-prefix revalidation before send.
No table creation/extension, policy rollover or signing is involved.

Expanded independent SDK parity exposed a real base58 encoder defect: leading
zero bytes emitted NUL characters, and an all-zero value acquired an extra digit.
The shared encoder now emits canonical leading `1` characters. SDK-backed tests
cover every leading-zero count for 32-byte keys, decode round trips and zero
signatures. The normal packet suite now covers 16 Kamino operation vectors and
24 lane-specific swap samples, with account/data mutations and Prime lookup
preparation/mapping-drift rejection. This is local construction proof, not program
execution, fresh-quote feasibility, signer proof or live activation.

The refreshed setup sample at slot 444525343 values the deployed 1,400-byte
borrow-policy allocation at 1.018417 USDC before fees, still above the accepted
1-USDC transaction cap; the 1,250-byte repay allocation values at 0.918441 USDC.
The five remaining OnRe/Maple lanes still differ at farm account positions, and
two farm user accounts remain absent. Do not interpret these setup/authority
gaps as capacity-pending completion. Full linked worker execution, remaining
five-lane repair/support, queue, deployment and canaries remain unfinished.
Checkpoint: `phase3/prime-sibling-construction-2026-09-05.json.gz`.

#### Setup-rent staging feasibility — same checkpoint

A decisive local probe now shows the deployed Squads binary accepts a system-owned,
zero-data policy PDA that was partially rent-funded in a prior transaction. The
second transaction creates the exact original policy, topping up only remaining
rent. Compared with direct creation from the same cloned prestate, final account
bytes, owner and balance are identical. Both the 1,400-byte borrow and 1,250-byte
repay shapes pass. No constraints, seeds, destination semantics or total setup
cost are omitted or weakened.

For the local SVM rent schedule, borrow rent is 10,634,880 lamports: each payment
debits 5,322,440 lamports including its own fee. Repay rent is 9,590,880: each
debits 4,800,440. These are local mechanics figures, not mainnet prices. The sole
verifier retains the two structured witnesses under `localJupiterRepair.setupStaging`
and rejects absent/duplicate shapes, changed policy bytes or omitted payment fees.

This supplies a potential in-cap setup mechanism; it supersedes the assumption
that the entire rent must be paid in one transaction. Before production use,
implement setup admission/persistence, fresh exact-seed/empty-PDA checks, both
priced debits and fees under the existing transaction/family/goal caps, reservation
for the complete remaining setup, and interrupted/ambiguous prefunding recovery.
A prefunded PDA is unfinished setup, not completion or permission to abandon funds.
The existing lease must serialize this with runtime activity and policy changes.
No prefunding or installation was broadcast, and no cap increase is assumed.

#### Unsigned setup construction and complete cost measurement — 2026-09-05

`policy_setup.go` now constructs the two exact OnRe/USDC borrow/repay repair
candidates and their preceding System rent-funding messages. The existing Settings,
admin, delegate, vault, programs, account vectors and policy amount bound are fixed;
only the seed, operation, blockhash and measured rent funding vary. The two farm
placeholder substitutions are the only constraint differences from the retained
catalog. This is candidate construction, not installed authority or registration
of the rejected runtime lane.

The independent installed SDK decodes the retained original PolicyCreate payload,
applies those two substitutions, and checks payload bytes, PDA derivation, instruction
accounts/privileges, packet fit and message serialization. Eight cases cover both
operations and seeds 1, 170, 256 and uint64-max. Legacy account ordering within a
privilege group may differ; resolved instruction semantics and SDK serialization
round-trip must match, not an arbitrary SDK ordering convention.

The cost measurement values each exact message's fee plus its rent contribution,
rounds conservatively, rejects a non-rent-exempt initial system account, stale or
wrong-message fees and per-transaction cap excess, and exposes the entire remaining
setup cost before prefunding. A pure existing-budget reducer test preserves prior
spend and the completion reserve across serialization/restart, ambiguous prefunding
and proven-unsent creation. This does **not** prove durable setup journal integration.

The sole verifier retains these checks as `preflight.localPolicySetup`, explicitly
with `productionSetupAdmission: false`; they cannot independently satisfy R01.
The production build-input decoder and queue still reject this setup request.
Next integration must bind finalized Settings/seed and exact replacement identity,
serialize with the existing lease, persist the full setup intent before funding,
and recover/reconcile both payments before exposing live setup. Never substitute a
setup completion reserve for an existing position exit. Shared-goal setup charging
for historical-family sibling repairs remains unresolved. No new send command,
policy installation, cap exception or runtime binding is introduced here.

Prefer a single PolicyCreate when its freshly priced full debit fits; the measured
two-payment path is a fallback, not a mandatory extra transaction. Checkpoint
`phase3/unsigned-policy-setup-public-2026-09-05.json.gz` (gzip SHA-256
`1d6c2393d8727daa4cb1ae5ba7a04275c84fa0cd3a5a1556d540677360a6f36f`)
records passing local setup, existing PostgreSQL journal and linked lending/return
checks plus fresh public chain/binding/rent observations. The earlier
`unsigned-policy-setup-2026-09-05.json.gz` run lacked sandbox access to the local
database socket and public RPC; it is not a runtime regression. Full Go race tests
passed (31.449s), TypeScript checking passed, and verifier/binding tests passed
(12 tests, 95 expectations). All R01–R08 remain FAIL overall; production database
and deployment observations still lack credentials in this run. Nothing was
signed, installed, deployed or broadcast. The disposable test database is stopped.

#### Finalized setup prestate and fresh exact-payment pricing — 2026-09-05

The existing `--inspect-phase3-setup-rent` command now also measures exact unsigned
OnRe/USDC borrow/repay replacement candidates. It reads finalized Settings, validates
the existing zero external Settings authority, single full-permission admin,
threshold/time lock and forward seed, derives the replacement PDA, and requires it
to be absent. Current native valuation, actual message fees and measured allocation
rent determine direct creation versus the two-payment fallback. A final confirmed
guard rejects newer Settings changes, occupied targets, stale pricing or insufficient
admin balance. It does not adopt or abandon a previously prefunded PDA.

Candidates are independent alternatives at the currently finalized next seed,
**not** a two-policy installation batch. Each creation requires a refreshed seed.
SDK-backed Settings variants and controlled RPC checks cover authority/permission
changes, seed overflow/absence, truncation, changed prestate, target occupancy,
malformed owner responses, underfunding and stale observations. The normal worker's
account reads retain confirmed commitment; the shared decoder now distinguishes
a genuinely absent optional account from a malformed present account with no owner.

This closes fresh seed and exact-payment pricing measurement, not durable setup
admission, interrupted-prefunding recovery, deployment identity/farm validation or
live installation. The sole verifier records six local setup tests and the fresh
candidate observations; `productionSetupAdmission` remains false. Production must
persist the complete setup intent and reserve before funding, recover it under the
same lease after restart, and never replace an open position's exit reserve.

Checkpoint `phase3/fresh-policy-setup-2026-09-05.json.gz` has gzip SHA-256
`e1ff4abd1fe6373e42772bd845d15eb55802393fb595ca4f012d38fb37511a65`.
At confirmed slots 444540205/444540210, both independent candidates use finalized
next seed 140: borrow payments value at 507,832 micros each (1,015,664 total),
while direct repay-policy creation values at 915,537 micros, all including fees.
These are expiring observations, not reserved costs or a live batch authorization.
All six local setup checks, existing local PostgreSQL journal and linked-return
checks pass; full Go race tests pass (32.239s), TypeScript checks pass, and verifier
tests pass (12 tests/95 expectations). All R01–R08 remain FAIL overall. No production
mutation occurred; the disposable test database is stopped.

#### Durable setup intent and journal schema — 2026-09-05

Setup bookkeeping now atomically persists the complete exact-price candidate,
initial payment reservation and remaining completion reserve in the existing
operation journal and goal budget. `state.phase3SetupIntent` is only a pointer to
that row. Exact retry returns the original intent without updating its expiry,
identity, costs or reserve; a restarted worker loads the same pending operation.
Ordinary runtime decisions are fenced while the pointer exists, and setup metadata
cannot be inserted through the generic decision path. An orphaned pointer prevents
budget reinitialization. No production caller or setup send command is enabled yet.

Cancellation is limited to an initial `decided` intent with no signed wire,
signature, broadcast intent or booked spend, and atomically releases only its
unspent reservation. Signed, potentially submitted and settled intents retain the
pointer/reserve. Ordinary delegate recovery cannot discard or send a setup action.
Admin signing, prefunding reconciliation and creation continuation remain required;
this is durable initial-intent recovery, not full two-payment execution recovery.

Real PostgreSQL checks cover concurrent deduplication, reconnect/lease ownership,
restart, conflicting work, cancellation, corrupted/changed intents, missing budget,
cap/exit conflicts, stale observations, and lease expiry during the locked guard.
The verifier requires that setup subtest as well as the existing journal witnesses.

Inspection also found that migration 0072's vocabulary and Maple-only strategy
constraint would reject new Phase 3 lifecycle rows despite local builder success.
Registered migration 0074 adds four debt-conversion actions and two setup actions;
neutral lifecycle rows are limited to the six currently resolved runtime lanes,
and setup rows to the two OnRe/USDC candidates. The five pending farm-repair lanes
remain excluded from lifecycle scope. This is not eleven-lane completion or a
bypass of their unresolved binding review. The exact SQL is exercised on a
temporary PostgreSQL table with valid and forbidden engine/action/strategy cases.
Both migration registries compile. Production migration application is unproven.

For staged setup, the durable completion reserve is the existing 1-USDC allowance
for the one remaining transaction, not its currently measured quote. The saved
candidate still retains that exact quote separately. This prevents a small price
increase after prefunding from causing an unnecessary recovery HOLD within the
accepted caps. Admission must fit the first payment plus this reserve inside the
unchanged family/goal limits. A PostgreSQL-backed test reloads the actual reserved
budget, then exercises the existing reducer: a higher second-payment cost within
the cap fits, an over-cap payment is rejected, prior spend survives and unused
headroom is not booked as spend. This is not production continuation execution;
fresh pricing, signing and finalized reconciliation remain necessary there.

Checkpoint `phase3/durable-policy-setup-headroom-2026-09-05.json.gz` has gzip
SHA-256 `df565621ac029eb5eb5683ade1dd8ce9b29c2940779f7142e9778c5be18ef415`.
The verifier observed both required setup journal witnesses, seven local setup
checks and the linked lending/return check passing. Full Go race tests with the
disposable PostgreSQL database pass (41.760s); TypeScript checking and verifier
tests pass (12 tests/95 expectations); migration CLI compilation passed earlier
in this checkpoint's implementation. R01–R08 still all FAIL overall. Changes are
local and uncommitted; no production migration, deployment, signing, broadcast
or policy installation occurred. The disposable database is stopped, with its
data preserved. The implementation goal remains active; next work is setup
execution/reconciliation and the unresolved runtime bindings, followed by the
complete serialized canary sequence and immutable deployment/live verification.

#### Finalized prefund recovery into creation — 2026-09-05

The existing nonterminal recovery entrypoint now handles submitted setup prefunds
without sending: it matches the exact persisted wire against a finalized receipt,
checks transaction-scoped payer/PDA/System balances and fees, reloads finalized
Settings and the exact system-owned prefunded PDA, then prices only the unpaid
creation with a fresh blockhash. A final confirmed guard rejects changed accounts,
expired valuation or an underfunded payer. Missing receipt is not absence proof.

One transaction under the existing route lease retains that receipt, marks the
prefund reconciled, books its reserved upper once, and inserts/reserves one
creation operation from the original completion headroom. The root setup pointer
continues fencing ordinary work. Concurrent retries and restart load the same
child; over-cap costs, bad receipts, changed state or lease expiry roll back without
losing the original intent/reserve. Neither the settled parent nor its continuation
can use the initial never-signed cancellation path. Native receipt balances remain
in the journal for audit. No second ledger or new runtime lane is introduced.

The verifier now requires the actual database recovery witness plus the controlled
RPC receipt/continuation checks. Synthetic signature fixtures do not prove signer
possession or chain execution. Setup signing, final-send checks, policy-creation
terminal reconciliation and production deployment remain unfinished; this change
does not enable their broadcasts or satisfy top-level R01–R08.

Checkpoint `phase3/finalized-prefund-continuation-2026-09-05.json.gz` has gzip
SHA-256 `5ee5b9ba7cb2f45d637606fe197f2e12f16a2746686007e1eb0ab2689a8a3f3c`.
All three required local journal witnesses, eight setup tests and the linked
lending/return check pass. Full Go race tests with disposable PostgreSQL pass
(42.301s); TypeScript checking and verifier tests pass (12/95 expectations).
R01–R08 remain FAIL overall. Changes are local/uncommitted; no production
mutation, signing or broadcast occurred. Test database stopped with data retained.

#### Finalized policy creation and setup-fence release — 2026-09-05

The worker's setup recovery now reconciles both direct creation and the reserved
post-prefund creation. It requires the exact finalized wire and all five native
account balance deltas, then finalized policy/Settings/Clock readback. The policy
must match SDK encoding derived from retained installed accounts: exact settings,
seed/bump, unused transaction counters, delegated signer/permissions, threshold,
time lock, complete constraints, hooks, spending limits, expiration, rent collector
and zero allocation padding. Its program-assigned start cannot be in the future.

The deployed Squads creation probe confirms that existing Settings changes only
its policySeed. Readback normalizes that single u64 and compares the frozen
prestate hash, rejecting unrelated membership, archival-authority or counter drift.
Receipt/state evidence, finalized journal status, gross settlement and removal of
the root setup pointer commit atomically under the existing lease. Repeated
reconciliation does not spend twice. Unfinalized/altered receipts, changed policy
bytes and expired leases retain the pending operation and reserve.

The sole verifier requires both direct and staged database settlement witnesses
and ten local setup checks. These use controlled RPC/synthetic wires, not live
signatures. Setup signing/final-send enforcement, live installation, reviewed
runtime binding activation and remaining full-lifecycle/deployment proof are still
unfinished. No top-level condition is satisfied by this local slice alone.

Checkpoint `phase3/finalized-setup-creation-2026-09-05.json.gz` has gzip SHA-256
`99cb4da7dd6f1bed8e4238010a28be9e5da1d4188c68d4ac0e5b0efc6752b33b`.
All five required local journal witnesses, ten setup checks, deployed-program
creation checks and linked lending/return checks pass. Full Go race tests with
disposable PostgreSQL pass (43.040s); TypeScript checking and verifier tests pass
(12 tests/95 expectations). R01–R08 remain FAIL overall. All changes remain local
and uncommitted, with no production migration/deployment, signing or broadcast.
The disposable database is stopped with its data retained. Goal remains active.

#### Setup payment cost and signed-identity gates — 2026-09-05

Setup payment observation now refreshes finalized Settings/target state, exact
message fees, native valuation, rent, blockhash lifetime and the confirmed guard
for direct creation, prefunding and post-prefund creation. Prefunding additionally
reprices the still-unpaid creation inside its reserved 1-USDC allowance. Changed
rent or a fee above the frozen receipt bound requires an unsigned-intent refresh;
no request, seed, blockhash or reservation is replaced by this revaluation method.

The pre-sign cost gate validates the journal action, root pointer, settled parent
where applicable, complete reservation shape and unchanged 1/20/60 limits under
the route lease. It records fresh cost evidence without spending or replenishing
budget. The final-send path recognizes setup inputs separately from delegate
lifecycle inputs and verifies the exact wire with the pinned admin public key
before RPC. Its locked budget gate also requires the setup pointer/reservation
and recorded pre-sign cost check. Synthetic and other-signer signatures reject.

These are real cost/identity/journal boundaries, not an enabled admin signer or
send coordinator. Repaired-farm/deployment readiness, unsigned-intent refresh,
setup signer/build/simulation integration and signed-expiry recovery still need
completion. `AdvanceNonterminal` continues to HOLD setup at `signed`; no send was
enabled. Controlled database probes of the lower-level final gate use synthetic
wires and roll back, and do not claim production signer or broadcast proof.

#### Atomic unsigned setup refresh — 2026-09-05

An initial setup intent can now be replaced with a freshly observed plan in one
existing PostgreSQL transaction. The replacement pins the same operation, seed,
policy and Settings identity; reprices rent/fees and reserves the full remaining
payment without resetting family/goal spend. The old failed row retains its
plan and a replacement-operation link. Concurrent and restarted retries find
one replacement instead of allocating new headroom. A failed fresh prestate,
cap or lease check rolls the entire transition back, leaving the old intent and
reservation intact. Even a persisted wire hash without its wire blocks refresh.

This path applies only before any signing or prefunding. It does not refresh a
funded creation continuation, change farm bindings, load a signer or enable a
broadcast. The setup coordinator still needs build/simulation/sign integration
and funded/signed expiry handling before production activation. The sole
verifier now requires the corresponding real-database refresh witness; that
local proof does not satisfy the remaining R01–R08 requirements.

Recheck `phase3/unsigned-setup-refresh-recheck-2026-09-05.json.gz` (gzip SHA256
`b9d1d29420411243920d8afe2bca8df29d45866790bca410073042f4ce9de990`)
passes all seven named journal witnesses, including refresh. Targeted setup and
journal race tests pass (13.471s); TypeScript checking and 12 verifier tests pass.
The preceding refresh snapshot retains a failing test-observation decoding case,
corrected by clearing a reused map before decoding the next database snapshot.
All eight top-level conditions remain FAIL. No production mutation occurred;
the full implementation goal remains active.
