# Cross-mint opt-in verifier

This is the standing release contract for making cross-mint Earn optimization ready to wire into `loyal-apps`. Run it as an independent verifier, not as a description of what the implementation intended to do. Return `PASS_READY_FOR_LOYAL_APPS_WIRING` only when every required local, database, current-Jupiter, finalized-mainnet, reconciliation, and cleanup row is present. Compilation, a quote, simulation, submission, or one canary is not a substitute for finalized effects.

The older `cross-mint-jupiter-saga-verifier.md` still owns the durable three-leg saga, crash recovery, finality, capacity, and fencing invariants. This document supersedes its swap-policy shape and opt-in scope.

## Claim boundary

PASS means this repository contains the Rust policy/action SDK, policy observation, one-row-per-policy catalog, typed planner bindings, execution preflight, and recovery behavior needed for `loyal-apps` to add a separate cross-mint toggle. It also means a disposable smart account has exercised the immutable policy set and required routes on mainnet-beta with the normal test wallet.

PASS does not mean `loyal-apps` is already wired, workers were deployed, production routing was enabled, or any production user's policy was changed.

## Resolved product decisions

- V1 covers exactly the six stablecoins already supported by Earn routing: CASH, USDG, PYUSD, USDC, USDT, and USDS. Rust owns their mint and token-program metadata and generates all 30 ordered non-self pairs.
- Existing Earn policies continue to authorize withdraw and deposit. Cross-mint enrollment adds swap authority only; it never replaces or widens an Earn policy.
- The product toggle installs the complete swap-policy set after Earn setup. “On” is not true until every policy has finalized and strict readback succeeds. “Off” blocks new starts before policy removal begins. Removal of either policy makes the on-chain set incomplete; active movements continue or recover from their immutable bindings.
- Users do not choose a hand-maintained list of exact pairs in V1. Their risk profile is expressed by whether cross-mint is enabled, the maximum Jupiter slippage, and the daily source-mint cap. Same-mint policy selection remains independent.
- The policy set is immutable after enrollment. Supporting a new mint, dialect, or materially different risk envelope requires the user to remove and recreate it; routine route, venue-account, quote, amount, hop, or ALT changes do not.
- This repository owns only the Rust SDK. `loyal-apps` owns the current TypeScript integration surface.

## The deliberately small design

“Mint is data, not architecture.” The policy is a durable capability envelope, not a route compiler and not 30 copies of the same idea.

One policy containing six source-mint spending limits and both current Jupiter V2 ExactIn dialects serializes to 1,298 bytes and cannot fit Solana's 1,232-byte packet limit. Two deterministic source-token-program shards are the minimum layout that fits:

```text
classic source policy:    USDC, USDT, USDS
Token-2022 source policy: CASH, USDG, PYUSD
```

Each create transaction is 1,148 bytes, leaving 84 bytes. Each policy has exactly:

- three daily spending limits, one for each source mint in its shard;
- Jupiter V6 as the only top-level interaction program;
- RouteV2 at constraint index 0 and SharedAccountsRouteV2 at index 1;
- the exact smart-account vault authority;
- an output-token-account constraint allowing only this vault's six canonical ATAs;
- the exact dialect discriminator, `slippage_bps <= enrolled maximum`, and `platform_fee_bps == 0`.

The three spending limits are also the source-mint allowlist. Squads applies them to every writable token account owned by the vault and rejects a decrease in any mint without an active limit. A source mint's cap is shared across all five destinations and both dialects, so changing dialect cannot double the allowance.

The policy intentionally does not freeze Jupiter's route accounts, hops, AMMs, ALTs, input amount, or quote. Jupiter validates its own internal account graph; the worker certifies each fresh build. Adding those dynamic details to user policy state would make normal routing drift require policy updates.

## Security boundary and honest limitations

The on-chain policy must prevent authority and custody escape even if the delegated signer is wrong. The worker must prevent a bad economic decision while the signer is operating normally. Finalized reconciliation must prove what actually happened.

- On chain: only the enrolled delegated signer may call Jupiter through the policy; source loss is bounded per mint per day; output can land only in a canonical ATA owned by the same vault; maximum slippage and zero platform fee are enforced.
- Before signing: the worker rejects self swaps, unsupported mints/extensions, wrong token programs or ATAs, unknown instructions, stale ALTs/blockhashes, privilege escalation, unexpected fees, excessive value loss, packet overflow, and compute overflow. It maps the build's actual dialect to index 0 or 1 without changing policy.
- After finalization: advancement uses the movement-attributed source debit and target credit from finalized transaction metadata, never predicted output or aggregate ATA balances.

