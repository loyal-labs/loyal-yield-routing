# ASK-2211 Autodeposit client cutover verifier

Run the following command from the isolated routing worktree:

```sh
bash scripts/verify-ask-2211-autodeposit-client-cutover.sh \
  /private/tmp/ASK-2211-loyal-yield-routing \
  /private/tmp/ASK-2211-loyal-app
```

This file is the frozen verifier-first goal. The command must exit nonzero on
the first failed required condition. It may print the final PASS line only
after every condition succeeds.

## Required conditions

1. Web and mobile build Autodeposit setup and close transactions with the
   shared SDK, submit them directly, and do not call backend prepare or
   transaction-confirmation routes.
2. The backend has no Autodeposit setup/close prepare or confirm handlers and
   accepts no client-posted setup/close signature as reconciliation evidence.
3. Pause/resume, floor changes, Execute Now, canonical state reads, and
   realtime-token issuance remain backend operations.
4. The existing balance-sweep LaserStream monitor discovers setup and close
   from stable watched smart-account changes. It may fetch the exact
   transaction named by a LaserStream update for account discovery, but it
   has no transaction subscription, address-history scan, client watch
   registration, or second service.
5. `balance_sweep_targets` is the single current-state row. It separates
   user intent from observed chain lifecycle, derives sweep eligibility from
   both, and production code no longer reads or writes the parallel
   `autodeposit_vault_configs` or `autodeposit_chain_projections` tables.
6. A finalized, monotonic chain observation can produce pending, active,
   inconsistent, or closed state. Stale observations cannot reactivate a
   closed target or overwrite pause/floor intent.
7. First active observation schedules bootstrap work exactly once per setup
   generation. Closed observation disables scheduling and cancels pending
   work without losing already-submitted transaction reconciliation.
8. Routing emits `earn.autodeposit.configuration.changed` as a durable SSE
   invalidation. Web and mobile refetch canonical state after it. Solana
   signature confirmation remains the client's transaction-success signal.
9. Web and mobile support authenticated SSE cursor replay, token renewal,
   resync, and a bounded canonical-state fallback. Mobile lifecycle changes
   close and reopen the stream safely.
10. Focused app tests, mobile and web lint, Rust tests/checks, formatting,
    migration verification, and `git diff --check` pass without a frontend
    build.

## Rejected shortcuts

- Source-only claims without focused behavior tests.
- Client pre-registration of future account addresses.
- `getSignaturesForAddress`, `searchTransactionHistory`, or a broad SPL Token
  owner scan.
- Treating an SSE event as proof that a Solana transaction landed.
- Mirroring the same active state through multiple tables or synchronization
  triggers.
- Removing pause/resume, floor changes, or Execute Now.

## Verdict

The exact final line must be:

```text
PASS: ASK-2211 Autodeposit is client-sent and account-projected
```
