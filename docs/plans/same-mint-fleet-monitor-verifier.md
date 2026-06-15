# Same-Mint Fleet Monitor Verifier

Use this document as the fixed verifier for the DB-driven same-mint fleet
monitor and live same-mint deposit/optimize/withdraw experience. Do not treat it
as an implementation checklist. The work passes only when a skeptical runner can
verify every required condition below from the repo, Neon control-plane rows,
Timescale candidate data, Solana chain state, transaction signatures, and local
or Render worker logs.

## Goal

`same-mint-yield-monitor` must operate as a fleet worker in production: every
poll discovers active `loyal_yield.managed_vaults` with active
`loyal_yield.route_policies`, optimizes only same-mint Safe USDC Kamino policies
whose delegated signer allowlist contains `YIELD_ROUTER_KEYPAIR`, and naturally
stops watching a vault once the frontend or full-withdraw path marks the policy
or vault inactive.

The verifier must also prove the frontend-critical lifecycle end to end with
real chain and DB effects:

1. use the `SOLANA_TESTING_PK`-attached vault to create/update the policy and
   record the active policy/vault rows in Neon;
2. fund and deposit real USDC into Kamino Main USDC, then reconcile and record
   the Main position in optimizer state;
3. prove the fleet orchestrator picks up that active DB row and moves the funds
   into the currently best positive-edge eligible Safe USDC reserve;
4. fully withdraw from the final current reserve, return funds, close/refund
   closeable policy/ATA/obligation state, reconcile zero positions, and mark the
   policy/vault inactive so the fleet no longer monitors it.

Local setup, initial funding/deposit, obligation setup, full-withdraw wallet
recovery, policy removal, and DB deactivation may use `SOLANA_TESTING_PK`.
Fleet optimization and protected policy-mediated value movement must use only
`YIELD_ROUTER_KEYPAIR`. Overall PASS is impossible with dry-run evidence alone.

## Commands Under Verification

Run secrets-backed commands through:

```sh
op run --env-file=.env.1password -- sh -c '<command>'
```

Required command surfaces:

```sh
cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor -- --once --all-active-vaults
```

```sh
cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor -- --once --all-active-vaults --execute
```

```sh
cargo run -p loyal-yield-orchestrator --bin same-mint-monitor-e2e -- --settings <PUBKEY> --vault-index 1 --amount-raw <U64>
```

Approved live E2E command:

```sh
cargo run -p loyal-yield-orchestrator --bin same-mint-monitor-e2e -- --settings <PUBKEY> --vault-index 1 --amount-raw <U64> --execute
```

```sh
cargo run -p loyal-yield-orchestrator --bin same-mint-reserve-swap -- --settings <PUBKEY> --vault-index 1 --full-withdraw-reserve <RESERVE>
```

`--full-withdraw-main-usdc` must remain as a compatibility alias for Main USDC.

## Required Checks

### 1. Fleet Discovery

PASS only if `--all-active-vaults` selects exactly the DB rows matching this
intent:

- `managed_vaults.active = true`
- `route_policies.active = true`
- the policy contains route mode `same_mint_kamino`
- the policy universe is USDC-compatible through `stable_mints` and
  `kamino_liquidity_mints`
- the delegated signer list contains the pubkey derived from
  `YIELD_ROUTER_KEYPAIR`

The output must report the number of discovered vaults and one JSON result per
vault. Inactive policies or inactive vaults must be absent from the fleet output.

### 2. Reconciled Planning Source

PASS only if each active fleet vault reconciles chain state into
`loyal_yield.vault_reserve_positions_current` before planning. The reconciled
state must include the policy-eligible Safe USDC reserves and must be the source
used by the optimizer. A vault with stale or empty frontend-only
`user_yield_positions` data can still plan when chain reconciliation finds a
current Kamino position.

### 3. Per-Vault Isolation

PASS only if one vault's skip, blocker, or execution failure is captured in that
vault's JSON result without stopping the rest of the poll. A mixed fleet dry-run
with at least one skipped vault and one planned or independently skipped vault
must return a top-level fleet result instead of exiting early.

### 4. Optimizer Signer Boundary

