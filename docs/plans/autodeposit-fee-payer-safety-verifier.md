# Autodeposit Fee-Payer Safety Verifier

Run this verifier from the `loyal-yield-routing` repository root. Treat every
item under **Required** as mandatory. Report each item as `PASS` or `FAIL`, then
report `OVERALL: PASS` only when every required item passes.

## Required

1. `scripts/execute-autodeposit-policy.ts` derives the Kamino top-up fee payer
   from the same-mint dry-run result and reads that account's SOL balance at
   `confirmed` commitment immediately before the irreversible user-to-vault
   pull.
2. When the fee-payer balance is below the configured minimum, execution fails
   with a clear error and the pull callback is not invoked. A balance equal to
   the minimum is accepted and invokes the pull exactly once.
3. The minimum is the internal constant
   `AUTODEPOSIT_TOP_UP_FEE_PAYER_MIN_LAMPORTS = 20_000_000`; no environment
   override or runtime parsing can weaken it.
4. `render.yaml` and `.env.example` have no fee-payer-minimum configuration
   diff from `main`.
5. The final execution output records the checked fee-payer public key, observed
   balance, configured minimum, and commitment so operators can verify which
   safety check allowed a pull.
6. `bun run autodeposit:test` passes and includes observable regression coverage
   for rejection-before-pull and the exact-threshold success boundary.
7. The touched TypeScript files pass an explicit no-emit ES2022 typecheck:
   `bunx tsc --noEmit --pretty false --target ES2022 --module ESNext
   --moduleResolution Bundler --skipLibCheck --types bun
   scripts/execute-autodeposit-policy.ts
   scripts/execute-autodeposit-policy.test.ts`. The repository's bare root
   `tsc` invocation is not used because its current default target rejects
   existing BigInt literals and its incremental configuration writes
   `tsconfig.tsbuildinfo` even with `--noEmit`.
8. Targeted ESLint passes for the changed TypeScript files. No frontend build or
   live write is used for verification.
9. `git diff --check` passes, `git status --short` contains only this hotfix and
   verifier documentation, and a secret scan of the diff finds no private keys,
   database URLs, RPC credentials, or API tokens.
10. The change is committed on an ASK-1731 branch, pushed, and filed as a concise
    conventional-commit pull request.

## Nice to have

- The pull remains separately retryable for failures unrelated to fee-payer SOL,
  such as expired blockhashes and ambiguous RPC confirmations.

## Verdict

Output one `PASS` or `FAIL` line for each required item and finish with exactly
one overall verdict. Any unverified required item makes the overall verdict
`FAIL`.
