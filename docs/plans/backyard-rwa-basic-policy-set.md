# Backyard RWA smart account: basic policy set (v1 draft, 2026-09-09)

## Goal

Compile and, after separate review and simulation gates, install the
owner-approved basic policy set on the Backyard Squads settings
`5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6` (vault index 0 =
`ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh`, delegated executor
`62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5`). The set is exactly four
legacy `PolicyCreate` policies at seeds 141–144: CollateralLifecycle,
DebtLifecycle, SwapRoutesA, and SwapRoutesB. There are no bridge policies,
spending limits, updates, or lookup tables in this basic set.

## What is installed today (finalized slot 445753557)

| Seeds | Count | What | Fate |
| --- | --- | --- | --- |
| 17–24, 29, 32–50 | 28 | Backyard Kamino-vault route (main-usdc / four-market, seeds 43/44 are its deposit/withdraw policies) | out of scope, untouched |
| 57–61, 66 | 6 | PRIME/USDC packets (open ×2, delever ×2, swap ×2) bound in the Go manifest | retire after rebind |
| 62–65 | 4 | strategy-one Voltr bridge: allocate, report NAV, stage, restore idle | replaced by strategy-two bridge |
| 67–136 | 70 | strategy-one lane catalog: 11 lanes × 4 Kamino ops + 3 swap shards | retire after rebind |
| 137–139 | 3 | Maple farm rollovers (exact farm pins broke on a Kamino farm change) | retire after rebind |
| 140 | 0 | one-shot repair policy, already removed | — |

111 policies, 1.229 SOL of rent. Settings policy seed counter = 140, so the
next PolicyCreate takes seed 141.

## Why 70 became 7

Every catalog policy pins all 17 Kamino accounts as exact pubkeys, so one
Kamino op fills a 1,232-byte legacy packet and one policy. A Kamino farm
change then invalidates the exact farm pins (rollovers 137–139). The
three-family shape pins only the endpoints that can redirect value or
authority (vault, vault-owned obligation, approved reserves/markets, vault
custodies) and leaves KLend to enforce reserve/market, mint/custody,
token-program, farm and market-authority coherence, which it does on every
instruction anyway. The basic set keeps the same boundary but splits the
four swap bicliques into two two-constraint shards so every policy remains a
legacy packet without an ALT.

## Privy-style reading of the set

Privy: a policy is a list of rules; a rule = method + conditions + ALLOW/DENY;
any DENY wins, otherwise an ALLOW is required, otherwise deny; on Solana every
instruction must independently ALLOW. Squads ProgramInteraction is the same
model: each instruction executed under a policy must match one constraint
(program id + data predicates + account predicates); no match → reject.

Rule group 1 — CollateralLifecycle (seed A)

```
ALLOW KLend depositReserveLiquidityAndObligationCollateralV2 IF
  account[0] == vault (signer)
  account[1] is KLend-owned obligation AND obligation.owner == vault
  account[4] ∈ {ONyc reserve 6Zxk…, PRIME reserve BUTN…, syrupUSDC reserve AwCy…}
  account[9] ∈ {vault ONyc ATA, vault PRIME ATA, vault syrupUSDC ATA}
ALLOW KLend withdrawObligationCollateralAndRedeemReserveCollateralV2 IF (same conditions)
```

Rule group 2 — DebtLifecycle (seed A+1)

```
ALLOW KLend borrowObligationLiquidityV2 IF
  account[0] == vault; account[1] obligation owned by vault
  account[2] ∈ {OnRe market 47tf…, Figure market CqAo…, Maple market 6WEG…}
  account[8] ∈ {vault USDC ATA, vault PYUSD ATA, vault USDS ATA}
ALLOW KLend repayObligationLiquidityV2 IF
  account[0] == vault; account[1] obligation owned by vault
  account[2] ∈ same markets; account[6] ∈ same stable custodies
```

Rule group 3 — SwapRoutes, four directed bicliques split into two shards:
SwapRoutesA (seed A+2) holds the two stable→RWA bicliques, SwapRoutesB
(seed A+3) the two RWA→stable bicliques

