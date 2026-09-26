# Earn MAX plan: reliable moves, then higher APY

Owner: Vlad. Driver: agent. Created 2026-09-26. Parent: ASK-2293.
Status: the working plan. Follow it in order. Update the status column when a step moves.
Every step that changes worker rules or production is shown to Vlad before deploy.

## Where we are (2026-09-26 21:50 UTC)

- Vault $381.66, all in the AUTO loop at 1.5x (LTV 33%). Expected ~12% APY at today's rates.
- The optimizer now uses correct borrow rates (fix `c0811f0`); the 30-day window works (first real move done).
- Problem: moves need many tries and restart the worker (13 tries, 10 restarts on 09-26).
- Problem: APY is capped by fixed 1.5x leverage, idle deposits and one usable lane.

## Goal

1. Every step of a move lands in 1-2 tries, with no worker restarts.
2. Earn MAX APY clearly above regular Earn: target 15%+ when borrowing is cheap, never below the best
   token's own yield when it is not.

## Part A: reliable moves (first; every APY step below triggers moves)

| # | Step | Task | Status |
|---|------|------|--------|
| A1 | Freshness window in seconds (~13 s), not 32 slots. Slots are ~270 ms now (8.6 s window); our pipeline needs 9-12 s. | ASK-2313 | Todo |
| A2 | Any refusal before sending = retry on the next tick, never a worker restart. Real faults still stop the worker. Replaces the growing retry list. | ASK-2313 | Todo |
| A3 | Skip the 1-2 min blockhash wait when the attempt was never broadcast (no broadcast intent recorded). Verify first that no path sends without the mark. | ASK-2313 | Todo |
| A4 | Measure on the next real move: tries per step and restarts. Pass = most steps in 1-2 tries, 0 restarts. | ASK-2313 | Todo |
| A5 | Only if A4 fails: speed up admission (quote, exit pricing, projections; now 7-9 s). | ASK-2313 | Decide after A4 |

## Part B: higher APY (in order of impact)

| # | Step | Task | Expected effect (09-26 rates) | Status |
|---|------|------|------|--------|
| B1 | Correct borrow rates in the optimizer | ASK-2310 | AUTO 1.5x 9% -> 12% | Done (`c0811f0`) |
| B2 | Variable leverage per lane: choose 1x / 1.5x / 2x / up to the safe cap by the real spread; 1x (no loan) when borrowing costs more than the token pays. Chris's leverage grid (`scripts/rwa-decision.ts`). | ASK-2309 | AUTO ~12% -> ~15-18% at 2-3x while borrow < yield | Todo |
| B3 | Idle cash into the current loop (top-up tranche), so new deposits earn at once. | ASK-2308 | No more idle weeks after deposits (was ~60% idle) | Todo |
| B4 | Enable OnRe (then Prime) for entry. | ASK-2311 | OnRe 1.5x ~12.8% today; more choice when rates move | Todo |
| B5 | Fix the doubled APR/APY in the shared Kamino collector (other services). | ASK-2312 | Correct data everywhere | Todo |

## Part C: small cleanup (with the next worker change, no separate deploy)

- Remove the timing logs (after A4 measurements).
- Earn MAX UI: APY badge = 7-day share price; check it after a week of full deployment.

## Safety rules (unchanged)

- 60% hard LTV rule, liquidation buffer, persist-before-send, one operation at a time, latches.
- Real money: ask Vlad. Rule changes: explain the trade-off first. Deploy only between money operations.
- The $500 top-up is not needed for the optimizer test anymore (done with $381 on 09-26).

## Log

- 2026-09-26: plan created. B1 done. First optimizer move Maple -> AUTO done (Maple unwind 18:19-18:24,
  AUTO entry 20:36-21:34 UTC). Codec 2x rate bug found (ASK-2312). Slow-swap cause found (A1-A3).
