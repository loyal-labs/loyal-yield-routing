# Loyal Yield Routing

This repo experiments with yield-routing automation for Squads smart accounts. The current implementation centers on `loyal-actions`, a Rust SDK for constructing delegated route actions:

- Kamino withdraw/deposit actions scoped by whitelisted markets and liquidity mints
- swap actions scoped by whitelisted route mints
- all-in-one actions that can cover Kamino plus swap lanes

The Rust tests keep Squads authorization separate from protocol validation. Squads bounds the delegated signer to the vault, approved markets/mints, route mints, and instruction discriminators. Each external protocol still validates its own account relationships.

## Development

Install dependencies with Bun:

```bash
bun install
```

Run the Next.js app:

```bash
bun dev
```

Run the frontend build or lint checks:

```bash
bun run build
bun run lint
```

Use the mounted 1Password env file for local non-critical secrets. Store the
delegated yield router signer as `YIELD_ROUTER_KEYPAIR`, using a hex
encoded private key. The orchestrator accepts either a 32-byte private seed or a
64-byte Solana keypair encoded as hex, and exposes
`yield_router_keypair_from_env()` so transaction code can load the signer
without writing key material to disk or logs.

## Squads Policy Monitor

Run the Helius Squads policy monitor with the Neon-backed sink:

```bash
op run --env-file=.env.1password -- sh -c 'cargo run -p loyal-squads-policy-monitor -- --postgres-url "$NEON_DATABASE_URL"'
```

The monitor also reads `NEON_DATABASE_URL` directly when `--postgres-url` is omitted.

For Rust SQLx validation against Neon, set `DATABASE_URL` from the same direct
Neon URL. Avoid the pooled `-pooler` URL for these tests because SQLx prepared
statements need a stable backend connection.

```bash
op run --env-file=.env.1password -- sh -c 'DATABASE_URL="$NEON_DATABASE_URL" cargo test -p loyal-yield-orchestrator -p loyal-squads-policy-monitor'
```

## Same-Mint Route Runner

The pre-production same-mint runner lives in `crates/loyal-yield-orchestrator`:

```bash
op run --env-file=.env.1password -- sh -c 'DATABASE_URL="$NEON_DATABASE_URL" cargo run -p loyal-yield-orchestrator --bin same_mint_route_runner'
```

Required env:

- `SOLANA_RPC_URL`: mainnet RPC URL.
- `YIELD_ROUTER_KEYPAIR`: hex-encoded delegated signer keypair.
- `SAME_MINT_RESERVE_APYS_JSON`: JSON array of reserve APY rows.
- `SAME_MINT_ROUTE_CONFIG_JSON` or `SAME_MINT_ROUTE_CONFIG_PATH`: route address and quote config.

Submission is off by default. Set `SAME_MINT_SUBMIT_TXS=true` only after dry-run simulation succeeds. The runner uses that same flag to decide whether the loop stops after simulation or submits the batch.

Route config shape:

```json
{
  "routes": [
    {
      "vault_id": 1,
      "source_reserve": "source reserve pubkey or DB label",
      "target_reserve": "target reserve pubkey or DB label",
      "liquidity_mint": "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
      "policy_account": "Squads ProgramInteraction policy account",
      "delegated_signer": "delegated signer pubkey",
      "vault_index": 0,
      "vault": "Squads vault pubkey",
      "withdraw_constraint_index": 0,
      "deposit_constraint_index": 2,
      "source_accounts": {
        "reserve": "source Kamino reserve",
        "market": "source Kamino lending market",
        "lending_market_authority": "source market authority",
        "liquidity_mint": "USDC mint",
        "reserve_liquidity_supply": "source reserve liquidity supply",
        "collateral_mint": "source collateral mint",
        "vault_liquidity": "vault USDC token account",
        "vault_collateral": "vault source collateral token account"
      },
      "target_accounts": {
        "reserve": "target Kamino reserve",
        "market": "target Kamino lending market",
        "lending_market_authority": "target market authority",
        "liquidity_mint": "USDC mint",
        "reserve_liquidity_supply": "target reserve liquidity supply",
        "collateral_mint": "target collateral mint",
        "vault_liquidity": "vault USDC token account",
        "vault_collateral": "vault target collateral token account"
      },
      "quote": {
        "redeem_collateral_to_liquidity_bps": 10000,
        "deposit_liquidity_bps": 10000,
        "max_redeem_collateral_raw": 1000000
      }
    }
  ]
}
```

## Squads Tests

Run the lean Squads test suite:

```bash
bun run test:squads
```

Run the ignored historical Kamino replay:

```bash
bun run test:squads:e2e
```

The action SDK lives in `crates/loyal-actions`. The Squads test crate lives in `crates/squads-test-harness` and consumes the SDK through small test adapters:

```rust
let route_action_setup = create_three_step_yield_route_actions(
    loyal_action_context(context, wallet_b.pubkey()),
    yield_route_universe_from_mock_reserves(
        vec![USDC_MINT, PYUSD_MINT],
        vec![main_usdc, prime_usdc, main_pyusd],
    ),
    vec![mock_jupiter_swap_lane(true)],
    YieldRouteActionSeeds::default(),
)?;
```

The SDK returns delegated action accounts, create instructions, and named route actions. Route tests build executable Squads instructions through the fluent action surface instead of assembling Squads constraint indexes directly:

```rust
let deposit_ix = route_action_setup
    .deposit()?
    .build(delegated_signer, vault_index, deposit_instructions, deposit_accounts);
```

Swap actions use typed execution arguments, for example `.jupiter()?.build(JupiterSwapExecution { ... })` or `.hub()?.build(HubSwapExecution { ... })`. Swap-only tests can use `create_swap_yield_route_action()`.

Loyal Hub lane-load tests live under `crates/squads-test-harness/tests/loyal_hub_lane_simulation.rs`. That test module keeps its simulation support local: LiteSVM, Squads execution, SPL Token accounts, Loyal Actions, and the Hub SBF still run normally, while the support code derives expected balances, lane metrics, scheduling conflicts, and planner output from recorded simulation events.

### Test Crate Map

The Squads test crate is grouped by domain modules for onboarding.

| Module | Owns |
| --- | --- |
| `squads` | Squads PDA derivation, settings setup, smart-account instructions, payload basics |
| `runtime` | LiteSVM setup, funded contexts, program loading, heap-frame helpers, transaction sending |
| `policies` | Raw Squads policy families |
| `actions` | Adapters from funded contexts and mock reserves into `loyal-actions` inputs |
| `policies/program_interaction` | Low-level historical Squads ProgramInteraction helpers |
| `protocols` | Mock Jupiter/Kamino/Loyal Hub instruction data, SPL account seeding, SBF mock loading |
| `types` | Shared public test structs and crate-private Squads wire types |

New scenario tests can use `squads_test_harness::prelude::*` for runtime/mock helpers and import action builders from `loyal_actions`. Keep route action construction in `crates/loyal-actions`; keep mock protocol state in `protocols`.

See `docs/squads-testing.md` and `docs/plans/squads-yield-routing-policy.md` for the policy model and test coverage.