PASS only if fleet execution does not require or read `SOLANA_TESTING_PK`.
`same-mint-yield-monitor --all-active-vaults --execute` must shell into
`same-mint-reserve-swap --optimization-cycle --reconcile-from-chain --execute`
with settings, vault index, source reserve, and target reserve, and route
execution must use `YIELD_ROUTER_KEYPAIR` as delegated signer and fee payer.

Setup/admin commands may use `SOLANA_TESTING_PK`; fleet optimization may not.

### 5. Fail-Closed Missing Obligation

PASS only if a fleet optimization targeting a reserve whose destination
obligation is missing returns `blocked_missing_obligation_setup` for that vault
and proves no setup/admin transaction was sent, no route policy was mutated, and
no rebalance decision/current-position write was made by the optimizer.

### 6. Generic Full Withdrawal

PASS only if:

- `--full-withdraw-reserve <RESERVE>` withdraws the selected current reserve.
- `--full-withdraw-main-usdc` behaves as an alias for Main USDC.
- the protected Kamino withdraw is signed by `YIELD_ROUTER_KEYPAIR`.
- wallet USDC recovery and route-policy removal are signed by the
  `SOLANA_TESTING_PK` settings authority.
- execute mode reconciles zero current positions after chain confirmation.
- execute mode marks the selected `route_policies` and `managed_vaults` rows
  inactive after confirmed cleanup.
- the output reports wallet/vault USDC return evidence, rent refund evidence,
  closed policy account evidence, closeable ATA/obligation cleanup evidence,
  inactive DB row evidence, and that the vault is no longer discoverable by
  `--all-active-vaults`.

If the policy account, a closeable ATA, or a closeable obligation remains open
without an explicit non-closeable reason, this section is FAIL.

### 7. Live Full-Flow E2E

PASS only if `same-mint-monitor-e2e` defaults to dry-run, refuses setup when the
precondition is false, and after explicit approval runs a real `--execute`
flow that proves this exact sequence with chain signatures and DB readbacks:

- derive setup/admin authority from `SOLANA_TESTING_PK`.
- derive optimizer signer from `YIELD_ROUTER_KEYPAIR`.
- require fresh Safe USDC candidate data and a positive APY edge from Main USDC
  to another eligible Safe USDC reserve before creating policy or depositing.
- create/update the policy for the `SOLANA_TESTING_PK`-attached vault and record
  the active `route_policies` and `managed_vaults` rows in Neon.
- transfer real USDC, deposit into Kamino Main USDC, and record/reconcile the
  deposited Main reserve position in `vault_reserve_positions_current`.
- run `same-mint-yield-monitor --once --all-active-vaults --execute` until this
  vault reports `executed` or timeout.
- verify the optimizer chose the highest-APY eligible Safe USDC reserve, wrote a
  confirmed same-mint rebalance decision, submitted a confirmed route signature,
  and reconciled the final reserve position.
- run generic full withdrawal from the current reserve selected by the optimizer.
- verify `YIELD_ROUTER_KEYPAIR` signed the protected route execution and
  protected full-withdraw Kamino withdraw, while `SOLANA_TESTING_PK` signed
  wallet recovery and policy removal.
- verify wallet/vault USDC return, closed policy account evidence, closeable
  ATA/obligation cleanup evidence, rent refund evidence, inactive DB rows, zero
  current positions, and no remaining monitored vault for the selected
  settings/vault index.

If Safe USDC candidate rows are stale or absent, `same-mint-monitor-e2e` must
exit before setup with `blocked_no_fresh_candidate_precondition`. If Main USDC
is already the highest eligible reserve, `same-mint-monitor-e2e` must exit
before setup with
`blocked_no_positive_edge_precondition`.

This live E2E must exercise the orchestrator pickup path. The proof can use the
local command invocation of `same-mint-yield-monitor --once --all-active-vaults
--execute` or an already-deployed Render worker, but it must not bypass the
monitor by directly calling `same-mint-reserve-swap` for the optimization move.
For production rollout, repeat the pickup proof with Render logs before enabling
continuous execution.

Dry-run output is required as a safety preview, but dry-run output alone is
FAIL for this section and FAIL for the overall verifier.

### 8. Render Rollout Shape

PASS only if Render keeps the pinned light-worker image workflow and the monitor
command is:

