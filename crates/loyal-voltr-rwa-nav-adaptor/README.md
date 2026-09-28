# Loyal Voltr RWA NAV adaptor v3

This program is a narrow authenticated bridge between one Voltr strategy and
one immutable Squads vault. It neither selects routes nor calculates economic
NAV. A serialized worker supplies the fixed `ReportV1` snapshot; this program
authenticates its origin and freshness, bounds the reported NAV against the
value Voltr actually books, and moves tokens so that Voltr's strategy custody
is exactly zero at every instruction boundary.

v3 config is a hard version bump: a v2 config account fails to decode, so the
strategy-one v2 config cannot be driven by v3 code.

## Immutable config

`initialize_config` creates a one-time v3 config bound to the pinned Voltr
program and one Voltr vault/strategy/strategy-authority PDA. It also freezes
the Squads program, Settings and Settings authority, vault index and rederived
vault PDA, asset mint/token program/ATA, maximum NAV, maximum report age, the
NAV step bound, and the minimum report interval.

There is no config update or mutable rebind instruction. The init wire is
exactly 41 bytes:

```text
vault_index: u8
max_report_nav_raw: u64
max_report_age_slots: u64
max_step_bps: u64        // <= 10_000 (100% of the receipt position); 0 allowed
step_floor_raw: u64      // 0 allowed
min_report_interval_slots: u64 // 0 disables
```

The on-chain layout keeps the v1/v2 width (16 + 12 pubkeys + 5 u64 + 32
bytes). The two u64 slots v1/v2 used for reserved `last_sequence` and
`last_observed_slot` now hold `max_step_bps` and `step_floor_raw`, the slot
they reserved for `last_nav_raw` holds `min_report_interval_slots`, and the
trailing `last_snapshot_digest` is the only field that must remain zero.

## Capital and report paths

Deposit and withdrawal receive exactly twelve accounts:

| # | Account | Privileges |
|---|---------|------------|
| 0 | Voltr strategy authority | signer, writable |
| 1 | adaptor config | read-only |
| 2 | asset mint | writable |
| 3 | strategy custody ATA (owned by the Voltr `vault_strategy_auth`) | writable |
| 4 | SPL Token program | read-only |
| 5 | Squads Settings | read-only |
| 6 | Squads vault PDA | read-only |
| 7 | Squads asset ATA | writable |
| 8 | report ticket PDA | writable |
| 9 | withdrawal holding ATA | writable |
| 10 | withdrawal holding authority PDA | read-only |
| 11 | Voltr strategy init receipt | writable or read-only (never written) |

Voltr's outer capital instruction already holds the receipt writable, and
Solana merges privileges by key, so account 11 arrives writable: the adaptor
accepts either privilege, requires it to be a non-signer, and never writes to
it. Both of these conditions are required on every capital/NAV path:

- the Voltr strategy authority is the exact PDA derived from the pinned Voltr
  program, configured vault, and configured strategy, and is a signer;
- the exact report-ticket PDA was armed, inside the report freshness window and
  before the Voltr call, by a direct adaptor instruction signed by the exact
  Squads vault PDA.

The Squads Settings check requires only that the pinned settings signer is
present in the member list with permission mask 7; member count, threshold and
timelock are unconstrained. The Settings layout is 56..58 threshold (u16),
58..62 timelock (u32 seconds), 62..70 transaction_index, and the adaptor reads
none of them. Audit finding U2 leaves the Squads timelock unconstrained, so
enabling one hardens governance without another adaptor upgrade — that is why
the old 58..62 == 0 requirement was dropped, not anything about the
transaction index at 62..70, which is per-transaction state the adaptor never
touches. The pinned Settings and vault addresses themselves remain exact.

Voltr selects and unwraps the configured eight-byte adaptor discriminator, then
forwards the amount followed by its original Borsh `Option<Vec<u8>>`
`additional_args`. Therefore deposit, withdrawal, and zero-amount NAV refresh
accept exactly this 78-byte instruction wire and no other framing:

