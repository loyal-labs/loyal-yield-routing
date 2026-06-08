# Loyal Yield Orchestrator

The orchestrator runs Loyal's yield-routing loop for managed Squads vaults. It
reads active route policies and current vault positions from Neon, reads fresh
reserve APY rows from Timescale, reconciles vault positions from Solana RPC,
plans same-mint and cross-mint routes, builds delegated Squads route actions,
and submits them with the delegated yield-router signer.

## Production Worker

Run production commands from the repo root in a permanent interactive terminal.
Sign in to 1Password once, then run the worker through the mounted
`.env.1password` environment.

```sh
op signin --account loyalteam.1password.com
```

Build the release binary:

```sh
op run --env-file=.env.1password -- sh -c 'DATABASE_URL="$NEON_DATABASE_URL" cargo build --release -p loyal-yield-orchestrator --bin yield_route_worker'
```

Run a production smoke test first. This reconciles and plans once, but does not
submit transactions:

```sh
op run --env-file=.env.1password -- sh -c 'DATABASE_URL="$NEON_DATABASE_URL" cargo run -q -p loyal-yield-orchestrator --bin yield_route_worker -- --cluster mainnet --once --dry-run'
```

Run continuously for production. Do not pass `--once` or `--dry-run`:

```sh
op run --env-file=.env.1password -- sh -c 'unset YIELD_ROUTE_CONFIG_JSON YIELD_ROUTE_CONFIG_FILE; exec target/release/yield_route_worker --cluster mainnet --min-edge-bps 1 --batch-size 8 --max-vaults 50 --debounce-secs 2 --max-apy-age-secs 900'
```

Required environment variables are read from `.env.1password`:

- `NEON_DATABASE_URL`: Loyal orchestration database.
- `TIMESCALEDB_URL`: Kamino reserve/APY source database.
- `SOLANA_RPC_URL`: Mainnet Solana RPC endpoint.
- `YIELD_ROUTER_KEYPAIR`: delegated route signer keypair.

`YIELD_ROUTE_CONFIG_JSON` and `YIELD_ROUTE_CONFIG_FILE` are test override
inputs. Leave them unset for normal production use.

## Architecture

The live worker entrypoint is `src/bin/yield_route_worker.rs`.

At startup it:

- Connects to Neon through `OrchestratorStore` and applies orchestrator
  migrations.
- Connects to Timescale through `TimescaleRouterClient`.
- Loads the delegated signer with `yield_router_keypair_from_env`.
- Reads the latest fresh reserve rows and evaluates one startup routing pass.
- Subscribes to Timescale updates and re-runs after a debounce when APY state
  changes.

The routing pass is owned by `YieldRoutingLoop` in `src/same_mint_loop.rs`.
Each pass:

- Loads active managed vaults and route policies from Neon.
- Reconciles current Kamino collateral balances from Solana RPC through
  `RpcPositionReconciler`.
- Resolves Timescale reserve rows into route targets with
  `KaminoReserveMetadataResolver`.
- Plans eligible routes with `YieldRoutePlanner`.
- Requests live Jupiter quote and swap-instruction data through
  `JupiterRouteQuoteProvider` for cross-mint routes.
- Persists planned decisions and attempts in Neon.
- Builds Squads route instructions with `build_yield_route_transaction`.
- Simulates and submits through `RpcRouteSubmitter`.

Same-mint routes execute as one delegated Squads action. Cross-mint Jupiter
routes can execute as split withdraw, swap, and deposit actions when the active
policy metadata includes a separate swap policy account. Split routes are
submitted sequentially so later steps observe the state produced by earlier
steps.

Jupiter setup and cleanup instructions are intentionally rejected by the quote
provider. Required vault token accounts must already exist, or they must be
created in a separate setup transaction before the protected route executes.
