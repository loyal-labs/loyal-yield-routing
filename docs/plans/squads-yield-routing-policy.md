# Loyal Actions Yield Routing Design

## Goal

Wallet A owns the Squads smart account and vault `0`. Wallet A funds the vault, swaps SOL to the starting reserve mint, and creates delegated Loyal Actions for Wallet B. Wallet B should only be able to execute yield-routing moves across approved Kamino markets/mints and approved swap routes.

The APY reports point to a fee-aware router that changes reserves only when the expected gain clears execution cost:

- The five-minute report found `5m` checking best on the five-day tape, but only narrowly ahead of `20m` after costs.
- The hourly March-to-May report found hourly evaluation useful, with actual reserve changes only when net edge survives swap loss and the `5,000` lamport reserve-change fee.
- Same-mint reserve changes need only `withdraw -> deposit`.
- Cross-mint reserve changes need `withdraw -> swap -> deposit`.

## Squads Primitive

Use `ProgramInteractionPolicy` with `time_lock = 0`, threshold `1`, and Wallet B as the sole policy signer.

The key Squads behavior from the `policies` branch is that each policy execution validates the submitted inner instruction against the selected `instruction_constraint_index`. Several policy-execution instructions can still be packed into one outer Solana transaction, so route atomicity comes from the outer transaction:

```text
same mint:
  policy exec: Kamino withdraw
  policy exec: Kamino deposit

cross mint:
  policy exec: Kamino withdraw
  policy exec: Jupiter swap
  policy exec: Kamino deposit
```

Each policy is bound to one Squads `account_index`, so Wallet B can only act through vault `0`.

## Minimal Policy Shape

The SDK supports three-action and all-in-one action setups:

```text
withdraw action:
  Kamino withdraw against optimized-route markets and liquidity mints

swap action:
  stable swaps between mints used by the optimized route

deposit action:
  Kamino deposit against optimized-route markets and liquidity mints
```

For each allowed Kamino action, keep the Squads boundary market/mint-bounded but do not re-model the full Kamino account graph:

```text
program_id = Kamino Lend
account[0] = vault 0 PDA
account[2] = approved optimized-route market pubkey
account[3] = approved optimized-route liquidity mint
account[10] = SPL Token
data discriminator = deposit or withdraw
```

The liquidity mint, vault token account, collateral token account, reserve supply, collateral mint, and reserve authorities are intentionally left to Kamino's own instruction validation. Encoding all of those accounts into Squads policy state duplicates protocol logic and was the source of the heap-heavy policy shape.

For optimized-route mints, add one Jupiter constraint:

```text
program_id = Jupiter v6
account[0] = vault 0 PDA
account[1] = source vault token account
account[2] = destination vault token account
account[3] = approved optimized-route source mint
account[4] = approved optimized-route destination mint
account[5] = SPL Token
account[6] = token account owned by SPL Token
account[7] = token account owned by SPL Token
account[8] = mock Jupiter authority
data discriminator = stable exact-in
```

This keeps the delegated surface lean: every policy execution must still match an allowed constraint, use the vault-owned token accounts, and sign through the selected Squads vault.

## Loyal Actions SDK

Tests should not assemble route actions directly. Use `crates/loyal-actions` and the Squads test adapters:

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

The SDK derives action accounts, deduplicates the route universe, returns Squads create instructions, and exposes named route actions for execution. Tests choose the delegated signer, route universe, and explicit protocol lanes; test adapters own mock Jupiter details.

For swap-only tests, use `create_swap_yield_route_action(...)`. Route execution should go through `withdraw`, `deposit`, `jupiter`, or `hub` actions and call `build` with the action-specific arguments, so tests do not duplicate Squads constraint-index plumbing.

## Practical Recommendation

Start with route-universe policies created from the optimized APY path. That is more restrictive than a broad permanent router policy, but avoids churn per individual rebalance:

1. The off-chain router decides a fee-aware move from the APY tape and quote cache.
2. Wallet A creates route actions for the optimized market/mint universe.
3. Wallet B relays one outer transaction containing the needed action executions for each step.
4. Wallet A removes or replaces the underlying Squads accounts when the optimized universe changes.

Later, if policy churn becomes too heavy, move to a helper-first design: one immutable/versioned router helper is whitelisted by Squads, and the helper owns dynamic reserve checks, quote validation, cooldown enforcement, and fee accounting. That is a larger trust surface, so the exact-policy path is the better first implementation.

## Current Test Evidence

`crates/squads-test-harness/tests/usdc_pyusd_kamino_route.rs` covers the small deterministic route:

- Wallet A creates the smart account and funds vault `0`.
- Wallet A performs the initial SOL-to-USDC setup swap.
- Wallet A creates withdraw, route-mint swap, and deposit actions for the delegated Wallet B through `loyal-actions`.
- Wallet B switches Main USDC to Prime USDC by packing reserve-withdraw and reserve-deposit policy executions into one transaction.
- Wallet B switches Prime USDC to Main PYUSD by packing reserve-withdraw, route-mint stable-swap, and reserve-deposit policy executions into one transaction.

The test uses LiteSVM, the real Squads SBF, SPL Token state transitions, mocked Kamino/Jupiter programs only for external protocol logic, and the route-mint stable-swap helper for the USDC-to-PYUSD leg. `swap_intents.rs` keeps the live Jupiter fixture contract check separate from route policy creation.

`crates/squads-test-harness/tests/kamino_hindsight_e2e.rs` is ignored by default and covers the heavy historical route:

- Loads the hourly Kamino APY cache for the March 1 to May 18 window.
- Recomputes the fixed-start hindsight route beginning from USDC Prime.
- Creates delegated actions through `loyal-actions`.
- Uses unique markets and mints from the optimized route universe. The whitelist excludes stable mints that the route never touches.
- Replays same-mint and cross-mint route changes, checks account state after every move, accounts for route signature fees, and verifies the final withdrawal value against the fixed-start hindsight result.
