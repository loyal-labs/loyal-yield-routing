# Loyal Yield Routing

This repo experiments with yield-routing automation for Squads smart accounts. The current implementation focuses on a lean policy shape for optimized Kamino reserve farming:

- one withdraw policy for whitelisted Kamino reserves
- one swap policy for whitelisted route mints
- one deposit policy for whitelisted Kamino reserves

The Rust tests keep Squads authorization separate from protocol validation. Squads bounds the delegated signer to the vault, approved reserves, approved route mints, and instruction discriminators. Kamino and Jupiter are responsible for validating their own internal account relationships.

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

The Squads test crate lives in `crates/squads-test-harness`. New yield-routing tests should use `create_squads_yield_route_policy_instructions()` with a `SquadsYieldRoutePolicyWhitelist` instead of assembling ProgramInteraction policies by hand:

```rust
let route_policy_setup = create_squads_yield_route_policy_instructions(
    context,
    wallet_b.pubkey(),
    SquadsYieldRoutePolicyWhitelist {
        stable_mints: vec![USDC_MINT, PYUSD_MINT],
        kamino_reserves: vec![main_usdc, prime_usdc, main_pyusd],
    },
);
```

The helper returns the three route policy pubkeys plus the create-policy instructions. Swap-only tests can use `create_squads_yield_route_swap_policy_instruction()`.

See `docs/squads-testing.md` and `docs/plans/squads-yield-routing-policy.md` for the policy model and test coverage.