The policy cannot prove quote freshness or fair market value. `quotedOut` and `minOut` are signer-supplied Jupiter data, so a compromised delegated signer can select a poor but structurally valid swap until the per-mint daily cap is exhausted. The cap is the on-chain blast-radius bound; fresh independent build certification is the economic control. Jupiter's upgrade authority is also inside the trust boundary because the policy delegates internal route validation to the deployed Jupiter program.

The generalized envelope cannot encode `input_mint != output_mint`. The production worker rejects self swaps; custody still cannot leave canonical vault ATAs and the spending cap bounds malicious churn. These limitations must remain visible to the product risk decision rather than being hidden behind additional abstractions.

## Pre-withdraw admission

A new movement may start only when one atomic current observation proves:

```text
cross-mint product enrollment is complete and on
AND source Earn-withdraw capability is finalized
AND the source-shard swap policy and both dialect projections are finalized
AND target Earn-deposit capability is finalized
AND a fresh Jupiter build for the exact pair and amount is certified
```

The immutable execution plan binds withdraw, swap, and deposit to their own policy accounts and action indexes. The swap binding additionally records policy kind, canonical manifest fingerprint, source/target mint and token programs, both dialect indexes, enrolled cap, and maximum slippage. The worker recomputes the manifest fingerprint from finalized policy bytes and chooses the actual dialect only after the fresh build is parsed.

Missing or stale policy evidence, an incomplete dialect projection, partial enrollment, quote failure, unsupported extension, or unfit transaction is a planning rejection before withdrawal. After a finalized withdrawal, revocation or opt-out stops new work but does not strand custody: the existing saga continues, recovers source, deposits target, or quarantines ambiguity.

## Durable movement invariants

```text
source reserve
  -- finalized withdraw credit W --> source-mint idle
  -- finalized source debit W / target credit O --> target-mint idle
  -- finalized deposit debit D --> target reserve, residual R = O - D
```

- Persist exact signed bytes before broadcast and retry only those bytes while valid.
- Advance only from finalized, movement-attributed deltas. Preserve pre-existing balances.
- A deposit can terminalize with dust only when `D > 0` and finalized reserve math proves `0 < R < minimum_deposit_amount_raw`; persist `kamino_unmintable_rounding_dust` evidence.
- Before swap, recover to a safe source-mint reserve when continuation is unsafe. After swap, deposit to a safe target-mint reserve; never automatically reverse the swap.
- Keep target capacity reserved until completion, source recovery, provable cancellation, or explicit manual intervention.
- One fencing-token winner may continue. An expired signature permits a replacement generation only after history and balance evidence prove no effect. Otherwise quarantine.
- Opt-out and `start_new_movements=false` block new withdrawals independently of `continue_or_recover_existing`.

## Required verification matrices

### Policy and authorization

1. Generate exactly six canonical mints and 30 unique ordered non-self pairs from one registry.
2. Build both production policy creates and independently decode their bytes.
3. Prove each create fits, the six-source variant does not, and no update is needed across all 30 pairs.
4. Through Squads SBF/LiteSVM, execute both dialects and both source shards; reject wrong shard/source, unsupported mint, non-canonical output, excessive slippage, nonzero fee, wrong dialect index, cap overflow, removal, and broadened bytes.
5. Treat the local mock as policy-byte enforcement only. Token-2022 CPI, reverse-direction routing, and both Jupiter dialects require current mainnet evidence.

### Current Jupiter matrix

For all 30 pairs, fetch a fresh ExactIn build, load finalized ALTs and token/mint accounts, parse the complete instruction set, compile the policy-wrapped v0 transaction, enforce 1,232-byte and compute limits, and simulate without sending. Record dialect, hops, unique accounts, raw/wrapped bytes, compute, and rejection reason.

### Immutable-policy mainnet matrix

On one fresh disposable smart account:

1. create both policies once and strictly read them back at finalized commitment;
2. fund all six vault ATAs at low value;
3. execute every ordered non-self pair without any policy update;
4. for each swap, persist signed bytes before send, finalize, verify identical landed wire, and reconcile exact source debit plus target credit at or above signed minimum;
5. return every residual token to the test wallet, close all six ATAs, remove both policies, and prove finalized absence.

A skipped pair, a signature without finalized effects, an aggregate-balance assertion, or a policy update between routes fails the matrix.

### Historical full-route matrix

The ten directions recovered from optimizer history are:

```text
USDC -> USDS    USDS -> PYUSD    PYUSD -> USDG    USDG -> USDS
USDS -> USDG    USDG -> USDC     USDC -> PYUSD    PYUSD -> USDC
USDC -> USDG    USDS -> USDC
```

Run each as a low-value finalized `withdraw -> swap -> deposit` route using the same immutable two-policy design plus the existing Earn policies. Each leg must reconcile before the next begins, and cleanup withdrawal must reconcile afterward. Preserve the evidence classification (`exact_historical_endpoints`, `safe_substitution`, `safe_substitution_current_capacity`, or `direction_only_inference`) rather than inventing missing historical reserve endpoints.

