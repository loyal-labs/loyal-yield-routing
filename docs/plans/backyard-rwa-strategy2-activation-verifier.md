# Backyard RWA: strategy-two activation verifier-first contract

Status: active acceptance contract v1 (2026-09-08). Derived from
`docs/plans/backyard-rwa-adaptor-strategy2-audit-2026-09-08.md` (§2, §3, §5–§8),
`docs/evidence/voltr-reset-litesvm-2026-09-08.results.json` (17/17 PASS, dump
slot 445,235,325) and `docs/evidence/voltr-repair-litesvm-2026-09-07.results.json`
(T1 PASS, deficit 119). This file alone defines done for Phases 0–3. Scope:
vault `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`, adaptor
`FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW` (v2), worker
`go/backyard-rwa-worker`, Squads bridge policies, one new Voltr strategy.
Mainnet is read-only for proof; every signed step in Phases 1–2 is
operator-executed. Nothing here authorizes broadcasting.

## How to verify

```sh
# LiteSVM matrix (clones deployed mainnet programs + fixtures; --ignored is required)
CARGO_TARGET_DIR=/Users/user/loyal/loyal-yield-routing/.phase3-recovery/target \
  cargo test -p squads-test-harness --test voltr_repair_paths --test voltr_reset_sequence \
  -- --ignored --nocapture

# Squads one-shot repair policy path (PolicyCreate -> ExecuteSync -> PolicyRemove)
CARGO_TARGET_DIR=/Users/user/loyal/loyal-yield-routing/.phase3-recovery/target \
  cargo test -p squads-test-harness --test voltr_repair_paths voltr_squads_repair_policy \
  -- --ignored --nocapture

# Adaptor v3 LiteSVM matrix (cases V1-V11, bootstrap milestones M0-M3)
CARGO_TARGET_DIR=/Users/user/loyal/loyal-yield-routing/.phase3-recovery/target \
  cargo test -p squads-test-harness --test voltr_adaptor_v3_matrix \
  -- --ignored --nocapture

# Re-dump mainnet fixtures read-only (writes crates/squads-test-harness/fixtures/voltr-repair/)
python3 scripts/fetch_voltr_repair_accounts.py

# On-chain decode: Voltr/adaptor ProgramData deploy slots, vault config, book identity
python3 scripts/voltr_deploy_check.py
python3 scripts/voltr_withdraw_forensics.py   # receipt + withdraw-window forensics, read-only

# Worker rules and monitors
cd go/backyard-rwa-worker && go build ./... && go test ./...

# Operator tool only (not a verifier): clear a reviewed durable route hold
cd go/backyard-rwa-worker && go run ./cmd/backyard-rwa-worker clear-hold --route <route-id> --reason "<text>"

# Adaptor v3 / strategy-two bootstrap tooling typecheck
cd tools/backyard-voltr && bun run check

# Canonical adaptor v3.3 build, capacity and hash gate
bun run build:adaptor
```

Fixture slot lives in `crates/squads-test-harness/fixtures/voltr-repair/_manifest.json`
and as `dumpSlot` in each results JSON. `voltr_reset_sequence` runs steps R0–R8
(R5a/R6 hazard probes on discarded clones); `voltr_repair_paths` runs cases T0–T6.

## Phase 0 — freeze, suspend, re-proof

### P0.1 — old worker cannot act
Requirement: Render service `srv-dabkt0ojo6nc7381o9fg` (`loyal-backyard-rwa-worker`,
image `backyard-rwa-worker:sha-4f5445ee068f577b4eec0cf8b931ac421db60c2b`) is
suspended with `autoDeploy: false` before any Phase 1 signature. It binds config
`9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj`, ticket `C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5`,
policies 62–65 and custody `FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M` (audit B1).
Pass: Render shows the service suspended, the pinned image unchanged, and no new
worker-journal rows after suspension.
Check: Render dashboard/API state capture + journal query.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase0-render-suspend.json`.

### P0.2 — live re-proof at a fresh slot
Requirement: fixtures re-dumped at a slot ≥ 445,250,000, then both LiteSVM tests
PASS against the re-dumped fixtures.
Pass: `_manifest.json` slot ≥ 445,250,000; `voltr_repair_paths` T0–T6 and
`voltr_reset_sequence` R0–R8 all report PASS.
Check: `fetch_voltr_repair_accounts.py` then the cargo test line above.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase0-reproof-<slot>.json`.

### P0.3 — Voltr program identity unchanged
Requirement: Voltr ProgramData deploy slot is still 445,223,838 and the adaptor
ProgramData deploy slot is still 443,528,877. Any change → HOLD and full re-proof
(see `SR-1`); never bump an assertion blindly (audit S4).
Pass: script reports both slots equal to the pinned values, and the TS
`verifyDeploymentIdentities` gate (previously pinned at 433,299,444) is re-proven
deliberately against the new value, not skipped.
Check: `python3 scripts/voltr_deploy_check.py`.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase0-deploy-check.json`.

## Phase 1 — mainnet reset (operator-signed, in this order)

### P1.1 — zero the fee and decay knobs
Requirement: vault admin `updateVaultConfig`: `LockedProfitDegradationDuration = 0`
(field 2), `AdminPerformanceFee = 0` (field 5). `WithdrawalWaitingPeriod` stays
600 and `DisabledOperations` stays 0.
Pass: on-chain config reads 0/0 with waiting period 600.
Check: `scripts/voltr_deploy_check.py` decode; record tx signature.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase1-config.json`.

### P1.2 — repair report through the seed-63 policy
Requirement: hand-built wire (the worker will not build this — its projected NAV
is 0): `arm_report(op 0, amount 0, nav = idle + receipt1 − tv)` + `deposit_strategy(0)`,
with nav recomputed from a fresh read immediately before send. The NAV value sits
at capital-data offset 51 / arm-data offset 39 and is unconstrained by policy 63,
which is why the wire must be hand-built (audit §7.2).
Pass: `tv == idle == 3,793,417`, `receipt1 == 3,793,536`, no fee LP minted,
locked profit 0.
Check: `scripts/voltr_deploy_check.py` after send; LiteSVM T1 is the rehearsal.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase1-repair-report.json`.

### P1.3 — harvest the fee artifact
Requirement: create manager/admin/treasury LP ATAs idempotently, then `harvestFee`
(permissionless). Moves the 89,601,150 unminted admin-fee LP into the admin LP ATA
(audit U6).
Pass: `feeState.accumulatedLpAdminFees == 0` and admin LP ATA balance == 89,601,150.
Check: decode `feeState` + admin LP ATA after the call.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase1-harvest.json`.

