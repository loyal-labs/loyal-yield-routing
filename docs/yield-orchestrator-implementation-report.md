# Yield Orchestrator Implementation Report

## Scope

This branch turns the yield orchestrator from policy/decision storage into an executable, durable same-mint routing service skeleton. It also adds the production-shaped execution edge for Kamino redeem/deposit through Squads policy execution and a Solana RPC adapter for simulation, submission, and confirmation polling.

The branch intentionally does not commit local editor settings or the generated database schema HTML artifact:

- `.zed/`
- `docs/database-schema.html`

## Breakdown

### Repository and testing documentation

- Expanded `README.md` into a repo map covering the app shell, TypeScript package, Rust action SDK, Loyal Hub program, test harness, router, orchestrator, and policy monitor.
- Added practical development and verification command groups.
- Updated Squads testing docs with the Jupiter batch capacity probe.

### Jupiter batch capacity artifacts

- Added a deterministic `crates/squads-test-harness/tests/jupiter_swap_batch_size.rs` capacity probe.
- Added mock Jupiter batch execution support in `crates/mock-yield-protocols-program`.
- Added human-readable batch capacity docs in `docs/jupiter-swap-batch-capacity.md`.
- Added a self-contained interactive transaction breakdown in `docs/jupiter-swap-tx-breakdown.html`.

### Orchestrator durable pipeline

- Added migration `0002_orchestrator_pipeline.sql` with durable worker cursors, target snapshots, reconcile jobs, rebalance attempts, batches, batch-decision links, Solana account cache, and worker events.
- Added pipeline domain types for worker stages, queue statuses, attempts, batches, submission observations, and confirmation observations.
- Added `pipeline_store.rs` with Postgres-backed queue claiming, idempotent target updates, reconcile job fanout, attempt recording, batch insertion, and lease sweeping.
- Added structured worker modules for target selection, vault scan, reconcile shaping, planning, simulation, batching, submit, confirm, and sweeper behavior.
- Added an executable binary at `crates/loyal-yield-orchestrator/src/bin/loyal-yield-orchestrator.rs`.

### Production-shaped execution edge

- Added real Kamino deposit and redeem instruction builders in `src/kamino.rs` using the production Kamino program id and discriminators from `loyal-actions`.
- Added Squads policy execution composition in `src/policy_execution.rs`, including compiled inner-instruction merging and Borsh payload encoding for `execute_transaction_sync` policy execution.
- Added `SimulationWorker::build_same_mint_policy_execution` to build a Squads policy execution from a same-mint policy route, Kamino account graph, signer, vault index, and amount.
- Added `src/rpc.rs` as the Solana RPC boundary for latest blockhash, simulation, transaction send, signature status polling, and error classification.
- Added an ignored live preproduction simulation test in `tests/preproduction_same_mint_route.rs`.

### Signer parsing

- Replaced the manual hex decoder in the orchestrator signer with the Rust `hex` crate while preserving the existing 32-byte seed and 64-byte Solana keypair input behavior.

## Verification

The orchestrator test suite was run against an isolated local temporary Postgres database:

```bash
DATABASE_URL=postgresql://localhost/loyal_yield_orchestrator_codex_rpc_test_20260603 cargo test -p loyal-yield-orchestrator
```

Result:

- 43 unit tests passed.
- The binary target compiled.
- The ignored preproduction live-RPC simulation test compiled and remained ignored.
- The temporary database was dropped after the run.

## Live Preproduction Test

The live test is intentionally ignored and requires explicit environment values:

- `YIELD_ROUTER_KEYPAIR`
- `LOYAL_YIELD_PREPROD_RPC_URL`
- `LOYAL_YIELD_PREPROD_SAME_MINT_FIXTURE`

Run it with:

```bash
op run --env-file=.env.1password -- sh -c 'cargo test -p loyal-yield-orchestrator --test preproduction_same_mint_route -- --ignored --nocapture'
```

The fixture must provide the policy account, two instruction constraint indexes, vault owner, vault token accounts, source and target Kamino reserve account graphs, and the raw amount to simulate.

## Remaining Gaps

- The orchestrator can build real Kamino policy execution instructions from an account graph, but it does not yet discover that account graph from RPC.
- The reconcile worker still needs real on-chain Kamino position/account scanning.
- The executable currently runs the target and vault-scan parts of the durable loop; simulation, batch signing, submit, and confirm stages are implemented as modules but still need to be wired into the runtime loop.
- Live preproduction success depends on an existing Squads policy account whose constraint indexes match the supplied fixture.

## Proposed Commits

1. `docs(repo): expand yield routing workspace map`
2. `chore(repo): refresh Bun lockfile metadata`
3. `fix(hub): align high-lane swap tests with lane inventory`
4. `test(squads): add Jupiter swap batch capacity probe`
5. `feat(yield-orchestrator): add durable same-mint execution pipeline`
6. `docs(yield-orchestrator): add implementation report`
