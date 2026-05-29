# Yield Routing Devnet Assembly Plan

We are not doing portfolio optimization yet. The first version moves one managed position into one selected reserve. No splitting. No farmer-level APY amortization. The goal is a naive loop that is easy to inspect and replay.

## Done

Policies are in good shape. LiteSVM covers same-mint routes, Jupiter routes, Loyal Hub routes, route packing, Hub lanes, and adversarial account substitutions.

Reserve monitoring exists as a data boundary. `crates/loyal-yield-router` reads Timescale rows, catches up from a cursor, and subscribes to reserve updates. It does not decide anything.

Loyal Hub filling works locally. The program can fill exact-in swaps from hot inventory, enforce fee caps, use lane-scoped inventory, and rebalance between lanes. Production fees and inventory policy are still open.

## Missing

We need the router loop. It should read reserves, read the current position, score candidates, write the decision, build the route, simulate it, execute it, and record the result.

We need position state. Store the smart account, vault index, current reserve, current mint, deposited amount, estimated USD value, required token accounts, last rebalance time, and pending transaction state.

We need a quote and cost log for cross-mint moves. Store quote age, context slot, route labels, price impact, min out, priority fee estimate, compute estimate, Hub fill, Hub fee cap, and Jupiter residual. Same-mint can ship first.

The naive scorer should be simple: approved stable mints and Kamino markets only; reject stale rows, weird APY prints, low liquidity, capped reserves, and missing quotes; use a 20-minute EWMA, one-hour mean, and six-hour mean; switch only when edge beats cost plus a buffer; enforce a 20-minute cooldown.

Before moving funds, run shadow mode. Write every decision, skip reason, quote, position snapshot, and simulation preview. One week of decisions should be replayable without external API calls.

Execution should roll out as same-mint Kamino switches, then Jupiter cross-mint swaps, then Loyal Hub fills with Jupiter residual fallback, then multi-wallet packing with v0 transactions and ALTs.

Account setup must be explicit. Pre-create or setup vault token accounts, Kamino collateral accounts, Hub inventory ATAs, and ALTs before delegated execution.

## Devnet

Replace the placeholder `LOYAL_HUB_SWAP_PROGRAM_ID` with a real deployed program-id flow.

Add scripts to initialize Hub config, set admin, set hub authorizer, set inventory rebalancer, set lane count, allow mints, create lane inventory ATAs, and fund inventory.

Add devnet smoke tests for initialize, pause, set max fee, swap, rebalance, withdraw inventory, and unpause.

Devnet should prove Hub authority wiring and inventory movement first. Full Kamino/Jupiter routing may still need LiteSVM or cloned-state testing if devnet liquidity does not match mainnet assumptions.

## Gates

Before calling this devnet-ready:

```sh
bun run test:squads
bun run test:squads:e2e
bun run test:squads:hub-hindsight
bun run verify:qedgen
cargo test -p loyal-yield-router
TIMESCALEDB_TEST_URL=... cargo test -p loyal-yield-router --test timescale_live -- --ignored
bun run lint
bun run build
```

Also resolve or explicitly accept the QEDGen probe notes around `lane_count` and `mint_count`.

## Build Order

Start with the router loop, dry-run mode, durable reserve cursor, position snapshots, and decision snapshots. Then add same-mint scoring plus LiteSVM execution. After that, add quotes, cross-mint scoring, Jupiter execution, Loyal Hub fallback, devnet scripts, smoke tests, and one week of shadow mode.
