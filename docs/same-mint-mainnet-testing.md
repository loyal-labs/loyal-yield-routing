# Same-Mint Mainnet Testing Checklist

This document explains what is still needed to test same-mint USDC yield routing
on mainnet with `crates/loyal-yield-orchestrator`.

The implementation can already plan a route, prepare Kamino redeem/deposit
instructions, simulate the Squads policy execution transaction, optionally submit
it, and store the result in the orchestrator database. What remains for mainnet
testing is the operational state around that runner: database rows, account
configuration, APY input, and a carefully staged submit switch.

## Target Flow

For the first mainnet test, keep the route limited to USDC:

```text
EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v
```

The expected test flow is:

1. The orchestrator receives APY data for at least two Kamino USDC reserves.
2. The loop selects the reserve with the highest supply APY as the target.
3. The store finds active vaults that hold another reserve for the same mint.
4. The planner creates one same-mint rebalance decision.
5. The preparer builds a Squads `ProgramInteraction` instruction containing:
   - redeem collateral from the source Kamino reserve;
   - deposit the resulting USDC liquidity into the target Kamino reserve.
6. The executor simulates the transaction on mainnet RPC.
7. Submission happens only if `SAME_MINT_SUBMIT_TXS=true`.
8. Simulation and submission results are persisted in the orchestrator DB.

## Environment

Use the 1Password-mounted env file. Do not place plaintext secrets in `.env`,
source files, command arguments, logs, or chat.

Required:

```text
DATABASE_URL or NEON_DATABASE_URL
SOLANA_RPC_URL
YIELD_ROUTER_KEYPAIR
SAME_MINT_ROUTE_CONFIG_JSON or SAME_MINT_ROUTE_CONFIG_PATH
```

For a one-shot dry run, also set:

```text
SAME_MINT_RESERVE_APYS_JSON
```

For a Timescale-triggered smoke run, also set:

```text
TIMESCALEDB_URL
SAME_MINT_TIMESCALE_SYMBOLS=USDC
```

Useful optional filters:

```text
SAME_MINT_TIMESCALE_RESERVES=<reserve_a>,<reserve_b>
SAME_MINT_TIMESCALE_MARKETS=<market_a>,<market_b>
SAME_MINT_TIMESCALE_CHANGED_FIELDS=supply_apy
SAME_MINT_TIMESCALE_MIN_SUPPLY_USD=<minimum_supply_usd>
SAME_MINT_WATCH_ONCE=true
SAME_MINT_WATCH_TIMEOUT_SECS=60
```

Planner and batch controls:

```text
SAME_MINT_MIN_EDGE_BPS=1
SAME_MINT_ESTIMATED_COST_LAMPORTS=0
SAME_MINT_BATCH_SIZE=1
```

Keep this unset or false until dry-run simulation has been reviewed:

```text
SAME_MINT_SUBMIT_TXS=true
```

## Database State

The orchestrator migrations must be applied to the database used by
`DATABASE_URL` or `NEON_DATABASE_URL`.

The same-mint candidate query needs these current rows:

- an active row in `loyal_yield.managed_vaults`;
- an active policy referenced by that vault's `active_policy_id`;
- current source and target rows in
  `loyal_yield.vault_reserve_positions_current`;
- source row has `has_value = true` and `amount_raw > 0`;
- source and target rows share the USDC liquidity mint;
- target row exists for the target reserve selected by APY.

For Kamino, `amount_raw` must represent the source collateral-token amount that
the policy will redeem. It should not be a UI-scaled USDC amount unless the
collateral exchange rate makes that intentionally correct.

The route loop writes to the decision and execution tables as it progresses.
For dry runs, a successful simulation leaves decisions ready for submission and
records the preflight slot. Submission results are only written after
`SAME_MINT_SUBMIT_TXS=true`.

## Policy And Signer State

The Squads smart account and ProgramInteraction policy must already exist on
mainnet.

The active policy must allow the two Kamino legs used by the route config:

- source reserve collateral redeem;
- target reserve liquidity deposit.

The signer loaded from `YIELD_ROUTER_KEYPAIR` must be:

- funded for transaction fees;
- present in the policy's delegated signer set;
- authorized for the constraint indexes referenced by the route config.

## Route Config

`SAME_MINT_ROUTE_CONFIG_JSON` or `SAME_MINT_ROUTE_CONFIG_PATH` must provide the
exact account mapping for each source/target reserve pair.

Shape:

