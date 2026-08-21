# Durable Earn LaserStream reconciliation verifier

Run `scripts/verify-earn-laserstream-reconciliation.sh` from the isolated
implementation worktree. Pass `--app-root` only when Loyal App is not available
at the repository's usual sibling path.

The verifier is adversarial. It returns PASS only when LaserStream account
updates are durably accepted independently from chain proof and canonical Earn
reconciliation. One PASS line is printed per condition and the script exits
nonzero on the first violation.

## Required conditions

1. The production request has the five account channels and no transaction
   channel, reserve fan-out, broad empty filter, or duplicate address.
2. The stream reader only normalizes and durably enqueues Earn updates. Enqueue
   of every affected vault job and replay-cursor advancement are one database
   transaction; the cursor never advances without the durable jobs.
3. Duplicate delivery has one job per `(consumer, event, vault)` and cannot
   duplicate canonical deposits, holdings, or cleanup.
4. A crash after enqueue is recoverable: canonical state may still be old, but
   the job is pending and a later in-process consumer run completes it.
5. Chain/RPC proof failure is recorded on the job with its attempt count and
   next retry time. It does not escape the LaserStream event loop, stop ATA
   updates, rebuild the subscription, or trigger a full ATA seed.
6. The reconciliation consumer runs in the existing monitor process, not in a
   new Render/fleet/Loyal App worker. It claims bounded work, fences completion,
   and atomically applies the canonical mutation plus job completion.
7. The production cleanup inventory lookup uses owner-scoped token-account RPC
   for both SPL Token and Token-2022. It never scans a whole token program with
   `getProgramAccounts`.
8. Policy-only onboarding, invisible deposits, top-ups, and both full-withdraw
   cleanup classes retain the canonical outcomes covered by the previous E2E.
   Principal and observed holding semantics, active-attempt selection,
   cross-reserve position reuse, and exact-once history remain intact.
9. The durable cursor represents ingestion, so it can advance while proof work
   is pending. Restart/replay converges from queued work without losing the
   failing event or replaying the monitor's whole lifetime.
10. Earn traffic creates no balance-sweep wallet observations, surplus lots, or
    executions. A proof failure also causes no bulk ATA reseed.
11. Loyal App exposes no old Earn cron routes/schedules and adds no replacement
    worker. Routing adds no new deployed service.
12. Current migrations remain registered and the durable Earn job migration is
    registered once, after current migration 48, in both routing registries. Apply and
    check work in isolated PostgreSQL.
13. Formatting, targeted Rust tests/checks, database assertions, and whitespace
    checks pass.

## Rejected shortcuts

- Performing RPC or canonical reconciliation in the LaserStream event loop.
- Advancing the cursor before the complete affected-vault job set is durable.
- Treating a failed proof as a consumed/completed job.
- Retrying proof failures by restarting LaserStream or reseeding every ATA.
- A generic blockchain event store, transaction subscription, or new worker.
- `getProgramAccounts` against either token program.
- A fixture that writes expected canonical rows without the production store
  and reconciliation code.

## Verdict

The final line must be `PASS: durable LaserStream ingestion and isolated Earn
reconciliation converge exactly once`. Any unmet condition is an overall FAIL.
