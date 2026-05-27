# Quasar Loyal Hub Rewrite Plan

## Verdict

The Loyal Hub swap program can be expressed cleanly with Quasar account structs,
instruction handlers, SPL Token CPI helpers, and explicit account constraints.
The runtime replacement should wait until the test runtime can execute Quasar's
entrypoint ABI.

## Blocker

Quasar 0.0 emits a two-pointer SVM entrypoint:

```rust
pub unsafe extern "C" fn entrypoint(ptr: *mut u8, instruction_data: *const u8) -> u64
```

The current Squads test crate runs on LiteSVM 0.7.1, which calls the legacy
one-pointer entrypoint. A prototype Quasar rewrite compiled with
`cargo build-sbf`, but every `loyal_hub_swap` test failed during config
initialization:

```text
Program ... consumed 1 of 200000 compute units
Program ... failed: Access violation in unknown section at address 0xfffffffffffffff8 of size 8
```

That address matches Quasar reading the instruction data length at
`instruction_data - 8` while the runtime has not supplied Quasar's second
entrypoint pointer. The blocker is the runtime entrypoint ABI.

## Target Shape

The rewrite should preserve Loyal Hub's product behavior: program ID `[42; 32]`,
the config PDA seed `b"config"`, lane authority seed
`b"hub-authority", &[lane_id]`, canonical hub-authority ATA inventory accounts,
lane bounds, allowed-mint checks, paused state, max fee bps, normalized fee-cap
logic, and the Squads/Loyal Actions flow.

Use Quasar's `#[account]` for `HubConfig`, `#[derive(Accounts)]` for each
instruction context, one-byte `#[instruction(discriminator = N)]` values, and
`quasar-spl` account/CPI types for SPL Token checks and `transfer_checked`.
Keep manual PDA verification for lane-dependent hub authorities, because the
current Quasar signer helper path does not capture instruction seed args well
enough for the dynamic lane model.

Keep the existing instruction byte layout where possible. That preserves the
Squads data constraints at offset `0` and keeps the max-fee check at its current
fixed offset for `swap_exact_in`.

## Runtime Plan

First add a QuasarSVM Rust smoke test for `initialize_config`, then expand it to
swap success and the key rejection cases: paused config, excessive fee,
noncanonical inventory ATA, invalid lane, and disallowed mint.

After QuasarSVM passes, decide whether the Squads test crate can move from
LiteSVM to QuasarSVM while keeping smart-account execution coverage. If it
cannot, keep the native SBF for Squads E2E tests and hold the Quasar rewrite
until LiteSVM supports Quasar's entrypoint ABI or Quasar offers a legacy
entrypoint mode.

## Implementation Order

1. Build a program-local Quasar prototype with `quasar-lang` and `quasar-spl`.
2. Prove the prototype under QuasarSVM.
3. Run the Quasar SBF through the Squads smart-account flow.
4. Update `loyal-actions` and `squads-test-harness` constraints only if account
   order or data offsets actually change.
5. Keep QEDGen `check` and `proptest` gates. Treat generated Quasar codegen in
   `target/qedgen` as a drift signal until QEDGen's Quasar backend emits
   compilable SPL Token CPI and dynamic PDA code.

## Required Checks

Run `cargo check -p loyal-hub-swap-program`,
`cargo test -p loyal-hub-swap-program`, the QuasarSVM Rust tests,
`cargo test -p loyal-actions`, `cargo test -p squads-test-harness --test
loyal_hub_swap`, `cargo test -p squads-test-harness --test
loyal_hub_swap_qed_parity`, and `bun run test:squads`.