```sh
/usr/local/bin/same-mint-yield-monitor --all-active-vaults --poll-interval-seconds 300
```

The Render worker must not include `SOLANA_TESTING_PK`. Continuous execution can
be enabled only after this verifier records:

- local live E2E PASS from `same-mint-monitor-e2e --execute`;
- dry-run Render logs showing fleet candidate selection plus per-vault
  skip/plan results;
- an explicit rollout decision naming whether the production orchestrator pickup
  proof was local `--once --all-active-vaults --execute` or deployed Render
  worker execution.

### 9. Local Checks

PASS only if these commands pass locally:

```sh
NO_DNA=1 cargo fmt --check
```

```sh
NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin same-mint-yield-monitor --bin same-mint-reserve-swap --bin same-mint-monitor-e2e
```

```sh
NO_DNA=1 cargo test -p loyal-yield-orchestrator --bins
```

Focused tests should cover fleet discovery filtering, active-flag stop
monitoring, chain-reconciled planning when current rows are empty, per-vault
failure isolation, the E2E APY-edge precondition, and full-withdraw reserve
selection.

### 10. Required Live Evidence

PASS only if the verifier record includes the exact command output or a durable
log link for all of the following:

- live dry-run fleet poll showing all Safe USDC candidates reconciled with
  `sendsTransactions: false`;
- live `same-mint-monitor-e2e --execute` run showing policy/vault row creation
  or update, Main USDC deposit signature, current-position reconciliation,
  monitor pickup, confirmed same-mint route signature, final best-reserve
  position, full-withdraw signature, zero-position reconciliation, inactive DB
  rows, and no remaining fleet discovery for the selected vault;
- direct DB readback queries for the selected policy/vault/decision rows after
  deposit, after optimization, and after full withdrawal;
- chain readback proving the wallet/vault USDC return and rent refund/close
  evidence.

## Verdict Format

Report:

```text
Fleet Discovery: PASS|FAIL - note
Reconciled Planning Source: PASS|FAIL - note
Per-Vault Isolation: PASS|FAIL - note
Optimizer Signer Boundary: PASS|FAIL - note
Fail-Closed Missing Obligation: PASS|FAIL - note
Generic Full Withdrawal: PASS|FAIL - note
Live Full-Flow E2E: PASS|FAIL - note
Render Rollout Shape: PASS|FAIL - note
Local Checks: PASS|FAIL - note
Required Live Evidence: PASS|FAIL - note
Overall Verdict: PASS|FAIL
```

Overall verdict is `PASS` only when every required section passes. If any
section fails, keep this verifier unchanged and make the smallest next change
needed for the failing section. A run that only proves dry-run discovery,
reconciliation, or planning must be reported as `FAIL - live full-flow E2E not
yet proven`.

## Current Verification Record

Recorded on June 15, 2026. The evidence below supports implementation, dry-run,
cleanup, blocker checks, one successful local live E2E, and the Render dry-run
rollout of the fleet worker.

- Local checks before live attempt: PASS for `NO_DNA=1 cargo fmt --check`,
  `NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin
  same-mint-yield-monitor --bin same-mint-reserve-swap --bin
  same-mint-monitor-e2e`, `NO_DNA=1 cargo test -p loyal-actions`,
  `NO_DNA=1 cargo test -p loyal-yield-orchestrator --bins`, and
  `git diff --check`. After adding E2E breadcrumbs and the stale-candidate
  preflight, focused PASS for `NO_DNA=1 cargo check -p
  loyal-yield-orchestrator --bin same-mint-monitor-e2e` and `NO_DNA=1 cargo
  test -p loyal-yield-orchestrator --bin same-mint-monitor-e2e`.
- Local checks after final doc cleanup: PASS for `NO_DNA=1 cargo fmt --check`,
  `NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin
  same-mint-yield-monitor --bin same-mint-reserve-swap --bin
  same-mint-monitor-e2e`, `NO_DNA=1 cargo test -p
  loyal-yield-orchestrator --bins`, `NO_DNA=1 cargo test -p loyal-actions`,
  and `git diff --check`. The only warning was the existing
  `SelectedVault.swap_lanes` dead-code warning in `same-mint-reserve-swap`.