### P1.4 — cancel the stale request and re-request everything
Requirement: `cancelRequestWithdrawVault` on the pending request (refunds
78,196,265 LP, burns 21,745,257), then `requestWithdrawVault(all, isWithdrawAll)`.
Claiming the existing request as-is would pay only 1,767,782 (frozen quote).
Pass: escrow empty after cancel; one new request covering all LP.
Check: decode request/escrow accounts.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase1-request.json`.

### P1.5 — claim after the waiting period
Requirement: wait ≥ 600 s, then `withdrawVault`.
Pass: payout 3,793,394; residual `tv == idle == 23` with the 1,000 dead-weight LP.
Check: idle ATA, tv, LP supply decode.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase1-claim.json`.

### P1.6 — reset post-conditions, decoded on chain
Requirement: all of these hold at one slot after P1.5: `tv == idle`;
`receipt1 == 3,793,536` and unchanged; `feeState.accumulatedLp{Manager,Admin,Protocol}Fees == 0`;
LP supply == 1,000 dead weight (or Loyal seed only); strategy-one custody == 0;
`D = tv − idle − Σreceipts == 3,793,536` (conserved, never removed).
Pass: every value matches; any mismatch → HOLD, no strategy-two work.
Check: `scripts/voltr_deploy_check.py` + `scripts/voltr_withdraw_forensics.py`.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase1-postconditions.json`.

### P1.7 — calendar rule for re-enabling degradation
Requirement: `LockedProfitDegradationDuration = 86,400` is set back at least 24 h
after P1.2 and before any tester or third-party deposit. Re-enabling sooner
resurrects the repair report's 1,000,119 locked profit and haircuts early exits
~49% (LiteSVM T10.7).
Pass: config decode shows 86,400 with a timestamp ≥ 24 h after the P1.2
signature; performance fees remain 0.
Check: config decode + recorded signatures.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase1-degradation.json`.

## Phase 2 — worker rules, monitors, bootstrap, policies, canary

### P2.1 — NAV is observed external value, never receipt ± amount
Requirement: strategy NAV = observed external E (Squads USDC + holding ATA +
Kamino net position), excluding custody. Delete the `bridgeExpectedEffects`
custody projection (`execution_observe.go`) that produced the phantom.
Pass: named test proves custody is absent from reported NAV and that over-staging
does not raise the book.
Check: `go test ./... -run TestObservedExternalNAVExcludesCustodyBalance`.
Evidence: test output in the PR; audit §3 rule 3, LiteSVM T8.

### P2.2 — stage exactly, restore exactly
Requirement: the staged amount is journaled before send; restore moves exactly the
journaled staged amount, and stage stages exactly that amount — no projection, no
rounding drift.
Pass: named test fails on any restore amount ≠ journaled staged amount.
Check: `go test ./... -run TestRestoreAmountEqualsJournaledStagedAmount`.
Evidence: test output; audit §1.1 (staged-and-swept 3,793,417 vs requested 1,000,000).

### P2.3 — monitors M1–M8, drift HOLD, churn tolerance
Requirement: each monitor is evaluated on one confirmed account batch before
`Decide` and as a reconciliation post-condition; any failure →
`HoldManualRecovery`. M1 `tv == idle + custody2 + receipt2` (receipt1 excluded,
asserted == 3,793,536); M2 `receipt2 == last armed NAV`, `|E − receipt2| ≤
tolerance` absent a reconciled mutation; M3 `custody2 == 0` at rest, `custody ==
amount` (Option A) at withdraw build; M4 `idle ≥ Σ pending quotes`; M5 = P2.5;
M6 = `SR-1`; M7 `accumulatedLp*` bounded, fees 0; M8 ticket
`last_consumed_sequence` == journal's last reconciled sequence. Plus audit S1
drift HOLD and S2 churn tolerance (bps + age).
Pass: every row below has a named passing test that fails when the fault is
injected.

| Monitor / rule | Named Go test |
| --- | --- |
| M1 book identity | `TestMonitorBookIdentityHoldsOnVaultMismatch` |
| M2 receipt vs armed NAV | `TestMonitorReceiptMatchesArmedNAVAndObservedExternal` |
| M3 custody at rest / build | `TestMonitorCustodyZeroAtRestAndStageAmountPresent` |
| M4 idle covers pending quotes | `TestMonitorIdleCoversPendingRequestQuotes` |
| M5 reserve health | `TestKaminoRefreshGateRejectsStaleOrPausedReserve` (P2.5) |
| M6 program identity pin | `TestMonitorProgramDataPinHoldsOnChange` |
| M7 fee accumulators | `TestMonitorFeeAccumulatorsBoundedAndFeesZero` |
| M8 single reporter | `TestMonitorSingleReporterSequenceMismatchHolds` |
| S1 unexplained drift | `TestUnexplainedNavDriftHoldsWithoutReconciledMutation` |
| S2 report churn | `TestReportChurnToleranceSuppressesUnchangedValuation` |

