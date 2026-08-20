# Autodeposit atomic finalization verifier

The implementation is complete only when the unchanged command below exits zero and prints
`PASS_AUTODEPOSIT_ATOMIC_FINALIZATION`:

```sh
bun run verify:autodeposit-atomic-finalization
```

The verifier must prove all of these properties against a disposable PostgreSQL database:

1. A confirmed Kamino top-up finalizes the deposit, total position, holding event, execution,
   claim, scheduled slot, and execution lots in one database operation.
2. A failure forced at the final execution update rolls the entire operation back. No deposit,
   principal change, holding event, claim transition, or slot transition may survive.
3. Replaying the same confirmed signature is idempotent: it cannot add principal, deposits,
   holding events, or execution lots twice.
4. A production-shaped partial state that already contains the signature's deposit and updated
   position can finish without a unique-key failure or a second principal increment.
5. Chain reconciliation used by the TypeScript executor is read-only. The Rust observer does not
   write `user_yield_positions`; the database finalizer is the only accounting writer.
6. Losing the claim lease is classified as executor contention and remains silent. A deterministic
   post-confirm persistence failure remains mapped to exit code 21 and the existing
   `yield_persistence_failed` operational alert.
7. Migration 45 is registered in the production `yield-migrations --apply` binary.

The verifier is deliberately production-shaped: it executes the same PostgreSQL function called
by `execute-autodeposit-policy.ts`, under the same unique position and signature constraints that
failed in production. Source-only substring checks are supporting guards, not the atomicity proof.
