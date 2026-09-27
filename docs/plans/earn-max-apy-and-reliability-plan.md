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
| A1 | Freshness window in seconds (~13 s), not 32 slots. Slots are ~270 ms now (8.6 s window); our pipeline needs 9-12 s. | ASK-2313 | Done (`13290aa`, live 09-27 00:38; measured 268-272 ms -> 48-49 slots) |
| A2 | Any refusal before sending = retry on the next tick, never a worker restart. Real faults still stop the worker. Replaces the growing retry list. | ASK-2313 | Done (`2d06e87`, live 09-27 00:29) |
| A3 | Skip the 1-2 min blockhash wait when the attempt was never broadcast. | ASK-2313 | Blocked: the build step simulates the SIGNED wire (sigVerify true) before intent, so the RPC provider holds sendable bytes; not safe as is. Vlad chose (c) 09-27: measure after A1 (A4) first; if still needed, simulate unsigned first, then A3. |
| A4 | Measure on the next real move: tries per step and restarts. Pass = most steps in 1-2 tries, 0 restarts. | ASK-2313 | Next real move (B2 or B3) |
| A5 | Only if A4 fails: speed up admission (quote, exit pricing, projections; now 7-9 s). | ASK-2313 | Decide after A4 |
| A6 | Kamino health hold (reserve refresh age) latches only after 3 in a row, like FIX25 for refresh failures. Gen 8 latch 09-26 21:52: AUTO reserve last refreshed 345 slots (~93 s) before the check; nothing unsafe. | ASK-2313 | Done (`3aadeea`, live 09-27 00:16; also fixes the unrefreshed USDC reference reserve behind the 23:19 latch) |
| A7 | A latch always pages. The relay folded the 21:52 latch into the open Errors window for the worker (outcome suppressed), so nobody saw it for ~50 min. | ASK-2313 | Done (loyal-app #799, live 09-26 23:14) |

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

- 2026-09-27 00:40: A2 live (`2d06e87`), A1 live (`13290aa`). A3 blocked (signed-wire simulation before intent); Vlad chose to measure first (A4). Next: B2 variable leverage.

- 2026-09-27 00:18: A6 live (`3aadeea`). A7 live (loyal-app #799). Gen 9 latch 23:19 cleared; kamino_stale auto-clear standing OK until A6 proves itself.

- 2026-09-26 22:45: gen 8 kamino_stale latch at 21:52 stopped NAV reports (Earned frozen). The latch alert was suppressed by the relay mute window. Added A6, A7.

- 2026-09-26: plan created. B1 done. First optimizer move Maple -> AUTO done (Maple unwind 18:19-18:24,
  AUTO entry 20:36-21:34 UTC). Codec 2x rate bug found (ASK-2312). Slow-swap cause found (A1-A3).
