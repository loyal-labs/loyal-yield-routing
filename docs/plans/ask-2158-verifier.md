# ASK-2158 Verifier: ALT Catalog Revision Fence

Run this verifier from a clean ASK-2158 implementation checkout:

```sh
bash scripts/verify-ask-2158-alt-catalog-race.sh
```

## Required end state

The verifier returns `PASS` only when all of these are true:

1. A disposable PostgreSQL fixture creates an active shared-market catalog
   revision A, records its durable catalog state, then publishes pending revision
   B before A's preflight/reconciliation work is applied.
2. Revision-fenced preflight for A returns a normal no-op after B becomes current.
   It does not return `StoreInvariant`, and it does not change the B catalog head,
   lookup-table operations, provisioning requests, or broadcast permits.
3. Revision-fenced reconciliation for A likewise returns a normal no-op with the
   same zero-mutation evidence.
4. Strict validation of the still-current pending revision B still fails with the
   typed store invariant `shared-market catalog has no target generation`. A real
   same-revision corruption is therefore not hidden by the stale-snapshot path.
5. The provisioner treats the stale no-op as no work and leaves re-reading to its
   existing watch loop. It does not add a retry supervisor, backoff, or retry
   boundary around `run_operation_batch`.
6. The ASK-2143 verifier still passes, proving transient read-only RPC recovery,
   non-transient RPC failure behavior, and the no-batch-replay boundary are
   unchanged.
7. Focused database behavior, formatting, compilation, and diff-integrity checks
   pass without external RPC, signer loading, or production writes.

## Explicitly outside this verifier

- Production deployment or restart.
- ClickStack alert-rule mutation.
- Generic retry infrastructure.
- Signed transaction or broadcast retry changes.
- Schema migration changes.

## Verdict

`PASS` only if the verifier script exits zero and prints
`PASS: ASK-2158 ALT catalog revision fence`. Any missing race fixture, stale
snapshot error, stale-path mutation, hidden same-revision invariant, ASK-2143
regression, compiler failure, or formatting failure is `FAIL`.
