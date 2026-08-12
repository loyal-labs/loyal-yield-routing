# Earn Multi-Mint Release Readiness Verifier

Status: frozen before release-readiness implementation on 2026-08-11.

Run this document cold against:

- `APPS_REPO=/private/tmp/loyal-apps-earn-multi-mint-simple`
- `ROUTING_REPO=/private/tmp/loyal-yield-routing-earn-multi-mint-simple`
- app base `e26f2d18632825a86007789665f9abed38f1ce0d`
- routing base `5125f7f464d15b7ee5624642e5a3fe81cba026fc`

This verifier has two deliberately separate verdicts:

1. `CODE_RELEASE_CANDIDATE`: code can be reviewed and dark-deployed safely.
2. `USER_READY`: the deployed system has finalized production evidence.

The current implementation task may achieve the first verdict. It must not
claim the second without deployment and bounded on-chain canary authorization.
No verifier command below updates user policies, changes rollout controls,
deploys services, signs, or broadcasts.

## R0 — Scope and reviewability

Required for `CODE_RELEASE_CANDIDATE`:

- Diffs contain only six-mint Earn deposit, holdings, withdrawal, accounting,
  routing, rollout-control, tests, and verifier/operator documentation.
- No schema migration, cross-mint route, non-USDC autodeposit, worker topology,
  on-chain policy mutation, or generated ABI change is present.
- Formatting-only churn is removed where it obscures review.
- `git diff --check`, `git status --short`, and complete diff inspection pass in
  both repositories.

## R1 — One fail-closed product rollout allowlist

Required:

- The app has one code path that parses a symbol allowlist containing only
  CASH, USDG, PYUSD, USDC, USDT, and USDS.
- Missing/blank configuration enables USDC only. Invalid or duplicate entries
  fail startup/build or the request; they never broaden access.
- The same resolved allowlist drives the selector and every web/mobile manual
  deposit prepare endpoint. A crafted request for a disabled mint fails before
  reserve lookup, policy work, or transaction construction.
- Holdings and withdrawals remain readable for disabled mints so rollback
  cannot strand funds.
- Autodeposit remains hard-coded to USDC.
- The router's existing `EARN_ROUTER_ENABLED_STABLE_MINTS` remains a separate
  service rollout gate and preserves same-mint execution.

Focused tests must prove default USDC-only, staged subsets, all-six enablement,
invalid configuration, and disabled-mint request rejection.

## R2 — User-flow wiring

Required for all enabled mints:

- Existing selector/icon components display the enabled intersection with the
  active cluster and show the correct wallet balance.
- Selection reaches prefetch and submit as `{ mint, amountRaw }`; changing it
  invalidates stale preparation.
- Review, error, CTA, and destination copy use the selected asset.
- All positive idle/reserve holdings render separately with mint and market
  identity. A row opens withdrawal with its exact `sourceId`.
- `{ sourceId, amountRaw | "max" }` is the only public withdrawal intent. The
  backend freshly resolves exactly one source. Partial and Max never drain a
  second source.
- Exact all-source zero proof remains required before cleanup.

Run the focused Earn UI, holdings, request-contract, withdrawal-route, cleanup,
and mobile route tests. Test modules with global mocks may be run in separate
Bun processes.

## R3 — Policy and transaction safety

Required:

- A new policy encodes the six-mint-compatible policy shape.
- Existing compatible policies are reused without mutation.
- Legacy policies accept USDC/USDT/USDS. CASH/USDG/PYUSD return HTTP 409
  `earn_policy_update_required` before transaction construction.
- Every selected reserve is Safe, same-mint, and validated on chain for Klend
  owner, market, liquidity mint, and declared token program before asking
  Kamino to construct the deposit.
- ATA derivation, transfers, withdrawals, and cleanup preserve the selected
  mint's classic SPL Token or Token-2022 program.
- Existing users retain holdings/withdraw access even when deposit enablement
  is off or their policy is legacy.

## R4 — Holdings, earnings, and APY

Required:

- One complete fresh RPC snapshot is current money truth: every authorized
  reserve holding plus directly-derived idle ATAs for all policy product mints.
