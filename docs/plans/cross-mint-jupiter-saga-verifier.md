# Cross-mint Jupiter movement verifier

This document is the release contract for cross-mint yield routing. Implementation order is deliberately secondary: the feature is done only when the observable conditions below pass.

## Product boundary

- A cross-mint movement is one durable `rebalance_decision` linked to one immutable economic opportunity.
- `signed_route_submissions` are append-only attempts and receipts, not a competing movement state machine.
- The only production venue in V1 is Jupiter ExactIn. There is no venue abstraction, new on-chain program, Hub wrapper, atomic-route claim, split routing, or reverse-swap recovery.
- V1 enables only classic SPL pairs among USDC, USDT, and USDS. Mixed Token/Token-2022 pairs remain fail-closed until their exact Jupiter instruction and credited-output semantics pass the same verifier.
- Existing same-mint behavior is unchanged.

## Authoritative sequence

```text
source reserve
  -- finalized withdraw delta W --> source-mint idle
  -- finalized Jupiter debit W / credit O --> target-mint idle
  -- finalized deposit O --> target reserve
```

The next submission may be persisted and signed only after the previous signature is finalized and its effect is reconciled atomically with movement advancement. `confirmed` commitment is insufficient for cross-leg continuation.

## PASS conditions

The verifier returns PASS only when behavioral evidence proves all of the following:

1. At most one active movement exists per vault and at most one nonterminal submission exists per movement.
2. The movement is the only mutable business lifecycle. Opportunity data is immutable intent; submissions contain exact signed bytes, signatures, lifetimes, attempts, and effect receipts.
3. Exact signed bytes are persisted before broadcast and are retried unchanged while valid.
4. A new generation after expiry is allowed only after signature-history and balance evidence prove no effect. Ambiguous effect freezes progression, recovery, replacement, and capacity release.
5. Withdraw reconciliation records source-ATA credit `W`; swap reconciliation records exact source debit and target credit `O`; deposit uses exactly `O`. Planned amounts, quote output, and aggregate ATA balances never substitute for finalized deltas.
6. Preexisting source and target balances remain untouched. Any intermediate owner mutation invalidates unsigned work. If transaction-history attribution cannot prove the movement-owned remainder, the movement freezes for user action rather than consuming fungible excess.
7. Source-idle quote failure, bad economics, target invalidation, or unavailable Jupiter produces a source-mint deposit recovery. Target-idle destination failure atomically rebinds capacity to a safe reserve of the same target mint and deposits there. A successful swap is never automatically reversed.
8. Policy revocation or loss of every safe recovery target produces an explicit `manual_intervention` terminal outcome with funds remaining user-owned; it is never reported as successful recovery.
9. Target capacity is owned by the movement and remains reserved through intermediate legs. It is released only by target completion, source recovery, provable user closure, or explicit manual-intervention handling.
10. Continuation leases and fencing admit one winner under duplicate wakeups, worker restarts, and concurrent claims.
11. `start_new_movements=false` blocks new withdrawals while `continue_or_recover_existing=true` continues or safely freezes existing movements.
12. A Jupiter build is rejected unless program IDs, signer/writable privileges, source and destination ownership, mints, ExactIn amount, minimum output, platform fee, setup instructions, ALTs, packet size, compute budget, and policy constraints match the accepted quote and movement.
13. Candidate selection compares no move, same-mint move, and cross-mint move using an explicit cost breakdown before withdrawal. After withdrawal it compares swap-and-deposit with source recovery; after swap it selects safe target-mint placement without re-running sunk economics.
14. Continuation and recovery work is claimed before new optimization. Active movements exclude their vaults from new planning.

## Required evidence

### Policy and transaction behavior

LiteSVM/Squads executes withdraw, swap, and deposit as three separate transactions. Tests derive `W` and `O` from before/after balances, preserve preexisting balances, demonstrate that a later failure does not roll back an earlier finalized leg, exercise source recovery and target fallback, retain the captured Jupiter policy test and adversarial ownership rejection, and keep same-mint named actions green.

### Durable lifecycle

The disposable `fleet_verify` database suite injects restarts for every leg: before persistence; after persistence before broadcast; after broadcast before status; after finalization before reconciliation; and after reconciliation before continuation. It proves idempotence, finalized-only progression, one fenced continuation, movement-scoped capacity, expiry/no-effect generation, ambiguity quarantine, recovery, fallback rebind, kill switches, and migration/query compatibility. It must never target a production database.

### Regression and live read-only checks

- `bun run test:squads`
- `bun run test:squads:e2e` when policy composition, heap/compute, or replay-sensitive construction changes
- `bun run verify:cross-mint:store` with `CROSS_MINT_STORE_TEST_DATABASE_URL` pointing only at a disposable database whose name contains `cross_mint_store_test`
- `bun run verify:cross-mint:jupiter-live` through the persistent 1Password environment, optionally with a funded public `JUPITER_LIVE_FEE_PAYER`; this compiles and simulates but never signs or sends a production transaction
- targeted `cargo check` for loyal-actions, loyal-yield-store, loyal-yield-orchestrator, and loyal-fleet-worker
- `bun run lint` and `bun run build`
- through `.env.1password`: migration checksum checks, fleet planner `--once --dry-run --json`, current Jupiter build contract refresh, transaction compilation, fit, and simulation

Live checks are read-only. No production transaction, deployment, or rollout is part of this verifier without separate explicit approval.

## Failure output

Every failed condition reports the invariant, movement/leg identity, expected evidence, observed evidence, and whether the safe response is retry-identical-bytes, create-new-generation-after-proved-no-effect, recover source, fallback target, freeze ambiguous, or require user action. Source-string checks, enum-shape tests, and mocked default assertions do not count as evidence.