```
ALLOW Jupiter sharedAccountsRoute IF account[2] == vault AND
  (account[3] ∈ {USDC, USDS} custodies  AND account[6] ∈ {ONyc, PRIME} custodies)
  (account[3] ∈ {USDC, PYUSD}            AND account[6] ∈ {PRIME, syrupUSDC})
  (account[3] ∈ {ONyc, PRIME}            AND account[6] ∈ {USDC, USDS})
  (account[3] ∈ {PRIME, syrupUSDC}       AND account[6] ∈ {USDC, PYUSD})
  AND data[0..8] == c1209b3341d69c81 (full SharedAccountsRoute discriminator,
  not just its c120 prefix) AND account[9] == the Jupiter program id, the
  platform_fee_account sentinel, so no fee can be routed to a foreign account
```

Covered lanes: ONyc/USDC, ONyc/USDS, PRIME/USDC, PRIME/PYUSD, PRIME/USDS,
syrupUSDC/USDC, syrupUSDC/PYUSD. Not covered (by design): USDG, AUTO, USDe,
CASH, ONyc/PYUSD, syrupUSDC/USDS.

What the set does not bound on chain: amount per op, swap slippage, a bad
quote. The worker bounds those; a per-mint Squads spending-limit policy is the
optional on-chain backstop and is a separate follow-up.

## Executor rebind (Go worker)

Manifest v2: four Kamino/Jupiter policy accounts (seeds 141–144) instead of
per-lane packets. The Go worker must bind each lifecycle action to the
corresponding family and constraint index before activation.
Binding per action: DepositCollateral → (Collateral, 0); WithdrawCollateral →
(Collateral, 1); BorrowDebt → (Debt, 0); RepayDebt → (Debt, 1); swaps →
(SwapRoutesA idx 0/1 for stable→RWA, SwapRoutesB idx 0/1 for RWA→stable, by
source/destination custody). Lane graph
(market, reserves, obligation, custodies, farms) stays in runtimeActivation.
Farm accounts: derive the obligation farm user state for reserves that have a
farm (OnRe USDC debt farm, Maple USDC debt farm) instead of the KLend
placeholders in borrow positions 12/13 and repay 9/10. Runtime routes:
PRIME/USDC, Maple/syrupUSDC/USDC, OnRe/ONyc/USDC.

## Gates before any mainnet write

1. Rust: policy constraints for the ST999 boundary, fingerprint parity test,
   measured legacy packets `1136, 1136, 818, 818` bytes for seeds 141–144,
   all ≤ 1,232 bytes. The deterministic artifact is
   `docs/evidence/backyard-rwa-basic/policy-artifact-v1.json`.
2. LiteSVM: the four policies installed on a cloned settings; each loop op
   for the three runtime lanes passes; mutation matrix rejects foreign
   obligation, foreign reserve, foreign custody, wrong discriminator,
   forbidden swap pair.
3. Unsigned simulation of the four PolicyCreate transactions against current
   mainnet at a fresh blockhash.
4. sol review of the artifact and the Go rebind.
5. Install: one leg per transaction, journaled, astra-executed, readback
   evidence JSON.
6. Go worker on manifest v2, tests green, lifecycle audit updated to the new
   catalog.
7. Only after the new worker binding is live and reconciled, separately review
   retirement of superseded strategy-one policies. This basic-policy brief does
   not authorize PolicyRemove operations.

## Phase 1 implementation checkpoint

The Rust compiler is `compile_backyard_basic_policy_set` in
`crates/loyal-actions/src/backyard_basic_policy_set.rs`, with the offline bin
`compile-backyard-basic-policy-set`. It derives the seven approved topology
templates, uses obligation tag 1/id 0, reuses the earn-max lifecycle and swap
constraint builders, and records four instruction-data SHA-256 fingerprints.
The compiler is deterministic and does not read RPC, sign, or broadcast.
