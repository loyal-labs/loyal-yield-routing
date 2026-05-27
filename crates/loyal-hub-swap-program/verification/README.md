# Loyal Hub Swap Verification

This directory contains the authored QEDGen contract for the Pinocchio
Loyal Hub swap program. `loyal_hub_swap.qedspec` is the committed behavioral
source of truth. The byte-level ABI source of truth is
`crates/loyal-hub-abi/schema/loyal_hub_abi.schema`. Generated QEDGen files are
scratch output and belong under `target/qedgen`.

## What It Covers

- `initialize_config`, `set_max_fee`, `set_paused`, `swap_exact_in`,
  `withdraw_inventory`, and `rebalance_inventory`.
- Config domain preservation: max fee stays within 10,000 bps, lane count stays
  non-zero, and pause state stays boolean.
- Stable-mint admission, lane bounds, admin/rebalancer/authorizer checks,
  fee-cap math, duplicate token-account rejection, and SPL Token transfer
  effects via `import Token from "spl"`.

The implemented program preserves the Loyal Hub wire ABI: one-byte instruction
tags, existing account order, canonical inventory accounts, and fixed Squads
constraint offsets. The generated ABI crate owns byte layout; QEDGen owns the
behavioral contract and generated proptest gate.

`bun run verify:hub-abi-spec-drift` checks the overlap between the generated ABI
schema and this spec. Run it whenever handler accounts, instruction arguments,
or their modeled QEDGen equivalents change.

## Current Blocker

`qedgen check --coverage`, the generated proptest tests, and the Pinocchio
probe are the active verification gates. The Quasar program scaffold gate is
kept as a known drift signal and currently fails in generated code. QEDGen
2.30.0 emits Anchor CPI snippets for `Token.transfer`, omits a usable `Pubkey`
import in generated Quasar state, and lowers dynamic PDA seed arguments as
account-field reads such as `ctx.hub_authority.lane_id`.

The Pinocchio probe should report no indexed-slice catalogue sites. It may
still print spec-less paired-validator notes for `lane_count` and `mint_count`;
those are heuristic findings over distinct domain checks. The command should
exit successfully.

Do not hand-edit generated files under `target/qedgen`. Fix the spec only when
the change remains truthful; otherwise treat the failing Quasar scaffold as an
upstream QEDGen codegen issue. QEDGen Pinocchio scaffold generation is not a
required gate until upstream support exists.

## Commands

```bash
bun run verify:qedgen:check
bun run verify:hub-abi-spec-drift
bun run verify:qedgen:probe
bun run verify:qedgen:codegen
bun run verify:qedgen:proptest
bun run verify:qedgen
```

`verify:qedgen:codegen` regenerates a Quasar scaffold in ignored scratch space
and runs `cargo check` against it. It is intentionally a failing gate until the
generated Quasar support code compiles, so `verify:qedgen` does not include it.