- Compact Safe USDC policy dry-run: PASS for
  `same-mint-reserve-swap --settings 6jgkucnbz1RuHq6NULqACQY3r2XegHaWhgPpaCEGPCA3
  --vault-index 1 --update-policy --provision-lookup-table`, which reported
  `policy_update_dry_run`, `sendsTransactions: false`,
  `authoritySigner: BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`,
  `delegatedSigner: oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C`,
  `constraintCount: 2`, `stableMints` and `kaminoLiquidityMints` containing
  only USDC, Safe USDC markets Main/Figure/Maple/OnRe/Ethena, no simulation
  error, `instructionDataBytes: 717`, `packetSizeBytes: 991`, and
  `fitsPacketDataSize: true`.
- Earlier E2E dry-run: PASS for
  `same-mint-monitor-e2e --settings 6jgkucnbz1RuHq6NULqACQY3r2XegHaWhgPpaCEGPCA3
  --vault-index 1 --amount-raw 1000000`, which reported
  `monitor_e2e_dry_run`, no sends, Main USDC at 358 bps, best Safe USDC reserve
  OnRe `AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z` at 536 bps, and a
  178 bps positive edge. The previewed execute phases are policy update,
  obligation setup for OnRe, deposit into Main USDC, fleet monitor
  `--once --all-active-vaults --execute`, and full withdrawal from OnRe.
- E2E dry-run after stale-candidate preflight: PASS for rerunning the same
  settings/vault dry-run command. It now logs phase breadcrumbs on stderr
  (`start`, `timescale_connect_start`, `timescale_candidates_loaded`,
  `fresh_candidates_filtered`) and exits before setup with
  `blocked_no_fresh_candidate_precondition` when all four Safe USDC candidate
  rows are older than the default 21600-second freshness window. This prevents
  the E2E script from creating policy or depositing when the monitor would
  later skip with `no_eligible_fresh_candidate_data`.
- Fleet dry-run: PASS for
  `same-mint-yield-monitor --once --all-active-vaults`, which discovered one
  active DB vault, read four Safe USDC candidates with `sendsTransactions:
  false`, requested and reconciled all four loaded candidate reserves
  (OnRe/Figure/Main/Maple), wrote current chain state into snapshot `235`, and
  skipped planning because the current live policy is still Main/Figure-only and
  those policy-eligible candidate rows were outside the default freshness
  window. The output included `policyEligibleCurrentPositions` proving planning
  uses the policy-eligible subset of the chain-reconciled rows.
- Relaxed-age planning dry-run: PASS for fail-closed market authorization with
  `same-mint-yield-monitor --once --all-active-vaults
  --max-candidate-age-seconds 999999`, which read four Safe USDC candidates but
  treated only Main/Figure as policy-eligible because the active DB policy has
  not yet been live-updated to the five-market Safe USDC policy. It reconciled
  current positions into snapshot `234` and skipped with
  `already_at_winner_or_no_positive_edge` instead of planning unauthorized OnRe.
- Full-withdraw dry-run: PASS for
  `same-mint-reserve-swap --settings 6jgkucnbz1RuHq6NULqACQY3r2XegHaWhgPpaCEGPCA3
  --vault-index 1 --full-withdraw-reserve
  9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu`, which reported
  `full_withdraw_reserve_dry_run`, `sendsTransactions: false`, protected
  withdraw signer/fee payer `oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C`,
  wallet recovery and policy close signer/fee payer
  `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`, reserve
  `9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu`, amount `819434`, and no
  preflight blockers. The dry-run also reported policy account rent, vault/ATA
  rent, wallet USDC before balance, and close/remove-policy transaction packet
  summaries.
- Live stale-position preflight: PASS for `same-mint-monitor-e2e --execute`
  before cleanup, which exited before child sends with
  `blocked_existing_position_precondition` because DB current positions showed a
  nonzero Figure position (`819434`) for the selected vault.
