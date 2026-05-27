# Squads Testing Setup

This repo has a small Rust test crate for Squads smart-account flows without pulling in the whole passkey registry stack from `passkey-work`.

Run the current tests with:

```bash
bun run test:squads
```

The action SDK lives in `crates/loyal-actions`. The Squads test crate lives in `crates/squads-test-harness` and provides LiteSVM setup; Squads PDA derivation for settings, vault namespaces, policy accounts, and program config; `create_squads_smart_account` instruction construction; spending-limit helpers; sync-transfer and sync-transaction helpers; SPL Token account seeding helpers; local protocol SBF loading; and account-meta hashing.

## Crate Architecture

The Rust crate is organized as small vertical slices instead of a generic bag of helpers.

| Module | Responsibility |
| --- | --- |
| `squads` | Squads addresses, settings-account setup, smart-account instructions, and compiled instruction payload encoding |
| `runtime` | LiteSVM construction, Squads SBF fixture loading, funded test contexts, heap-frame helpers, and transaction submission |
| `actions` | Adapters from `FundedSquadsTestContext` and seeded mock Kamino accounts into `loyal-actions` inputs |
| `policies` | Raw policy builders, settings lifecycle helpers, and spending-limit creation |
| `policies/program_interaction` | Older low-level ProgramInteraction helpers used by focused tests |
| `protocols` | Mock protocol instruction data, SPL Token account seeding, and local SBF loading for external protocols |
| `types` | Shared structs and Borsh payload models; most Squads wire types stay `pub(crate)` so the public API stays small |

New tests should prefer `squads_test_harness::prelude::*` for scenario-style runtime/mock imports and import route action builders from `loyal_actions`.

Use `create_funded_squads_test_context()` for tests that need the common funded starting point. The default context airdrops `1 SOL`, creates a Squads smart account with the wallet as signer, and sends `0.5 SOL` into vault index `0`, leaving both the wallet and vault funded for the scenario under test. Use `create_funded_squads_test_context_with_config()` when a test needs a different seed, vault index, or funding split.

The current end-to-end paths live in `crates/squads-test-harness/tests/`. `spending_limits.rs` covers delegated SOL withdrawals, `swap_intents.rs` covers the SOL-to-USDC setup swap plus delegated USDC-to-PYUSD stable-swap ProgramInteraction path using SPL Token transfers, and `kamino_reserves.rs` covers delegated deposit/withdraw against a whitelisted Kamino Main Market USDC reserve using SPL Token CPIs plus denial for the Prime/Figure USDC reserve and denial after deposit-policy removal.

For yield-routing tests, build actions through the SDK:

```rust
let route_action_setup = create_three_step_yield_route_actions(
    loyal_action_context(context, delegated_signer),
    yield_route_universe_from_mock_reserves(stable_mints, kamino_reserves),
    vec![mock_jupiter_swap_lane(true)],
    YieldRouteActionSeeds::default(),
)?;
```

That call hides Squads settings, authority, vault index, action account derivation, compact ProgramInteraction payload construction, and the three-action split. Test adapters supply mock protocol contracts such as `mock_jupiter_swap_lane(...)`; the `loyal-actions` SDK only receives explicit protocol configuration.

Test code should list the stable mints and seeded Kamino accounts needed by the optimized route, send `route_action_setup.instructions`, then build route instructions through the named actions:

```rust
let withdraw_ix = route_action_setup.withdraw()?.build(
    delegated_signer,
    vault_index,
    withdraw_instructions,
    withdraw_accounts,
);
```

Use `create_swap_yield_route_action(...)` for swap-only setup. Jupiter and Loyal Hub swaps use typed build arguments, such as `.jupiter()?.build(JupiterSwapExecution { ... })` and `.hub()?.build(HubSwapExecution { ... })`.

Loyal Hub lane simulations have their own test-local support under `tests/loyal_hub_lane_simulation/`. Keep that framework out of the crate public API unless another test family needs it. It records accepted swaps, rejected swaps, accepted rebalances, and scheduler-blocked rebalances as events, then derives balances and metrics from those events before comparing them with LiteSVM state.

The setup mirrors the lean parts of `passkey-work`: one static Squads settings account can own many deterministic vault namespaces, and tests should keep the Squads verifier or gateway signer explicit. Future yield-routing tests can build on these helpers instead of recreating PDA seeds and Borsh payload packing in every test.

For tests that need to execute the real Squads program, provide a compiled SBF binary path through:

```bash
SQUADS_SMART_ACCOUNT_PROGRAM_SO=/path/to/squads_smart_account_program.so bun run test:squads
```

When the environment variable is omitted, the test loader uses the committed Squads fixture at `crates/squads-test-harness/fixtures/squads/squads_smart_account_program.so`, then falls back to the sibling `../passkey-work/target/deploy/squads_smart_account_program.so` path used during development.

`bun run test:squads` builds the local test-only protocol mock first:

```bash
cargo build-sbf -- -p mock-yield-protocols-program
```

The test loader installs that SBF binary at the Jupiter and Kamino program IDs so Squads still gates the outer protocol calls while the mocked protocol logic moves real SPL Token balances underneath.
