# Backyard RWA vault: adaptor, strategy two, and bridge infrastructure audit

Date: 2026-09-08. Scope: Voltr vault `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`, Loyal custom NAV adaptor `FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW` (v2, report-ticket), the Squads bridge policies (Settings seeds 62–65), the Go worker `go/backyard-rwa-worker`, and the bootstrap of a second Voltr strategy on the same vault. Goal: the same class of accounting failure must not be reachable in production.

Evidence base: mainnet events decoded for every transaction on the HXtk strategy receipt and on other live Voltr vaults; LiteSVM proofs against the deployed Voltr and adaptor binaries (`crates/squads-test-harness/tests/voltr_repair_paths.rs`, `voltr_reset_sequence.rs`; results in `docs/evidence/voltr-repair-litesvm-2026-09-07.results.json` and `docs/evidence/voltr-reset-litesvm-2026-09-08.results.json`); source reads of the adaptor, the worker, the policy compiler, and the Voltr SDK 2.1.1.

Legend: [V] verified on chain or in LiteSVM against deployed binaries, [I] inferred from SDK/IDL/docs, [U] unknown.

## 1. What actually happened, corrected

1. The hole was dug by Loyal's restore flow, on the Voltr binary that was live at the time. The worker staged cash from the Squads USDC ATA into Voltr's strategy custody ATA in a separate transaction, then called `withdraw_strategy`. The adaptor's withdraw path moves no tokens. The old Voltr credited `totalValue` only with cash that entered custody inside the adaptor CPI, so the pre-staged cash was swept to idle uncredited. [V] Sep 2 tx `hnQZ9v9i…` (119 raw) and Sep 4 tx `46UBvSw1…` (requested 1,000,000, staged and swept 3,793,417). Both events show implied credit 0.
2. The worker also mis-reported NAV in the second incident. It projects the post-operation NAV arithmetically for the custody legs (`execution_observe.go`, `bridgeExpectedEffects`): restore NAV = external + Squads + (custody − amount). With custody 3,793,417 and amount 1,000,000 it reported 2,793,417 as strategy value while Voltr swept all of it. That number is exactly the phantom receipt today.
3. Voltr upgraded the vault program at slot 445,223,838 (2026-09-07 about 01:04 UTC, upgrade authority `G2FC…`, Ranger). [V] Both incidents ran on the previous binary; every LiteSVM proof in this repo runs the new one. On the new binary `withdraw_strategy` credits the entire swept custody balance and the vault tracks each strategy's custody balance in the receipt's reserved bytes at offset 128. Rule that fits every observed case on the new binary: `tv' = tv − old + new − amount_in + swept + (custody_after − receipt[128])`, then `receipt[128] := custody_after`. The SDK in `node_modules` predates the upgrade; no changelog was found. Semantics changed underneath us without notice.
4. There is no per-epoch operation limit. [V] Post-upgrade, vault `3maCuTJV…` ran 16 strategy operations inside epoch 1030, `Gj8kURFs…` 15, and the HXtk receipt itself has 1,195 transactions in epoch 1027. `AdaptorEpochInvalid` (6010) fires in LiteSVM only when `Clock.epoch == 0`. The only real gate is the adaptor ticket: one report per slot, because `report.sequence == observed_slot` must exceed `last_consumed_sequence`.
5. The 89,601,150 unminted admin-fee LP (47% of `lpSupplyInclFees`) came from the very first crank on Sep 1, which reported NAV 1,791,997 on a vault with zero LP supply: Voltr minted 5% of the whole NAV as fee LP at par. [V] Never report NAV while LP supply is zero, and never with a performance fee before the high-water mark is calibrated.

## 2. Findings by severity

### Blockers (before strategy two goes live)

