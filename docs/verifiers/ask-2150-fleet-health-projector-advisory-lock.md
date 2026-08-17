# ASK-2150 fleet health projector verifier

Verify the local checkout only. Do not use production credentials, query Neon,
change Render, or deploy anything.

The implementation passes only when `bash scripts/verify-ask-2150-fleet-health-projector.sh`
exits zero and every assertion below is backed by its temporary PostgreSQL run.

## Required behavior

1. A refresh opens one database transaction and acquires one cluster-scoped
   PostgreSQL transaction advisory lock with `pg_try_advisory_xact_lock`.
2. A competing refresh returns `Busy`; contention is not an error and cannot
   terminate the projector loop.
3. Rolling back or dropping the lock-holding transaction releases the lock
   immediately. The next refresh can publish without waiting for a TTL.
4. The freshness check, source aggregation, source-watermark read, snapshot
   upsert, and commit all occur on the same transaction/connection.
5. A snapshot newer than the configured refresh interval returns `NotDue` and
   is not rewritten.
6. Two concurrent refresh attempts produce at most one `Published` result.
7. A forced database error during publication rolls back the transaction and
   preserves the previous snapshot byte-for-byte.
8. An existing live row in `fleet_health_projection_leases` cannot block,
   authorize, or otherwise alter publication and is not mutated by refresh.
9. The runtime no longer constructs PID-derived owners, accepts lease TTL
   configuration, claims application leases, or treats advisory-lock
   contention as fatal.
10. The checked-in Render command and its release verifier no longer pass the
    removed `--lease-seconds` option.

## Required evidence

- The dedicated isolated database-contract test passes on a database whose
  name contains `fleet_verify`.
- The projector unit tests and `cargo check` pass.
- Static runtime/config assertions in the verifier script pass.
- `cargo fmt --all -- --check` and `git diff --check` pass.

Overall verdict is `PASS` only if all required behavior and evidence pass.
`NOT_RUN`, missing dependencies, timeouts, or partial evidence are failures.