- Incomplete/stale/failed reads remain unknown and cannot authorize cleanup.
- Principal is confirmed external deposits minus withdrawals, keyed by mint;
  live balance changes never rewrite principal.
- Concurrent source exposures remain keyed by source ID. The chart aggregates
  source intervals; current APY is nominal-value weighted and idle contributes
  zero.
- Missing/stale APY coverage is unavailable, never borrowed from another mint
  or primary position.

## R5 — Routing and publication

Required:

- `EarnUniverse` contains exactly the six assets and correct token programs.
- Idle observation derives ATA from vault + asset mint + token program.
- Planning, revalidation, instruction construction, confirmation, and
  reconciliation preserve one mint and token program.
- `publish_complete_vault` atomically publishes full reserve+idle state;
  `apply_observed_patch` cannot delete unseen holdings.
- No timestamp/JSON worker capability fence exists. Rollout gating changes
  eligibility only, never holdings visibility or withdrawal availability.
- Historical USDC wire names exist only at serialization/persistence
  boundaries and do not impose USDC execution behavior.

Run:

```sh
cargo fmt --check --manifest-path crates/loyal-yield-orchestrator/Cargo.toml
cargo check -p loyal-yield-orchestrator
cargo test -p loyal-yield-orchestrator
```

## R6 — Build, lint, and regression evidence

Required:

- `bun run --cwd packages/smart-account-vaults typecheck` passes.
- The frontend production build completes using the repository's documented
  1Password-backed environment mechanism. Do not print secrets.
- All new files pass scoped Ultracite. For pre-existing modified files, run the
  identical file set at base and head; head must not increase diagnostics.
- Focused money/wire/snapshot/accounting tests pass.
- Full relevant suites are compared against base. A baseline failure may be
  recorded only when the same test fails identically at base and no changed
  behavior depends on it.
- No local frontend build output or generated SDK distribution is tracked.

## R7 — Read-only readiness artifact and operator handoff

Required:

- A checked-in read-only command emits one bounded JSON report with a row for
  each product mint: symbol, mint, token program, rollout-enabled state,
  eligible Safe reserve count, selected reserve, reserve identity result, APY
  freshness/coverage, and explicit blockers.
- The command defaults to no writes and contains no signer or transaction-send
  path. Missing RPC/Timescale configuration yields `unknown`, never success.
- Operator documentation gives the exact dark-deploy order, per-mint enable
  order, rollback actions, and evidence to capture. Rollback disables deposits
  and routing for one mint while keeping reads and withdrawals available.
- The artifact distinguishes `codeVerified`, `dataReady`, `deployed`,
  `canaryFinalized`, and `userReady`; none is inferred from another.

## R8 — External production evidence

Required only for `USER_READY`, and impossible to substitute with local tests:

- Both reviewed commits are merged and immutable deploy artifacts are live.
- Production readbacks prove reserve/APY coverage for each enabled mint.
- Desktop and mobile flows are exercised against the deployed build.
- A fresh-policy canary for each mint records simulation, submitted signature,
  finalized signature, before/after wallet and vault/reserve balances, app
  reconciliation, partial withdrawal, selected-source Max, and final cleanup.
- At least one finalized non-USDC same-mint optimization is reconciled.
- Monitoring and per-mint rollback controls are live.

Do not perform R8 without explicit deployment and value-movement approval. An
unperformed R8 must be reported `NOT RUN`, not `PASS` or `FAIL`.

## Commands and verdict

Run the focused commands named above, production build, scoped lint comparison,
`git diff --check`, status, and complete diff review. Record exact test counts,
base/head diagnostic counts, and any baseline failures.

Report R0–R8 individually as `PASS`, `FAIL`, or `NOT RUN`.

- `CODE_RELEASE_CANDIDATE` is PASS only when R0–R7 all pass.
- `USER_READY` is PASS only when R0–R8 all pass with finalized external
  evidence.
- Never soften this verifier to match the implementation. Amend it only if it
  misstates the product contract, and record the amendment before more work.
