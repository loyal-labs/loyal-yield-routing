# Same-Mint Yield Routing Implementation Report

This report summarizes the same-mint yield-routing implementation added to
`crates/loyal-yield-orchestrator`. The current flow targets Kamino reserve
switching where the source and target reserves share the same liquidity mint,
for example USDC on mainnet.

## Scope

The implementation adds a pre-production route path that can:

1. Select the reserve with the best supplied APY.
2. Find active vaults that currently hold a different reserve for the same mint.
3. Plan one rebalance decision per vault.
4. Prepare Kamino redeem and deposit instructions for that decision.
5. Simulate the batched route transaction on Solana RPC.
6. Optionally submit the same batch when submission is explicitly enabled.
7. Store simulation and submission results in the orchestrator database.

Submission is disabled by default. Dry runs can stop after simulation so mainnet
testing does not mark decisions failed just because transaction submission was
intentionally withheld.

## Added Modules

### `kamino.rs`

Builds Kamino reserve instructions and policy-safe payload fragments:

- `KaminoReserveInstructionAccounts`
- `kamino_redeem_reserve_collateral_policy_payload`
- `kamino_deposit_reserve_liquidity_policy_payload`
- direct Kamino `Instruction` builders for lower-level uses

The policy payload builders compile Kamino instruction account tables without
marking the vault as an outer transaction signer. The Squads policy execution
path supplies the delegated signer and vault context separately.

### `same_mint_loop.rs`

Owns the routing loop:

- chooses the max-APY target reserve;
- asks the store for same-mint candidate vaults;
- plans rebalance decisions;
- requests quotes and executable route instructions from an executor;
- simulates batches;
- submits batches when enabled;
- advances decision state through the database.

`SameMintRoutingLoopConfig::submit_batches` controls dry-run behavior. When it is
`false`, successfully simulated decisions remain `ready` with their preflight
slot recorded.

### `mainnet_same_mint_executor.rs`

Implements `SameMintRouteExecutor` with real Solana RPC transaction simulation
and submission:

- loads `SOLANA_RPC_URL`;
- signs with `YIELD_ROUTER_KEYPAIR`;
- rejects empty route instruction batches;
- calls `simulate_transaction`;
- calls `send_and_confirm_transaction` only when `SAME_MINT_SUBMIT_TXS=true`.

The executor does not invent quote math or account mappings. It requires a
`SameMintRoutePreparer` to supply both quote values and executable instructions.

### `same_mint_preparer.rs`

Adds `ConfiguredSameMintRoutePreparer`, a concrete `SameMintRoutePreparer`
implementation. It loads route config from `SAME_MINT_ROUTE_CONFIG_JSON` or
`SAME_MINT_ROUTE_CONFIG_PATH` and prepares one Squads `ProgramInteraction`
instruction containing:

1. Kamino redeem collateral from the source reserve.
2. Kamino deposit liquidity into the target reserve.

The route config includes:

- source and target reserve labels;
- liquidity mint;
- optional vault id;
- Squads ProgramInteraction policy account;
- delegated signer;
- vault index and vault pubkey;
- withdraw/deposit constraint indexes;
- source and target Kamino account sets;
- basis-point quote assumptions and optional route caps.

The preparer fails closed when a matching route is missing, quote bps are
invalid, the quote rounds to zero, the amount exceeds a configured cap, or the
compiled Squads payload cannot be encoded.

### `src/bin/same_mint_route_runner.rs`

Adds a binary runner that wires:

- `DATABASE_URL` or `NEON_DATABASE_URL`;
- `SOLANA_RPC_URL`;
- `YIELD_ROUTER_KEYPAIR`;
- `SAME_MINT_RESERVE_APYS_JSON`;
- `SAME_MINT_ROUTE_CONFIG_JSON` or `SAME_MINT_ROUTE_CONFIG_PATH`;
- optional planner and batch-size env vars.

It prints the final `SameMintRoutingLoopReport` as JSON.

## Store And State Changes

The orchestrator store now exposes `same_mint_candidate_vaults`, which finds
active vaults holding a non-target reserve for the same liquidity mint while the
target reserve is present in that vault's current reserve universe.

Decision state transitions now record `preflight_chain_slot` when simulation
completes. That gives dry-run testing a durable chain-slot marker before any
transaction is submitted.

## Mainnet USDC Testing Setup

For a USDC mainnet dry run, configure:

- `DATABASE_URL="$NEON_DATABASE_URL"` through `op run`.
- `SOLANA_RPC_URL` pointing to a mainnet RPC endpoint.
- `YIELD_ROUTER_KEYPAIR` as the delegated signer hex keypair.
- `SAME_MINT_RESERVE_APYS_JSON` containing at least two USDC reserves with APY
  bps.
- `SAME_MINT_ROUTE_CONFIG_JSON` or `SAME_MINT_ROUTE_CONFIG_PATH` containing the
  Kamino/Squads account route config.
- current vault snapshots and reserve positions in the orchestrator DB.
- an active Squads ProgramInteraction policy detected by the policy monitor.

Dry run command:

```bash
op run --env-file=.env.1password -- sh -c 'DATABASE_URL="$NEON_DATABASE_URL" cargo run -p loyal-yield-orchestrator --bin same_mint_route_runner'
```

Set `SAME_MINT_SUBMIT_TXS=true` only after simulation succeeds and the route
config has been reviewed.

## Verification

Commands run during implementation:

```bash
cargo fmt --package loyal-yield-orchestrator --check
cargo test -p loyal-actions
```

An isolated source-file harness was also used to compile and test the new
orchestrator modules and runner without requiring SQLx online query validation.
It passed 15 tests covering Kamino payloads, the same-mint loop, the mainnet
executor, the configured preparer, and the runner target.

The full command below still requires a live `DATABASE_URL` or a SQLx prepared
query cache because this crate uses SQLx query macros:

```bash
cargo test -p loyal-yield-orchestrator --lib
```

## Remaining Production Hardening

The configured preparer is intentionally explicit and reviewable, but it still
expects quote ratios and Kamino accounts to be supplied externally. The next
hardening step is to replace the static quote bps with a live Kamino reserve
state decoder that converts collateral shares to redeemable liquidity and then
validates deposit outcomes from current reserve state.
