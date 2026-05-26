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
