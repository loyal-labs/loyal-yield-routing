# ASK-1973 isolated fleet E2E verification

Verified at `2026-08-06T19:59:48Z` with:

```sh
op run --env-file=/Users/zotho/Dev/loyal/.env.1password.loyal-noncritical-env -- \
  bun run verify:ask-1973-fleet-e2e
```

Result: `PASS`

## Verified surfaces

- Built all six durable fleet binaries in the release profile from the
  refactored workspace crate graph.
- Applied database migrations 1 through 32 to a disposable local PostgreSQL
  instance.
- Passed the isolated database verifier with 4,160 runnable jobs, 10,000
  ALT-cold jobs, and 10,000 inert jobs. All 4,160 runnable jobs were claimed;
  the baseline and ALT-cold claim p95 values were 1,023,839 microseconds and
  984,702 microseconds. The run observed 64 nonoverlapping concurrent leases,
  zero database deadlocks, zero duplicate active-vault movements, and zero
  overlapping-lane limit violations.
- Passed seven deterministic planner rounds over 10,000 vaults. Planning p95
  was 22,086 microseconds against the 10,000,000-microsecond limit, with
  economic priority ordering preserved.
- Started the planner, revalidator, executor, confirmer, reconciler, and
  priority provisioner together against the disposable database and loopback
  RPC. The real revalidator and executor each claimed and completed 2,080
  deliberately incomplete jobs at their production concurrency of 16 and 4.
  All 4,160 jobs were durably terminalized with reasons and no lease remained.
- The loopback RPC audit observed exactly five `getGenesisHash` calls and no
  account, fee, status, signing, or transaction method.
- Passed the health-poll contention harness. Interval pacing reduced status
  view backend time from 50,341 milliseconds to 7,800 milliseconds and duty
  cycle from 83% to 13%, with zero victim-pool acquisition timeouts in both
  arms.

## Boundary

This is local isolated verification. It does not prove a registry image,
Render deployment, production database behavior, or a successful Solana
transaction. No image was pushed, no service was deployed, and no production
database or RPC was accessed. The process-load cohort intentionally exercises
fail-closed worker parsing, leasing, concurrency, and durable transitions; the
isolated database verifier covers successful queue, fence, and concurrency
contracts without requiring external infrastructure.

The full evidence bundle for this run was emitted to
`/tmp/ask1973-fleet-e2e-evidence.RsrKt0` on the verification host.