- Live cleanup of old Figure position: PASS for `same-mint-reserve-swap
  --full-withdraw-reserve
  9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu --execute`. Protected withdraw
  used optimizer signer `oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C`
  (`3nzruMnzrmkpjpWxNL4CqLsMHiUrXpQJkYzPyh2UZa1VGUuLhcdQqm3FrQcNkE4uuKE7zf8DxTJPud7DHwAQx5L4`),
  wallet recovery used settings authority
  `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`
  (`4Zx7EyWfNXqFFKuhKGUG8GjfRGqoeEMGRKjt4L5oedqUyL8mPhPsM6eX6FvwNUhcRfVUJ5BFS6BGJpRz4Mf72YDd`),
  policy removal used the settings authority
  (`2oEmwT7T6NBSHLPeReXTiAdBx77SeSsfZx4C5ARuGE6WjnCpQqJ3TMHSg3ccZo8p8g9jZSZhZJ5LHgcBHYoftgMe`),
  wallet USDC delta was `1000078`, tracked positions/obligations were zero or
  closed, and policy/vault rows were inactive.
- Live full-flow E2E attempt: FAIL - ran
  `same-mint-monitor-e2e --settings
  6jgkucnbz1RuHq6NULqACQY3r2XegHaWhgPpaCEGPCA3 --vault-index 1 --amount-raw
  1000000 --execute` after cleanup. Setup/deposit completed far enough that DB
  readback showed active vault `340`, policy `83`, policy account
  `HPhDWjk7VDZefbcZfxSbmGYDFYmzwW5pwSx5C8UxfP4N`, and Main reserve position
  `842460`, but monitor attempts never reported `executed` and the E2E command
  timed out. Diagnosis dry-run showed the monitor discovered the vault and
  reconciled all four Safe USDC reserves into snapshot `245`, but skipped with
  `no_eligible_fresh_candidate_data` because every candidate row was stale
  (`freshCandidateCount: 0`, `staleCandidateCount: 4`).
- Live cleanup of timed-out Main position: PASS for `same-mint-reserve-swap
  --full-withdraw-reserve D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59
  --execute`. Protected withdraw used optimizer signer
  `oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C`
  (`3TsdGUXpS7NaGMGRugd5rgQxGxv2Z7nNTKrrNciHxSgpnifrKbrv5WEFYYXc78Lena1H1UTwCMzFQjDDCf8CysPw`),
  wallet recovery used settings authority
  `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`
  (`iDttepnGViZNzvkQoBUNb8D4xaMjCgg68zJvPCZAMHkuD5QNL52dKbPc55yoi7JExzG7NAken5XTj45S6qSAn9j`),
  policy removal used the settings authority
  (`2NV3tWCBDWeq8X7izXmiz59tktLgwNmZdgFc6MW4Cmbj2bPs2H5wYuHrnbgqxgT9co11xFdxgihogsV9WnGBnoyR`),
  wallet USDC delta was `1000000`, policy account was closed, Main obligation
  was closed, vault USDC ATA was closed, `positionCleanupProof` showed all
  tracked positions zero and inactive policy/vault rows, and a post-cleanup
  fleet dry-run reported `discoveredVaultCount: 0`.
- Safe USDC APY refresh: PASS for `kamino-reserve-monitor --once` scoped to
  Safe USDC reserves Main/Figure/Maple/OnRe/Ethena. Timescale readback after
  the refresh showed fresh rows: OnRe 535 bps, Figure 442 bps, Main 372 bps,
  Maple 220 bps, and Ethena 0 bps. Ethena is active Safe USDC but is excluded
  from monitor candidates by `SupportedReserveLatestQuery::safe_usdc` because
  its `total_supply_usd_estimate` was below the 100000 minimum.
- Fresh E2E dry-run: PASS for the same settings/vault/amount command. It
  reported four fresh monitor candidates, Main 372 bps, OnRe
  `AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z` 535 bps, and a 163 bps
  positive edge before setup.
- Fresh E2E dry-run after cleanup: PASS for the same settings/vault/amount
  command. It logged `candidateCount: 4`, `freshCandidateCount: 4`,
  `staleCandidateCount: 0`, Main at 372 bps, OnRe at 535 bps, and a 163 bps
  positive edge. The previewed execute phases still include policy update,
  OnRe obligation setup, Main deposit, fleet monitor
  `--once --all-active-vaults --execute`, and generic full withdrawal from
  OnRe.
