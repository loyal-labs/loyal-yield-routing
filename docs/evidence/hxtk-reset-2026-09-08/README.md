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
binding. Send/reconcile paths also hold an exclusive per-leg claim; a stale
claim requires proving its pid is gone before `--break-claim` may be used.
Ambiguous-send fields remain volatile under `sendStatus`:
`{ verdict, sendError, submission, attemptedAtUnixMs, signature }`.

Evidence verdicts remain claims about the observed simulation only. The
2026-09-08 regeneration retained these statuses; the live read changed only
finalized context slots/timestamps. The repair-policy simulation remains the
previously recorded PASS artifact because the current local compiler produced
an unreviewed 801-byte/`f2ef...` PolicyCreate instead of the pinned
837-byte/`796624...` artifact, so the tool refused to emit replacement
evidence. In
particular, `PENDING_*`, `REPAIR_FROZEN_STATE_MISMATCH`, and
`PRE_RESET_OR_PARTIAL` are blockers or current-state observations, not a
completed reset. The accompanying LiteSVM result files under `docs/evidence/`
remain separate proof artifacts and do not authorize mainnet mutation.
