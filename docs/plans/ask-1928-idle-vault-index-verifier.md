# ASK-1928 Idle-Vault Index Verifier

## Goal

Verify that the Yield Neon migration adds the exact partial index needed by the
admin Earn rebalance audit and that PostgreSQL uses it to materially accelerate
the audit's correlated idle-vault decision lookup.

## Required checks

1. Migration `0033_idle_vault_decision_lookup_index.sql` creates
   `rebalance_decisions_idle_signature_id_idx` on
   `loyal_yield.rebalance_decisions (signature, id DESC)`.
2. The index predicate is exactly
   `execution_plan->>'kind' = 'idle_vault_deposit'`.
3. Migration 33 is registered after PR #24's migration 32 in the production
   `yield-migrations` runner.
4. When `ADMIN_REBALANCE_DATA_FILE` is supplied, the verifier confirms the
   admin query uses the same signature equality, predicate, descending ID
   order, and one-row limit.
5. An interrupted concurrent-build catalog entry with `indisready = false` and
   `indisvalid = false` is dropped and rebuilt before migration success.
6. On an isolated local PostgreSQL cluster with production-scale cardinalities
   (19,620 deposits and 3,621 decisions), the post-migration plan uses the new
   index and returns the same aggregate lookup result.
7. The rebuilt index is both ready and valid in `pg_index`.
8. The indexed execution is at least 10 times faster than the unindexed
   execution and completes in under 500 ms.
9. The verifier never connects to Neon or any other external database.

## Command

From the `loyal-yield-routing` repository root:

```sh
ADMIN_REBALANCE_DATA_FILE=/path/to/loyal-app/admin/src/app/\(admin\)/earn/rebalance/rebalance-data.ts \
  bun run verify:admin-rebalance-index
```

The command must finish with `PASS: ASK-1928 idle-vault index verifier`.
