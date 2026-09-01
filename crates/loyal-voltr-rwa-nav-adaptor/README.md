# Loyal Voltr RWA NAV adaptor v2

This program is a narrow authenticated bridge between one Voltr strategy and
one immutable Squads vault. It neither selects routes nor calculates economic
NAV. A serialized worker supplies the fixed `ReportV1` snapshot; this program
authenticates its origin, freshness, and ordering before returning the reported
NAV to Voltr.

## Immutable config

`initialize_config` creates a one-time v2 config bound to the pinned Voltr
program and one Voltr vault/strategy/strategy-authority PDA. It also freezes the
Squads program, Settings and Settings authority, vault index and rederived vault
PDA, asset mint/token program/ATA, maximum NAV, and maximum report age.

There is no config update or mutable rebind instruction.

## Capital and report paths

Deposit and withdrawal receive exactly eight accounts: Voltr strategy authority,
writable config, asset mint, strategy ATA, SPL Token program, Squads Settings,
Squads vault, and Squads asset ATA. Both of these conditions are required on
every capital/NAV path:

- the Voltr strategy authority is the exact PDA derived from the pinned Voltr
  program, configured vault, and configured strategy, and is a signer;
- the Squads vault is the exact PDA rederived from the immutable Squads program,
  Settings, and vault index, and is a signer.

`ReportV1` is exactly 57 bytes after the amount:

```text
version: u8 = 1
sequence: u64
observed_slot: u64
nav_after_raw: u64
snapshot_digest: [u8; 32]
```

Extra bytes, skipped/replayed sequences, regressed/future/stale slots,
over-cap NAV, and zero digests fail before config state changes. The accepted
sequence, slot, NAV, and digest are saved before return data is set.

A nonzero deposit transfers exactly the requested raw asset amount from the
Voltr strategy ATA into the Squads asset ATA. A zero deposit transfers nothing
but can accept a new report. Withdrawal never pulls through a delegate: it
requires the requested funds already staged in the strategy ATA, accepts the
post-withdraw report, and returns that reported NAV.

The current external gate is V02's canonical signed-unsent
Squads -> Voltr -> adaptor simulation. It must prove that both signer privileges
and writable config privilege propagate through Voltr; this program intentionally
does not fall back to address-only authorization.
