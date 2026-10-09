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