- B1. Old worker image can restart and act on strategy one. `render.yaml` pins `backyard-rwa-worker:sha-4f5445ee…`, whose manifest binds config `9hDH…`, ticket `C71B…`, policies 62–65, and stages into the custody ATA `FTDWN5Ay…`. The route lease only prevents concurrent writers. Required: `PolicyRemove` seeds 62–65, rotate the delegated signer so the new policies name a key the old image does not hold, bump the adaptor config version pinned by `execution_observe.go`, and change the lease owner identity.
- B2. Bridge admission is structurally unavailable for the manifest's selected lane (`Maple/syrupUSDC/USDC`): `admitPhase3Bridge` → `phase3BudgetFamilyForLane` returns empty → `bridge_admission_snapshot_unavailable`, so every bridge decision cycles decided → failed. Extend the family table to the lane strategy two will run, or select an OnRe-family lane.
- B3. Voltr `totalValue` and the strategy receipt are never read by the worker. The vault account is not in any pinned address list; reconciliation compares the adaptor's return data (the worker's own number) and three ATA balances. The worker cannot see book ≠ real. See monitors M1–M8.
- B4. Merge hazards: main's `render.yaml` has no worker service; main carries a stray, un-embedded migration `0055_backyard_rwa_worker.sql` that differs from the recovery tree's `0070`; the recovery tree embeds `0070–0075`; images build only on push to main. Delete main's `0055`, merge `0070–0075` verbatim, apply before the new worker starts.

### Must fix before third-party deposits

- U1. Single hot key. `BAqgbERm…` (the `SOLANA_TESTING_PK` setup admin) is simultaneously the Squads Settings sole signer (threshold 1, mask 7), the policy install authority, the Voltr vault admin, and the adaptor program upgrade authority (ProgramData `Drvzixa…`, last deploy slot 443,528,877). [V] One leaked key drains the Squads vault, redirects custody flows via an adaptor upgrade, or reassigns the vault manager. Move the upgrade authority and the vault admin to separate cold keys or a multisig now. Note the adaptor coupling in U2.
- U2. The adaptor requires the Squads Settings to have exactly one signer with mask 7 and threshold 1 (`processor.rs`, `valid_settings_authority_graph`). Adding a second Squads member, or a timelock, bricks every adaptor path (fail-closed, but a full halt). v3 must relax this to "expected signer present with mask 7" before Squads governance can be hardened.
- U3. NAV is unconstrained on chain. The report value at capital-data offset 51 (arm-data offset 39) is deliberately unconstrained by policies 62/63/65, and the adaptor bounds it only by `max_report_nav_raw = 2e12` (twice the vault cap). A compromised delegated key can move the share price at will. LiteSVM T10: with `lockedProfitDegradationDuration = 0` an immediate depositor captures its pro-rata share of any over-report (49,999 of 100,000); with 86,400 an immediate exit captures nothing but a depositor who waits 24 hours captures it; under-reports hit instantly in both cases. Required: (a) policy data constraint `U64Le ≤ vault cap` at offsets 39 and 51 in all report-bearing policies, re-measured for packet fit; (b) in the adaptor, pass the strategy receipt read-only and enforce `|nav_after − receipt.positionValue| ≤ amount + max(bps × receipt, floor)` per report; (c) keep `lockedProfitDegradationDuration ≥ 86,400` and `withdrawalWaitingPeriod = 600` in production; (d) performance fee 0 until the high-water mark is calibrated with real depositors.
- U4. Liveness trap. A report whose transaction lands after `observed_slot + 32` fails on chain with adaptor error 9 (`ReportSlot`); `lifecycle.go` maps any failed transaction to `manual_recovery("confirmed_transaction_error")`, and `UnresolvedCapitalRecoverySQL` then blocks all execution until an operator edits the row. A failed Solana transaction moves nothing. Classify adaptor errors 9 and 18 as retryable, and refuse to send when the confirmed slot is past `observed_slot + 32 − margin` instead of relying on the coincidentally equal budget window.
- U5. Stale valuation. `validateKaminoRefresh` only requires `refreshedSlot > 0`; no maximum age on reserve or oracle updates; reserve `config.status` and the lending market's emergency flag are never decoded. A paused market keeps reporting the last price. Enforce `slot − reserve.lastUpdate.slot ≤ 32`, an oracle age bound, and status 0 for every report-bearing action; HOLD otherwise.
- U6. Fee artifact. The 89.6M fee LP must be harvested and drained (reset steps R2/R3) before any depositor exists; otherwise the admin owns 47% of every deposit's share value.
- U7. Amount caps. On-chain policy cap is 1,000,000 USDC per execution while the software cap is 1 USDC. Compile strategy-two policies with an operational cap (100–1,000 USDC) and roll seeds when raising it.