```text
adaptor_discriminator: [u8; 8]
amount: u64
additional_args_some: u8 = 1
additional_args_len: u32 = 57
report: ReportV1
```

`Option::None`, a raw unwrapped report, a non-57 vector length, or trailing
bytes are rejected. `ReportV1` itself is exactly 57 bytes:

```text
version: u8 = 1
sequence: u64
observed_slot: u64
nav_after_raw: u64
snapshot_digest: [u8; 32]
```

`ArmReport` is a strict 79-byte direct-adaptor instruction and is unchanged in
v3: it has no receipt account, so the NAV step bound is enforced only where the
receipt is available, in the capital path. It binds operation, amount, and the
exact 57-byte report into a SHA-256 digest stored in the single ticket PDA
derived from `["report_ticket", strategy_config]`. A successful capital call
consumes the ticket, retains only the monotonic last sequence, records the
consumption slot, and clears its active sequence and digest.

What the program enforces on chain is the freshness window plus that wire
digest: the capital call accepts only the exact armed payload, only while it is
fresh. Executing arm and capital call in one Squads transaction (sync execute
of a single ProgramInteraction payload) is the operational recommendation
because it removes the window entirely, but the on-chain rule does not require
it — separate transactions are accepted as long as the report is still inside
its freshness window.

Ticket initialization is griefing-resistant: the ticket PDA is deterministic,
so anyone can pre-fund it with lamports and a plain `create_account` would fail
forever on the non-zero-lamport account. Initialization instead tops up the
rent shortfall from the payer and then allocates and assigns the account under
the PDA signature, and only ever touches a system-owned account with no data.

## Withdrawal: in-CPI holding pull

The withdraw path moves no Squads funds and never relies on pre-staged custody.
It first sweeps whatever is already in the strategy custody ATA to the Squads
asset ATA — donation residue included, under the strategy authority — and
requires that sweep to leave custody empty (`CustodyNotEmpty`), then pulls
exactly `amount` from the withdrawal holding ATA into custody with
`transfer_checked` under the adaptor's own PDA signature, requiring custody to
hold exactly `amount` before returning (`CustodyMismatch`):

```text
holding_auth = find_program_address(["withdrawal_holding", config], adaptor_program)
holding_ata  = ATA(holding_auth, asset_mint, SPL Token)
seeds        = ["withdrawal_holding", config, bump]
```

The holding ATA address, its owner (`holding_auth`), its mint, and its plain
SPL Token state are validated on every call. A holding balance above `amount`
is left in place; Voltr sweeps the custody ATA after this CPI returns, so the
adaptor leaves custody holding exactly the requested amount.

## Donations

A donation landing in the strategy custody ATA can neither block nor alter a
report. The step equation prices only the flow Voltr itself explains — the
capital instruction's `amount` — so custody residue is swept whole to the
Squads vault but never enters
`abs((nav_after_raw + outflow) - (position_value + inflow)) <= step`. A
donation of any size leaves every honest verdict unchanged: on deposit the
whole balance leaves together, on a zero-amount refresh the residue alone is
swept, and on withdrawal it is swept before the exact `amount` is pulled from
holding.

Unpriced residue is unreported external value until a later report by the
worker includes it, which is the safe direction: NAV under-states. Recognition
also requires a nonzero step budget: either `max_step_bps` or `step_floor_raw`
must be nonzero. With both set to zero, the receipt can never move, so a
donation remains unreported forever. When a report can recognise it, recognition
is bounded to at most one `step` per report if the donation is larger than one
step; scheduling that later report is a worker obligation. An unreported
donation therefore appears as safe-direction drift in the worker's NAV-drift
monitor, and the worker must hold or recover it rather than treat the book as
settled. `CustodyNotEmpty` (21) remains the fail-closed postcondition that a
sweep emptied custody, and `CustodyMismatch` (23) is the fail-closed
postcondition that a withdrawal left custody holding exactly `amount`.

## Deposit and refresh: whole-custody sweep

