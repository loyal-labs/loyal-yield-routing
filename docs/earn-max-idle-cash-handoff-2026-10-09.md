# Earn MAX idle cash: handoff to Chris, 2026-10-09

Issue: [ASK-2321](https://linear.app/askloyal/issue/ASK-2321). Owner: Chris Cherniakov. Related: ASK-2293 and ASK-2316 (the latter's title concerns PRIME and is not this task).

## Start here

**Draft implementation, not approved for deployment. No funds were signed or submitted, and no production service or configuration was changed during this takeover.** Public read-only account/quote requests and unsigned simulations were performed. The user wants leveraged Earn MAX, prioritizes protecting their ~$100k, and most recently asked to assess investing at most $10k while keeping the remaining cash idle. They now requested this handoff rather than further investigation.

The existing deployed planner only supports debt-free top-ups. An existing AUTO/PYUSD loan reaches `single_loop_position_ready`, leaving new Voltr cash idle. The proposed recurring path is Voltr idle USDC → Squads → USDC/AUTO swap → collateral-only Kamino deposit. These top-up legs preserve principal and neither borrow nor repay; subsequent ordinary leverage operations are separate. Do not replace Earn MAX with an unleveraged product to declare this task solved.

## Code and integration boundary

Repository: `loyal-labs/loyal-yield-routing`, package `go/workers/internal/backyard`. The original workspace is `/home/exedev/dev/loyal-yield-routing-ASK-2316-debt-preserving-topups`. Tested parent: `0f1321f751562304fd034726b5a65a60aa69eaf4`. Original 49-file work and takeover 53-file work are preserved in the evidence bundle, including untracked sources; node_modules is excluded.

The draft PR uses `ask-2321-idle-cash-review-base` at that exact parent and head `ask-2321-idle-cash-handoff`. It intentionally presents the isolated implementation against its tested base. **It is not a PR ready to merge into main.** At handoff, main was `21ae0f47` and includes substantial worker/program refactors. Chris must port/reconcile the reviewed changes onto current main and rerun the relevant checks; do not merge the historical base into main or assume the saved tests validate today's main. Earlier withdrawal/NAV fixes are historical context; some were already merged separately (e.g. PR #298).

Implemented locally, still requiring independent review:

- `decide_debt.go`, `execution_observe.go`, `worker.go`, `selector*.go`: debt-bearing top-up selection, dispatch and priority; withdrawal, risk and unfinished recovery take precedence.
- `topup_loan*.go`, `kamino_release.go`: immutable finalized original debt SF/rate/Clock/borrow marker, collateral and deployed KLend identity; exact protocol-derived interest interval rather than principal rebasing or arbitrary tolerance.
- `topup_tranche.go`, `topup_store.go`, `topup_handoff.go`, `state.go`, `store.go`: receipt-derived custody ownership, durable origins, completed carry and restart recovery; handoff to separately authorized withdrawal/emergency operations.
- `budget_topup_admission.go`, `budget_entry_admission.go`, `budget_redeposit_admission.go`, build/send/store admission: preserve debt and enforce fresh quote, custody, loan, capital and locked authority checks.
- `budget_topup_recovery.go`, `budget_auto_emergency_funding.go`, `partial_risk_authorization.go`, partial repayment/exit pricing: whole-position recovery funding and reservation before allocation; hard-risk owned USDC → AUTO → PYUSD funding, repayment and return within existing independent emergency authority. Returning unused top-up cash alone does not protect the already invested portion.
- Allocation now includes the next swap's conservative cost before taking idle cash. Spending caps, valuation margins, installed permissions and deadlines were not raised to force eligibility.

The inherited patch is broad and interdependent. Passing individual checks is not approval of its entire authority/recovery design. Use the original safety review as background, then the later reports for superseding outcomes.

## Verified checks and exact limits

Evidence paths below are relative to the attached archive root; `takeover/` preserves the original temporary directory names.

| Evidence | Observed result |
| --- | --- |
| `takeover/earn-max-takeover-mlzuudw8/tests.jsonl` | 70 top-level / 222 including subtests passed, zero skipped, fresh disposable Postgres. |
| `takeover/earn-max-takeover-4ewy3xp0/tests.jsonl` | Reserve-derived quote/decimal fixtures: normal $20 completion, interruptions after allocation and collateral, and $100k pre-allocation refusal passed (24.33s). |
| `takeover/earn-max-takeover-7fy4osjv/tests.jsonl` | Race run: recovery/refusal and seven guard tests passed; combined invocation failed because the next-deposit fixture changed cash without its matching book value. |
| `takeover/earn-max-takeover-2rtvux17/tests.jsonl` | Corrected normal/next-deposit case passed under race (71.88s). This does not turn the earlier invocation into an all-green run. |
| `takeover/earn-max-final-vet.log` | `go vet ./internal/backyard` passed; empty output. |
| `takeover/earn-max-budget-review/` | Both focused synthetic DB admission comparisons passed; unsafe counterfactual was rejected as a policy change, not shipped. |
| `takeover/earn-max-10k-chain-kn8qay2y/tests.jsonl` | Scaled $10k recovery cases passed; normal collateral-deposit wire validation failed. |
| `takeover/earn-max-10k-chain-po6y3bmy/tests.jsonl` | Scratch metadata adjustment did not fix normal $10k failure. |
| `takeover/earn-max-10k-preflight/fixture-units.log` | Confirmed scaled fixture unit mismatch: 9 decimals and 30,000,000,000,000 raw vs 1,000,000,000,000 wire ceiling; live AUTO uses 6 decimals. |

The offline chains use the real planner/admission/compiler/locked build-send validators and reopen/release/reacquire DB leases between receipts. RPC, protocol execution and finalized receipts are modeled. Production-delegate envelopes contain synthetic signatures; separate compiled shapes are signed with a deterministic local test key and checked by `BuildResult.validateForDelegate`. Actual production-key `PersistSigned`, full `Worker.Tick`, a full Solana VM lifecycle and funded live execution were not proved. Disposable clusters were stopped and sockets verified; no shared database was used.

**The $10k normal lifecycle is unfinished.** Scaling the small synthetic fixture is not a valid real-token test. The initially suspected blockhash mismatch was not the root cause. Correct the fixture's economic units consistently across reserves, balances and quotes; do not raise the production wire ceiling to force a pass. Scratch overlays/metadata changes are evidence, not fixes to cherry-pick.

## Money and quote findings

All quotes are point-in-time evidence from 2026-10-09, not reusable approval. The public probes reject RPC submission and use zero-signature wires. The installed AUTO policy matched its pinned identity. Reserve prices were stale; the existing unsigned reserve-refresh simulation produced fresh decoded prices without writing on chain.

| Proposed allocation | Quoted output value (USDC) | Input/quote value gap | Slippage allowance | Conservative valuation margin | First-swap conservative bound |
| --- | ---: | ---: | ---: | ---: | ---: |
| $100,000 | $99,959.593956 | $40.406044 | $499.797970 | $1,969.500911 | $2,509.704925 |
| $10,000 | $9,998.973006 | $1.026994 | $49.994865 | $197.009468 | $248.031327 |

Each wire's network-fee quote was 5,000 lamports, additional to the USDC bound; no fee was charged. The table's bound is not money lost or a fee deducted. It includes token -1% and USDC +1% uncertainty margins. AUTO token count differs from USDC value: ~97,654 AUTO was valued near $99,960, not $97,654.

The current $500 gate is cumulative normal-entry conservative execution cost; the actual remaining production allowance was not read. ~$248 for the $10k first swap therefore does not establish full admission or enough budget for all legs. The gate is not a guarantee that market or emergency-exit losses cannot exceed $500.

The $100k unsigned swap simulation reached Jupiter and failed with 6024/InsufficientFunds, consistent with the observed empty Squads source; funds remained in Voltr. The diagnostic Go test exited zero because evidence collection succeeded, not because the swap succeeded. A funded full flow was not simulated. The $10k probe compiled a 940-byte wire but did not repeat the known unfunded swap simulation.

Tighter $100k slippage alone did not resolve the cap: 25/10/0 bps yielded conservative bounds ~$2,264.84 / $2,117.87 / $2,023.08. A separate $90k synthetic counterexample showed that removing valuation margins admits ~$450 instead of rejecting ~$2,223 while the same full-payoff funding and gross exit reservation pass. Those guards do not replace the removed conservative-cost protection. `unsafe_counterfactual.go` is explicitly a rejected experiment. Do not ship it, reset cost history, or split amounts to evade the cumulative gate.

At 17:30:10Z / slots 454942444–445, idle cash was 100,005.156664 USDC and the swap source had zero USDC. A hypothetical $10k allocation leaves 90,005.156664 USDC idle. Existing debt was ~$905.53 against ~$2,096.29 collateral; collateral-only addition was estimated to reduce LTV from ~43.19% to ~7.51%. This is a snapshot estimate; ordinary later borrowing could raise LTV again. Logs at 17:27:57Z independently showed idle 100005156664 raw, LTV 4320 bps and no withdrawal demand. Current production budget/eligibility remain unknown.

Idle Voltr cash is outside the Kamino loan's collateral, but vault LP NAV is pooled. The remaining $90k is not an individually ring-fenced or guaranteed withdrawal amount. Avoid promising zero liquidation/protocol/withdrawal-liquidity risk.

## Bounded next milestones for Chris

1. Review and port the existing patch onto current main, especially loan origin proof, custody ownership, interrupted recovery and independent emergency authority. Preserve the financial guards; separate a risk-policy redesign from a bug fix.
2. Repair the $10k fixture with actual token decimals and coherent prices/balances. Prove normal completion, next-deposit behavior, withdrawal, interrupted recovery, restart/ambiguous submission and hard-risk exit with compiled real wire and persist/sign validation. Keep failures visible.
3. Implement and prove a durable **$10k cumulative canary allocation cap** for the debt-bearing path, covering every allocation path and surviving restart. A $10k per-tranche cap or a watcher alone would allow repeated allocations. The existing flat-start canary requires a flat/unwind-complete position and does not cap this path. Preserve risk handling and withdrawals.
4. Refresh actual position/ownership, remaining accumulated budget, executable entry/exit depth, capacity, oracle/policy/program identity and withdrawal liquidity. Establish the intended post-top-up borrowing policy; the collateral-only LTV snapshot is insufficient for the later leveraged state.
5. Prepare a separately reviewed immutable release/rollback artifact and bounded activation. Any money-moving watcher must stop/report the first held/failed operation, worker exit or restart. Reconcile finalized receipts and balances; do not use the full user balance as a debugging trial.

## Evidence and reproduction

The Linear attachment contains the original 49-file handover and backup, later 53-file snapshot, final changed-source snapshot, historical reports, public quote/RPC responses, unsigned wires, exact diagnostic sources/overlays, test logs and reviewed KLend ProgramData fixture. `SHA256SUMS.json` records each file. No dependencies, PG data directories, production credentials or node_modules are included. Older reports saying “no live reads” or “uncommitted” describe their earlier checkpoint; this document supersedes those status statements.

Read `takeover/earn-max-takeover-decision.md`, `takeover/earn-max-live-test/REPORT.md`, `takeover/earn-max-slippage-test/REPORT.md`, `takeover/earn-max-budget-review/REPORT.md`, then the $10k logs/results. The original `historical/IDLE-CASH-HANDOVER.md` includes earlier blockers, commands and historical incident context. Its local paths are historical, not portable dependencies.

For the offline baseline, run from `go/workers` with Go 1.25.1, `GOTOOLCHAIN=local`, `GOMAXPROCS=2`, `GOMEMLIMIT=1GiB`, the included `programdata.bin` as `BACKYARD_KLEND_PROGRAMDATA_FIXTURE`, and a fresh disposable PostgreSQL cluster:

```sh
go test -p=2 ./internal/backyard -run 'Test(.*Topup|AutoEmergency|DebtClear|LeverageExit)' -count=1 -timeout=5m -json
go vet ./internal/backyard
```

Set `PHASE3_TEST_DATABASE_URL` to that disposable database only; tests require the isolated socket prefix `/private/tmp/backyard-phase3-pg.*`. The saved runners use PostgreSQL 18, initialize auth-local trust/auth-host reject, bind no TCP interface, stop in `finally`, and verify their own socket no longer responds. Rewrite their absolute checkout/artifact paths for your machine before running. Never point these at production or start a Solana validator. Overlay files are standalone diagnostic evidence; their original absolute paths must also be rewritten. Preserve the recorded failed results rather than presenting reproduction scripts as guaranteed passing checks.