Check: `go test ./... -run 'TestMonitor|TestUnexplainedNavDrift|TestReportChurn'`.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase2-monitors.json`.

### P2.4 — failed-transaction classification and the send fence
Requirement: adaptor errors 9 (`ReportSlot`) and 18 are retryable, not
`manual_recovery("confirmed_transaction_error")` from `lifecycle.go`; and the
worker refuses to send when the confirmed slot is past `observed_slot + 28`
(margin 4 inside the on-chain 32-slot limit). A failed transaction moves nothing.
Pass: named test proves both, and that `UnresolvedCapitalRecoverySQL` is not
entered for those errors.
Check: `go test ./... -run TestAdaptorErrors9And18AreRetryableAndLateSendRefused`.
Evidence: test output; audit U4.

### P2.5 — Kamino staleness and status gates
Requirement: for every report-bearing action, `slot − reserve.lastUpdate.slot ≤ 32`,
an oracle age bound holds, reserve `config.status == 0`, and the lending market is
not in emergency mode. `refreshedSlot > 0` alone is not a gate. HOLD otherwise.
Pass: named test rejects a stale reserve, a paused reserve, a stale oracle and an
emergency market, and passes a fresh healthy one.
Check: `go test ./... -run TestKaminoRefreshGateRejectsStaleOrPausedReserve`.
Evidence: test output; audit U5.

### P2.6 — lane admission for the selected lane
Requirement: the lane strategy two runs must map to a non-empty
`phase3BudgetFamilyForLane` entry, so `admitPhase3Bridge` cannot cycle
decided → failed on `bridge_admission_snapshot_unavailable` (audit B2). Either
extend the family table to the selected lane or select an OnRe-family lane.
Pass: named test proves admission for the manifest's selected lane and returns
`unavailable` for a genuinely unknown lane.
Check: `go test ./... -run TestSelectedLaneHasPhase3BridgeFamily`.
Evidence: test output + manifest lane in the PR description.

### P2.7 — strategy two bootstrapped on the current adaptor
Requirement: `initialize_config` (fresh keypair, vault index 0, `maxReportNavRaw`
1e12, `maxReportAgeSlots` 32), `initialize_report_ticket`, Voltr `initializeStrategy`
(manager signer; atomic admin transaction Manager `BAqg…` → initializeStrategy →
Manager `ST999…`, or a Squads proposal), custody ATA for the new
`vault_strategy_auth`. No `addAdaptor`, no `removeAdaptor` (audit §6).
Pass: on chain, receipt2 exists with `positionValue == 0`, custody2 ATA exists
with balance 0, config decodes with the pinned bounds.
Check: decode after each step; `scripts/voltr_deploy_check.py`.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase2-strategy2-init.json`.

### P2.8 — four policies at seeds ≥ 140 with the NAV cap
Requirement: compile and install four policies at seeds ≥ 140 with a new delegated
signer; report NAV constrained `U64Le ≤ 1,000,000,000,000` at arm-data offset 39
and capital-data offset 51; amount cap 100,000,000 raw (100 USDC operational cap,
audit U7); re-measure packet fit. Readback must decode the installed bytes.
Pass: readback shows exactly four policies, the offsets above, the caps, and the
new signer; no policy names the old signer.
Check: `cargo run -p loyal-actions --bin compile_voltr_custom_policy` + Squads
settings readback decode.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase2-policies-140.json`.

### P2.9 — old generation removed
Requirement: `PolicyRemove` seeds 62–65 only after P2.8 readback passes. The seed-63
`41nzu42c3KPgJfWhnV5jbfxjHbvVU6HXaiJmzzYNqvBP` REPORT_NAV policy is retired with
them. With 62–65 gone and the signer rotated, the old image cannot act (audit B1).
Pass: settings readback shows no policy at seeds 62–65; the seed-63 policy is gone.
Check: Squads settings decode.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase2-policy-removal.json`.

### P2.10 — merge, migrations, image
Requirement: merge to main with migrations 0070–0075 embedded verbatim, stray
un-embedded `0055_backyard_rwa_worker.sql` deleted, `render.yaml` carrying the
worker block, migrations applied before the new worker starts, and the worker image
built/pushed by the `worker-images` workflow and deployed to Render (audit B4).
Pass: main contains exactly 0070–0075 embedded and no 0055; Render runs a
`sha-<commit>` image of the merged main; migration list matches.
Check: `grep -c "007[0-5]_backyard" crates/loyal-yield-store/src/store.rs` (embedded
list), `ls crates/loyal-yield-store/migrations | grep 0055` (empty), `render.yaml`
worker block, Render image tag.
Evidence: merge commit SHA + Render deploy ID in the PR.

### P2.11 — 1 USDC canary lifecycle
Requirement: one canary, 1 USDC: deposit → allocate → refresh → restore →
withdraw on strategy two, all monitors green, book == real at every step, no
performance fee LP minted.
Pass: evidence JSON records each step's signature, decoded `tv`, `idle`,
`receipt2`, `custody2`, monitor verdicts, and ends with `custody2 == 0`,
`receipt2 == 0` after full withdrawal, and `tv` back to its pre-canary value.
Check: `scripts/voltr_deploy_check.py` pre/post + journal export.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase2-canary.json`.

## Phase 3 — adaptor v3

### P3.1 — v3 behaviour set
Requirement: one adaptor upgrade shipping all of: withdraw cash pulled inside the
CPI from `ATA(holding_auth, mint)`, `holding_auth = PDA(["withdrawal_holding", config])`,
after asserting custody == 0 at withdraw entry; deposit/refresh moves the entire
custody balance out to Squads inside the CPI (no residue, audit S3); NAV step
bound `|nav_after − receipt.positionValue| ≤ amount + max(bps × receipt, floor)`
(audit U3b); `CONFIG_VERSION = 3` with no layout change. The settings-graph check
stays strict (exactly one signer, mask 7, threshold 1) while the key stays single;
its relaxation is U2 under "Not encoded", not part of Phase 3.
Pass: accounts 9 → 11; a v2 config is rejected with a version error.
Check: `cargo test -p loyal-voltr-rwa-nav-adaptor` + the LiteSVM matrix (P3.2).
Evidence: `docs/evidence/backyard-rwa-strategy2/phase3-adaptor-v3.json`.

### P3.2 — LiteSVM matrix against current and retained dumps
Requirement: the full matrix (T0–T6, R0–R8 plus new v3 cases) PASSES against the
current Voltr/adaptor fixture dump, and against the pre-upgrade dump if retained.
Pass: both runs PASS; `book == real` in every over-staging, residue and
NAV-sniper case.
Check: cargo test line above.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase3-matrix-<slot>.json`.

### P3.3 — deployer dry-run
Requirement: the adaptor deployer performs a dry-run upgrade before the real one.
Pass: dry-run log retained; ProgramData slot and sha256 recorded before and after.
Check: `crates/loyal-voltr-rwa-nav-adaptor-deployer` dry-run invocation.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase3-dryrun.json`.

### P3.4 — policies re-pointed to the holding ATA
Requirement: stage/withdraw policies are re-pointed to the holding ATA by forward
seed rollover; the worker's stage destination and `E` both include the holding
balance (M3 becomes `holding == amount` at withdraw build).
Pass: readback shows the new policies; no policy references the custody ATA as a
stage destination.
Check: Squads settings readback + worker manifest test.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase3-policies-repoint.json`.