### Should fix

- S1. Unexplained-drift HOLD is missing. In today's state the worker would livelock (decided → built → simulation error → new epoch, every ~10 s). Add `|StrategyNAV − PriorReported| > max(bps × PriorReported, floor)` with no reconciled mutation → `HoldManualRecovery("nav_drift_unexplained")`. This would have stopped the incident before the first bad report.
- S2. Report churn: `CapitalMutated` is true on every slot a Kamino reserve refreshes, so with an open position the loop reports as fast as the pipeline allows. Use a bps tolerance plus age.
- S3. Deposit path leaves residue. The adaptor moves exactly `amount` out of custody; any dust stays. On the new binary dust is credited into `totalValue` at the next operation and can trigger a performance fee; on the old it was lost. v3: the deposit/refresh path moves the entire custody balance to Squads and the observed NAV counts it.
- S4. TS verifier expectations pin the Voltr program at deploy slot 433,299,444; every bootstrap gate that calls `verifyDeploymentIdentities` now fails and must be re-proven deliberately, not bumped blindly.
- S5. `A/README.md` claims arm and consume must be in one transaction; the adaptor only enforces slot age. Fix the doc.

## 3. The version-invariant design rule

Define E as value outside Voltr custody (Squads USDC, holding ATA, Kamino net position), C as the strategy custody ATA balance, I as idle, R as the receipt. On the new binary Voltr itself books C, so the adaptor's NAV must exclude C or it is double counted; on the old binary C was unbooked. The rule that is correct under both:

1. Custody is exactly zero at every instruction boundary. Deposit and refresh paths move the entire custody balance out to Squads inside the CPI; the withdraw path requires C == 0 at entry.
2. Cash for a withdrawal enters custody inside the adaptor CPI, in the exact requested amount, pulled from a holding ATA owned by an adaptor PDA. The Squads vault is not a signer inside the CPI, so a holding account is required. This is the shape of Voltr's own Kamino adaptor and of Voltr's Trustful adaptor.
3. NAV is always the independently observed external value E, never `receipt ± amount`. With over-staging on the new binary, `receipt − amount` overstates the book (LiteSVM T8: dangerous direction); observed E keeps book == real in every tested case.
4. Fail closed on anything else: custody not zero, holding short, report older than 32 slots, reserve stale or paused, program identity changed.

## 4. Two implementation options for the custom adaptor

Option A, minimal, current binary only. Keep the deployed adaptor bytes or add an equality upgrade (`custody == amount` at withdraw entry, `custody == 0` at refresh, custody moved fully on deposit). Worker: stage exactly `amount` into custody, never carry custody into the armed NAV, restore amount equals the journaled staged amount, monitors M1–M8, U4 fixes. Policies 62–65 unchanged unless the NAV cap is added now. Estimate 2–3 days, plus 1 day if the adaptor equality upgrade ships. Residual risk: correctness depends on Voltr continuing to credit swept custody; a future change is detected by M1 after one restore, with damage bounded by one restore leg. Acceptable only with the monitor live and small caps.

