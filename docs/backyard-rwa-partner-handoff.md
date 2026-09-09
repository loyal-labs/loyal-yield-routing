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

After the finalized Phase 1 reset and degradation restore, the deployed strategy-two worker observes HXtk, allocates through the approved policies, refreshes and reports NAV, and restores withdrawal liquidity. A user request waits 600 seconds on chain before claim. The worker holds on any unsafe or mismatched observation.

The one-shot repair policy at seed `140` is retired after the repair. Strategy two uses the next four Settings-derived policies at seeds `141–144`; legacy bridge policies at seeds `62–65` remain installed until replacement readback passes and are then retired. The strategy-one policy catalog at seeds `67–136` is not touched by this release; this release installs `141–144` and retires `62–65` only. Any retirement or simplification of that catalog is a separate later step.

## Capability boundary

The current live adaptor is v2; the v3.3 upgrade is a separate finalized gate. The strategy-two config and delegated executor are new keypairs, operator-derived offline; only the worker configuration is generated from the shared seed journal. No optimizer, automatic market switching, caller-selected route, consumer Earn Max behavior, or second money-moving executor is part of this handoff. The single hot key stays this week; key separation is required before any third-party money. The joint session uses OUR canary funds unless key separation is completed first.

## Monitoring and recovery

The integration page is read-only evidence for AUM/NAV, custody, position, route state, deposits, withdrawals, and operation history. M1 holds on a book identity mismatch; M2 on a receipt/armed-NAV mismatch; M3 on custody residue or an unexpected staged amount; M4 when idle does not cover pending quotes; M5 on reserve/oracle/status/emergency-mode failure; M6 on Voltr or adaptor identity drift; M7 on fee accumulator or nonzero performance-fee LP; and M8 on a ticket sequence mismatch or unknown nonzero sequence. Any monitor red or route latch is fail-closed `HOLD`.

For a nonterminal operation, reconcile its persisted wire/signature and finalized protocol/account post-state first. Never blind-resend, never replay a one-shot leg, and never retry by hand. Preserve the journal and use the reset runbook’s same-journal reconcile path after an ambiguous send. Recovery may continue only after the relevant finalized readback and reconciliation pass.

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
