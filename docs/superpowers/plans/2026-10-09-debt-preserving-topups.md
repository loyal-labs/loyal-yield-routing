# Debt-preserving top-ups: approved implementation boundary

## Approval and rollout
The user approved implementing debt-preserving top-ups, conditional on protecting deposited principal. Zero risk is not claimed. No live funds may be used as an implementation test; no full-balance trial. Root controls any later staged rollout only after review, relevant tests, realistic unsigned/simulated checks, and explicit evidence. Do not clear debt to unlock the old debt-free path. Current capital, liquidity, fee, slippage, program-policy, withdrawal and liquidation guards remain unchanged.

## Architecture
Extend the existing journaled top-up tranche, not a new money mover. Allocation -> USDC-to-collateral swap -> collateral-only deposit may increase collateral but must neither borrow nor repay principal. Interest accrual is accounted using validated reserve evidence. Any later ordinary leverage action remains separate and governed by existing authority. Do not infer a tranche's ownership from idle balances or a caller-provided reason.

## Boundaries to cover
- Planner: recognize debt-bearing top-ups without treating borrowed debt custody as post-payoff residue. Preserve precedence for pending/signed work, withdrawals, hard risk, unwind, NAV and existing borrow-cycle completion. Keep idle buffer and every tranche/deposit-room/bridge cap.
- Allocation: exact journaled amount, unchanged existing collateral/debt/custody, truthful post-allocation NAV, fresh accounting. Use debt-aware whole-position exit COST reservation; never erase debt or grant execution consent to projected payoff templates.
- Swap: exact USDC provenance/debit, bounded output, correct empty destination and untouched debt custody. Validate existing obligation and accrued debt, including build/send refresh. Don't reuse debt-free entry assumptions or adopt arbitrary cash as top-up funding.
- Deposit: reuse the debt-preserving redeposit projection with explicit top-up origin. Refresh both collateral and borrow reserves in the correct wire order. Collateral increases; debt changes only by validated interest, no borrow/repay instruction. Keep ordinary debt-free/flat paths intact.
- Persistence/recovery: retain complete exit reserves, immutable current input/wire, and origin/amount across restart after each leg. No double allocation, no reset cumulative budget, no dropping signed/ambiguous reservations. Concurrent deposits/withdrawals and changed prestate must not bypass priority or proof.

## Validation gates
Use current topup allocation/swap/deposit/wire and redeposit suites, plus actual DB persistence invariants where needed. Positive AUTO/PYUSD debt-bearing case; existing debt-free behavior; reject stale/mixed provenance, unexpected debt/custody/reserve change, missing exit budget, old interest window, unauthorized payoff/borrow, and withdrawal/risk/unwind conflicts. Validate interruption/restart after each leg. Compare exact wire account/order and projection invariants, not superficial fields. Existing literal caps/freshness/debt-clear gates must remain unchanged. No production deployment or financial action by implementation worker.

## Scope control
One focused implementation pass; no unrelated refactor or new platform/service. If safe tranche provenance or debt-aware pricing cannot be established with existing primitives, stop and report the precise blocker instead of weakening a guard. Source impact trace: ../voltr-handoff-20261002/earn-max-kamino-stale/debt-preserving-topup-impact.txt (absolute path in task message).

## Confirmed implementation extension
Source review found no durable top-up ancestry in existing B3. Root approved a
compact existing-route-JSON tranche/origin binding for implementation only.
Creation and verified chain-effect transitions must be atomic with the current
phase3/journal transactions, with lane, exact origin/amount and remaining-stage
ownership. Balance-only inference is not sufficient. Preserve existing
fencing/generation and signed ambiguity behavior. Test duplicate allocation,
restart after each stage and interruption by withdrawal/risk/unwind. No new
service/table is planned. Parent supplies a bounded disposable PostgreSQL
fixture when persistence tests are ready. No live activation has been approved
as risk-free; present evidence and residual risks before activation.

