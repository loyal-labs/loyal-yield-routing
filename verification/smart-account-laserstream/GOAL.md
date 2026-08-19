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
7. `loyal-app` exposes a long-lived targeted durable-job consumer that reuses
   the existing deposit and cleanup reconciliation functions and their
   canonical writers. The two superseded Vercel cron schedules are absent. It
   does not duplicate Earn accounting in Rust. Its focused tests pass.
8. The isolated E2E starts a disposable local PostgreSQL database, applies the
   production Yield migrations, feeds simulated policy creation, deposit ATA,
   obligation, shared-binding, balance-sweep-only, and policy
   deletion events through a production-backed fixture binary, and proves by
   SQL that:
   - exactly two vault jobs exist;
   - the unrelated balance-sweep-only account creates no Earn job;
   - vault A advances from policy creation at slot 100 to policy closure at
     slot 120;
   - vault B coalesces deposit ATA, obligation, and shared-account signals and
     retains slot 110 and the deposit signature;
   - exactly five Earn receipts exist after replay;
   - the durable cursor is 120;
   - a forced pre-commit failure leaves the cursor, receipts, and jobs
     unchanged;
   - replay and process restart leave all counts and high-water marks
     unchanged.
9. Focused Rust formatting, compilation, and tests pass, and both implementation
   worktrees have no whitespace errors.

## Nice to have

- A local RPC fixture exercises the existing slot-pinned proof readers.
- The targeted app consumer is exercised against the disposable database in
  addition to its focused dependency-injected tests.

## Verdict

`PASS` only when every Required condition passes. Otherwise print each failed
condition and finish with a nonzero exit status.
