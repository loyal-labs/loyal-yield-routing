# Same-Mint Safe USDC Monitor Verifier Plan

Use this document as the fixed verifier for the same-mint Safe USDC monitor work.
Do not treat it as an implementation checklist. The implementation is done only
when the required checks below can be run against the repo, databases, chain
state, and Render worker state and all required checks pass.

## Goal

Build and verify a monitor/executor that starts from the vault attached to
`SOLANA_TESTING_PK`, keeps funds in same-mint USDC routes, reads Safe-basket
Kamino reserve APY from TimescaleDB, moves the full USDC position into the
highest eligible Safe USDC reserve, and deploys the monitor as a pinned Render
light worker after the local end-to-end proof succeeds.

## Scope

Required v1 scope is intentionally narrow. Candidate reserves are USDC only, and
candidate markets come from active Safe-basket Kamino rows in Timescale
`kamino.supported_reserves`. The selected vault is the active managed vault whose
active route policy authority matches the pubkey derived from `SOLANA_TESTING_PK`,
unless the verifier command explicitly supplies `--settings` and `--vault-index`.

Setup uses `SOLANA_TESTING_PK` as the settings authority and funding identity.
Same-mint route execution uses `YIELD_ROUTER_KEYPAIR` as the delegated policy
signer. Neon, reached through `NEON_DATABASE_URL`, is the control-plane source of
truth. TimescaleDB, reached through `TIMESCALEDB_URL`, is the market-data source.

Passing this verifier does not require cross-mint movement, fee-aware APY,
rolling-window optimization, broad multi-vault scanning, or broad test cleanup
before the local end-to-end route is proven.

## Commands Under Verification

Run the implementation commands through:

```sh
op run --env-file=.env.1password -- sh -c '<command>'
```

The implementation must expose commands equivalent to these surfaces:

```sh
cargo run -p loyal-yield-orchestrator --bin same-mint-reserve-swap -- --deposit-main-usdc <AMOUNT_RAW> --reconcile-from-chain --execute
```

```sh
cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor -- --once
```

```sh
cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor -- --once --execute
```

If the final command names differ, update this section only when the replacement
commands preserve the same observable behavior.

## Required Checks

### 1. Setup Deposit

PASS only if a local setup run can leave the selected vault in this state:

- An active route policy exists for the selected settings and vault index.
- The policy authority equals the pubkey derived from `SOLANA_TESTING_PK`.
- The policy delegated signer allowlist includes `YIELD_ROUTER_KEYPAIR`.
- The vault has a confirmed Kamino Main Market USDC position on chain.
- `loyal_yield.vault_reserve_positions_current` matches the confirmed chain
  position after reconciliation.
- No Neon position row is updated before chain confirmation.

Evidence to record:

Record the settings pubkey, vault index, vault pubkey, policy account, deposit
signature, and reconciled position details.

### 2. Safe USDC Candidate Selection

PASS only if a monitor dry-run reads TimescaleDB and prints the eligible reserve
set before choosing a target.

The candidate query must be equivalent to:

```sql
SELECT
  l.observed_at,
  l.reserve,
  l.market,
  l.market_name,
  l.liquidity_mint,
  l.symbol,
  l.supply_apy,
  l.total_supply_usd_estimate
FROM kamino.supported_reserves sr
JOIN kamino.latest_reserve_updates l
  ON l.reserve = sr.reserve
 AND l.market = sr.market
 AND l.liquidity_mint = sr.liquidity_mint
WHERE sr.active = true
  AND 'safe' = ANY(sr.risk_baskets)
  AND sr.liquidity_mint = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v'
  AND l.reserve_last_update_stale = false
  AND l.total_supply_usd_estimate > 100000
  AND l.supply_apy >= 0
  AND l.supply_apy < 0.5
ORDER BY l.supply_apy DESC, l.observed_at DESC;
```

The dry-run must fail or skip safely if candidate data is stale according to the
implemented freshness limit. It must not execute from stale APY data.

Evidence to record:

Record the full candidate list, selected reserve, selected APY, current vault
reserve, current APY, and skip reason if no move is planned.

### 3. Local Execute

PASS only if a local execute run moves the selected vault from its current Safe
USDC reserve to the highest eligible Safe USDC reserve when that target differs
and the APY edge is positive.

The execute run must:

- Reconcile current chain state before planning.
- Create or reuse exactly one active same-mint rebalance decision.
- Initialize the target market obligation through the approved setup path if it
  is missing.
- Execute the protected withdraw and deposit route through the active policy.
- Confirm the submitted transaction on chain.
- Finalize the decision in Neon only after confirmation.
- Reconcile the final chain position into
  `loyal_yield.vault_reserve_positions_current`.

Evidence to record:

Record the decision id, source reserve, target reserve, raw amount, route
transaction signature, final decision status, and final current-position row.

### 4. Idempotent Second Run

PASS only if a second monitor run after a successful move does not create a new
active decision for the same vault and same target.

The second run must print one of these safe skip reasons:

- already at the selected reserve
- no positive APY edge
- active decision already exists
- no eligible fresh candidate data

Evidence to record:

- before and after count of active decisions for the vault
- printed skip reason

### 5. Render Dry-Run Deployment

PASS only if the monitor is deployable as a Render light worker without changing
the existing pinned-image workflow.

Required Render evidence:

Render evidence must show that `Dockerfile.light-workers` copies the monitor
binary into the runtime image, `render.yaml` defines the light-worker service or
command, and the service uses a pinned
`ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-...` image rather than
`latest`. Required secrets must be `sync: false` and absent from logs. Render logs
must show dry-run candidate selection plus a skip or decision result. Execution
mode can be enabled only after the local end-to-end proof passes and the operator
approves the live send.

### 6. Focused Tests After Proof

PASS only if focused tests are added after the local end-to-end proof succeeds.
Do not block the proof on broad test cleanup.

Required focused coverage:

Focused coverage must include Safe USDC candidate filtering, highest-APY
selection, `SOLANA_TESTING_PK` authority-based vault resolution, stale candidate
skips, idempotent second runs, and active-decision duplicate prevention.

Required checks before commit:

```sh
NO_DNA=1 cargo fmt --check
```

```sh
NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin same-mint-yield-monitor
```

Run additional narrow tests only for modules changed by the implementation.

## Verdict Format

For each verification run, report:

```text
Setup Deposit: PASS|FAIL - note
Candidate Selection: PASS|FAIL - note
Local Execute: PASS|FAIL - note
Idempotent Second Run: PASS|FAIL - note
Render Dry-Run Deployment: PASS|FAIL - note
Focused Tests After Proof: PASS|FAIL - note
Overall Verdict: PASS|FAIL
```

Overall verdict is `PASS` only when every required section passes. If any section
fails, keep this verifier unchanged and plan the smallest next change needed to
make the failing section pass. Revise this verifier only if it misstates the real
goal, and state the reason before changing it.
