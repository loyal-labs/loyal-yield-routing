# Direct Earn LaserStream reconciliation verifier goal

Run `verification/smart-account-laserstream/verify.sh` from this verifier
worktree against separate routing and Loyal App implementation worktrees.

The verifier is adversarial. It returns PASS only when confirmed LaserStream
account updates directly converge canonical Earn state inside the existing
LaserStream process. It must reject the previous Neon receipt/job handoff and
any new Loyal App or fleet reconciliation worker.

## Required conditions

1. The production request builder emits one confirmed account subscription with
   filters `balance_sweep_wallet_atas`, `earn_policy_accounts`,
   `earn_vault_accounts`, `earn_idle_token_accounts`, and `earn_obligations`.
   It has no transaction filters, no reserve fan-out, no empty broad filter,
   and deterministic deduplicated addresses.
2. Account updates and account deletion/tombstones preserve their filter names,
   signature, slot, and every affected vault binding. A pubkey shared with the
   balance-sweep filter does not lose either binding.
3. The production event loop invokes Earn reconciliation directly through a
   bounded in-process path. No `earn_reconciliation_jobs`,
   `earn_reconciliation_receipts`, job leasing/fencing, fleet-worker Earn lane,
   or Loyal App consumer exists.
4. Canonical Earn mutations happen before the durable LaserStream cursor
   advances. A failed proof or forced pre-commit failure advances no cursor and
   leaves no partial canonical state. Replay after a write-before-cursor crash
   is safe because canonical writes are idempotent.
5. The same production reconciliation engine is exercised with a deterministic
   chain reader in the isolated E2E. The fixture layer supplies simulated
   confirmed transaction/account evidence; it does not write expected database
   rows itself.
6. Policy-only onboarding recovery validates and records the route/setup policy
   pair, managed vault, and `setup_policy_confirmed` onboarding state.
7. Invisible deposit recovery records exactly one deposit by signature, the
   correct principal, an active aggregate position, one holding event, managed
   reserve/idle state, and completed onboarding. Replay creates no duplicates.
8. Full-withdraw cleanup covers both cases:
   - `confirm_missed`: zero proof succeeds and policies are already closed;
   - `cleanup_pending`: zero proof succeeds while policies remain open.
   Both cases deactivate canonical policies/vaults, zero reserve and idle rows,
   and close/zero the active position using the correct evidence signature.
9. A positive-balance proof is a successful no-op: it writes no zero snapshot
   and advances the cursor because a later balance change will wake the vault
   again. RPC context below the withdrawal slot is retryable: it writes nothing
   and does not advance past that event.
10. Events are processed in stream order without dropping an earlier signature
    when later updates affect the same vault. Restart replay converges to the
    same final database state.
11. Earn fixture traffic creates zero balance-sweep wallet events, surplus lots,
    or executions.
12. Loyal App no longer exposes or schedules `earn-deposit-reconcile` or
    `earn-cleanup-reconcile`, and it adds no replacement worker.
13. Focused Rust formatting, compilation, tests, isolated PostgreSQL assertions,
    and whitespace checks pass.

## Rejected shortcuts

- Source-string assertions in place of database behavior.
- A fixture binary that inserts expected rows without calling the production
  reconciliation engine.
- Advancing the cursor before reconciliation and relying on a later scan.
- A durable job/receipt table disguised under another name.
- A transaction subscription or generic blockchain event store.
- Moving the consumer into `loyal-fleet-route-reconciler`.

## Verdict

Print one PASS line per required condition and finish with overall PASS only if
every condition holds. Otherwise print the exact failed condition and exit
nonzero.
