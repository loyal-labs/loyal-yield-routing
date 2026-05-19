# Squads Testing Setup

This repo has a small Rust test crate for Squads smart-account flows without pulling in the whole passkey registry stack from `passkey-work`.

Run the current tests with:

```bash
bun run test:squads
```

The crate lives in `crates/squads-test-harness`. It provides LiteSVM setup; Squads PDA derivation for settings, vault namespaces, policy accounts, and program config; `create_squads_smart_account` instruction construction; spending-limit and ProgramInteraction policy builders; sync-transfer and sync-transaction helpers; real SPL Token mint/account seeding helpers; test-only Jupiter/Kamino SBF program loading; and account-meta hashing.

Use `create_funded_squads_test_context()` for tests that need the common funded starting point. The default context airdrops `1 SOL`, creates a Squads smart account with the wallet as signer, and sends `0.5 SOL` into vault index `0`, leaving both the wallet and vault funded for the scenario under test. Use `create_funded_squads_test_context_with_config()` when a test needs a different seed, vault index, or funding split.

The current end-to-end paths live in `crates/squads-test-harness/tests/`. `spending_limits.rs` covers delegated SOL withdrawals, `swap_intents.rs` covers the SOL-to-USDC setup swap plus delegated Jupiter USDC-to-PYUSD ProgramInteraction path using SPL Token transfers, and `kamino_reserves.rs` covers delegated deposit/withdraw against Kamino Main Market's USDC reserve using SPL Token CPIs plus denial for the Prime/Figure USDC reserve and denial after policy removal.

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
