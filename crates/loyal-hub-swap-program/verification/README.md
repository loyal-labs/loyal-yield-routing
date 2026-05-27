# Loyal Hub Swap Verification

This directory contains the authored QEDGen contract for the Quasar-native
Loyal Hub swap rewrite. `loyal_hub_swap.qedspec` is the committed source of
truth. Generated QEDGen files are scratch output and belong under
`target/qedgen`.

## What It Covers

- `initialize_config`, `set_max_fee`, `set_paused`, `swap_exact_in`,
  `withdraw_inventory`, and `rebalance_inventory`.
- Config domain preservation: max fee stays within 10,000 bps, lane count stays
  non-zero, and pause state stays boolean.
- Stable-mint admission, lane bounds, admin/rebalancer/authorizer checks,
  fee-cap math, duplicate token-account rejection, and SPL Token transfer
  effects via `import Token from "spl"`.

The spec uses a framework-native ABI. It does not preserve the old custom
instruction tag parser or account order.

## Current Blocker

`qedgen check --coverage` and the generated proptest tests pass. The Quasar
program scaffold gate currently fails in generated code. QEDGen 2.30.0 emits
Anchor CPI snippets for `Token.transfer`, omits a usable `Pubkey` import in
generated Quasar state, and lowers dynamic PDA seed arguments as account-field
reads such as `ctx.hub_authority.lane_id`.

Do not hand-edit generated files under `target/qedgen`. Fix the spec only when
the change remains truthful; otherwise treat the failing Quasar scaffold as an
upstream QEDGen codegen issue.

## Commands

```bash
bun run verify:qedgen:check
bun run verify:qedgen:codegen
bun run verify:qedgen:proptest
bun run verify:qedgen
```

`verify:qedgen:codegen` regenerates a Quasar scaffold in ignored scratch space
and runs `cargo check` against it. It is intentionally a failing gate until the
generated Quasar support code compiles.
