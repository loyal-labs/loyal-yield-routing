# Backyard RWA vault partner handoff

## Canonical identities

- Voltr vault: `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`
- USDC mint: `EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v`
- Voltr program: `vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8`; upgraded at slot `445,223,838`; executable SHA-256 `bf1c1831b3d6350f4340badb942bd2e7bfaca4aa89276cb65e8480aa30d44c56`
- Loyal adaptor: `FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW`; v2 is live; v3.3 executable SHA-256 `836ded9ff4e79cda9fafbafcffcf9f2e9f395c762c69ba5af4630e82a9d8a4d0` after the upgrade
- Squads Settings: `5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6`
- Squads vault: `ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh`
- Delegated executor: `TBD — filled in after strategy-two bootstrap and delegated-signer readback (operator checklist step 3)`
- Strategy-two config, report ticket, receipt, and custody ATA: `TBD — filled in after strategy-two bootstrap and finalized account readback (operator checklist step 3)`
- Strategy-one receipt: `3,793,536`; frozen and orphaned
- Go worker: Render service `loyal-backyard-rwa-worker`; image `backyard-rwa-worker:sha-<commit>`
- Worker image tag: `TBD — filled in after the backyard-rwa-worker-image workflow publishes the immutable image (operator checklist step 5)`

## Operating flow

Deposited USDC first becomes Voltr idle. After the finalized Phase 1 reset and degradation restore, the bound adaptor moves allocatable capital to the exact Squads USDC account, and the deployed strategy-two worker observes HXtk, refreshes and reports NAV, and restores withdrawal liquidity. The serialized Go worker owns the two enabled routes: fixed `PRIME/USDC` and the Phase 2 representative `Maple/syrupUSDC/USDC`. It swaps USDC to the selected collateral, deposits it into Kamino, and attempts leverage only while confirmed reserve and risk limits allow it. Every other catalogued lane fails closed. If capacity or risk blocks a safe action, the worker records a typed durable `HOLD` and sends no risk-increasing transaction.

A withdrawal request immediately stops risk increases. The worker unwinds the required budget, swaps back when necessary, reports NAV through the atomic one-use adaptor ticket, and restores Voltr idle. The user can claim after the onchain 600-second wait.

The one-shot repair policy at seed `140` is retired after the repair. Strategy two uses the next four Settings-derived policies at seeds `141–144`; legacy bridge policies at seeds `62–65` remain installed until replacement readback passes and are then retired. The strategy-one policy catalog at seeds `67–136` is not touched by this release; this release installs `141–144` and retires `62–65` only. Any retirement or simplification of that catalog is a separate later step.

## Capability boundary

The current live adaptor is v2; the v3.3 upgrade is a separate finalized gate. The strategy-two config and delegated executor are new keypairs, operator-derived offline; only the worker configuration is generated from the shared seed journal. There is no optimizer, automatic market switching, caller-selected route, registry, pre-hook, post-hook, consumer Earn Max behavior, or second money-moving executor in this release. The single hot key stays this week; key separation is required before any third-party money. The joint session uses OUR canary funds unless key separation is completed first.

## Monitoring and recovery

The integration page is read-only evidence for AUM/NAV, custody, position, route state, deposits, withdrawals, and operation history. M1 holds on a book identity mismatch; M2 on a receipt/armed-NAV mismatch; M3 on custody residue or an unexpected staged amount; M4 when idle does not cover pending quotes; M5 on reserve/oracle/status/emergency-mode failure; M6 on Voltr or adaptor identity drift; M7 on unapproved fee terms or invalid LP accounting; and M8 on a ticket sequence mismatch or unknown nonzero sequence. Any monitor red or route latch is fail-closed `HOLD`.

For a nonterminal operation, reconcile its persisted wire/signature and finalized protocol/account post-state first. Never blind-resend, never replay a one-shot leg, and never retry by hand. Preserve the journal and use the reset runbook’s same-journal reconcile path after an ambiguous send. Recovery may continue only after the relevant finalized readback and reconciliation pass.

## Worker fee policy (2026-10-01 support change)

The approved fee tuple is admin performance 2000 bps (20%) and all seven other
terms zero. Different terms cause manual recovery. Checked LP arithmetic
rejects overflow before a money action. Gross strategy NAV remains assets minus
debt; effective supply includes unharvested fee LP. Stored withdrawal-request
ceilings remain unchanged.

Accumulated fee LP above 1% of effective supply warns at most once per 30
minutes. It does not stop NAV reports or withdrawal unwinds. Automatic
harvesting and on-chain HWM resets remain outside this change.

