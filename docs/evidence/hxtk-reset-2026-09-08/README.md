# HXtk reset evidence

These files are read-only rehearsal evidence for vault
`HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`. They are generated with the
`reset:hxtk` commands in `--simulate` mode against finalized public state.
They are not send, signature, broadcast, finalization, or reconciliation
proof: every file must keep `sent: false`, `signed: false`, and
`broadcast: false`.

The reset tool now reports `canonicalStateRoot` in every JSON output. Execute
and reconcile always derive it from `os.userInfo().homedir` as
`~/.loyal/hxtk-reset/<vault>/`; `HOME` and `HXTK_RESET_STATE_ROOT` cannot select
another fence namespace. Simulation resolves and prints that path but never
creates or writes under it. Operator state files bind the journal metadata and
finalized journal bytes to that root; later legs refuse a missing or changed
binding. Send/reconcile paths also hold an exclusive per-leg claim with a
process-held 16-byte token; stale claims require local-hostname and dead-pid
proof before an atomic `--break-claim` rename may be used. Canonical state
writes carry a generation and use a same-directory `0600` compare-and-swap.
Ambiguous-send fields remain volatile under `sendStatus`:
`{ verdict, sendError, submission, attemptedAtUnixMs, signature }`.

Evidence verdicts remain claims about the observed simulation only. The
2026-09-08 regeneration retained these statuses; the live read changed only
finalized context slots/timestamps. During the 2026-09-08 review cycle, a stale
shared-target compiler binary produced an 801-byte PolicyCreate payload. The
hard pin refused it before any evidence was written: the reviewed artifact is
837 bytes with SHA-256
`796624dfef068d71db889913de3527f36c23e19e11aafc9e370f021b649f153e`. The
per-checkout compiler target and exec-time binary provenance now prevent that
stale-target substitution. In
particular, `PENDING_*`, `REPAIR_FROZEN_STATE_MISMATCH`, and
`PRE_RESET_OR_PARTIAL` are blockers or current-state observations, not a
completed reset. The accompanying LiteSVM result files under `docs/evidence/`
remain separate proof artifacts and do not authorize mainnet mutation.