- Live full-flow E2E: PASS for `same-mint-monitor-e2e --execute` with settings
  `6jgkucnbz1RuHq6NULqACQY3r2XegHaWhgPpaCEGPCA3`, vault index `1`, and amount
  raw `1000000`.
  - Before setup: DB readback found vault `340` and policy inactive, with no
    nonzero current positions.
  - Policy create/update: `policy_created_and_updated`, policy id `86`, policy
    account `GriLGqFtktyjLG5mvCf2FCGSWknE9BNbBw7nLUuDdwUi`, settings-authority
    signature `5H9UBHAKeSCUqEe7g2tMihH74LGsj2SZvnKfLzN5oyXGcT3miYDHA6b8H2ERCJQJHEFqDgqfqKNttiGMUrXuBSUS`,
    finalized at slot `426564056`.
  - OnRe obligation setup: skipped with `setup_obligation_reserve_skipped_existing`
    because the OnRe obligation already existed; no setup send was needed.
  - Initial deposit: Main USDC Kamino deposit signature
    `3LQqhHQc67FMeEd5XhFnpBnMkGmf7KjshKZmKTo5Cwdv2QZ7kdXUD3FrFcTBzroMzfkjKfBybGUPrZyM3jXQ5E9E`,
    finalized at slot `426564099`, logged Main reserve deposit amount
    `1000000` and obligation collateral `842459`. DB readback after deposit
    showed Main reserve `D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59` with
    amount `842459`, `hasValue: true`, and active policy/vault rows.
  - Monitor pickup: attempt 1 returned selected vault `status: executed`,
    source Main, target OnRe. Confirmed route signature
    `3uwuqggX2WYwtKSfJQLCxTEMLFtKcPFwQ9mazZKKDdScP5nZ2auXxsUxjiLjEh5UbsaRQ3nsrzD9Bz8TsN5HQF8h`
    was finalized at slot `426564208`; Neon decision `211` records status
    `confirmed`, source Main, target OnRe, amount `842459`, and
    `post_snapshot_id: 250`. DB readback after optimization showed OnRe amount
    `842459`, `hasValue: true`.
  - Highest-APY proof: the fresh precondition and monitor selection prove OnRe
    was the highest-APY eligible candidate at execution time. The decision row
    itself currently stores APY bps as zero because `same-mint-reserve-swap`
    records decisions from chain-reconciled current positions, which do not
    carry APY data.
  - Full withdrawal from optimizer-selected OnRe: protected Kamino withdraw
    signature `2mxxUzwGosc8JJgedhcJb3yFqW8oyhVSWDt3EhhFbLvq7t8Nwy5KiBGXBiH4tdYwP19XCVHtwYbksaWr71N68twQ`,
    signer `oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C`, finalized at slot
    `426564233`, with logs for `WithdrawObligationCollateralAndRedeemReserveCollateralV2`
    and account close.
  - Wallet recovery: signature
    `4mdVkF2SjDWzsZVHi8G4pAHuz9U568TRg44vKMyrAvzr5GHQYcu9gyD23g4F3CeHX1CfpGyY1PcJEazyMVhUbpwv`,
    signer `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`, finalized at slot
    `426564237`; E2E evidence reported wallet USDC delta `999998`.
  - Policy removal: signature
    `2ZoGoAtWfvzzwEFmHbJjd9bXCi3JScL2wUg9hZEkXdThGEoCr3tS93Nt16bFrLYSmD9V7NzRs5mPNdeedsRxXwbX`,
    signer `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`, finalized at slot
    `426564241`, and `policyClosed: true`.
  - Cleanup verification: post-withdraw DB readback showed vault `340` and
    policy `86` inactive, delegated signer still
    `oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C`, and Main/Figure/OnRe
    current positions all zero at snapshot `251`. The post-cleanup fleet poll
    returned `discoveredVaultCount: 0`.
- Direct DB readback after the successful E2E: PASS. Neon query for vault `340`
  and decision `211` showed inactive vault/policy rows, policy account
  `GriLGqFtktyjLG5mvCf2FCGSWknE9BNbBw7nLUuDdwUi`, route mode
  `same_mint_kamino`, delegated signer
  `oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C`, confirmed decision signature
  `3uwuqggX2WYwtKSfJQLCxTEMLFtKcPFwQ9mazZKKDdScP5nZ2auXxsUxjiLjEh5UbsaRQ3nsrzD9Bz8TsN5HQF8h`,
  and zero current positions for Main/Figure/OnRe.
