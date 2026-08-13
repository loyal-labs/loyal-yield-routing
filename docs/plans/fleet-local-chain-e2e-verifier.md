# Fleet local-chain E2E verifier

Run this verifier only after the LiteSVM verifier passes. Try to falsify every
condition. `FULL_CHAIN_E2E: PASS` requires every condition below.

## Required 1: LiteSVM gate and finalized fixture

- The combined runner completes `docs/plans/fleet-litesvm-e2e-verifier.md`
  before it creates keys or starts a validator.
- The input is a finalized Mainnet fixture whose account closure passes the
  offline verifier.
- A fresh local validator receives ordinary accounts byte-exactly. Captured
  upgradeable programs are registered through the validator deployment path
  and their ELF bytes, program identities, and authorities are verified.
- No account is selected by trial and error at validator startup.

## Required 2: isolated production-shaped pipeline

- PostgreSQL, the validator, and the instrumented RPC proxy bind to loopback.
- Keys are generated in a mode-0700 temporary directory and loaded through the
  normal signer path. No production database, endpoint, account, or private key
  is inherited.
- The planner publishes nonzero work. The revalidator, executor, confirmer,
  and reconciler each claim and complete nonzero production-role work.
- ALT demand is discovered by simulation, provisioned by the production
  provisioner, and re-admitted by the long-running production planner.

## Required 3: actual effect and exactly-once terminal state

- One opportunity reaches `completed`, one decision has one signature, and one
  submission reaches `reconciled`.
- Main collateral decreases to zero and Prime collateral increases from zero.
- No ALT operation is incomplete or permanently failed and no usage lease is
  left active.
- A rerun of every role claims no work, does not add a signature, and leaves
  stable database and chain position state unchanged.

## Required 4: useful attributed load evidence

- Raw proxy rows reconcile exactly with summary request and source counts.
- Production processes and synthetic RPC clients have separate counts.
- Production-process RPC errors are zero. Synthetic invalid-transaction probe
  errors remain labelled synthetic.
- Evidence reports request throughput, max in-flight requests, and per-method
  calls, errors, p50, p95, p99, and maximum latency.

## Required 5: truthful boundary

- This fixture proves the USDC Main-to-Prime route pipeline and explicit
  before/after chain snapshots.
- It does not claim the reconciler's complete stable-mint position sweep. That
  sweep requires the complete supported stable-mint exit catalog, while this
  focused fixture contains the Main/Prime USDC closure.
- Evidence contains no production endpoint or private-key array.

## Required 6: automated adversarial checker

`bun run verify:fleet-local-chain-e2e` passes a positive fixture and rejects:

1. validator evidence without the LiteSVM prerequisite;
2. a fake live clone;
3. liveness without nonzero role work;
4. terminal database rows without an on-chain balance delta;
5. duplicate signatures;
6. a rerun that changes state; and
7. a production endpoint or signer claim.

## Commands

```sh
bash -n scripts/fleet-local-chain-e2e/run.sh
bun run verify:fleet-litesvm-e2e
bun run verify:fleet-local-chain-e2e
bun run fleet:local-chain-e2e -- --fixture fixtures/<capture>/manifest.json
git diff --check
```

## Verdict

```text
LITESVM_E2E: PASS | FAIL
FINALIZED_FIXTURE: PASS | FAIL
ISOLATED_PRODUCTION_PIPELINE: PASS | FAIL
EXACTLY_ONCE_CHAIN_EFFECT: PASS | FAIL
ATTRIBUTED_LOAD_METRICS: PASS | FAIL
TRUTHFUL_BOUNDARY: PASS | FAIL
FULL_CHAIN_E2E: PASS | FAIL
```
