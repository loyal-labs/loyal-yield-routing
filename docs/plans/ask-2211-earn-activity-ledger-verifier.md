# ASK-2211 Earn activity ledger verifier

Run this verifier cold against isolated Loyal App and Yield Routing worktrees.

```sh
bash scripts/verify-ask-2211-earn-activity-ledger.sh \
  /private/tmp/loyal-yield-routing-ask-2211-activity-ledger \
  /private/tmp/loyal-app-ask-2211-activity-ledger
```

## Required conditions

1. A finalized Autodeposit setup writes one permanent `autodeposit_created`
   activity event with a stable idempotency key.
2. A finalized Autodeposit close writes one permanent `autodeposit_closed`
   event without deleting or rewriting the setup event.
3. Replaying the same setup or close evidence creates no duplicate activity.
4. Finalized Autoswap setup and close write permanent `autoswap_created` and
   `autoswap_closed` events through the same activity ledger.
5. The projector writes each lifecycle event in the same database transaction
   as the corresponding current-state update. A failed event insert must roll
   back the current-state change, so the caller cannot advance replay after a
   partial write.
6. The web Activity API reads configuration history from the append-only
   activity ledger. It must not reconstruct history from mutable
   `balance_sweep_targets` confirmation fields.
7. The response includes Autodeposit and Autoswap create and close events, and
   excludes internal `snapshot_reconciled` observations.
8. The routing database migration, focused database behavior test, app
   repository and formatter tests, Rust checks, web TypeScript, scoped lint,
   formatting, and both worktree diff checks pass.

## Verdict

`PASS` only when every required condition above passes. Any missing event,
duplicate replay, non-atomic state change, legacy target-history dependency,
user-visible snapshot event, or failing command is an overall `FAIL`.

