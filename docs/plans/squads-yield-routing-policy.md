# Squads Yield Routing Policy Design

## Goal

Wallet A owns the Squads smart account and vault `0`. Wallet A funds the vault, swaps SOL to the starting reserve mint, and creates a delegated policy for Wallet B. Wallet B should only be able to execute yield-routing moves across approved Kamino reserves and approved swap routes.

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

Create small ProgramInteraction policies rather than one large all-in-one policy. The heavy historical route should use three policies:

```text
withdraw policy:
  Kamino withdraw against optimized-route reserves

swap policy:
  stable swaps between mints used by the optimized route

deposit policy:
  Kamino deposit against optimized-route reserves
```

For each allowed Kamino reserve action, keep the Squads boundary reserve-bounded but do not re-model the full Kamino account graph:

```text
program_id = Kamino Lend
account[0] = vault 0 PDA
account[1] = approved optimized-route reserve pubkey
account[2] = approved optimized-route market pubkey
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

## Harness Abstraction

Tests should not assemble the three route policies directly. Use the route policy bundle helper from `crates/squads-test-harness`:

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

The helper derives the default three route policy accounts, binds them to the funded Squads context's settings/vault, deduplicates the whitelist, and returns both the policy pubkeys and the create-policy instructions. Tests only choose the delegated signer and the accounts that belong in the optimized route universe.

For swap-only tests, use `create_squads_yield_route_swap_policy_instruction(context, signer, stable_mints)`. For mock stable exact-in execution, use `execute_squads_yield_route_stable_swap_instruction(...)` so tests do not duplicate Jupiter account ordering.

## Practical Recommendation

Start with route-universe policies created from the optimized APY path. That is more restrictive than a broad permanent router policy, but avoids churn per individual rebalance:

1. The off-chain router decides a fee-aware move from the APY tape and quote cache.
2. Wallet A creates a withdraw policy, a route-mint swap policy, and a deposit policy for the optimized reserve/mint universe.
3. Wallet B relays one outer transaction containing the needed policy executions for each step.
4. Wallet A removes or replaces the policies when the optimized universe changes.

Later, if policy churn becomes too heavy, move to a helper-first design: one immutable/versioned router helper is whitelisted by Squads, and the helper owns dynamic reserve checks, quote validation, cooldown enforcement, and fee accounting. That is a larger trust surface, so the exact-policy path is the better first implementation.

## Current Test Evidence

`crates/squads-test-harness/tests/usdc_pyusd_kamino_route.rs` covers the small deterministic route:

- Wallet A creates the smart account and funds vault `0`.
- Wallet A performs the initial SOL-to-USDC setup swap.
- Wallet A creates withdraw, route-mint swap, and deposit policies for the delegated Wallet B through `create_squads_yield_route_policy_instructions`.
- Wallet B switches Main USDC to Prime USDC by packing reserve-withdraw and reserve-deposit policy executions into one transaction.
- Wallet B switches Prime USDC to Main PYUSD by packing reserve-withdraw, route-mint stable-swap, and reserve-deposit policy executions into one transaction.

The test uses LiteSVM, the real Squads SBF, SPL Token state transitions, mocked Kamino/Jupiter programs only for external protocol logic, and the route-mint stable-swap helper for the USDC-to-PYUSD leg. `swap_intents.rs` keeps the live Jupiter fixture contract check separate from route policy creation.

`crates/squads-test-harness/tests/kamino_hindsight_e2e.rs` is ignored by default and covers the heavy historical route:

- Loads the hourly Kamino APY cache for the March 1 to May 18 window.
- Recomputes the fixed-start hindsight route beginning from USDC Prime.
- Creates exactly three delegated policies through the route policy bundle helper: withdraw, route-mint swap, and deposit.
- Uses unique reserves and mints from the optimized route universe. The whitelist excludes stable reserves that the route never touches.
- Replays same-mint and cross-mint route changes, checks account state after every move, accounts for route signature fees, and verifies the final withdrawal value against the fixed-start hindsight result.
