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

### `timescale_same_mint.rs`

Maps Kamino TimescaleDB reserve rows into same-mint routing APY inputs:

- reads rows from `loyal-yield-router`'s `TimescaleRouterClient`;
- converts fractional APY values like `0.0521` into bps (`521`);
- exposes env names for the local Kamino APY stream contract;
- provides small parsing helpers for watcher filters.

This keeps the APY source compatible with
`loyal-labs/kamino-streaming-apy`, whose local TimescaleDB sink writes
`kamino.reserve_updates`, maintains `kamino.latest_reserve_updates`, and emits
`LISTEN/NOTIFY` updates on `kamino_reserve_updates`.

### `src/bin/same_mint_route_watcher.rs`

Adds a Timescale-triggered runner. It wires the orchestrator DB, Solana RPC
executor, configured route preparer, and Kamino APY subscription:

- `DATABASE_URL` or `NEON_DATABASE_URL`
- `TIMESCALEDB_URL`
- optional `TIMESCALEDB_SCHEMA` and `TIMESCALEDB_NOTIFY_CHANNEL`
- optional `SAME_MINT_TIMESCALE_SYMBOLS`, `SAME_MINT_TIMESCALE_RESERVES`,
  `SAME_MINT_TIMESCALE_MARKETS`, and changed-field filters
- normal route config/signing env used by `same_mint_route_runner`

When an APY notification arrives, the watcher fetches the latest matching
reserve rows, maps them into `SameMintReserveApy`, runs the same-mint loop, and
prints the JSON report. Set `SAME_MINT_WATCH_ONCE=true` for harness or smoke
runs.

### `src/bin/same_mint_local_validator_e2e.rs`

Adds an opt-in local harness that attempts the full requested flow:

1. Starts a temporary Homebrew Timescale/Postgres cluster under `/private/tmp`.
2. Applies orchestrator migrations and a local Kamino `reserve_updates` schema.
3. Starts `solana-test-validator`.
4. Preloads mock Kamino reserve, market, mint, collateral, and token accounts.
5. Loads the Squads fixture and mock Kamino SBF program at their production IDs.
6. Creates a Squads smart account and same-mint ProgramInteraction policy.
7. Seeds an initial Main USDC Kamino deposit.
8. Records policy and vault state in the orchestrator DB.
9. Inserts a Prime USDC APY update into Timescale.
10. Subscribes to the APY notification, runs the same-mint route, and checks DB
    submission plus token balances.

The harness is deliberately a binary rather than a default test because it
starts local services and depends on SBF/runtime compatibility.

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

## Local Validator Setup

The local path avoids mainnet env and DB state by using generated state:

```bash
cargo build-sbf -- -p mock-yield-protocols-program
DATABASE_URL=postgres://zotho@127.0.0.1:15432/loyal_check \
  cargo run -p loyal-yield-orchestrator --bin same_mint_local_validator_e2e
```

`cargo build-sbf` currently exits nonzero after building the mock Kamino artifact
because workspace post-processing also looks for `loyal_hub_swap_program.so`.
The needed mock artifact is still produced at:

```text
target/sbpf-solana-solana/release/mock_yield_protocols_program.so
```

The harness also accepts:

- `SAME_MINT_LOCAL_VALIDATOR_BIN` to choose a specific installed validator.
- `SAME_MINT_LOCAL_VALIDATOR_RPC_PORT` and
  `SAME_MINT_LOCAL_VALIDATOR_FAUCET_PORT` to avoid port conflicts.

Current local-validator blocker found during testing: Agave 3.1.12 and 3.1.13
load the Squads and mock Kamino program accounts and programdata accounts at
genesis, but invoking the Squads fixture fails during simulation with
`Unsupported program id` / `Program is not deployed`. The harness prints the
program account and programdata metadata before setup transactions so this is
visible. The likely missing piece is a Squads SBF fixture built for the installed
Agave runtime, or a compatible validator release for the existing fixture.

## Verification

Commands run during implementation:

```bash
DATABASE_URL=postgres://zotho@127.0.0.1:15432/loyal_check cargo check -p loyal-yield-orchestrator --bins --tests
DATABASE_URL=postgres://zotho@127.0.0.1:15432/loyal_check cargo test -p loyal-yield-orchestrator
```

The orchestrator test pass ran 25 tests, including the Timescale APY mapper,
Kamino instruction builders, same-mint loop, mainnet executor, configured
preparer, and store idempotency coverage.

The local validator harness was run with the installed Agave 3.1.12 and 3.1.13
validators. Both runs reached validator startup and confirmed executable Squads
and mock Kamino program accounts plus programdata accounts, then failed on the
first Squads instruction with the runtime loader error described above.

The full crate still requires a live `DATABASE_URL` or a SQLx prepared query
cache because this crate uses SQLx query macros:

```bash
cargo test -p loyal-yield-orchestrator --lib
```

## Remaining Production Hardening

The configured preparer is intentionally explicit and reviewable, but it still
expects quote ratios and Kamino accounts to be supplied externally. The next
hardening step is to replace the static quote bps with a live Kamino reserve
state decoder that converts collateral shares to redeemable liquidity and then
validates deposit outcomes from current reserve state.
