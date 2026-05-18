# Squads Testing Setup

This repo has a small Rust test crate for Squads smart-account flows without pulling in the whole passkey registry stack from `passkey-work`.

Run the current tests with:

```bash
bun run test:squads
```

The crate lives in `crates/squads-test-harness`. It provides LiteSVM setup, Squads settings, vault, and policy PDA derivation, program config seeding, `create_squads_smart_account` instruction construction, spending-limit policy instruction construction, sync-transfer instruction construction, and account-meta hashing.

Use `create_funded_squads_test_context()` for tests that need the common funded starting point. The default context airdrops `1 SOL`, creates a Squads smart account with the wallet as signer, and sends `0.5 SOL` into vault index `0`, leaving both the wallet and vault funded for the scenario under test. Use `create_funded_squads_test_context_with_config()` when a test needs a different seed, vault index, or funding split.

The current end-to-end path lives in `crates/squads-test-harness/tests/spending_limits.rs`. It starts from that context, creates a spending-limit policy that lets a delegated wallet withdraw from vault index `0`, checks allowed withdrawals, checks an oversized withdrawal failure, removes the policy, and checks that the delegated wallet can no longer withdraw.

The setup mirrors the lean parts of `passkey-work`: one static Squads settings account can own many deterministic vault namespaces, and tests should keep the Squads verifier or gateway signer explicit. Future yield-routing tests can build on these helpers instead of recreating PDA seeds and Borsh payload packing in every test.

For tests that need to execute the real Squads program, provide a compiled SBF binary path through:

```bash
SQUADS_SMART_ACCOUNT_PROGRAM_SO=/path/to/squads_smart_account_program.so bun run test:squads
```

When the environment variable is omitted, the test loader also checks the sibling `../passkey-work/target/deploy/squads_smart_account_program.so` path used during development. Do not commit generated SBF binaries. Keep them in `target/deploy` or point the environment variable at a local checkout.