Option B, robust, adaptor v3. Withdraw path pulls `amount` from `ATA(holding_auth, mint)` with `holding_auth = PDA(["withdrawal_holding", config])` via `invoke_signed` inside the CPI after asserting custody == 0; deposit path asserts custody == amount at entry and moves everything out; refresh asserts custody == 0; accounts 9 → 11; `CONFIG_VERSION` 3 with no layout change; receipt-relative NAV step bound (U3b); relaxed settings-graph check (U2). Worker: stage destination becomes the holding ATA, holding balance is part of E, custody must be 0 at rest. Policies: four new at seeds ≥ 140 with the NAV cap, operational amount cap, and a new delegated signer; retire 62–65. Estimate 5–8 days including proofs and canary. Residual risk: the NAV value itself (bounded) and key custody (U1).

Recommendation: A's worker rules and monitors first, because every later step needs them to be observable; B before opening deposits to third parties or raising caps above canary size. A alone is acceptable for this week's canary on the repaired vault.

## 5. Fail-closed monitor set

Evaluate on one confirmed account batch before `Decide`, and as post-conditions in reconciliation. Any failure → `HoldManualRecovery`.

- M1. `tv == idle + custody2 + receipt2`. The orphaned strategy-one receipt is excluded because its entire value is the parked phantom; assert `receipt1 == 3,793,536` and never changes.
- M2. `receipt2 == last armed NAV` and `|observed E − receipt2| ≤ tolerance` unless a reconciled mutation explains it.
- M3. `custody2 == 0` at rest; at withdraw build time `holding == amount` (B) or `custody == amount` (A).
- M4. `idle ≥ Σ pending request quotes` before their deadlines. Nothing on chain reserves idle for pending claims.
- M5. Reserve status 0, `slot − reserve.lastUpdate.slot ≤ 32`, oracle age bound, market not in emergency mode.
- M6. Voltr and adaptor ProgramData `(slot, sha256)` equal the manifest-pinned values; on change, HOLD until the LiteSVM matrix is re-run against the new dump. This is the defense against semantics changing under us.
- M7. `feeState.accumulatedLp*` bounded relative to LP supply; performance fee 0 until calibrated.
- M8. Single reporter: ticket `last_consumed_sequence` equals the journal's last reconciled sequence; an out-of-band crank halts the worker.

## 6. Voltr facts that constrain operations (all [V] unless marked)

- Books: `tv' = tv + new − old − amount` on deposit; on the new binary withdraw credits swept custody and tracks custody at receipt offset 128. `D = tv − idle − Σreceipts` is conserved by every instruction, so the 119/3,793,536 gap can only be parked, never removed.
- Underflow gate: `tv + new ≥ old` (nav 118 fails, 119 passes). Any report on strategy one below 3,793,536 either fails (6004) or silently writes tv down; equal is a no-op. Remove its policies.
- `closeStrategy` requires `positionValue == 0`; `removeAdaptor` and `updateVaultAdaptorPolicy` never touch books; `updateVaultAdaptorPolicy` needs Voltr's protocol admin.
- `harvestFee` is permissionless. `cancelRequestWithdrawVault` refunds LP worth the frozen quote at the post-burn price and burns the rest. Claims pay `min(frozen quote, current unlocked quote)` and burn all escrowed LP. Deposits price on full `tv`; requests quote on unlocked `tv`.
- Locked profit: `lastUpdatedLockedProfit` is set by profit reports and decays over `lockedProfitDegradationDuration` from `lastReport`. LiteSVM T10.7: re-enabling 86,400 within a day of the repair report resurrects its 1,000,119 locked profit and haircuts early exits by about 49%. Re-enable at least 24 hours after the repair, before opening to users.
- Instant and direct withdraw paths are structurally closed on HXtk: `instantWithdrawVault` needs waiting period 0 (it is 600), and `instant/directWithdrawStrategy` need an admin-created `DirectWithdrawInitReceipt` that does not exist. Never create it; keep the waiting period non-zero. `DisabledOperations` bit mapping is [U]; leave at 0.
- `initializeStrategy`: `strategy` is an opaque key (the adaptor config account for FSj27), receipt PDA `["strategy_init_receipt", vault, strategy]`, auth PDA `["vault_strategy_auth", vault, strategy]`, custody ATA created permissionlessly. No per-vault strategy limit; the per-(vault, program) adaptor receipt is reused, so strategy two needs no `addAdaptor` and no wait. Never `removeAdaptor` and re-add: a fresh receipt may be stamped with the current epoch (43 of 266 receipts on chain are; the selection rule is [U]).
- Admin config fields (`updateVaultConfig`): 0 MaxCap, 1 StartAtTs, 2 LockedProfitDegradationDuration, 3 WithdrawalWaitingPeriod, 4/5 manager/admin performance fee (u16 bps), 6/7 management fees, 8 redemption, 9 issuance, 10 Manager, 11 PendingAdmin, 12 DisabledOperations. `maxCap := tv` is the well-defined deposit pause.

