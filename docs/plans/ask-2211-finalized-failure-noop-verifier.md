# ASK-2211 finalized failure no-op verifier

Run this verifier from the repository root:

```sh
bash scripts/verify-ask-2211-finalized-failure-noop.sh \
  --app-root /private/tmp/ASK-2211-loyal-app
```

Required conditions:

1. A finalized policy transaction whose metadata contains a chain error is classified as producing no state change. It is not returned as a retryable proof error.
2. In disposable PostgreSQL, a durable failed-transaction job ahead of a later transaction for the same vault completes as a no-op. The later job becomes claimable and also completes.
3. Neither job writes an Earn mutation, and neither remains pending or carries retry evidence.
4. Transient proof lag remains retryable, and the existing same-transaction sibling and exactly-once reconciliation checks still pass.
5. Formatting, the focused monitor tests, the affected Rust checks, and `git diff --check` pass.

Verdict format: the script exits zero and prints `PASS: finalized failed Earn transactions complete as no-op without starving later work`. Any nonzero exit is `FAIL`.

