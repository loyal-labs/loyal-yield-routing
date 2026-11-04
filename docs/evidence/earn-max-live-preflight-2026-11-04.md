# Earn MAX live preflight — November 4, 2026

## Result

The candidate preserves the current investments when borrowing pools are full.
At slot **465379320**, its lifecycle decision was **HOLD / no_room** and
its allocation selector returned **KEEP / no_worthwhile_executable_move**.
Every sampled leverage target remained unchanged. No optional downward
approval was produced. The live worker's stored selection was also KEEP.

This validates immediate deployment continuity for the sampled state, not
unconditional release readiness. Three representative debt-repayment wires
simulated successfully in the earlier run. The final run obtained two priced
rotation alternatives and chose KEEP. Prices for increasing ONyc and
AUTO/PYUSD leverage were unavailable; the later repayment rechecks explicitly
encountered quote-service limits. Do not mark those rechecks successful or use
expired quotes to authorize a later transaction.

No production migration, deployment, restart, database write, signing or
transaction broadcast was performed. Migration 0137 was applied to captured
target values **in memory only**.

## Identity and timing

- Vault: `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`.
- Route: `rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh`.
- Current worker: `c8d714d498f1db5d1383d3337f99262d60d5474d`.
- Candidate: main `38e8dd309d7c3b0cfb8ea1952fcfda04505bd82a` plus the local loss-control changes.
- Final capture: 2026-11-04T17:05:27.254838097Z through 2026-11-04T17:05:32.696043762Z.
- Earlier successful repayment simulations: 2026-11-04T17:01:18.080711473Z through 2026-11-04T17:01:50.96485002Z.
- Source fingerprint: `d3b1537f3eb2001ec25adc8f34b1806e394407401332085f29783cc36cd53e54`.
  It hashes sorted Backyard Go file paths and bytes, followed by migration 0137.
- Final diagnostic binary SHA-256: `1a6cdd5649231ff500373fc616cc0d17862b21b61ecfb607573a8646209bb970`.

The probe was compiled through a temporary Go overlay, reused the candidate's
actual decision functions and transaction builders, and was never installed
as a worker. Its account view was refreshed with real confirmed RPC data for
one-shot pricing; production maintains that view through its stream.

## Holdings and target continuity

| Position | Marked collateral | Marked debt | Observed leverage | Stored target → migration target | New borrowing |
|---|---:|---:|---:|---|---|
| OnRe/ONyc/USDC | $206,512.95 | $68,837.69 | 1.5000× | 1.75× → 1.75× | blocked |
| AUTO/AUTO/PYUSD | $51,128.73 | $17,030.13 | 1.4994× | 1.75× → 1.75× | blocked |
| Prime/PRIME/PYUSD | $10.33 | $0.00 | 1.0000× | 1× → 1.5× | blocked |
| AUTO/AUTO/wYLDS | $10,674.19 | $7,035.81 | 2.9338× | 3× → 3× | tiny remaining room |

Observed leverage is collateral divided by collateral minus debt, before
exit allowances. A target is not the amount already borrowed: the two main
positions remain around 1.5× despite their stored 1.75× targets, because they
have no additional borrowing room. The candidate holds those positions.

The legacy PRIME/PYUSD target is the one stored as 1×. The migration translates
it to its former effective meaning, 1.5×, and keeps upward approval false.
That position contains only about $10.33 and no debt. Neither the translation
nor a restart instructs the worker to buy or sell it. A new true-1× reduction
still requires fresh economic justification and approval.

There was no nonterminal operation, recovery latch, ambiguous submission or
unfinished rotation in the captured state. Existing approved upward targets
are preserved; this is not a promise to cancel them if borrowing room later
returns. Installed program identities and observation freshness passed.

## Cash and withdrawal demand

- Vault NAV, including the existing exit allowance: **$175,349.24**.
- Cash available in the vault: **$872.18**.
- Current withdrawal demand: **$250.48**, already covered by cash.
- Cash after that demand: **$621.69**.
- Proposed cash target: **$17,534.92**.
- Gap to that target before the pending withdrawal: **$16,662.75**.

The higher cash target does not trigger sales to fill this gap. New inflows
must fund it; the proposed reserve protection is largely unfunded today.
Large withdrawals can therefore still require collateral sales after rollout.

## Actual unsigned simulations