```json
{
  "routes": [
    {
      "vault_id": 1,
      "source_reserve": "<source_kamino_reserve>",
      "target_reserve": "<target_kamino_reserve>",
      "liquidity_mint": "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
      "policy_account": "<squads_program_interaction_policy>",
      "delegated_signer": "<delegated_signer_pubkey>",
      "vault_index": 0,
      "vault": "<squads_vault_pubkey>",
      "withdraw_constraint_index": 0,
      "deposit_constraint_index": 1,
      "source_accounts": {
        "reserve": "<source_reserve>",
        "market": "<source_market>",
        "lending_market_authority": "<source_market_authority>",
        "liquidity_mint": "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
        "reserve_liquidity_supply": "<source_reserve_liquidity_supply>",
        "collateral_mint": "<source_collateral_mint>",
        "vault_liquidity": "<vault_usdc_token_account>",
        "vault_collateral": "<vault_source_collateral_token_account>"
      },
      "target_accounts": {
        "reserve": "<target_reserve>",
        "market": "<target_market>",
        "lending_market_authority": "<target_market_authority>",
        "liquidity_mint": "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
        "reserve_liquidity_supply": "<target_reserve_liquidity_supply>",
        "collateral_mint": "<target_collateral_mint>",
        "vault_liquidity": "<vault_usdc_token_account>",
        "vault_collateral": "<vault_target_collateral_token_account>"
      },
      "quote": {
        "redeem_collateral_to_liquidity_bps": 10000,
        "deposit_liquidity_bps": 10000,
        "max_redeem_collateral_raw": 1000000,
        "min_deposit_liquidity_raw": 1
      }
    }
  ]
}
```

The quote fields are guardrails used by the static route preparer:

- `redeem_collateral_to_liquidity_bps` estimates liquidity received from
  collateral redemption;
- `deposit_liquidity_bps` can reserve a margin before deposit;
- `max_redeem_collateral_raw` caps the route amount for mainnet testing;
- `min_deposit_liquidity_raw` prevents tiny or rounded-to-zero deposits.

Start with a very small `max_redeem_collateral_raw` for the first submit test.

## One-Shot Dry Run

Use the manual APY runner when you want deterministic input without waiting for
Timescale notifications:

```bash
op run --env-file=.env.1password -- sh -c 'DATABASE_URL="$NEON_DATABASE_URL" cargo run -p loyal-yield-orchestrator --bin same_mint_route_runner'
```

`SAME_MINT_RESERVE_APYS_JSON` must contain at least two reserves for the USDC
mint. APYs are integer basis points:

```json
[
  {
    "reserve": "<source_reserve>",
    "liquidity_mint": "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
    "supply_apy_bps": 250,
    "borrow_apy_bps": 0
  },
  {
    "reserve": "<target_reserve>",
    "liquidity_mint": "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
    "supply_apy_bps": 420,
    "borrow_apy_bps": 0
  }
]
```

Expected dry-run result:

- the JSON report has the target reserve with maximum APY;
- at least one candidate vault is found;
- a planned decision advances through simulation;
- `submitted_batches` stays empty while `SAME_MINT_SUBMIT_TXS` is unset;
- the DB records simulation metadata and a preflight chain slot.

## Timescale-Triggered Smoke Run

Use the watcher when `loyal-labs/kamino-streaming-apy` is writing reserve
updates into TimescaleDB.

```bash
op run --env-file=.env.1password -- sh -c 'DATABASE_URL="$NEON_DATABASE_URL" SAME_MINT_WATCH_ONCE=true cargo run -p loyal-yield-orchestrator --bin same_mint_route_watcher'
```

The watcher listens for Kamino APY updates, fetches the latest matching rows,
converts fractional APY values to basis points, and runs the same route loop.

Use `SAME_MINT_WATCH_ONCE=true` for the first smoke test so the process exits
after one notification and prints one report.

## Submission Gate

Only submit after a dry run has proven:

- the candidate vault is the intended vault;
- the source and target reserves are correct;
- the route amount is below the intended cap;
- simulation succeeds on the current mainnet slot;
- the delegated signer and policy account match the reviewed policy;
- the vault token accounts in route config match the actual Squads vault ATAs.

Then set:

```text
SAME_MINT_SUBMIT_TXS=true
```

Run the same command again. The executor will call
`send_and_confirm_transaction` and persist submission results.

## What Is Still Manual

These pieces are not automated by the current mainnet runner:

- discovering live Squads ProgramInteraction policies and inserting
  `route_policies`;
- reconciling live Squads vault Kamino positions into
  `vault_reserve_positions_current`;
- discovering and validating all Kamino reserve accounts used by route config;
- deriving conservative quote ratios from live Kamino reserve state;
- creating a production operator flow that reviews a dry-run report before
  enabling submission.

Until those are automated, mainnet testing requires reviewed manual DB state and
reviewed manual route config.

## Current Local Harness Blocker

The local-validator harness exists for end-to-end testing without mainnet state,
but the current run is blocked before policy execution. Agave 3.1.12 and 3.1.13
load the Squads and mock Kamino program accounts at genesis, yet the first
Squads instruction fails with `Unsupported program id` / `Program is not
deployed`.

That does not block mainnet dry-run testing, because mainnet uses the deployed
Squads program. It does mean the local harness still needs either a compatible
Squads SBF fixture or a validator/runtime pairing that can execute the existing
fixture.