### P3.5 — canary re-run on v3
Requirement: the P2.11 canary is repeated on v3 with all monitors green.
Pass: same post-condition shape as P2.11, plus `custody == 0 at every instruction
boundary` asserted by the worker journal.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase3-canary.json`.

### P3.6 — cap raise
Requirement: after a green v3 canary, raise the operational amount cap by rolling
seeds forward (never editing a live policy in place).
Pass: new readback shows the raised cap at new seeds; old seeds removed.
Evidence: `docs/evidence/backyard-rwa-strategy2/phase3-cap-raise.json`.

## Standing rules

- `SR-1` Program identity: any change to Voltr or adaptor ProgramData (deploy
  slot or sha256) → worker `HoldManualRecovery` until the P3.2 matrix is re-run
  against the new dump and re-proven (audit M6). This is the defense against
  semantics changing under us, as they did at slot 445,223,838.
- `SR-2` Strategy one is orphaned: `receipt1 == 3,793,536` must never change, and
  no report on strategy one below 3,793,536 is ever sent (underflow gate: below
  fails 6004, equal is a no-op, below silently writes tv down).
- `SR-3` Never create a `DirectWithdrawInitReceipt`; keep `WithdrawalWaitingPeriod`
  non-zero (600); leave `DisabledOperations` at 0.
- `SR-4` Never report NAV while LP supply is 0 or before the high-water mark is
  calibrated; performance fee stays 0 at launch.
- `SR-5` `maxCap := tv` is the deposit pause if anything above trips.

## Verdict

- `PASS`: all P0–P3 requirements hold with current on-chain or test evidence at
  the recorded slots, and `SR-1`–`SR-5` are unviolated.
- `FAIL`: name the first false requirement and its evidence; fix the falsified
  assumption, never the acceptance text.
- `HOLD`: `SR-1` fired, a Phase 1 post-condition mismatched, or the matrix is
  stale. Resume condition: re-dump, re-run, re-prove.

## Decisions (fixed; do not re-open here)

1. Keep the custom adaptor; do not switch to Voltr's own Kamino/Trustful adaptor.
2. One hot key (`BAqgbERm…`) for now. Key separation (audit U1) remains a gate
   before third-party deposits, not before this week's canary. The adaptor
   settings-graph check stays strict while the key stays single (U2, "Not
   encoded"); Phase 3 does not relax it.
3. Option A operating rules (P2.1–P2.6) this week; adaptor v3 (P3.1) next.
4. NAV cap in policies (P2.8) + step bound in the adaptor (P3.1).
5. Fees 0 at launch.
6. Locked-profit degradation re-enabled ≥ 24 h after the repair report (P1.7).
7. Strategy-one receipt is orphaned: never crank it below 3,793,536 (`SR-2`).

## Amendments

Append entries below; never edit a numbered requirement in place. Each entry:
date, requirement IDs touched, what changed, and why the change preserves or
strengthens meaning. Weakening any P0–P3 requirement or a standing rule requires
the operator.

### 2026-09-16 — operator-approved hot-admin continuation

The operator explicitly directed: "Let's use hot-adminWallet setup for now."
For the initial user/partner rollout, decision 2 and the U1 pre-third-party
cold-key gate are superseded by that instruction. Retain the existing
`BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ` administrator. This is a deliberate
hot-admin launch configuration; it does not establish cold-key isolation or
multisig security. The exact settings-graph checks, delegate policy boundaries,
deposit/servicing limits and all accounting and lifecycle release gates remain
required. No authority change or on-chain transaction is implied by this entry.

### 2026-09-08 — v1.1 scope narrowing

Both entries narrow scope and weaken nothing.

- **P3.1**: removed the settings-graph relaxation from the v3 behaviour set. The
  adaptor's strict check (exactly one signer, mask 7, threshold 1) is retained
  while the key stays single; U2 moved to "Not encoded" as a pre-third-party
  gate. Decision 2 reworded to match.
- **P2.10**: corrected the embedded-migration Check path from
  `crates/loyal-yield-store/src/lib.rs` to `crates/loyal-yield-store/src/store.rs`,
  where the `include_str!` constants live. Path accuracy only; the requirement is
  unchanged.

### 2026-09-08 — v1.2 fleet review cycle

Entries 1–6, 8 and 9 are number corrections, path corrections, or
strengthenings; none weakens a requirement or a standing rule. Entry 7 (g)
weakens a standing formulation and is `OPERATOR CONFIRMATION REQUIRED` —
proposed, not accepted.

- **P1.2 / P1.6 — post-repair state and the signed conserved gap (number
  correction, meaning preserved).** The crafted repair report parks the
  phantom in strategy one's receipt, so the post-repair state is
  `tv == idle == 3,793,417`, `receipt1 == 3,793,536`, strategy-one custody 0,
  LP supply unchanged. The conserved gap is
  `D = tv − idle − Σ receipts = −3,793,536`, unchanged by every Voltr
  operation: it holds identically at the P1.2 post-state and at the P1.5
  residual `tv == idle == 23`. The current P1 wording already states
  `receipt1 == 3,793,536` (P1.2 Pass, P1.6) — that figure is correct and
  unchanged. The correction is P1.6's line
  `D = tv − idle − Σreceipts == 3,793,536`: evaluated with its own formula
  the value is `−3,793,536`; the `3,793,536` in the text is the magnitude of
  the same invariant. Same meaning, signed figure recorded so a future
  readback compares like with like. Evidence paths: the Phase 1 LiteSVM
  simulations live at `docs/evidence/hxtk-reset-2026-09-08/*.simulated.json`
  with runbook `docs/runbooks/hxtk-reset-2026-09-08.md` (branch
  `fleet/wires`), and the reset proof at
  `docs/evidence/voltr-reset-litesvm-2026-09-08.results.json`. The
  `docs/evidence/backyard-rwa-strategy2/phase1-*.json` paths remain the
  on-chain capture targets (pending operator execution).
- **P2.7 — the bootstrap is THREE separate transactions (strengthens).** On
  the CURRENT adaptor (v2): (A) `initialize_config` with a fresh config
  keypair; (B) `initialize_report_ticket`; (C) custody ATA create + manager
  round-trip `updateVaultConfig(Manager=BAqg…) → initializeStrategy →
  updateVaultConfig(Manager=ST999…)`. For adaptor v3 additionally:
  `CreateIdempotent` of the holding ATA. Tooling:
  `tools/backyard-voltr/src/activation/rwa-multiply-strategy2-bootstrap.ts`
  (branch `fleet/policies`), journaled and CONFIRM_MAINNET-gated. The
  LiteSVM proof of the same sequence is R5a in
  `crates/squads-test-harness/tests/voltr_reset_sequence.rs` and M2 in
  `crates/squads-test-harness/tests/voltr_adaptor_v3_matrix.rs` (branch
  `fleet/v3-proof`). Strengthens: the requirement now names the exact wire
  sequence and its transaction boundaries; the steps of the original text
  are unchanged in content.
- **P2.8 — Squads spending limit on the STAGE policy (strengthens).** The
  STAGE policy — the only policy under which USDC leaves the Squads vault —
  additionally carries a Squads spending limit: USDC, 300,000,000 raw per
  1-day period (`STRATEGY_TWO_STAGE_DAILY_SPENDING_LIMIT_RAW`,
  `tools/backyard-voltr/src/domain/rwa-multiply-strategy2-route-spec.ts`,
  branch `fleet/policies`), so Squads itself bounds vault USDC outflow per
  period regardless of how many stage instructions are packed into one
  execution; the verifier binds mint, amount and period. Packet fit
  re-measured ≤ 1,232 bytes (fleet review measurement; the bootstrap
  simulation on `fleet/policies` records packetBytes 625 / 279 / 931 for its
  three transactions, under the bound). What remains unbounded: repeated
  arm/capital pairs are NAV reports, bounded by the NAV cap
  (`report_nav_cap_raw`, this requirement) and the adaptor's report interval
  — not by USDC outflow. Strengthens: adds an independent protocol-level
  bound on vault USDC outflow.
- **P2.9 — cutover ORDER is part of the requirement (strengthens).** The
  order is: bootstrap strategy two → compile + install seeds 140–143 with
  the NEW delegated signer → finalized readback verified → STOP the old
  worker and verify zero running instances on Render → `PolicyRemove` 62–65
  → verify 62–65 absent on chain → only then deploy/start the strategy-two
  image. The interval is a planned bridge outage. A preflight refuses to
  start the strategy-two worker (and the installer refuses) while any of
  seeds 62–65 exists. Strengthens: the original text ordered `PolicyRemove`
  after the P2.8 readback only; the full order and the refusal gates are now
  part of the requirement.
- **P2.4 — classification and fence clarifications (strengthens).** (a) A
  failure receipt is used only when the signature is SETTLED
  (confirmed/finalized, slot > 0) and `meta.err` non-null; processed-only
  results keep observing. (b) An unreadable receipt never enters manual
  recovery — the row stays submitted and is re-fetched; before any timeout
  termination a FRESH finalized status is read: finalized failure →
  non-capital `failure_receipt_unavailable`; settled success → the success
  path; otherwise keep observing. (c) Failing-program attribution rebuilds
  the CPI stack from logs and attributes only when an explicit
  `Program <id> failed:` line or an AnchorError inside the innermost open
  frame exists; truncated or missing logs → `""` → capital stop. (d) The
  stale-send fence reads `observed_slot` from the persisted typed build
  input (JSONB); non-bridge wires are unfenced. Strengthens: each item
  narrows what may be retried or attributed, and (d) pins the fence input to
  durable state.
- **P2.1–P2.3 — worker monitor clarifications (strengthens).** M1/M3
  staged-transient checks use the Voltr-tracked custody figure at strategy
  receipt offset 128, not only the SPL token balance. S1 `CapitalMutated`
  must be self-explaining: it records which balance moved and by how much.
  M8: STAGE is not a ticket-consuming action — only arm/capital reports
  consume a ticket. M2 names the armed-NAV source it compares receipt2
  against. M6 pins both ProgramData sha256 values and the HOLD is durable
  (survives worker restart). M7 checks fee bps (`adminPerfFee` /
  `adminMgmtFee`) against the launch value 0. Strengthens: each monitor is
  now checkable against a named on-chain field or journal property.
- **P3.1 — v3.1 behaviour set (supersedes the v3 text for the items listed
  here; all other v3 items unchanged).** (a) The Voltr strategy receipt at
  capital account index 11 is accepted writable OR read-only (Solana merges
  privileges by key and Voltr's outer instruction lists it writable; the
  v3.0 read-only requirement failed every capital path in LiteSVM,
  InvalidAccount at index 11); it is never written and is PDA/owner-checked.
  (b) ONE flow-adjusted bound in u128:
  `|(nav_after + outflow) − (receipt.positionValue + inflow)| ≤ step`, with
  `step = max(max_step_bps × receipt.positionValue / 10,000, step_floor_raw)`;
  deposit: inflow = amount + swept residue, outflow 0; refresh: inflow =
  swept residue; withdraw: outflow = amount (the exact holding pull), inflow
  0. This replaces `≤ amount + max(bps × receipt, floor)`, under which a
  capital amount could hide a write-down — strengthens. (c) Report ticket
  format v2, 104 bytes, `last_consumed_slot: u64` at 96..104, all existing
  offsets kept; `min_report_interval_slots` enforced at CONSUMPTION on
  `Clock.slot` (`slot ≥ last_consumed_slot + interval`); v1 tickets
  rejected. (d) Ticket PDA creation tolerates a prefunded PDA (allocate +
  assign with seeds; never touches an initialized ticket). (e) Errors:
  ReportStep 19, ReportInterval 20, CustodyNotEmpty 21. (f) The README
  states arming and consumption may be separate transactions within the
  freshness window. (g) `OPERATOR CONFIRMATION REQUIRED` — NOT ACCEPTED AS
  WRITTEN: in v3.1 the settings-graph check would be relaxed to "the pinned
  Squads settings signer is PRESENT with permission mask 7; threshold,
  timelock, member count and other members unconstrained." This contradicts
  Decision 2 and amendment v1.1 (strict: exactly one signer, mask 7,
  threshold 1) and would move U2 from "Not encoded" into the shipped check.
  Rationale offered by the review: under the strict check, adding a member
  or a timelock bricks every adaptor path (a full halt, not a safety gain),
  and relaxing now avoids another adaptor upgrade when governance is
  hardened; the pinned-signer check keeps the fail-closed property. The
  operator decides; until confirmed, the strict check of Decision 2 / v1.1
  stays in force.
- **P3.2 — matrix file, cases and evidence path (path/name accuracy;
  meaning unchanged).** The v3 matrix is
  `crates/squads-test-harness/tests/voltr_adaptor_v3_matrix.rs` (tests
  `voltr_adaptor_v3_phase1`, `voltr_adaptor_v3_full_matrix`,
  `voltr_adaptor_v3_cpi_probe`) with cases V1–V11 (plus bootstrap milestones
  M0–M3) as labelled in that file; results at
  `docs/evidence/voltr-adaptor-v3-litesvm-2026-09-08.results.json` (branch
  `fleet/v3-proof`), which replaces the evidence path
  `docs/evidence/backyard-rwa-strategy2/phase3-matrix-<slot>.json`. The
  mainnet order is mirrored by the matrix: repair on v2 → swap ELF to v3 →
  bootstrap on v3. V6 boundary is `step = max(bps term, floor)`: at receipt
  500,000, 500 bps, floor 1,000,000 → step 1,000,000; nav 1,500,000 passes,
  1,500,001 fails 19.
- **P2.10 / P2.8 — evidence paths.** Strategy-two policy evidence at
  `docs/evidence/hxtk-strategy2-2026-09-08/` (`strategy-two-bootstrap` and
  `policy-remove-62-65`) with runbook
  `docs/runbooks/hxtk-strategy2-bootstrap-2026-09-08.md` (branch
  `fleet/policies`). These are the simulated-policy evidence; the
  `docs/evidence/backyard-rwa-strategy2/phase2-*.json` paths remain the
  on-chain capture targets (pending operator execution).

### 2026-09-08 — v1.3 sequential seeds, one-shot repair policy, adaptor v3.3

Entries 1–8 are corrections, evidence closures, or strengthenings; none
weakens a requirement or standing rule. The v1.2 (g) settings-graph proposal
remains `OPERATOR CONFIRMATION REQUIRED` and unchanged.

- **1. P1.2 (repair path) — supersedes “repair report through the seed-63
  policy”.** The live seed-63 policy pins a per-report digest and its account
  positions differ from the proof wire (simulation returns Squads 6069); the
  original LiteSVM repair proof also used the Squads vault PDA as a direct
  signer, with constraints checked structurally through `policy_check`.
  The seed-63 path was therefore never proven. The repair must execute through a
  ONE-SHOT report policy compiled by the same compiler as the live policy set
  (`compileCustomPolicyArtifact`, `nav-refresh`, digest unconstrained, no
  spending limit), created by the Settings signer `BAqgbERmUViqDSx961xpRBHGt68SpACiWL4t9696qZZ` (threshold 1) at the
  Settings’ NEXT sequential seed. Squads `PolicyCreate` assigns
  `settings.policy_seed` and ignores requested seeds: requesting 144 while the
  counter is 139 fails 6024 `MissingAccount`. The counter read 139 at slot
  445,313,800, so the repair policy is expected at seed 140 and the tool must
  abort if the counter moved. Execute it via `ExecuteSync` with delegated signer
  `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5` and vault index 0, then remove it with `PolicyRemove`; seeds are never
  reused. LiteSVM proof on branch `fleet/v3-proof`, commit `4f4e826`:
  `PolicyCreate` 1,073 B (seed 139→140, policy PDA
  `7vqKymJ4RcP9TUR9jT6G2ruuRp3j6rVhTzoYJWYTe2dR`), `ExecuteSync` repair
  1,067 B → `tv == idle == 3,793,417`, `receipt1 == 3,793,536`, custody 0,
  LP `99,941,522` unchanged, and `PolicyRemove` 318 B closes the PDA.
  Evidence: `docs/evidence/voltr-squads-repair-policy-litesvm-2026-09-08.results.json`.
  Installed constraints recorded there are: adaptor arm accounts
  `0→config 9hDH…`, `1→ticket C71B…`; data offset 0 `U8Slice
  a4aff629b28c230300`, offset 9 `U64Le 0`, offset 17 `U8Slice
  013900000001`; Voltr `deposit_strategy` pinned accounts
  `0,2,3,8,11,12,13,14,15,16,17`; data offset 0 `U8Slice
  f65239e283defdf9`, offset 8 `U64Le 0`, offset 16 `U8Slice
  0108000000f223c68952e1f2b6013900000001`. Mainnet tooling on
  `fleet/wires` is `tools/backyard-voltr/src/reset/hxtk-reset.ts`, with
  subcommands `repair-policy`, `repair`, and `repair-policy-remove`;
  `docs/runbooks/hxtk-reset-2026-09-08.md` is the runbook and
  `docs/evidence/hxtk-reset-2026-09-08/repair-policy.simulated.json` records
  the 0–255 seed occupancy (111 occupied; ranges 17–24, 29, 32–50, 57–139).
  Before signing, the wires tool’s decoded constraint list must equal the
  LiteSVM-installed list above. This strengthens P1.2 by making the proof and
  mainnet wire follow the real sequential-seed Squads path with identical
  constraints.

- **2. P1.1 ordering (degradation → 0 before the repair) — now evidenced; no
  requirement change.** The v3 matrix without this step paid a user 827,489 on
  a 1,000,000 deposit, exposing the locked-profit haircut from the repair jump.
  With the full reset-sequence base
  (P1.1 → repair → harvest → cancel → request → claim), V10 pays 999,999
  (9,999 bps). This strengthens P1.1’s rationale and preserves its meaning.

- **3. P2.8 / P2.9 seeds and spending limits — sequential, not fixed.**
  Strategy-two policies are not fixed at 140–143: they are the next four
  sequential seeds at install time, expected 141–144 after the one-shot repair
  policy takes 140. The installer must derive `policySeedBefore` from the
  finalized Settings counter and abort if it differs from the journaled
  expectation; removing seeds 62–65 does not free or recycle seeds. The daily
  USDC spending limit (300,000,000 raw / 1-day) is attached to EVERY policy
  in the strategy-two set (allocation, stage, restore, and report), as covered
  by the `fleet/policies` commit `0ba89d5` and Rust test
  `every_policy_carries_the_daily_usdc_spending_limit`. All four packets are
  ≤ 1,232 B. Creation uses the LEGACY `ProgramInteraction` payload; the compact
  payload fails 6024 when spending limits are present. The installer’s
  strategy-two `--execute` path and the worker start gate
  `go/backyard-rwa-worker/internal/backyardrwa/legacy_policy_gate.go` refuse
  while any seed 62–65 exists at finalized commitment. The LiteSVM proof that
  the daily limit rejects packed outflow beyond the period budget is pending on
  branch `fleet/policies` (task in progress). This strengthens P2.8/P2.9 by
  binding policy installation to the finalized Settings counter and applying
  the same independent outflow bound to every strategy-two policy.

- **4. P2.3 / SR-1 — durable `HOLD_MANUAL_RECOVERY` route-level latch.**
  `HOLD_MANUAL_RECOVERY` is a route-level latch persisted in the same
  transaction as the hold decision. Migration v76 is registered in the
  production `yield-migrations` binary and backfills existing terminal holds.
  The latch is checked at the top of every tick before any build, sign, or send,
  including the R03 signed-unsent recovery; it is restart-safe and can be
  cleared only by the operator command
  `backyard-rwa-worker clear-hold --route <id> --reason "<text>"`, which
  journals `HOLD_CLEARED` and refuses an empty reason. A manual-recovery
  decision produced at ANY stage (observe, construction refresh, or execution
  observe) is latched. Strategy-receipt integrity is also fail-closed:
  an absent receipt (at FINALIZED commitment, after one finalized re-read of a
  confirmed null), foreign owner, or non-192-byte receipt produces
  `HOLD_MANUAL_RECOVERY strategy_receipt_integrity`; confirmed-only nulls and
  transport errors remain tick errors. M6 hashes SHA-256 over
  `ProgramData.data[45:]` (executable bytes only), pinning Voltr ProgramData
  `3fiAyUjktZkZf6hcbBPy6U6UdkMdEFoT5S4sjtzAd5az` at slot 445223838 to
  `bf1c1831…44c56` and adaptor ProgramData
  `DrvzixaVmAuPVVJPtP5wykb9mvgDWqZbvZau9oiCUpHu` at slot 443528877 to `8361a469…6eb6d5`;
  `scripts/voltr_deploy_check.py` prints the same digest and validates
  `--expect-*-sha`. The latch registration/atomicity fixes remain pending on
  branch `fleet/worker-a`. This strengthens P2.3 and SR-1 by turning a safety
  decision into durable route state that survives restart and by pinning the
  executable bytes, not account metadata.

- **5. P3.1 — v3.3 (supersedes v3.1 item b and the v3.2 text).** The step
  bound prices ONLY Voltr-explained flows: deposit has
  `inflow = amount_in` and requires `custody_at_entry ≥ amount_in`; refresh
  has `inflow = 0`; withdraw has `outflow = amount`, with the unchanged
  equation
  `|(nav_after + outflow) − (receipt.positionValue + inflow)| ≤ step`,
  where `step = max(max_step_bps × position / 10,000, step_floor_raw)`.
  Any custody residue is swept whole to the Squads vault but remains UNPRICED:
  a donation can never block or alter a report, is unreported until a later
  report includes it (≤ one step per report), and requires a nonzero step budget.
  Withdraw first sweeps residue, then pulls exactly `amount` from the holding ATA.
  Postconditions require custody == 0 after the sweep (error 21) and custody ==
  amount after the pull (error 23 `CustodyMismatch`). The strategy receipt must
  be exactly 192 bytes (error 2), and Voltr-tracked custody at offset 128 must be
  0 (error 22 `TrackedCustodyNonZero`); a Voltr layout change halts by design,
  with M6 HOLD plus re-proof as the resume path. The deployer performs a
  capacity check against the pinned 115,384-byte ProgramData payload before any
  RPC or slicing and accepts `--spec v2|v3`; an extension remedy is the
  shortfall rounded to 8 bytes plus a reviewed re-pin. The canonical build is
  `bun run build:adaptor` (`scripts/build-adaptor.sh`), using `LTO=fat` and
  `codegen-units=1`; it exits non-zero when size exceeds capacity or the SHA
  differs from the pin. A plain build is 119,848 B and is refused. Artifact
  v3.3 is 107,832 B with SHA-256
  `836ded9ff4e79cda9fafbafcffcf9f2e9f395c762c69ba5af4630e82a9d8a4d0`,
  from `fleet/adaptor-v3` commit `fc792a3`. Astra’s verdict is
  “deploy after LiteSVM matrix”. This strengthens P3.1 by separating
  Voltr-explained flow from unpriced donations and making layout, capacity,
  and canonical artifact identity explicit. The v1.2 (g) settings-graph
  entry remains `OPERATOR CONFIRMATION REQUIRED` and unchanged.

- **6. P3.2 — v3.3 matrix result and path accuracy.** The matrix is 30/30
  PASS with the reset-sequence base, from `fleet/v3-proof` commit `7d1519d`
  and `docs/evidence/voltr-adaptor-v3-litesvm-2026-09-08.results.json`:
  V1 allocate through Voltr with writable receipt; V2 dust; V3 holding
  restore; V4 over-staged holding; V5 residue swept on withdraw; V6/V6b step
  edges; V7 interval 20; V8 direct call rejected; V9 v2 config rejected;
  V10 999,999 payout; V11 D conserved; V12 prefunded ticket ×2;
  V13 settings relaxation ×2; V14a/b/c donations never block; V15 error 22
  on both paths; V15b 191 B rejected by Voltr’s fixed-192 loader / 193 B
  adaptor error 2; and FINAL invariants. Receipt2 tracked custody is 0 at
  every snapshot. This preserves P3.2 while replacing its stale case count
  and evidence path with the verified v3.3 matrix.

- **7. P3.3 — deployer dry-run.** The dry-run remains required and is not
  treated as complete: `docs/evidence/backyard-rwa-strategy2/phase3-dryrun.json`
  is pending on branch `fleet/adaptor-v3` and must be produced with
  `--spec v3`, recording the before/after ProgramData slot and SHA. This
  preserves P3.3 and makes the remaining evidence boundary explicit.

- **8. `## How to verify` / `## Not encoded`.** `## How to verify` now names
  the ignored R-policy test `voltr_squads_repair_policy`, the canonical
  `bun run build:adaptor` gate, and the Go `clear-hold` command as an operator
  tool only, not a verifier. `## Not encoded` now records that “policy seeds
  are sequential per Settings; a stale journaled expectation must abort” is
  encoded in P1.2/P2.8, and notes that Codex-authored commits on
  `fleet/v3-proof` are unsigned because the repository signing hook was
  unavailable to the agent; they must be re-signed or squash-merged. This
  preserves the verifier/operator boundary and records the signing follow-up
  without treating an unsigned proof commit as release proof.

### 2026-09-08 — v1.5 consistency notes

- **P2.11 final `tv` requirement:** the effective pass condition is `tv == pre-canary tv + 1,000,000 − payoutRaw`, with the actual finalized `payoutRaw` recorded in the canary evidence; this implements the canary runbook’s conservation equation and preserves the residual when payout is below the deposit.
- **P1.7 timing:** the effective requirement is that the finalized degradation restore occurs at least 24 h after the repair signature and before any 1 USDC canary, tester, or third-party deposit; this implements the ordering correction in `docs/runbooks/operator-checklist-2026-09-08.md` and `docs/runbooks/strategy-two-canary-2026-09-08.md`.

### 2026-09-08 — v1.4 NAV-pinned one-shot policy, spending-limit proof, latch generation

This amendment strengthens P1.2, P2.3, and P2.8 throughout without editing a
numbered requirement.

- **P1.2 — the one-shot repair policy pins NAV on chain.** The compiler field
  `report_nav_exact_raw` is additive to `report_nav_cap_raw` and emits
  `Equals U64Le 3,793,536` at the arm-report NAV offset `39` and capital-report
  NAV offset `51`. The PolicyCreate data is `837` bytes with SHA-256
  `796624dfef068d71db889913de3527f36c23e19e11aafc9e370f021b649f153e`; it is
  byte-identical between the mainnet-tool evidence
  `docs/evidence/hxtk-reset-2026-09-08/repair-policy.simulated.json` (packet
  `1,151` B) and the LiteSVM proof from `09ae747` (create `1,109` B, repair
  `1,067` B, remove `318` B). NAV `3,793,535` and `3,793,537` are rejected by
  Squads `6064 ProgramInteractionInvalidNumericValue`; a wrong ticket sequence
  is rejected by adaptor error `8`.

  The tool’s provenance check compares live policy bytes with the finalized
  creation journal’s recorded hash, which is a dynamic continuity pin, then
  decodes live policy constraints and identities. Request reconciliation
  requires `withdrawableFromTs == requestTx.blockTime + 600` with the documented
  one-second Voltr rounding tolerance. Restore requires
  `restoreTx.blockTime - repairTx.blockTime >= 86,400` plus the finalized claim
  and closed request state. The per-leg canonical replay fence, journal
  barriers, exact-payout claim fence, and one-shot policy retirement remain
  chained inside `repair --execute`, with standalone policy removal only as
  recovery if the chained removal fails.

  The canonical fence is machine- and user-local, not checkout-local: by
  default it is `${HOME}/.loyal/hxtk-reset/<vault>/`, or an absolute
  `HXTK_RESET_STATE_ROOT` override subject to current-uid ownership and
  group/other permission checks. Simulation prints the resolved root but does
  not write it. The canonical state file, rather than the requested journal
  directory, binds checkout/state roots, pending metadata, signed wire, and the
  finalized-journal SHA-256. A pre-send snapshot failure occurs before the
  attempted mark and raw send, moves the wire to
  `<journal>.aborted-<unix ms>.json`, records `aborted-pre-send`, and leaves no
  pending file; recheck finalized state and rerun without `--allow-repeat`.
  An exception after the attempted mark remains `attempted` and must use the
  same journal’s reconcile path only after checking the expected signature.
  `--allow-repeat` is reserved for new journals on repeatable state-idempotent
  legs; one-shot, pending, and attempted legs cannot use it.

- **P2.8 — production spending-limit seam.** The daily-limit LiteSVM proof now
  exercises the production
  `create_program_interaction_action_instruction_with_daily_spending_limits`
  seam and rejects excess packed outflow with `6073
  ProgramInteractionInsufficientTokenAllowance`. The seed journal is bound to
  identities and revalidates its live Settings, genesis, repair-policy, config,
  delegated-signer, and finalized anchor before each use. The worker start gate
  independently checks the Settings anchor and genesis and refuses to continue
  after its bounded `60 s` deadline.

- **P2.3 — generation-aware latch and snapshots.** The route latch uses the
  migration-v77 generation column: CAS re-records preserve the current
  generation, and the fallback path is generation-aware and ignores
  `latched:*` re-records from another generation. Construction snapshots now
  carry the identity-enriched inputs needed to prove that the wire was built
  from the same route, strategy, policy, and program identities that were
  observed.

- **Not encoded update.** The old seed-63 path and static policy-account-hash
  pinning are superseded by the seed-140 one-shot policy and its dynamic
  continuity pin. Source `fleet/*` commits retain their original history, but
  the Codex squashes on `fleet/integration` are trailer-free and SSH-signed by
  the integrating environment; the repository has no
  `gpg.ssh.allowedSignersFile`, so the operator verifies the signer identity
  (or re-signs on squash-merge) before merging. This amendment
  strengthens the verifier/operator boundary and does not treat simulated
  evidence as live deployment proof.

## Not encoded (audit items with no checkable requirement yet)

- **v1.3 closure note:** Policy seeds are sequential per Settings; a stale
  journaled expectation must abort. This is now encoded in P1.2/P2.8 and is
  not an unencoded assumption.
- Source fleet commits may retain Codex trailers, but the integrated
  `fleet/integration` squashes are trailer-free and SSH-signed by the
  integrating environment; the repository has no
  `gpg.ssh.allowedSignersFile`, so the operator verifies the signer identity
  (or re-signs on squash-merge) before merging.
- Settings-graph relaxation (U2): required before Squads governance is hardened /
  before third-party money; not part of Phase 3. The deployed v2/v3 check stays
  strict — exactly one signer, mask 7, threshold 1 — so adding a Squads member or
  a timelock still bricks every adaptor path (fail-closed, but a full halt).
  Amendment v1.2 P3.1 (g): the v3.1 adaptor implements this relaxation, pending
  `OPERATOR CONFIRMATION REQUIRED`; until the operator confirms, the strict check
  stays in force and this item stays un-encoded.
- U1 cold-key separation: deferred by decision 2; no Phase 0–3 check enforces it.
  Must become a requirement before third-party money.
- §1.4 no per-epoch operation limit: informational; the real gate (one report per
  slot) is enforced by P2.4 and M8.
- §1.5 first-crank fee-LP origin and the §6 `DisabledOperations` bit mapping `[U]`:
  `SR-3`/`SR-4` forbid the hazardous settings; bit semantics stay unverified.
- Audit S5 (`A/README.md` says arm and consume must share a transaction; the
  adaptor only enforces slot age): fixed in the v3.1 README (amendment v1.2
  P3.1 (f)).
- Epoch-stamping rule for a fresh receipt after `removeAdaptor` + re-add (`[U]`,
  §6): avoided by P2.7's "never removeAdaptor", not verified.