Each transaction below withdrew 1% of its lane's collateral receipts (bounded
by available withdrawal room), sold or redeemed that collateral, and repaid
debt atomically. These were hypothetical capability checks, not approved
investment decisions. Every listed simulation returned success and lowered
LTV; no signed wire was created.

| Lane | Simulation slot | Wire bytes / limit | Keys / limit | Compute units | LTV before → after |
|---|---:|---:|---:|---:|---|
| AUTO/AUTO/PYUSD | 465378297 | 2012 / 4096 | 46 / 64 | 591,413 | 33.31% → 32.64% |
| AUTO/AUTO/wYLDS | 465378299 | 1386 / 4096 | 29 / 64 | 469,720 | 65.92% → 65.58% |
| OnRe/ONyc/USDC | 465378293 | 2208 / 4096 | 52 / 64 | 555,577 | 33.34% → 32.67% |

The final run again simulated AUTO/wYLDS successfully at slot 465379337.
Its ONyc and AUTO/PYUSD rechecks stopped at Jupiter HTTP 429 before their
money-moving wires were simulated. The final diagnostic made 24 quote GETs
and 20 swap-instruction construction POSTs; it ran alongside the existing
worker, so shared provider quota is a limitation of this check. No additional
requests were made to retry those blocked simulations.

The earlier snapshot's next action was REPORT_NAV because the ordinary
accounting update was due. By the final snapshot the existing worker had
updated its accounting and the next action was HOLD. The report-action
simulation branch therefore did not run; that accounting transaction is not
claimed as a live simulation pass here.

[Solana's simulation RPC](https://solana.com/docs/rpc/http/simulatetransaction)
supports unsigned transactions with signature verification disabled and does
not broadcast them. The probe additionally blocked submission RPC methods
and never loaded the policy signing key. Its database capture used a
repeatable-read, read-only transaction with a local 10-second statement timeout.

## Operational observations and remaining gate

### Complete-payoff follow-up, 17:21–17:22 UTC

A separate paced check completed the previously missing final-repayment gate.
It used the production economic-down preparation, the real interest-accrual
payoff bound, installed policies, cost guard and transaction compiler.
The hypothetical approval's 200 bps ceiling existed only inside the simulator;
it was not stored or granted to the worker and does not establish profitability.
Jupiter requests were spaced at least 1.8 seconds apart: two quotes and two
instruction requests completed without rate limits.

| Loan | Debt before (raw token units) | Debt after | Simulation slot | Wire bytes | Compute units |
|---|---:|---:|---:|---:|---:|
| ONyc / USDC | 68,837,864,024 | **0** | 465383903 | 1,790 | 509,447 |
| AUTO / PYUSD | 17,029,136,129 | **0** | 465383918 | 2,012 | 593,646 |

Both resulting requests also passed the compiler, signing and persisted-wire
validation with a newly generated test key. The live delegate's key was never
loaded and no transaction was submitted. Raw evidence is retained in
`/tmp/earn-max-release-preflight/payoff-result.json` and `payoff-run.log`.
The smaller AUTO/wYLDS position still has only the earlier successful partial
repayment simulation; its complete multi-step unwind was not simulated.

The existing worker had restarted at 16:22:21 UTC after an RPC HTTP 502 during
`getSignaturesForAddress`. It was active after the checks at 17:06:14 UTC,
with the same PID 1911247 and restart count 3. Its latest Earn MAX heartbeat
showed normal operation, automatic protection enabled, and the existing
position at target. This pre-existing restart is separate from the candidate.

Before production cutover, repeat the no-pending-work and target checks using
fresh state, and obtain fresh executable quotes without the diagnostic's
burst of parallel requests. Keep the documented stop-old-worker → migrate →
start-new-worker order. Do not reset pending work or force a trade to pass a
check. Stop rollout monitoring at the first failed/held operation or restart.

Not checked here: a complete multi-step unwind to true 1×, live signing or
broadcast, a 30-minute live policy window, or a production migration. The
completed local compiler/sign-validation and migration tests remain separate
evidence in [the loss-control report](../earn-max-loss-controls.md).

Private raw evidence on this VM: `/tmp/earn-max-readonly-preflight/` contains
`result.json`, `result-17-01.json`, probe source, build/run logs, source hashes,
and the post-check worker status. The initial attempt stopped at a stale
one-shot account view; that diagnostic issue was corrected without changing
worker code. Temporary remote files were removed after evidence retrieval.