## Approved proof seams (implementation only)
- Bind original obligation debt amount SF and cumulative borrow rate plus collateral baseline to the admitted tranche; revalidate using existing exact accrued-debt logic. Fresh observation alone is not a new principal baseline.
- A withdrawal/risk/unwind interruption needs an explicit journal-linked handoff from the tranche to the already-authorized destination flow. Preserve exact owned balances and verified receipt identity atomically, including direct swap/stage starts, partial consumption and demand disappearing. Never zero/adopt balances to make the ledger fit.
- A handoff does not create withdrawal or debt-clear authority. Existing financial authorization remains unchanged. New topups remain blocked until ownership and residual inventory are resolved. Do not weaken withdrawal or hard-risk priority.
- Define the small transition table and focused contract tests before wiring these seams. Stop if this needs a broader exit-authority change. Rounding residue must not cause a permanent active-tranche deadlock.

## Implementation transition invariants (review gate)

The rows below are the contract to wire into the existing route-lock transaction.
The new contract helpers are not execution authority. No planner enablement is
allowed until their production admission/reconciliation integration is reviewed.

| From / event | Ownership after finalized reconciliation | Required authority / rejection |
| --- | --- | --- |
| No active tranche (or completed carry) / allocation | Exact allocation USDC; retain only the prior recorded collateral carry | Origin is this operation; empty USDC/debt/strategy custody; collateral equals recorded carry; fresh original loan fractions/rate and collateral witness |
| Allocated / USDC-to-collateral swap | Actual swap output plus the exact prior carry; USDC becomes zero | Same origin, loan witness and predecessor receipt; full owned input; no other collateral or debt cash |
| Collateral / full-request deposit | Complete tranche; retain actual rounding remainder and authenticated deposit quantum | Existing bounded-deposit effects establish quantum = maximum debit - minimum debit + 1; remainder must be below it; no principal rebase |
| Complete carry / next allocation | New allocation origin links the completed predecessor and exact carry | New loan admission may establish the next tranche; old cash cannot be silently adopted; carry is not erased |
| Active tranche or carry / first authorized exit leg | Transfer exact inventory to the admitted withdrawal/risk/unwind origin, with its authority digest and finalized operation receipt | Source predecessor and destination authority both bound; direct swap/stage starts included; a reason string alone never authorizes handoff |
| Handoff / partial swap, repay, release, stage or restore | Keep actual remaining USDC/collateral/debt/strategy inventory; add only proven receipt credits from an already-authorized position release | Exact owned pre-balances; unchanged destination origin; existing debt-clear and partial-withdrawal guards remain decisive; no borrow or allocation |
| Handoff / demand disappears | Ownership and reservations persist; existing authorized continuation or a conservative hold | Capture existing partial-withdrawal continuation at a direct start; demand disappearance cannot clear inventory or grant full repayment |
| Handoff / all owned cash reconciles out | Complete with the handoff receipt retained; a remaining nonzero balance is not written off | No new top-up or route switch while handoff inventory remains unresolved; safety/withdrawal decisions keep their existing priority |

### Reviewed inert loan contract (runtime remains disabled)

`validatePrincipal` retains actual finalized obligation SF/rate, collateral,
Clock slot/time, borrow timestamp and account-batch hash. The origin reader uses
`FinalizedSlot` and one finalized account batch containing the actual loan and
reviewed KLend program/header/full image. It verifies identity before decoding
the loan. No virtual refresh or caller-provided finality flag establishes origin.

Deployed-verified KLend source is `a08760976f51a3a58c4a0c6ea27b4a0e565bca79`.
A top-up-only capability pins its ProgramData deploy slot and full allocated ELF
hash; the global Voltr/adaptor watcher is unchanged. Origin Clock time must be
strictly later than the stored borrow timestamp. A changed/reset marker or
regressing Clock fails. The proof assumes coherent finalized ancestry and the
ancestor-monotonic Clock implemented by the reviewed Agave runtime.

