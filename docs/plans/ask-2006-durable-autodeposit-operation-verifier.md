# ASK-2006 single-owner autodeposit verifier

The fix is complete only when one existing lot claim owns the unavoidable pull
and Kamino-deposit transactions. Transaction attempts are the only durable
record of transaction state. No parallel operation state machine or fleet
idle-balance handoff may participate in this direct path.

Run from the repository root:

```sh
bun run verify:autodeposit-durable-operation
bun run verify:autodeposit-stale-current-reserve
bun test scripts/durable-autodeposit-confirmation.test.ts scripts/execute-autodeposit-policy.test.ts
cargo fmt --all -- --check
cargo check -p balance-sweep-autodeposit-trigger -p loyal-fleet-worker -p loyal-yield-orchestrator
cargo test -p balance-sweep-autodeposit-trigger
cargo test -p loyal-yield-orchestrator --bin yield-migrations durable_autodeposit_operation_migration_is_registered_for_production
git diff --check
```

The first command must exit zero and print
`PASS_AUTODEPOSIT_DURABLE_OPERATION`. Every other command must exit zero.

Required observable properties:

1. `balance_sweep_lot_claims` is the sole job owner. It holds one expiring
   executor lease and one immutable direct-deposit plan. There is no
   `balance_sweep_autodeposit_operations` table or runtime query. Migration 40
   is registered in the production `yield-migrations` binary used by Render.
2. `balance_sweep_transaction_attempts` is the sole transaction ledger. Exact
   signed pull and top-up bytes are persisted before first broadcast and
   reconciled by signature before any replacement.
3. Progress is derived from attempt facts: no confirmed pull means pull work;
   confirmed pull without confirmed top-up means deposit work; both confirmed
   permit atomic completion. There is no second operation-state enum or copied
   pull/top-up signature state.
4. One claim lease fences concurrent executors. Completion first locks and
   validates that lease; every accounting mutation depends on that locked row.
   A stale owner cannot persist, broadcast, release, or complete work.
5. Deposit readiness and an immutable plan are stored before pull broadcast.
   Pull-only state cannot complete the claim, slot, execution, or success
   notification. Restart recovery of a persisted pull attempt does not depend
   on the target or its route policy remaining active.
6. The direct executor never publishes its funds to
   `vault_idle_token_balances_current`. The fleet planner cannot race the direct
   deposit: its idle-vault candidate query excludes a vault while a selected
   claim for the same mint has a confirmed pull and no confirmed top-up. The
   route helper only builds/simulates the top-up; the owner persists and
   broadcasts the exact returned transaction. A new direct pull is deferred
   while the vault already has idle funds, so a previously planned fleet
   deposit cannot consume the new pull.
7. Completion of the execution, app deposit/position/holding history, claim,
   and scheduled slot is one database statement gated by a confirmed top-up
   attempt owned by the same claim. Principal increases by this deposit delta;
   current holding uses the reconciled post-confirm Kamino total.
8. Retryable readiness, RPC, expiry, and send failures remain pending without
   an operational alert. Ambiguous top-ups retain a typed failure that reaches
   the trigger alert boundary. Ambiguous chain effect, lost
   ownership/invariants, or another explicit operator-action condition may
   alert. The removed idle-age SLA alert stays absent.
9. Runtime packaging contains every imported executor module and no removed
   duplicate operation-model module. The obsolete idle-handoff verifier is
   retired, and the still-relevant stale-reserve verifier uses the prepare-only
   route helper.

Verdict: FAIL if any required property or command fails. PASS only when every
required property and command passes.
