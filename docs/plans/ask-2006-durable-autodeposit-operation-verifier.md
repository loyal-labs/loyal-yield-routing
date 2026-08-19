# ASK-2006 durable autodeposit operation verifier

The implementation is complete only when one durable operation owns both unavoidable Solana transactions: the vault pull and the Kamino deposit.

Run from the repository root:

```sh
bun run verify:autodeposit-durable-operation
bun test scripts/durable-autodeposit-operation.test.ts scripts/durable-autodeposit-confirmation.test.ts scripts/execute-autodeposit-policy.test.ts
cargo fmt --all -- --check
cargo check -p balance-sweep-autodeposit-trigger -p loyal-fleet-worker
git diff --check
```

The first command must exit zero and print `PASS_AUTODEPOSIT_DURABLE_OPERATION`. The remaining commands must also exit zero.

The verifier must prove these observable properties:

1. Kamino deposit readiness is established before a pull may be broadcast.
2. Pull confirmation advances the same durable operation to a deposit-pending state; it does not complete the claim, slot, execution, or success notification.
3. Restarting after pull confirmation resumes the deposit and cannot broadcast the pull again.
   The exact signed Kamino transaction is persisted before its first broadcast and reconciled by signature before replacement.
   Concurrent runners are fenced so only one may submit the deposit leg.
   A post-pull source-balance baseline prevents an unrecorded prior deposit from consuming unrelated idle funds on retry.
4. A retryable pre-send or deposit failure remains pending and does not emit an operational alert.
5. A missing destination token account is treated as repairable readiness work before pull, not as a generic executor alert.
6. The operation completes, releases its claim, and becomes eligible for success notification only after the Kamino deposit is confirmed and its durable history is recorded.
7. Per-operation operational alerts are limited to ambiguous chain effect or loss of durable ownership/invariants. Global worker-stopped monitoring remains independent.
8. Mandatory post-pull completion is executed directly by the operation owner and never depends on economic planner eligibility or an idle-balance SLA alert.
9. Runtime images and workflow path filters include every new module imported by the autodeposit executor.

The verifier must fail closed when the operation model or its executor/trigger integration is absent.
