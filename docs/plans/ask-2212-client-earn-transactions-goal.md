# ASK-2212 verifier-first goal

Run `scripts/verify-ask-2212-client-earn-transactions.sh` with
`LOYAL_APP_DIR` pointing at the matching Loyal App worktree.

The verifier must return `PASS` only when all required conditions hold:

1. Web and mobile build initial deposit, top-up, partial withdrawal, multi-step
   full withdrawal, cleanup, policy refund, and vault-account refund
   transactions with the shared client SDK.
2. Supported clients do not call operation-specific Earn prepare, confirm, or
   position-reconcile endpoints. A read-only authenticated context endpoint may
   return provisioning, product policy, projected state, and addresses.
3. One confirmed LaserStream path watches every ready smart account and queues
   reconciliation for settings, wallet, vault, policy, supported wallet/vault
   token accounts, and safe-market obligations.
4. Deposit, withdrawal, cleanup, and refund projection uses confirmed chain
   evidence with a triggering-slot fence and does not require an onboarding
   attempt or pre-recorded full withdrawal row.
5. Cash-flow events and complete vault snapshots apply atomically, reject older
   snapshots, and deduplicate replay by stable chain identity.
6. Initial deposit, top-up, partial withdrawal, multi-step full withdrawal,
   cleanup, policy refund, vault refund, replay idempotency, and same-slot
   sibling processing each have an executable verifier scenario.
7. Canonical reads use confirmed commitment and `minContextSlot` no lower than
   the triggering update slot. Temporary RPC lag remains retryable and does not
   become an operational failure until it persists beyond the stale horizon.
8. SSE remains an invalidation channel and both clients refetch projected
   state after relevant Earn events.
9. The focused verifier, scoped app checks, `cargo check`, and Rust formatting
   all exit zero.

The goal text is fixed. Implementation plans may change, but this file and the
verifier conditions must not be weakened to obtain a pass.