For increasing stored rates, principal SF must lie in `[U-D,U]`, where
`U=floor(A0*R/R0)`, `N=currentClockSlot-originClockSlot+1`, and
`D=ceil(N*R/R0)-1`. Require `N*R <= 2^60*R0`; the derived interval remains below
one raw unit. Unchanged rate has `D=0`. The initial `+1` covers an old stored
obligation rate at origin. A separate raw-ceil equality would reject legitimate
rounding and is not used. Exact top-up wires still contain no borrow or repayment;
the interval alone does not claim to reconstruct arbitrary external history.

The principal origin is not an executable payoff window. A fresh attempt must
pass all existing phase3 payoff/rate/freshness/budget guards using the same origin;
spent counters, prior exit reserves and signed/ambiguous recovery stay unchanged.
Cost-only payoff projections never become repayment or full-exit authority.

Native tests now exercise the actual helper, finalized reader and reviewed offline
ProgramData image, including sequential floors, raw-ceil boundary, principal and
marker mutations, malformed provenance/topology, old stored rates and expiry.
Set `BACKYARD_KLEND_PROGRAMDATA_FIXTURE` to the reviewed offline ProgramData for
the image-reader test; it makes no network calls and the 10MiB image is not
checked in. Runtime integration, cash/handoff persistence, descendant capability
checks and budget-preserving fresh re-admission remain review gates.

## Completed-residue compatibility correction

Completed residue does not lock ordinary borrowing, redeposits, or emergency
funding. Their existing phase3 authority remains decisive. Each finalized
ordinary receipt touching collateral updates the journal-linked balance. A
positive ordinary credit is marked `ordinary`, not top-up inventory, and cannot
start another allocation. A full-custody deposit can establish its exact bounded
rounding remainder as completed carry; a zero balance also completes it. This
preserves provenance without adopting an arbitrary observed balance.

An active tranche may hand off to hard-risk protection only with the existing
`verifyDebtClearEmergency` proof bound to the admitted operation, observation,
account hash, slot and recomputed decision. A hard-risk reason or snapshot alone
is insufficient. This handoff does not grant full repayment consent or replace
any existing build/send checks.


## Takeover implementation and delivery boundary (2026-10-09)

This section supersedes the earlier inert-runtime and missing-first-hop status.
The changes remain local and uncommitted; no live activation is authorized.

The original stale-cash cause was the debt-free-only top-up path. The recurring
path now retains the original loan, admits only receipt-bound new cash, swaps it
into collateral, deposits without borrowing or repaying, reconciles the actual
receipt, and reports NAV. A completed tranche releases planning for new cash.

Allocation now prices the installed USDC-to-AUTO conversion and a fully funded
whole-position recovery before taking cash from Voltr. The entry's enforceable
minimum collateral must fund the payoff through AUTO-to-PYUSD at its enforceable
minimum. Optimistic output sizes remaining return costs only. An allocation
requiring additional release/repayment cycles is refused; cycle and horizon
limits are unchanged. The shared pricer still retains guarded cycle behavior for
other callers. Cost templates grant no execution authority.

For hard risk after allocation, a distinct reason classifies the exact owned
USDC conversion ahead of dust collateral. Admission requires a fresh private
coherent risk batch, independently recomputed hard risk, immutable original loan,
exact custody, and the existing emergency full-exit authorization. The first hop
and following receipts bind the same handoff. This is an off-chain classification
extension to an existing emergency authority, not a new installed policy edge.
It intentionally allows whole-position repayment and collateral return under
that authority, including the collateral deposited before this new tranche.
Normal top-ups continue to preserve the loan.

Recovery must fit the original allocation's reserve. It cannot enlarge that
reserve. Normal capital remains subject to the existing entry-cost cap and keeps
its protected exit reserve even if a later estimate is cheaper. Recovery carries
the unused original reserve through the existing durable unwind so that a
recovered risk reading cannot silently turn unfinished cash into new capital.

