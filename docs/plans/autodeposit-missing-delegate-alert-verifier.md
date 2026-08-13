# Autodeposit Missing-Delegate Alert Verifier

Run this verifier from the `loyal-yield-routing` repository root. Treat the
document as fixed after implementation starts. Report every required check as
`PASS` or `FAIL`, and report `OVERALL: PASS` only when all required checks pass.

## Goal

An autodeposit pull simulation that proves the wallet's expected SPL token
delegate is missing is a recoverable user-authorization state, not an executor
incident, but only after the worker has safely released the selected claim and
atomically demoted the target to `pending_delegation`.

The successful quarantine must produce a structured, non-paging lifecycle
signal and the executor's existing non-actionable exit code. A failed or
unproven quarantine must retain the generic failing exit so the Rust trigger
still emits `autodeposit_executor_failed`. No production deploy is part of this
verifier.

## Required Checks

### 1. Failure Recognition Remains Narrow

PASS only if the missing-delegate classifier accepts the exact autodeposit pull
simulation plus SPL Token `owner does not match` failure and rejects an owner
mismatch from another stage such as Kamino top-up.

Required evidence:

```sh
bun test scripts/execute-autodeposit-policy.test.ts \
  --test-name-pattern "autodeposit token delegate failures"
```

### 2. Successful Quarantine Is Non-Paging

PASS only if observable regression coverage proves all of the following:

- the selected claim is released and its supported lots are restored without
  exceeding `original_amount_raw`;
- the target transition is a compare-and-set from `active = true` and
  `lifecycle_status = 'active'` to `active = false` and
  `lifecycle_status = 'pending_delegation'`;
- the release operation reports whether the claim was released, the slot was
  released, and the target was actually demoted;
- only a result proving both claim release and target demotion is classified as
  `not_actionable`;
- the nonactionable exit code is assigned only after that successful release
  result returns;
- a structured event with status
  `autodeposit_target_paused_missing_delegate` is emitted without wallet,
  signer, claim-token, secret, RPC, or database credential fields;
- the event identifies recovery as user-owned and the target as requiring
  delegation repair.

### 3. Failed Or Unproven Quarantine Still Pages

PASS only if observable regression coverage proves:

- a release operation that throws cannot leave the process on the
  nonactionable exit code;
- a release result that did not release the claim or did not demote the target
  cannot use the nonactionable exit code;
- these paths retain the generic executor failure behavior so an operator is
  alerted that the safety transition itself failed.

This is a required safety boundary: never suppress an alert merely because an
error string looked like a missing delegate.

### 4. Trigger Alert Contract Is Preserved

PASS only if Rust regression coverage proves:

- `AUTODEPOSIT_NOT_ACTIONABLE_EXIT_CODE` maps to no operational alert;
- exit code `1`, a missing exit code, and unknown unsuccessful exits still map
  to `autodeposit_executor_failed`;
- existing top-up, persistence, preflight, and fee-payer alert mappings remain
  unchanged;
- the trigger counts a nonactionable exit separately from failures.

Required evidence:

```sh
cargo test -p balance-sweep-autodeposit-trigger executor_failure_alert
```

### 5. Executor Contract Tests Pass

The focused executor suite must protect the successful and failed quarantine
branches as observable behavior, not only source substrings.

Required evidence:

```sh
bun run autodeposit:test
```

### 6. Static Checks Pass

Required evidence:

```sh
bunx tsc --noEmit --pretty false --target ES2022 --module ESNext \
  --moduleResolution Bundler --skipLibCheck --types bun \
  scripts/execute-autodeposit-policy.ts \
  scripts/execute-autodeposit-policy.test.ts
```

```sh
bunx eslint scripts/execute-autodeposit-policy.ts \
  scripts/execute-autodeposit-policy.test.ts
```

```sh
cargo check -p balance-sweep-autodeposit-trigger
```

### 7. Scope And Safety Are Clean

PASS only if:

- no migration, Render configuration, worker image, or production service was
  changed;
- unrelated executor failures still use their existing retry and alert
  behavior;
- `git diff --check` passes;
- the attributable diff contains no private keys, database URLs, RPC
  credentials, API tokens, or Co-Authored-By trailers;
- existing unrelated worktree changes are not modified.

## Verdict

Output one `PASS` or `FAIL` line for required checks 1 through 7. Finish with
exactly one of:

```text
OVERALL: PASS
OVERALL: FAIL
```

Any unverified required check makes the overall verdict `FAIL`.