## 7. Verified reset sequence for HXtk (LiteSVM, 17/17 PASS, commit on `proof/voltr-reset-litesvm`)

1. Admin `updateVaultConfig`: LockedProfitDegradationDuration = 0, AdminPerformanceFee = 0.
2. Repair report through the existing REPORT_NAV policy (seed 63): `arm_report(op 0, amount 0, nav = idle + receipt1 − tv, recomputed at execution; today 3,793,536)` + `deposit_strategy(0)`. Result: tv == idle == 3,793,417, receipt1 3,793,536, no fee LP, locked profit 0. The worker will not build this (its NAV is 0); hand-build the wire.
3. `harvestFee` (any signer) after creating the manager, admin, and treasury LP ATAs idempotently: admin LP ATA +89,601,150.
4. `cancelRequestWithdrawVault` (refunds 78,196,265 LP, burns 21,745,257), `requestWithdrawVault(all, isWithdrawAll)`, wait ≥ 600 s, `withdrawVault`: payout 3,793,394; residual tv == idle == 23 with the 1,000 dead-weight LP. Claiming the existing request as-is would pay only 1,767,782 (its frozen quote).
5. Strategy two on the same adaptor: `initialize_config` (new keypair, vault index 0, max NAV 1e12 recommended, max age 32), `initialize_report_ticket`, Voltr `initializeStrategy` with manager signer (atomic admin transaction: Manager = BAqg → initializeStrategy → Manager = ST999, the partner-vault pattern; or a Squads proposal), custody ATA for the new `vault_strategy_auth`; under Option B also the holding ATA.
6. Four policies at seeds ≥ 140 (NAV cap, operational amount cap, new delegated signer), readback, worker manifest and constants, new image; then `PolicyRemove` 62–65.
7. Fresh-user lifecycle canary (1,000,000 in, 999,999 out, verified in LiteSVM R4/R6), allocate and restore cycles on strategy two (tv unchanged, book == real, verified R5a).
8. At least 24 hours after step 2: `lockedProfitDegradationDuration = 86,400` again; performance fee stays 0 at launch.

## 8. Go/no-go checklist before third-party money

- [ ] B1–B4 closed; old image cannot act (policies 62–65 removed, signer rotated, config version bumped).
- [ ] Monitors M1–M8 live and proven to HOLD on injected faults in LiteSVM.
- [ ] U3 NAV cap in policies and adaptor step bound; degradation 86,400; waiting period 600; fees 0.
- [ ] U1 keys separated: adaptor upgrade authority and vault admin off the hot key; adaptor settings-graph check relaxed (U2) if Squads governance changes.
- [ ] U4/U5 worker fixes merged; S1 drift HOLD merged.
- [ ] Fee LP harvested and drained (U6); LP supply equals dead weight or Loyal seed only.
- [ ] Option B shipped, or Option A running with caps ≤ canary size and an explicit date for B.
- [ ] Re-proof procedure documented: on any Voltr or adaptor ProgramData change, re-dump and re-run the LiteSVM matrix before resuming.