A further pre-allocation check compares spent execution costs, current allocation
cost and the already-priced next swap against the existing $500 pilot execution
cost cap. It refuses before allocation when that swap is already unaffordable.
It is not a promise that future quotes, deposit fees or prices cannot change.

### Concrete offline coverage

The database lifecycle fixture now uses reserve-derived quote prices and token
decimals. It runs normal $20 top-up completion, interruption after allocation,
and interruption after the normal collateral swap. Each leg uses production
planning/admission, compiled wire validation, locked build/send authorization,
database reopen/reacquired lease, receipt reconciliation, and receipt-derived
next balances. Recovery reaches debt-free/collateral-free custody and returns
cash to Voltr under the initial allocation reserve. Risk recovers after funding;
withdrawal demand appears and disappears through decoded receipt observations.
The normal path preserves principal and permits planning the next deposit.

The $100,000 fixture is a refusal test, not a successful investment test. With a
reserve-price quote and 50 bps minimum-output margin, its next swap has an
execution-cost upper bound of 2,470,297,541 micro-USDC, exceeding the unchanged
500,000,000 cap. This bound includes conservative valuation and is not an actual
fee or incurred loss. Rejection leaves Voltr cash and durable authority unchanged.
It does not establish the current live quote or remaining production allowance.

### Release boundary

No production keys, live accounts, chain signing/submission or deployment were
used. The database chain uses modeled RPC/simulation/receipts and synthetic
production-delegate signature envelopes. Separately, every wire shape is signed
with a deterministic local test key and checked with the same
`BuildResult.validateForDelegate` validator. The real production-key
`PersistSigned` entrypoint and network broadcast are not exercised. This is not
on-chain VM execution or a full Worker.Tick integration test.

The full $100k deposit is not cleared for activation. Fresh read-only eligibility,
independent review of the whole inherited patch, and an isolated release/rollback
basis remain release gates. No cap increase, valuation-margin reduction or
full-balance live trial is proposed. Market/quote/rate changes can still force a
conservative hold; offline passes do not guarantee loss-free execution.

Exact commands, results, source snapshot and remaining limits are recorded in
`/tmp/earn-max-takeover-decision.md`.

## Budget-separation review (2026-10-09)

Fresh unsigned public quote/compile tests are authorized and completed; the
older no-live-account-read statement above describes the earlier offline phase.
No live signing, submission or activation has occurred. Evidence:
`/tmp/earn-max-live-test/REPORT.md` and `/tmp/earn-max-slippage-test/REPORT.md`.
The first swap simulation stopped with insufficient source funds. Tighter
slippage (25, 10, 0 bps) compiled but all conservative charges exceeded $500;
zero slippage still yielded about $2,023.08 on that fresh quote.

A bounded offline review rules out omitting valuation margins as an equivalent
accounting correction. A synthetic $90k top-up with the real admission and
recovery paths is refused at $2,223.267838 with current valuation, but is admitted
at $450.000512 when only those margins are removed in a scratch overlay. Full
payoff funding and the identical gross exit reserve pass in both cases. Those
recovery guards do not substitute for the existing bounded execution-cost gate.
This test stops at admission without signing or moving funds. Two focused DB
runs passed; the disposable database was verified stopped. Full counterexample,
source findings and limits: `/tmp/earn-max-budget-review/REPORT.md`.

No financial source, cap, valuation bound or activation setting is changed by
this review. Separating the labels alone cannot unlock the deposit. Making an
execution-only counter decisive changes risk semantics; retaining an equivalent
conservative capital check keeps the current quote blocked. A viable change
needs independently justified tighter valuation evidence or an explicit risk
boundary change, plus review and funded/recovery proof. Neither is established.
Do not repeat slippage-only probes or evade cumulative accounting with batches.
The $500 normal-entry stop is not a guarantee against all market/recovery loss.
