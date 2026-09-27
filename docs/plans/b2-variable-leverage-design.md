# B2 design: variable leverage (ASK-2309)

Status: DRAFT for Vlad's OK (2026-09-27). No code before approval.
All numbers below were computed in code from live data at 2026-09-27 12:44 UTC
(worker `--selector-shadow`, Kamino Timescale, Jupiter quotes, on-chain AUTO reserve).

## What changes

Today the worker has one leverage: one borrow loop at a 50% borrow, which gives 1.5x
(`singlePassLeverage = 1 + TargetLTVBPS/10_000`, `TargetLTVBPS=5000`).
B2 lets it pick the level by the real spread (token yield minus borrow cost), and move
between levels without a full exit.

## Today's numbers (APY by level, correct borrow rates from `c0811f0`)

| Lane | Token yield | Borrow | Spread | 1x | 1.5x | 1.75x | 2x |
|---|---|---|---|---|---|---|---|
| AUTO/AUTO/PYUSD | 9.50% | 5.97% | +3.53 | 9.50% | 11.27% | 12.15% | 13.04% |
| OnRe/ONyc/USDC | 11.02% | 7.79% | +3.23 | 11.02% | 12.64% | 13.44% | 14.25% |
| Prime/PRIME/USDC | 6.57% | 5.53% | +1.04 | 6.57% | 7.09% | 7.36% | 7.62% |
| Maple/syrupUSDC/USDC | 5.17% | 5.65% | -0.48 | 5.17% | 4.93% | 4.81% | 4.69% |

AUTO in detail (NAV $381.69):

| Level | Loops | LTV | APY | AUTO price drop to the 45% alert | Drop to the 60% hard rule |
|---|---|---|---|---|---|
| 1.0x | 0 | 0.0% | 9.50% | no loan | no loan |
| 1.5x | 1 | 33.3% | 11.27% | 25.9% | 44.4% |
| 1.75x | 2 | 42.9% | 12.15% | 4.8% | 28.6% |
| 1.875x | 3 | 46.7% | 12.59% | already above | 22.2% |
| 2.0x | not reachable by loops | 50.0% | 13.04% | already above | 16.7% |

- AUTO reserve: borrow limit 78%, liquidation 80% (read from the reserve account).
- AUTO price over the last 7 days (Timescale): worst drop from peak 0.002%. Only 7 days of data.
- PYUSD reserve is 89% used. Its curve: 5.80% at 90%, 6.62% at 95%, 9.05% at 100% (APR).
  At 100% the borrow cost (9.47% APY) equals AUTO's yield, so leverage earns nothing.
- Chris's grid (`scripts/rwa-decision.ts buildLeverageGrid`): 1x to the cap in 0.25x steps;
  cap = min(policy target 50%, liquidation - buffer) = 2x.

## Honest read

- The plan said "AUTO 15-18% at 2-3x". That is wrong at today's rates: AUTO 2x = 13.0%, and
  2.5x = 60% LTV = the hard rule itself, so it is not allowed. 15%+ needs cheaper borrowing
  or a higher-yield token (OnRe 2x = 14.3%).
- Going 1.5x -> 1.75x on AUTO today: +0.88 pt, about $0.28 per 30 days on $381. The move
  costs about $0.02 (Jupiter round trip 0.023%) plus a few 5000-lamport fees. It pays, but
  only just clears the optimizer's $0.25 minimum at today's size.
- The bigger value is protection: when borrowing costs more than the token pays, go back to
  1x instead of paying to hold a loan.

## Proposal (recommended: option 1)

**Option 1: levels 1x / 1.5x / 1.75x, cap 45% LTV.**
- Up one level (one more borrow loop: borrow, swap to collateral, redeposit) when the spread
  is at least 2.0 pts on the 30-day forecast and the move beats the existing minimum benefit.
- Down to 1.5x when the spread falls below 1.0 pt; down to 1x when it falls below 0
  (borrow > yield). Down = withdraw collateral, swap to debt token, repay (the existing
  unwind legs, stopped part-way).
- The gap between the up and down thresholds prevents flip-flopping. One level per move.
- 1.75x ends at 42.9% LTV: under the 45% warning, 28.6% price drop to the 60% hard rule.
- No change to the 60% hard rule, the liquidation buffer, or the 50% borrow cap per loop.

**Option 2: also 2x (50% LTV).** +0.9 pt more on AUTO. Needs the 45% warning raised
(it would fire all the time) and more loops or a flash loan (loops only approach 2x:
1.75x, 1.875x, 1.9375x). Price drop to the hard rule shrinks to 16.7%. Not recommended now.

**Option 3: protection only (1x / 1.5x).** Smallest change: only the move down to 1x when
borrowing costs more than the token pays, and back up to 1.5x.

## Build order (after OK)

1. Selector: score each lane at each allowed level (today it scores 1.5x only); pick with
   the thresholds above. Shadow-only first: log the chosen level, no moves, for 1-2 days.
2. Up move: one extra borrow sized to reach the next level (1.5x -> 1.75x = borrow 25% of
   equity; the moment before redeposit is exactly 50% LTV, the existing per-borrow cap),
   reusing the existing borrow / swap / redeposit steps.
3. Down move: partial unwind to the lower level, reusing the existing withdraw / swap /
   repay steps.
4. Tests for each decision; then one real move up with Vlad watching (this is also A4:
   tries per step and restarts).
