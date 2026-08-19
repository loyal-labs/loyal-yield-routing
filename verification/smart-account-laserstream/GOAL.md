# Smart-account LaserStream verifier goal

Run `verification/smart-account-laserstream/verify.sh` from the verifier
worktree with independent routing and app implementation worktrees.

The verifier is adversarial. It decides whether the implementation has replaced
periodic Earn chain discovery with one multi-channel LaserStream wake-up path
without turning the monitor into a generic event-sourcing platform.

## Required conditions

1. The production request builder emits one confirmed `SubscribeRequest` with:
   - account filters `balance_sweep_wallet_atas`, `earn_policy_accounts`,
     `earn_vault_accounts`, `earn_idle_token_accounts`, and
     `earn_obligations`;
   - no transaction filters, because monitored account updates already carry
     the transaction signature;
   - no `earn_reserves` account filter;
   - deterministic, deduplicated address lists and no empty broad filter.
2. Production normalization preserves matching filter names and handles
   account updates and account deletion/tombstones. One pubkey
   may have both Earn and balance-sweep bindings without either being lost.
3. Every Earn update is reduced to affected-vault reconciliation hints. It is
   not decoded as a balance-sweep wallet observation and does not create a
   balance-sweep lot.
4. A single Neon transaction inserts a compact idempotency receipt, coalesces
   the affected vault job, and advances the replay cursor. If that transaction
   fails, none of those three effects is visible.
5. Replaying the same events and restarting the producer creates no duplicate
   receipts or jobs, never lowers the cursor, and preserves the highest trigger
   slot/signature for each vault.
6. The implementation contains no new generic raw LaserStream event table,
   projection-offset framework, catalog-generation state machine, or
   reserve-update fan-out.
7. The existing `loyal-fleet-route-reconciler` process exposes an independently
   bounded Earn lane that leases durable vault jobs, consumes every unprocessed
   receipt, performs targeted slot-pinned proofs, and commits canonical Earn
   writes with receipt completion and the fenced job transition in one Neon
   transaction. No new Loyal App or Render worker is introduced.
8. The isolated E2E starts a disposable local PostgreSQL database, applies the
   production Yield migrations, feeds simulated policy creation, deposit ATA,
   obligation, shared-binding, balance-sweep-only, and policy deletion events
   through production producer and consumer code, and proves by SQL that:
   - policy-only onboarding creates the expected policy, managed-vault, and
     onboarding rows;
   - invisible deposits create exactly one deposit, aggregate position, and
     holding event per signature;
   - `confirm_missed` and `cleanup_pending` close and zero canonical state only
     after the slot-pinned zero proof;
   - multiple signatures coalesced into one vault job are all consumed;
   - positive balances, RPC lag, and forced pre-commit failure write no false
     canonical state and leave work retryable;
   - a newer update fences an older lease;
   - replay and process restart create no duplicate accounting;
   - Earn updates create zero balance-sweep observations, events, or lots;
   - the producer replay cursor is monotonic and advances only with durable
     receipts/jobs.
9. Focused Rust formatting, compilation, and tests pass, and both implementation
   worktrees have no whitespace errors.

The complete design and rollout contract is recorded in
`docs/plans/ask-2173-earn-laserstream-reconciliation-plan.md`.

## Verdict

`PASS` only when every Required condition passes. Otherwise print each failed
condition and finish with a nonzero exit status.
