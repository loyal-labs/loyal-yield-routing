# Fleet local load lab verifier

Run this verifier cold against the worktree that implements the isolated fleet
load setup. Try to prove each condition false. Record `PASS` or `FAIL` with the
exact command/output used as evidence. Overall PASS requires every Required
condition below; a local-chain E2E result is reported separately and must never
be inferred from the component-suite verdict.

## Required 1: truthful scope

- The command and generated report call the current implementation a
  `component load lab`, not complete/end-to-end/chain E2E.
- The report separately attributes work performed by real fleet processes,
  synthetic SQL clients, and synthetic RPC clients.
- Process liveness, synthetic RPC traffic, and direct SQL mutations cannot make
  a real-worker progress check pass.
- A full-chain verdict is `NOT_RUN` or `FAIL` unless a real local validator or
  LiteSVM execution changes token balances and reaches a reconciled terminal DB
  state through the production pipeline.

## Required 2: isolation and signer boundary

- The entrypoint removes inherited production DB, RPC, telemetry, and signer
  variables before starting anything.
- PostgreSQL and RPC bind only to loopback, the database name begins
  `fleet_e2e_`, and cleanup refuses an unexpected path.
- No production key is required. No key material appears in tracked files,
  reports, logs, or process arguments.
- `rg -n "allow-local-signer" crates scripts docs package.json` returns no
  production bypass added for this lab.

## Required 3: one reproducible command

- `bun run fleet:local-load-lab -- --opportunities 1000 --duration-seconds 5`
  exits zero on a supported local machine and writes `evidence.json` plus
  `evidence.md`.
- The command applies real Yield migrations to disposable PostgreSQL, runs the
  deterministic planner benchmark, starts exact role entrypoint probes, runs
  production-shaped DB contention and a loopback RPC fault workload, and
  removes its processes/database on exit.
- The report records the Git commit, scenario inputs, UTC timestamps, and host.

## Required 4: causal evidence and useful metrics

- Evidence contains separate counters for `realWorker`, `syntheticSql`, and
  `syntheticRpc`; their totals reconcile with raw logs.
- Outbox growth created by `local_user_load` is labelled synthetic and is not
  presented as fleet amplification.
- Metrics include DB throughput/latency, exact health-query p50/p95/p99,
  deadlocks, locks/waits, database growth, RPC latency/errors/concurrency, and
  per-process CPU/RSS where a real process ran.
- The report identifies the known health-view contention when its p95 exceeds
  the configured threshold.

## Required 5: automated adversarial checker

`bun run verify:fleet-local-load-lab` must fail if any fixture presented to the
checker does one of the following:

1. labels synthetic RPC requests as real-worker requests;
2. labels `local_user_load` outbox rows as worker-generated amplification;
3. reports `FULL E2E PASS` without a nonzero completed/reconciled count and a
   local-chain balance delta;
4. marks real-worker progress PASS from liveness alone.

The checker must pass its positive fixture and all four negative controls.

## Required commands

```sh
bash -n scripts/fleet-local-load-lab/run.sh
bun run verify:fleet-local-load-lab
cargo test -p loyal-fleet-worker --lib
git diff --check
```

Also scan the attributable diff for plaintext secrets and Cyrillic text.

## Separate future full-chain verdict

Report `FULL_CHAIN_E2E: PASS` only when input introduced before opportunity
creation flows through nonzero real planner, revalidator, executor, confirmer,
and reconciler work; RPC is forwarded to a stateful local validator/LiteSVM
fixture; an ephemeral local signer uses the normal signer-loading path; and
final DB state plus pre/post token balances prove exactly-once execution.

The full-chain gate is now implemented as a staged verifier:

1. `bun run fleet:litesvm-e2e -- --fixture <manifest>` must pass the exact
   account-closure and transaction-level verifier in
   `docs/plans/fleet-litesvm-e2e-verifier.md`.
2. Only then may `bun run fleet:local-chain-e2e -- --fixture <manifest>` start
   the disposable validator, local database, RPC proxy, and production roles.
3. The validator evidence must prove nonzero planner, revalidator, executor,
   confirmer, and reconciler work; one reconciled submission/signature; a
   Main-to-Prime chain delta; no active ALT leases; and a no-op rerun of every
   role with identical stable DB and chain state.
   Upgradeable program bytecode is registered through the validator's program
   deployment interface (required for its program cache), then verified against
   the captured ProgramData bytes. Non-ProgramData accounts remain byte-exact;
   only the local ProgramData deployment slot and rent lamports may differ.
4. The full-chain checker must reject a fake live clone, liveness-only role
   evidence, terminal DB state without a balance delta, duplicate signatures,
   rerun mutation, and any production endpoint/signer claim.

## Verdict format

```text
Truthful scope: PASS | FAIL - evidence
Isolation and signer boundary: PASS | FAIL - evidence
One reproducible command: PASS | FAIL - evidence
Causal evidence and useful metrics: PASS | FAIL - evidence
Automated adversarial checker: PASS | FAIL - evidence
OVERALL COMPONENT LAB: PASS | FAIL
FULL_CHAIN_E2E: PASS | FAIL | NOT_RUN
```
