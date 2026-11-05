# Earn MAX 90/5/5 allocation targets — local verification

Implemented and verified locally on November 5, 2026 against main `a96ab545e`.
No commit, deployment, production configuration change or transaction broadcast
was performed. [Measurements and source hashes](earn-max-allocation-targets-2026-11-05.json)
record the final candidate. This supersedes the earlier 50% gross-cap candidate.

## Allocation policy

- Protect the existing withdrawal reserve first: 10% of NAV, with a $5,000
  floor and $25,000 cap, bounded by NAV. Reserve pending withdrawals separately.
- Of the remaining own capital, target 90% OnRe/AUTO combined, 5% Maple and
  5% PRIME. Count position collateral minus debt, plus loose collateral once
  per mint. Aggregate all PRIME debt lanes into one target and all AUTO lanes
  into the main bucket. The OnRe/AUTO split follows executable net benefit.
- Give profitable Maple/PRIME entries priority until their targets are filled.
  Cap each purchase's own-capital spend by its remaining target room; recheck
  stored entries and fresh purchases against current observations. Target
  shortfalls may remain in cash when no investment qualifies.
- Separately cap additional gross exposure at 100% of NAV per provider group;
  AUTO and PRIME share one group. Borrowed exposure counts toward this ceiling.
  These ceilings are not amounts earmarked to each group. Maple's much smaller
  own-capital target and the absence of new borrowing limit its admissions.
- Preserve the seven-day profitability test, full entry/exit costs, $0.25
  minimum benefit and 30-minute persistence. The 10 bps uncertainty allowance
  applies to the new investment amount, so a small purchase does not bear an
  allowance calculated from the entire vault.
- New Maple/PRIME investments do not borrow. Existing holdings and stored
  leverage levels are grandfathered. Automatic rotations and overflow moves
  remain paused. Signed recovery, withdrawals and necessary risk reductions
  retain priority.

At $100,000 NAV with no pending withdrawals, the reserve is $10,000; target own
capital is $81,000 OnRe/AUTO, $4,500 Maple and $4,500 PRIME. Borrowing increases
gross exposure but does not increase these own-capital target amounts.

## Checks

`scripts/verify-workers-v2.sh` passed on the final source, including formatting,
vet, all worker package tests and worker binary builds. Database tests used a
fresh disposable local Postgres cluster, which was stopped afterward.

Regression cases cover target sizing after reserve/withdrawal deductions,
borrowed exposure versus own capital, shared custody and provider aggregation,
profitable small-target priority over a larger core gain, return to core
allocations after targets fill, unfavorable rates and costs, restart persistence,
fresh quote rechecks, grandfathered holdings and safety/recovery priorities.

At **17:14:37–17:14:42 UTC**, slot **465785851**, a read-only replay of the current
vault returned **HOLD / at_target**, exactly matching the prior policy. All
four stored leverage targets remained unchanged: OnRe 1.75x, AUTO/PYUSD 1.75x,
AUTO/wYLDS 3x and PRIME/PYUSD 1.5x. No borrowing or optional reduction was
proposed; there was no pending operation or manual-recovery latch.

The probe used transaction-scoped read-only SQL and a transport that refuses
broadcast RPC methods. It never loaded a production signing key. Remote
diagnostic files were removed after the replay.

## Current constraints

Cash was **$2,425.10**, against a **$17,738.31** reserve target plus **$245.48**
in pending withdrawals: a **$15,558.70** cash gap before new investment. The
worker does not sell existing holdings to fill this reserve. The target grows
with NAV until the reserve cap is reached, so this gap is not a fixed deposit
threshold.

Maple's observed unborrowed yield was **4.9601%** annualized, approximately
**9.282 bps over seven days** before costs. That is below the **10 bps**
uncertainty allowance even before trading fees or the minimum benefit. Thus
the current policy cannot fill Maple's target at these rates. This is a soft
target subject to profitability, not an unconditional 5% investment order.
PRIME's equivalent seven-day carry was about 11.277 bps before costs; no fresh
funded entry quote was taken because the live vault lacks investable cash.

The earlier funded Maple/PRIME route simulations remain
[separate execution evidence](earn-max-diversification-policy-2026-11-05.md).
This target change introduces no new transaction shape; those funded route
simulations were not repeated. The live replay proves present-state continuity,
not future prices, availability or returns.

Targets can drift with prices, better fills and grandfathered positions. Gross
provider ceilings can restrict leverage or leave cash idle. Most exposure
remains in OnRe/AUTO; small allocations do not eliminate losses or shared
infrastructure risk. Paced withdrawals, longer withdrawal windows and an
overall economic exit-loss ceiling are outside this change.
