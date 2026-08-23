# Autoswap confirmed reconciliation verifier

## Standing goal

Run this verifier cold against clean `loyal-yield-routing` and `loyal-app`
worktrees:

```sh
bash scripts/verify-autoswap-confirmed-reconciliation.sh \
  --routing-root /path/to/loyal-yield-routing \
  --app-root /path/to/loyal-app
```

The verifier must try to disprove every required condition below and print
`PASS_AUTOSWAP_CONFIRMED_RECONCILIATION` only when all of them hold.

## Required conditions

1. Two exact, matching Autoswap policy accounts observed at Solana `confirmed`
   commitment reconcile to one canonical ready enrollment. A `finalized`
   observation is also accepted because it is stronger.
2. One policy, mismatched shards, mismatched authority, mismatched delegate,
   mismatched limits, or missing accounts never produce a ready enrollment.
3. Replaying the same confirmed account snapshot is idempotent. It does not
   create a second enrollment or advance its generation.
4. The ready transition inserts or updates the canonical enrollment atomically
   and emits one `autoswap_installed` realtime invalidation. Removal of both
   accounts removes the enrollment and emits `autoswap_removed`.
5. Fleet execution accepts finalized ordinary Earn withdraw/deposit policies
   together with confirmed-or-stronger Autoswap swap policies. It still rejects
   processed Autoswap observations and stale or mismatched policy bindings.
6. The web derives `off`, `finalizing`, `on`, and `paused` from the canonical
   projection. It does not apply a second finalized-only policy-pair rule or
   require a client confirmation API.
7. The browser lifecycle works end to end on a disposable local validator and
   PostgreSQL database: client creates both policies, account reconciliation
   observes them at confirmed, SSE invalidates the web state, the web reports
   Autoswap on, client removes both policies, SSE invalidates again, and the web
   reports Autoswap off.
8. Focused Rust checks, focused web contract tests, formatting, lint, and Git
   whitespace checks pass. No frontend production build is run locally.

## Verdict

- `PASS_AUTOSWAP_CONFIRMED_RECONCILIATION` only if every required condition
  passes.
- Otherwise print `FAIL_AUTOSWAP_CONFIRMED_RECONCILIATION` with the first false
  condition and exit nonzero.

Production migration, deployment, and wallet repair are post-implementation
operations and are not implied by a local PASS.
