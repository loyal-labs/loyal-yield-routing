# ASK-2211 client-sent Autodeposit LaserStream verifier

Run `bash scripts/verify-ask-2211-autodeposit-client-laserstream.sh` from the
isolated ASK-2211 worktree. The script is the frozen verifier-first goal. It
must exit nonzero on the first failed requirement and may print the final PASS
line only after every required condition below succeeds.

## Required conditions

1. The existing balance-sweep monitor keeps one finalized, account-only
   LaserStream request. There is no transaction filter, second connection, or
   new deployed service.
2. A ready/configured Earn wallet contributes exact settings, vault, vault
   USDC ATA, wallet USDC ATA, subscription authority, expected Autodeposit
   policy, and expected recurring-delegation addresses to the watch set.
3. User configuration (`desired_active`, floor, expected identities,
   generation) is stored separately from the objective on-chain projection.
   Replayed chain observations cannot re-enable a paused configuration or
   overwrite its floor.
4. One wallet-scoped finalized account snapshot derives only `pending`,
   `active`, `closed`, or `inconsistent`. It validates current policy,
   subscription authority, recurring delegation, and SPL-token delegation;
   it does not depend on a client-posted signature or stage confirmation.
5. Sweep eligibility requires both `desired_active` and projected `active`.
   Config-only, chain-only, incomplete, closed, and inconsistent states do not
   execute.
6. First activation schedules bootstrap surplus once per configuration
   generation. Close performs canonical shutdown. Duplicate, out-of-order,
   and close-before-create observations neither duplicate nor resurrect state.
7. Configuration changes wake the existing monitor immediately, polling stays
   as fallback, and a stored observation boundary lets a rebuilt watch set
   replay transactions sent immediately after configuration.
8. Projection changes emit one private `earn.autodeposit.changed` durable
   invalidation for the existing SSE service. SSE remains an invalidation;
   clients refetch canonical state.
9. Pause/resume, later floor changes, Execute Now, durable Earn jobs/cursors,
   and the existing Autodeposit trigger/executor remain intact.
10. Production migrations apply in disposable PostgreSQL. Production store and
    reconciliation code prove configuration/projection separation, lifecycle,
    eligibility, activation, close, replay idempotency, and realtime emission.
    Focused Rust tests/checks, formatting, and `git diff --check` pass.

## Rejected shortcuts

- Source-substring-only evidence.
- Global transaction subscriptions or broad SPL Token owner scans.
- A new worker/service.
- Client-supplied confirmation signatures.
- Per-stage event sourcing.
- Tests that write the expected final rows without invoking production
  store/reconciler writers.

## Verdict

The exact final line must be:

```text
PASS: ASK-2211 Autodeposit is client-sent and LaserStream-reconciled
```

