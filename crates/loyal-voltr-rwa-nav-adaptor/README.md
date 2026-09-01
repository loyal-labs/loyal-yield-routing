# Loyal Voltr RWA NAV adaptor v2

This program is a narrow authenticated bridge between one Voltr strategy and
one immutable Squads vault. It neither selects routes nor calculates economic
NAV. A serialized worker supplies the fixed `ReportV1` snapshot; this program
authenticates its origin and freshness before returning the reported NAV to
Voltr.

## Immutable config

`initialize_config` creates a one-time v2 config bound to the pinned Voltr
program and one Voltr vault/strategy/strategy-authority PDA. It also freezes the
Squads program, Settings and Settings authority, vault index and rederived vault
PDA, asset mint/token program/ATA, maximum NAV, and maximum report age.

There is no config update or mutable rebind instruction. The deployed v2 layout
retains its historical `last_*` fields for compatibility, but they are reserved
and every one must remain exactly zero.

## Capital and report paths

Deposit and withdrawal receive exactly nine accounts: Voltr strategy authority,
read-only config, asset mint, strategy ATA, SPL Token program, Squads Settings,
Squads vault, Squads asset ATA, and the writable report-ticket PDA. Both of
these conditions are required on every capital/NAV path:

- the Voltr strategy authority is the exact PDA derived from the pinned Voltr
  program, configured vault, and configured strategy, and is a signer;
- the exact report-ticket PDA was armed immediately before the Voltr call by a
  direct adaptor instruction signed by the exact Squads vault PDA.

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

`ArmReport` is a strict 79-byte direct-adaptor instruction. It binds operation,
amount, and the exact 57-byte report into a SHA-256 digest stored in the single
ticket PDA derived from `["report_ticket", strategy_config]`. The same atomic
Squads ProgramInteraction payload must then execute the Voltr capital call. A
successful capital call consumes the ticket, retains only the monotonic last
sequence, and clears its active sequence and digest.

Extra bytes, a zero sequence/slot, any sequence that does not equal its observed
slot, future/stale slots, over-cap amounts or NAV, and zero digests fail before
any token CPI. Config remains immutable and read-only. The one-use ticket plus
the serialized trusted Squads delegate and database journal are the Phase-1
replay boundary.

A nonzero deposit transfers exactly the requested raw asset amount from the
Voltr strategy ATA into the Squads asset ATA. A zero deposit transfers nothing
but can accept a new report. Withdrawal never pulls through a delegate: it
requires the requested funds already staged in the strategy ATA, accepts the
post-withdraw report, and returns that reported NAV.

The current external gate is V02's canonical signed-unsent
Squads -> ArmReport -> Voltr -> adaptor simulation. It must prove that Voltr
forwards the appended ticket as writable; this program intentionally does not
fall back to address-only authorization.