- Direct DB readback after cleanup refresh: PASS. A read-only Neon query showed
  vault `340` inactive, active policy id `86`, policy `86` inactive, policy
  account `GriLGqFtktyjLG5mvCf2FCGSWknE9BNbBw7nLUuDdwUi`, route mode
  `{same_mint_kamino}`, delegated signer
  `{oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C}`, latest decision `211`
  confirmed from Main to OnRe for amount `842459`, confirmed slot `426564209`,
  post snapshot `250`, and Main/Figure/OnRe current positions all zero with
  `has_value = false` at snapshot `251`.
- Post-cleanup fleet dry-run refresh: PASS for `same-mint-yield-monitor --once
  --all-active-vaults`. It returned `status: fleet_poll`, `execute: false`,
  `candidateCount: 4`, `discoveredVaultCount: 0`, and `results: []`, proving
  the inactive vault/policy rows are no longer monitored.
- Render rollout: PASS for dry-run fleet mode. The `worker-images` GitHub
  Actions run `27525329003` built and pushed the light-worker image for commit
  `d3497113aed8fedb83dbaa3ea398f40ac58aab37`; the light-worker matrix job
  completed successfully at `2026-06-15T05:15:33Z`. Render deploy
  `dep-d8nom81kh4rs73fe3td0` is live on
  `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-d3497113aed8fedb83dbaa3ea398f40ac58aab37`
  with image digest
  `sha256:b34feb49ef99616b91570f248fde65cc257523b45c8a3f606c3249c908adfa5b`.
  Service readback for `srv-d8n7gqbbc2fs73emk610` shows the command
  `/usr/local/bin/same-mint-yield-monitor --all-active-vaults
  --poll-interval-seconds 300`. The service env-var names are
  `NEON_DATABASE_URL`, `TIMESCALEDB_URL`, `SOLANA_RPC_URL`,
  `YIELD_ROUTER_KEYPAIR`, and `RUST_LOG`; `SOLANA_TESTING_PK` is absent.
- Render dry-run log: PASS. The first post-deploy worker poll at
  `2026-06-15T05:20:03Z` reported `status: fleet_poll`, `execute: false`,
  `allActiveVaults: true`, `candidateCount: 4`, `discoveredVaultCount: 0`,
  `pollIntervalSeconds: 300`, and `results: []`. There were no per-vault
  Render results because the verified E2E full withdrawal had already marked the
  selected vault and policy inactive. The local live E2E above proves the
  per-vault pickup/execution path; this Render dry-run proves the deployed
  worker is now in fleet mode and will ignore the cleaned-up vault.
- Current Verdict:
  Fleet Discovery: PASS - live fleet discovered the active policy/vault during
  the E2E and returned no rows after cleanup.
  Reconciled Planning Source: PASS - monitor reconciled chain positions before
  planning and optimization.
  Per-Vault Isolation: PASS - covered by focused tests and fleet result shape.
  Optimizer Signer Boundary: PASS - route execution and protected withdraw used
  `YIELD_ROUTER_KEYPAIR`; setup/deposit/wallet recovery/policy removal used
  `SOLANA_TESTING_PK`.
  Fail-Closed Missing Obligation: PASS - covered by tests and setup preflight
  behavior; live target obligation existed.
  Generic Full Withdrawal: PASS - live cleanup withdrew from OnRe, closed
  policy/ATA/obligation state where closeable, reconciled zeros, and deactivated
  rows.
  Live Full-Flow E2E: PASS - real deposit, monitor pickup, best-reserve move,
  and full withdrawal completed.
  Render Rollout Shape: PASS - deployed dry-run worker uses the pinned
  light-worker image, fleet command, `YIELD_ROUTER_KEYPAIR`, and no
  `SOLANA_TESTING_PK`.
  Local Checks: PASS - see local command results above.
  Required Live Evidence: PASS - command output, direct DB readback, and Solana
  RPC confirmation summaries recorded above.
  Overall Verdict: PASS