## Verifier-first execution order

1. Pure Rust: registry, policy bytes, detector, fingerprint, cap/slippage validation, and measured create packets.
2. Squads SBF/LiteSVM: production creates, readback, execution, cap aggregation, and adversarial mutations.
3. Monitor/store/planner/worker: one manifest event and one catalog row per policy, canonical pair authorization from both finalized shards, finalized on-chain fingerprint recomputation, transactional opt-out/revocation admission, typed immutable bindings, and same-mint regression.
4. Disposable Postgres: migrations through the current version, projection/removal/ambiguity tests, movement saga, all crash windows, and capability admission.
5. Current mainnet read-only 30-pair build/fit/simulation matrix.
6. Fresh immutable-policy 30-pair value-moving matrix.
7. Fresh ten-route withdraw/swap/deposit matrix.
8. Broad format, strict Clippy, tests, lint, build, and diff checks.

This order is intentional: the fastest verifier that can disprove an invariant runs first. Funded sends happen only after policy bytes, SBF enforcement, database contracts, compilation, and live simulation are green.

## Required commands

- focused `cargo fmt --check`, `cargo check`, strict Clippy, and tests for touched Rust crates;
- `bun run test:squads`;
- `bun run test:squads:e2e`;
- `bun run verify:cross-mint:store` against its disposable local PostgreSQL database;
- `bun run verify:cross-mint:jupiter-matrix` through the persistent 1Password session;
- `CONFIRM_MAINNET=1 CROSS_MINT_MAINNET_PAIR_LIMIT=30 bun run verify:cross-mint:jupiter-mainnet` through that session;
- `CONFIRM_MAINNET=1 CROSS_MINT_MAINNET_ROUTE_LIMIT=10 bun run verify:cross-mint:routes-mainnet` through that session;
- `bun run lint`, `bun run build`, and `git diff --check`.

Rust tests must follow `AGENTS.md`: protect policy bytes, account planning, parser/security boundaries, ABI/spec drift, database shape, finalized effects, or live-gated contracts. Tests that only restate fields, defaults, source text, or mocked JSON do not count.

## The Linus test

If the design needs one layer to compensate for another layer's accidental complexity, simplify it. The right decomposition is:

```text
policy:     who may spend, from which source universe, into which custody, how much
Jupiter:    whether its own route-account graph is valid
worker:     whether this exact fresh trade is economically and structurally acceptable
saga:       what finalized value actually moved and how custody safely continues
```

Do not add 30 exact-pair policies, a route DSL, a venue framework, policy updates for normal Jupiter drift, or duplicate lifecycle state. Each invariant has one owner and one evidence source.

## PASS conditions

PASS requires all of the following:

1. The two immutable source-sharded policy creates are the canonical Rust output, fit measured packets, and strictly decode from finalized account bytes.
2. The policy cap aggregates across every target and both dialects for each source mint; reverse and Token-2022 directions are proven on current mainnet.
3. Monitor, store, planner, and worker reject malformed manifests, incomplete dialects, stale fingerprints, wrong shard/token program, revoked policy, and changed finalized bytes.
4. Pre-withdraw admission requires separate finalized withdraw, swap, and deposit capabilities plus a fresh certified build.
5. All 30 current pair rows are built, simulated, sent, finalized, reconciled, and cleaned up without policy updates.
6. All ten historical routes finalize and reconcile withdraw, swap, deposit, and cleanup under the generalized policy set.
7. The durable restart/recovery matrix passes without duplicate effects, consumed pre-existing balances, stranded custody/capacity, or false terminal success.
8. Existing same-mint behavior and required repository checks remain green.
9. Evidence distinguishes implementation, simulation, finalized behavior, cleanup, and non-claims. No secret or production-user state appears in artifacts.

## Recorded verification result — 2026-08-16 UTC

All nine conditions passed for the two immutable source-sharded policies. The
tracked evidence, including both policy creates, all 30 finalized pair swaps,
all ten finalized full routes, reconciliation, cleanup, idempotent resume, and
repository gates, is in
[`../evidence/cross-mint-mainnet-2026-08-16.md`](../evidence/cross-mint-mainnet-2026-08-16.md).

`PASS_READY_FOR_LOYAL_APPS_WIRING`

## Verdict

Return exactly one:

- `PASS_READY_FOR_LOYAL_APPS_WIRING`
- `FAIL_IMPLEMENTATION`
- `FAIL_POLICY_OR_PACKET_FIT`
- `FAIL_CURRENT_JUPITER_DIALECT`
- `FAIL_MAINNET_FINALITY_OR_RECONCILIATION`
- `FAIL_TEST_FUNDS_OR_EXTERNAL_DEPENDENCY`

Only the first verdict completes this goal.