Routine NAV reports run hourly. At the recorded pilot book's LP precision,
minute-by-minute reports can round fees above the gain being reported. The
pinned-program local proof reproduces this: ten 260-raw gains accrue eight LP,
versus two LP for one 2,600-raw gain. One 15,600-raw hourly gain accrues nine LP,
leaving about 78.6% to existing holders. These are synthetic gain scenarios,
not a claim about live losses or a guaranteed effective fee.

Withdrawal reports retain their one-minute freshness rule. Reconciled capital
changes retain their existing reporting fallback; required post-transaction
reports and hard-LTV repayment keep priority. The worker still observes risk
at its existing poll cadence. The stored report timestamp stays exact; normal
NAV freshness follows the hourly policy and never covers an unreported capital
mutation. Operator canary entries still require a report younger than one minute.

Economic entries, rotations and leverage increases require a coherent book
HWM baseline. Unknown inputs or historical profit overhang hold economic moves
and clear advantage windows, without adding a global NAV/withdrawal latch.
Candidate forecasts reserve fees on the modeled NAV rise to its peak, plus
pending profit, repeated dilution and rounding. KEEP receives an upper return
bound using one terminal fee without rounding; it is not treated as fee-free.
Both paths assume terminal NAV settlement, fixed modeled rates, unchanged
holders and no later capital flows, safety interventions or foreign cranks.

The rounding budget counts hourly reports plus the bounded source and
destination recipes (at most 32 steps each), and initial/final reports—not
worker polls or selector wakeups. Minimum benefit, movement costs, uncertainty
and transaction spending limits remain in force. Low-margin moves can still
fail those checks; hourly reporting does not waive costs.

The shared displayed APY remains a continuous fee-paying estimate:
`expm1(0.8 * log1p(apy))` for positive carry, with zero and losses unchanged.
It is not a cadence-specific realized return. The historical zero-fee canary
below records its original configuration, not today's approved terms.

Deployment, latch clearance and monitoring restart require separate approval.

## Joint test plan with Backyard Finance

### Preconditions we complete

Complete these in operator-checklist order, with the contract-required timing correction: checklist step 7 (finalized degradation restore at least 24 h after the repair signature) must complete before the canary in step 6 or any joint session begins.

1. Step 0: suspend Render and capture `phase0-render-suspend.json`.
2. Step 1: complete the Phase 1 reset and finalized post-conditions.
3. Step 2: finalize adaptor v3.3 and its M6 readback.
4. Step 3: bootstrap strategy two and install policies at `141–144`.
5. Step 4: cut over, then retire `62–65` at finalized commitment.
6. Step 5: deploy the immutable worker image and pass its startup gates.
7. Step 7: complete the finalized degradation restore, at least 24 h after the repair signature and before any tester or third-party deposit.
8. Step 6: run our own 1 USDC canary with M1–M8 green before the session.

### What Backyard does during the session

From the deployed strategy-two wallet/client, use the Voltr SDK user deposit instruction for HXtk to deposit 1 USDC, watch worker allocation, request the withdrawal, wait 600 seconds from the finalized request transaction, and claim. The repository intentionally has no HXtk user-signer shell command; the client must provide the user signer and record finalized signatures.

Backyard observes the finalized deposit, allocation, refresh/restore, request, and claim signatures and slots on chain; decoded `tv`, idle, strategy-two receipt and custody, LP/USDC deltas, ticket sequence, and M1–M8 verdicts; and the corresponding AUM/NAV, custody, position, route, deposit, withdrawal, and operation-history state on the integration page. Pass requires the canary post-conditions verbatim: `tv == pre-canary tv + 1,000,000 − payoutRaw`, `custody2 == 0`, `receipt2 == 0`, receipt1 unchanged at `3,793,536`, and no performance-fee LP.

### Evidence and confirmations

We hand over `phase0-render-suspend.json`, `phase1-postconditions.json`, `phase2-canary.json`, the seed journal, and the worker config from `docs/evidence/backyard-rwa-strategy2/` and `docs/evidence/hxtk-strategy2-2026-09-08/` as applicable. Before the session, please confirm: can your client sign a Voltr SDK user deposit/request/claim for HXtk; do you have a funded test wallet; who is the contact for the test window; and does your integration page point at strategy two?

The single hot key stays this week and key separation is required before any third-party money. The joint session therefore uses OUR canary funds unless key separation is done first.

Abort on any monitor red, any `HOLD`, or any mismatch between the book and chain. Stop the session, preserve evidence, reconcile through the runbook, and retry nothing by hand.