Voltr stages `amount` into custody before the CPI. The adaptor then sweeps the
entire custody balance — residue included, even on a zero-amount NAV refresh —
to the Squads asset ATA under the strategy authority, and requires custody to
be empty when the CPI ends. Custody below `amount` at entry fails with
`InsufficientBridgeLiquidity`: that is impossible while Voltr stages the
requested amount, so it fails closed.

## NAV step bound

Account 11 must be the Voltr receipt
`find_program_address(["strategy_init_receipt", voltr_vault, config],
voltr_program)`, owned by the Voltr program. The adaptor reads
`position_value` (u64 LE at offset 104) and enforces one flow-adjusted
equation per capital call, computed in u128, with
`step = max(position_value * max_step_bps / 10_000, step_floor_raw)`:

```text
abs((nav_after_raw + outflow) - (position_value + inflow)) <= step
```

The flows are the ones Voltr itself explains, not everything the CPI moves:
on deposit `inflow` is the capital instruction's `amount` and `outflow` is 0;
on withdrawal `outflow` is the exact holding pull and `inflow` is 0; on a
zero-amount refresh both are 0. Swept donation residue moves tokens but is
never priced, so a third party cannot shift this bound, while a capital amount
can never hide a write-down or a write-up. Violations fail with `ReportStep`.

## Minimum report interval

`min_report_interval_slots` caps how often the reported NAV may move, measured
on the slot a ticket is consumed and not on the report's observation slot: many
sequences can land in one slot, so a sequence gap proves nothing. The v2 ticket
(104 bytes) records `last_consumed_slot` at consumption, and the authoritative
check in the capital path is
`clock.slot >= last_consumed_slot + min_report_interval_slots`;
`validate_ticket_can_arm` applies the same rule as a precheck. Zero disables
the bound. Together with the step bound this prevents a compromised reporter
from compounding a maximal step every slot.

v3 rejects v1 tickets and v2 configs, so strategy one cannot drive this
program.

Extra bytes, a zero sequence/slot, any sequence that does not equal its
observed slot, future/stale slots, over-cap amounts or NAV, zero digests, a
non-192-byte receipt, nonzero Voltr tracked custody, an unknown receipt, a
step-bound violation, and an interval violation all reject before any token
CPI. The sweep and pull postconditions are the exception: they are checked
after their token CPI, and failing them aborts the transaction, so no partial
state survives either way. Config remains
immutable and read-only. The one-use ticket plus the serialized trusted Squads
delegate and database journal are the Phase-1 replay boundary.

The current external gate is V02's canonical signed-unsent
Squads -> ArmReport -> Voltr -> adaptor simulation. It must prove that Voltr
forwards the appended ticket as writable and that the new holding and receipt
accounts survive Voltr's account forwarding; this program intentionally does
not fall back to address-only authorization.

## Building the deployable artifact

`bun run build:adaptor` (`scripts/build-adaptor.sh`) is the only supported
build. It runs `cargo-build-sbf` for this crate with
`CARGO_PROFILE_RELEASE_LTO=fat CARGO_PROFILE_RELEASE_CODEGEN_UNITS=1` and
`CARGO_TARGET_DIR` unset, prints the artifact's size and sha256, reads the
ProgramData capacity and expected hash from the deployer's `ADAPTOR_SPEC_V3`
pin, and exits non-zero if the size exceeds that capacity or the hash does not
match the pin (`--expect-sha <sha256>` overrides the pin check for re-pin
bootstrap).

The program only fits ProgramData (115,384 bytes) with link-time optimization:
the v3.3 artifact is 107,832 bytes (sha256
`836ded9ff4e79cda9fafbafcffcf9f2e9f395c762c69ba5af4630e82a9d8a4d0`), leaving
7,552 bytes of headroom. A default `cargo-build-sbf` build of the same source
is about 119 KB: it exceeds ProgramData capacity and does not match the pinned
hash, so the script and the deployer refuse it instead of shipping a surprise.
