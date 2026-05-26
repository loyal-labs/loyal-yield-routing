# Squads Testing Setup

This repo has a small Rust test crate for Squads smart-account flows without pulling in the whole passkey registry stack from `passkey-work`.

Run the current tests with:

```bash
bun run test:squads
```

The crate lives in `crates/squads-test-harness`. It provides LiteSVM setup; Squads PDA derivation for settings, vault namespaces, policy accounts, and program config; `create_squads_smart_account` instruction construction; spending-limit and ProgramInteraction policy builders; yield-route policy bundle helpers; sync-transfer and sync-transaction helpers; real SPL Token mint/account seeding helpers; test-only Jupiter/Kamino SBF program loading; and account-meta hashing.

## Harness Architecture

The Rust crate is organized as a small vertical-slice test harness instead of a generic bag of helpers:

- `squads` owns Squads-specific addresses, settings-account setup, smart-account instructions, and compiled instruction payload encoding.
- `runtime` owns LiteSVM construction, Squads SBF fixture loading, funded test contexts, heap-frame helpers, and transaction submission.
- `policies` owns raw policy builders and low-level policy families:
  - `policies/lifecycle.rs` covers policy removal and other settings-lifecycle instructions.
  - `policies/spending_limits.rs` covers spending-limit policy creation.
  - `policies/program_interaction/` covers raw `ProgramInteraction` policy constraints for Jupiter, Kamino, Loyal Hub, compact Squads payload encoding, and all-in-one route bundles. Its `mod.rs` is a facade over `stable_swap.rs`, `kamino.rs`, `route_bundles.rs`, and `common.rs`.
- `yield_route` owns user-facing route policy bundles such as three-policy, combined-Kamino, and all-in-one route setups.
- `protocols` owns mock protocol instruction data, SPL Token account seeding, and local SBF mock loading for Jupiter, Kamino, and Loyal Hub.
- `types` owns shared structs and Borsh payload models; most Squads wire types stay `pub(crate)` so the public API stays small.

Root-level exports remain available for existing tests. New tests should prefer `squads_test_harness::prelude::*` for scenario-style imports or module-qualified imports such as `squads_test_harness::yield_route::create_squads_yield_route_policy_instructions` when the dependency should be explicit.

Use `create_funded_squads_test_context()` for tests that need the common funded starting point. The default context airdrops `1 SOL`, creates a Squads smart account with the wallet as signer, and sends `0.5 SOL` into vault index `0`, leaving both the wallet and vault funded for the scenario under test. Use `create_funded_squads_test_context_with_config()` when a test needs a different seed, vault index, or funding split.

The current end-to-end paths live in `crates/squads-test-harness/tests/`. `spending_limits.rs` covers delegated SOL withdrawals, `swap_intents.rs` covers the SOL-to-USDC setup swap plus delegated USDC-to-PYUSD stable-swap ProgramInteraction path using SPL Token transfers, and `kamino_reserves.rs` covers delegated deposit/withdraw against a whitelisted Kamino Main Market USDC reserve using SPL Token CPIs plus denial for the Prime/Figure USDC reserve and denial after deposit-policy removal.

For yield-routing tests, prefer the abstraction in the Rust test crate:

```rust
let route_policy_setup = create_squads_yield_route_policy_instructions(
    context,
    delegated_signer,
    SquadsYieldRoutePolicyWhitelist {
        stable_mints,
        kamino_reserves,
    },
);
```

That call hides Squads settings, authority, vault index, default policy seeds, policy PDA derivation, compact ProgramInteraction payload construction, and the three-policy split. Test code should list the stable mints and Kamino reserve account structs needed by the optimized route, send `route_policy_setup.instructions`, then execute against `route_policy_setup.policies.withdraw`, `.swap`, and `.deposit`.

Use `create_squads_yield_route_swap_policy_instruction()` for swap-only tests and `execute_squads_yield_route_stable_swap_instruction()` for the mock Jupiter stable exact-in path.

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
