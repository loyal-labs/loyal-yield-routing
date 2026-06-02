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
- Bounded rebalance batches from one through `MAX_REBALANCE_TRANSFERS`
  transfers. The QEDGen DSL has no repeated-record parameter yet, so the spec
  keeps `rebalance_inventory` as the single-transfer ABI drift anchor and adds
  arity-specialized `rebalance_inventory_2` through `rebalance_inventory_16`
  model handlers.
- Runtime max-batch execution is covered by a Squads test with an explicit
  compute-budget instruction. A 16-transfer rebalance exceeds the default
  200,000 compute-unit transaction budget.

The implemented program preserves the Loyal Hub wire ABI: one-byte instruction
tags, existing account order, canonical inventory accounts, and fixed Squads
constraint offsets. The generated ABI crate owns byte layout; QEDGen owns the
behavioral contract and generated proptest gate.

`bun run verify:hub-abi-spec-drift` checks the overlap between the generated ABI
schema and this spec, including the arity-specialized rebalance batch model's
parameter order and transfer-account stride. Run it whenever handler accounts,
instruction arguments, or their modeled QEDGen equivalents change.

## Active Gates

`qedgen check --coverage`, the generated proptest tests, and the Pinocchio
probe are the active non-Kani verification gates.

The patched QEDGen probe skips `#[cfg(kani)]` model/proof code and should report
an empty Pinocchio catalogue for the production source. It should also report no
paired-validator findings for the current `lane_count` and `mint_count` domain
refinements. The command should exit successfully.

Do not hand-edit generated files under `target/qedgen`. Fix the spec only when
the change remains truthful.

## Kani Model Gate

`bun run verify:qedgen:kani` runs a smoke slice of QEDGen's `--kani` backend
against the spec-translated transition model. It covers representative config
effects, single-transfer rebalance preservation, and max-batch rebalance
reachability without running every arity-specialized rebalance proof. Keep it
separate from the default `bun run verify:qedgen` gate while runtime and CI cost
are still being proven. The script prints each selected proof before it starts,
then records whether that proof passed or failed with elapsed time.

Smoke mode also sets `QEDGEN_KANI_SKIP_GUARD_PROOFS=1` during fresh codegen
unless `KANI_HARNESS` is provided. This keeps the fast gate focused on the
selected model proofs instead of rendering the large split guard-rejection
catalogue for every rebalance arity. Full mode and custom proof-filter runs do
not set that skip by default.

Use the patched QEDGen fork explicitly:

```bash
export QEDGEN=/private/tmp/solana-skills/target/debug/qedgen
bun run verify:qedgen:kani
```

Reuse a cached model proof file when the spec has not changed and you only need
per-proof Kani feedback:

```bash
KANI_REGEN=0 \
  KANI_SOURCE=/private/tmp/qedgen-loyal-kani-split/programs/tests/kani.rs \
  bun run verify:qedgen:kani
```

Run the full Kani model gate only when a long local proof is acceptable:

```bash
bun run verify:qedgen:kani:full
```

Full mode uses the same per-proof Kani output as smoke mode, but does not filter
the generated proof set and does not skip generated guard-rejection proofs.

For focused debugging, pass comma-separated Kani proof filters. Add
`KANI_EXACT=1` when the filter is a complete proof name:

```bash
KANI_EXACT=1 \
  KANI_HARNESS=cover_lane_rebalance \
  bun run verify:qedgen:kani
```

This Kani gate verifies the current spec model. Use the generated impl gate
below when the proof target should be the committed Pinocchio dispatcher.

## Generated Pinocchio Kani Impl Gate

`bun run verify:qedgen:kani-impl` regenerates QEDGen's Pinocchio
`--kani-impl` harnesses, copies the generated `kani_impl.rs` into a temporary
Loyal Hub crate, and runs a smoke slice one proof at a time. The smoke slice
proves successful dispatch for `initialize_config`, `set_max_fee`, and
`set_paused`, plus generated SPL Token balance deltas for `withdraw_inventory`,
`swap_exact_in`, single-transfer rebalance, four-transfer rebalance, and max
16-transfer rebalance:

```bash
bun run verify:qedgen:kani-impl
```

Run the full generated impl gate to check every generated proof, including
initialization, both config mutations, and rebalance arities 1 through 16:

```bash
bun run verify:qedgen:kani-impl:full
```

The script prints each selected proof before it starts, then records whether
that proof passed or failed with elapsed time. For focused debugging, pass
comma-separated proof names with `KANI_IMPL_HARNESS`.

## Live Pinocchio Kani Impl Gate

`bun run verify:hub-kani-impl` runs the committed Loyal Hub program's
`#[cfg(kani)]` live-handler proofs. These proofs call the real
`process_instruction` dispatch with Loyal Hub ABI tags and real config account
bytes. The current slice proves the non-CPI config mutations and the
single-transfer `withdraw_inventory`, one-, two-, four-, eight-, and max
16-transfer `rebalance_inventory`, and paired-transfer `swap_exact_in` token
movements against projected SPL Token account balances:

```bash
bun run verify:hub-kani-impl
```

The live proof module uses Kani-only PDA assumptions for config, hub-authority,
and inventory-account derivation, because Kani cannot prove Pinocchio's
bump-search panic path unreachable from the hash loop. Production code still
uses Pinocchio's `find_program_address`.

## Closed Proof Gaps

- The live Pinocchio proof module projects SPL Token account balances for
  `withdraw_inventory`, `rebalance_inventory` batches through max 16 transfers,
  and paired-transfer `swap_exact_in`. The current QEDGen fork also emits
  accepted Loyal Hub account-state construction, generated config-byte
  assertions for `initialize_config`, `set_max_fee`, and `set_paused`, and
  generated success-path `Token.transfer` balance assertions, so the generated
  impl proofs are non-vacuous for those slices.
- The rebalance spec mirrors the runtime wire account list, where the source
  authority is an account and the destination authority is derived internally.
  The live proof and the generated QEDGen impl proof now cover the modeled max
  rebalance batch.
- Multi-CPI balance proofs now rely on concrete SPL Token account projection for
  disjoint `Token.transfer` resources. The patched QEDGen checker still keeps
  `multi_cpi_same_field` available for repeated transfers that can touch the
  same resource.
- The current QEDGen Kani model gate proves the spec-translated transition
  model. The generated impl gate proves the committed Pinocchio dispatcher for
  initialization, config mutations, and all modeled token movement. The
  live-handler gate remains as a committed local cross-check.

## CI Posture

- CI should keep `verify:qedgen:kani` optional/manual until the smoke slice is
  stable enough for routine runs and the full Kani proof cost is acceptable.
  Treat `verify:qedgen:kani-impl` the same way until repeated local runs make
  its temporary-crate cost predictable.

## Commands

```bash
bun run verify:qedgen:check
bun run verify:hub-abi-spec-drift
bun run verify:qedgen:probe
bun run verify:qedgen:proptest
bun run verify:qedgen:kani
bun run verify:qedgen:kani-impl
bun run verify:qedgen:kani-impl:full
bun run verify:hub-kani-impl
bun run verify:qedgen
```
