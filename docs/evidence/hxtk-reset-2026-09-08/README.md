# HXtk reset evidence

These files are read-only rehearsal evidence for vault
`HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`. They are generated with the
`reset:hxtk` commands in `--simulate` mode against finalized public state.
They are not send, signature, broadcast, finalization, or reconciliation
proof: every file must keep `sent: false`, `signed: false`, and
`broadcast: false`.

The reset tool now reports `canonicalStateRoot` in every JSON output. The
default is `${HOME}/.loyal/hxtk-reset/<vault>/`; an explicit
`HXTK_RESET_STATE_ROOT` override must be absolute, current-user-owned, and
private. Simulation resolves and prints that path but never creates or writes
under it. Operator state files bind the journal metadata and finalized journal
bytes to that root; later legs refuse a missing or changed binding.

Evidence verdicts remain claims about the observed simulation only. In
particular, `PENDING_*`, `REPAIR_FROZEN_STATE_MISMATCH`, and
`PRE_RESET_OR_PARTIAL` are blockers or current-state observations, not a
completed reset. The accompanying LiteSVM result files under `docs/evidence/`
remain separate proof artifacts and do not authorize mainnet mutation.
