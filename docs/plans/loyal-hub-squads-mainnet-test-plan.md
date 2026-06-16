# Loyal Hub Squads Mainnet Test Tooling Plan

## Summary

Add operational SDK and CLI coverage for the Loyal Hub Squads flows currently proven in `crates/squads-test-harness`. The goal is to reproduce the important behavior on devnet/mainnet with guarded, small-value smoke tests:

- user Squads vault creation and all-in-one yield-route policy creation
- user policy execution through Kamino deposits, Loyal Hub swaps, and optional Jupiter swaps
- treasury Squads vault creation for global Loyal Hub inventory replenish through Jupiter
- native Loyal Hub lane rebalance through Squads
- active-lane scheduler rejection before any live rebalance is submitted

Mainnet execution must remain guarded: simulate first, require `CONFIRM_MAINNET=1` for live funds, use tiny raw token amounts by default, and assert exact pre/post balance deltas.

## Existing Surfaces

- `packages/loyal-actions` already builds the all-in-one policy constraints through `createLoyalActionsSdk().initYieldRoutePolicy(...)`.
- `crates/loyal-hub-cli` already supports direct `rebalance-inventory`.
- `scripts/devnet-test-loyal-hub-swap-program.sh` already performs Hub state checks, user funding, swaps, rebalances, and cleanup.
- `scripts/jupiter-hub-rebalance.mjs` already performs the treasury-side Hub withdraw -> Jupiter swap -> Hub top-up shape, but not through a Squads vault.
- The LiteSVM source-of-truth scenarios live in:
  - `crates/squads-test-harness/tests/loyal_hub_swap.rs`
  - `crates/squads-test-harness/tests/usdc_pyusd_kamino_route.rs`
  - `crates/squads-test-harness/tests/loyal_hub_lane_simulation.rs`

## SDK Additions

Extend `packages/loyal-actions` with public builders for the operational pieces that are currently internal or test-local:

- Squads PDA helpers:
  - derive settings from seed
  - derive vault from settings and vault index
  - derive policy/action PDA from settings and policy seed
- Squads smart-account creation instruction helper:
  - caller must provide the live Squads treasury/program-config-derived treasury input
  - do not reuse the LiteSVM-only test treasury assumption
- Squads sync transaction builders:
  - compile arbitrary inner instructions
  - execute them through a vault using Squads sync transaction payloads
- ProgramInteraction policy execution builders:
  - execute all-in-one route policies with caller-provided withdraw/swap/deposit instructions
  - expose instruction constraint index selection for same-mint, Jupiter, and Loyal Hub routes
- Active-lane scheduler helper:
  - reject any planned rebalance whose source or destination lane is in the active swap lane set
  - error text should include `active lane`, matching the LiteSVM simulation expectation

Keep live Kamino instruction construction out of scope for this pass unless an existing reliable Kamino client surface is already available. The SDK should accept caller-provided Kamino/Jupiter instructions and make the Loyal/Squads wrapping deterministic.

## CLI Additions

Add a Bun operational CLI, tentatively `scripts/loyal-hub-squads-ops.ts`, with these commands:

- `create-vault`
  - creates a user or treasury Squads vault
  - prints `settings`, `vault`, `vaultIndex`, signer, and transaction signature or simulation result
- `create-all-in-one-policy`
  - creates a user all-in-one ProgramInteraction policy for Kamino withdraw/deposit plus selected swap lanes
  - supports `--swap-lanes loyal,jupiter`, `--risk`, `--max-fee-bps`, and Squads context args
- `execute-policy-route`
  - executes an all-in-one route through the policy using supplied serialized instruction bundles
  - supports Loyal Hub route execution with the Hub authorizer signer
- `treasury-jupiter-rebalance`
  - wraps Hub withdraw -> Jupiter swap -> Hub top-up in a treasury Squads sync transaction
  - reuses the quote/swap-instruction logic from `scripts/jupiter-hub-rebalance.mjs`
  - asserts exact Hub and treasury balance deltas
- `check-active-lane-rebalance`
  - pure preflight command for active-lane rejection
  - exits nonzero with an `active lane` error when a rebalance touches active lanes

Extend `crates/loyal-hub-cli` only if the Bun CLI cannot cleanly reuse the SDK for native Hub lane rebalance:

- `squads-rebalance-inventory`
  - same transfer syntax as existing `rebalance-inventory`
  - executes through a Squads vault configured as Hub `inventory_rebalancer`
  - simulates and reports balance changes through the existing Hub CLI transaction report shape

## Scenario Mapping

### `treasury_backed_simulation_covers_hub_jupiter_and_inventory_movement`

Operational coverage:

- create user Squads vault and treasury Squads vault
- fund Hub hot inventory with small USDC/PYUSD amounts
- execute:
  - full Hub fill
  - mixed Hub/Jupiter fill
  - Jupiter-only fill
- execute treasury rebalance through Squads:
  - withdraw USDC from Hub
  - swap through Jupiter
  - top up PYUSD into Hub inventory
- execute final treasury withdraw
- assert final user, treasury, and Hub balances

### `wallet_b_can_execute_all_in_one_policy_with_loyal_hub_swap_lane`

Operational coverage:

- create user Squads vault
- create all-in-one policy with Loyal Hub swap lane and optional Jupiter lane
- deposit USDC into the starting Kamino reserve through Squads sync execution
- execute one policy route:
  - Kamino withdraw
  - Loyal Hub USDC -> PYUSD swap on lane `0`
  - Kamino PYUSD deposit
- require Hub authorizer signer
- assert:
  - vault USDC/PYUSD token balances are `0`
  - source collateral is `0`
  - target PYUSD collateral equals expected Hub output

### `thirty_wallets_swap_across_hub_lanes_with_rebalance`

Operational coverage:

- support configurable wallet count and lane count
- default live mainnet run should use much smaller amounts than the LiteSVM stress case
- execute wave one across lanes with alternating directions
- reject active-wave rebalance locally before submission:
  - USDC lane `0 -> 2`
  - PYUSD lane `1 -> 3`
- after maintenance-window preflight passes, execute the same rebalances through Squads
- execute wave two on lanes `2` and `3`
- assert chain balances and total conservation over the smoke ledger

### `simulation_rejects_rebalance_on_active_swap_lane`

Operational coverage:

- use the scheduler helper directly
- active lanes: `1` and `3`
- proposed rebalance: USDC lane `0 -> 3`
- assert rejection contains `active lane`
- do not submit a live transaction for this negative case

## Validation Plan

Local/static gates:

- `bun run loyal-actions:typecheck`
- `bun run --cwd packages/loyal-actions test`
- `cargo check -p loyal-hub-cli -p loyal-actions`

Squads/Hub behavior gates:

- `bun run test:squads` when shared Squads/Hub instruction semantics change
- focused CLI `--simulate` runs for vault creation, policy creation, route execution, treasury rebalance, and lane rebalance

Mainnet acceptance:

- all live mainnet commands require `CONFIRM_MAINNET=1`
- use `op run --env-file=.env.1password -- sh -c '<command>'`
- every live step must:
  - simulate first
  - confirm the transaction
  - fetch fresh state after confirmation
  - assert exact raw-token deltas

## Assumptions

- Mainnet smoke uses real but tiny USDC/PYUSD raw amounts.
- Treasury and user Squads vaults are separate unless a test explicitly exercises shared ownership.
- Live Kamino route instructions are supplied by an existing Kamino-aware caller/tooling layer in this pass.
- Negative malformed-account cases stay in LiteSVM; live tests only run safe preflight/scheduler negatives.
- No plaintext secrets are written to env files, source files, command arguments, logs, or chat.
