# Earn MAX per-user production contract

**Version:** `earn-max-v1`  
**Date:** 2026-08-22  
**Status:** Authoritative implementation and release contract. This supersedes the fixed pooled-vault Multiply release-candidate contract.

## Outcome

```text
Objective:
  At the deployed loyal-apps and loyal-yield-routing revisions, one
  authenticated mainnet user completes a production-shaped Earn MAX lifecycle:
  install deterministic policies -> LaserStream observes ready -> deposit ->
  a real hookless Kamino Multiply position opens -> state and history reconcile
  -> withdrawal is requested -> unwind is claimable within 600 seconds -> claim
  -> policies are removed -> LaserStream observes removed. Confirmed Solana
  accounts and transactions, Neon projections, API responses, wallet/vault
  deltas, and deployed revisions reconcile independently.

Scope:
  One existing user Squads Settings account, one deterministic Earn MAX vault
  index, one manifest version, and one advertised strategy. An additional
  strategy is not exposed until the same real open and full-close proof passes
  for it. Mainnet at confirmed commitment. USX is excluded. No SVM, fixtures,
  hooks, guard, flash loans, pooled share accounting, generic executor, policy
  event ledger, second database, new on-chain program, or app confirmation API.

Verifier:
  op run --env-file=.env.1password -- bun run verify:earn-max:production

External gates:
  Current mainnet RPC, Neon, LaserStream, Render and deployed loyal-app read
  access; exact deployed revisions; and a funded authenticated disposable user
  lifecycle executed through the product. The verifier is read-only: it never
  signs, sends, deploys, restarts, repairs, or writes evidence.

Verdict:
  PASS_EARN_MAX_PRODUCTION_READY
  FAIL_EARN_MAX_PRODUCTION_READY <first false condition and evidence>
  BLOCKED_EARN_MAX_PRODUCTION_READY <dependency and exact resume condition>
```

`verify:earn-max:production` is the only product-readiness authority. Engine,
build, lint, route, and deployment checks are subchecks and cannot emit a
competing production PASS.

## Minimal ownership model

Solana owns policy existence and token/protocol balances. LaserStream is an
invalidation and replay transport; an exact confirmed account reload decides
current policy status. Neon stores only the product state needed to operate and
explain the position:

- `earn_max_policy_sets`: one current deterministic manifest projection;
- `multiply_route_states`: one current per-user route and withdrawal intent;
- `multiply_operations`: one row per literal worker transaction, including its
  reason and reconciliation evidence;
- `multiply_position_snapshots`: append-only valuation/performance inputs; and
- existing `projection_offsets`: the durable LaserStream cursor.

Do not add Earn MAX policy-event, decision, command, job, saga, outbox,
registry, allocation, confirmation, repair, or generic transaction tables.
Decisions belong on the operation that enacted them. The app may mutate only an
idempotent withdrawal intent through compare-and-swap. Policy sets, operations,
and snapshots are projector/worker owned.

## Deterministic topology

User Settings, vault, token accounts, obligation/farm users, policy seeds, and
policy PDAs are derived from the authenticated Settings account plus the frozen
manifest. Strategy reserve, mint, token-program, market, farm, and target-risk
facts are release data. Production code must not embed one user's current
Settings, vault, custodies, obligations, policy accounts, or dynamically
allocated policy seeds.

Deriving two different Settings accounts must produce distinct user-owned
accounts and policies while preserving the same strategy facts. The verifier
independently derives and decodes topology; it does not import the production
planner, transaction builders, repositories, or frontend mapper.

## One policy projection path

The existing Squads policy monitor uses LaserStream transactions at `confirmed`
with a durable cursor and bounded inclusive replay overlap. It has no parallel
WebSocket `transactionSubscribe` authority and no in-memory-only dedupe truth.

For a relevant create, update, or remove notification it reloads every expected
policy account at confirmed commitment, decodes the exact ProgramInteraction
semantics, and atomically writes the current manifest status and cursor.
Partial installation is `incomplete`; an exact complete manifest is `ready`;
removed/nonmatching accounts cannot remain ready. Replaying a real install or
removal input twice produces the same single row.

## Application contract

The deployed Earn MAX slice has exactly four authenticated endpoints:

```text
GET  /api/smart-accounts/earn-max/state
GET  /api/smart-accounts/earn-max/history
POST /api/smart-accounts/earn-max/transactions/prepare
POST /api/smart-accounts/earn-max/withdrawals
```

There are no policy, deposit, claim, or close confirmation endpoints. GET and
prepare calls are write-free. `transactions/prepare` accepts only:

```text
install_policies | deposit | claim | close_policies
```

It derives all identities server-side and rejects caller-selected programs,
accounts, seeds, routes, reserves, strategies, and claim destinations. Returned
transactions are unsigned, canonical, ALT-resolved, packet-fitting, and contain
no hook, guard, flash, ledger, unexpected setup, or cleanup instruction.
Install safely resumes with only missing policies. Deposit requires the complete
projected manifest. Claim requires claimable funds and pays only the authenticated
wallet. Policy close requires zero debt, zero collateral, no nonterminal
operation, and no remaining product-owed claim.

`POST /withdrawals` derives user topology and destination from authentication,
accepts a bounded amount or `max` plus an idempotency key, returns the same
intent for the same key, rejects a concurrent different request, and never
broadcasts a transaction itself.

## Accounting contract

State and history expose their route generation, source slots, valuation inputs,
provenance, and freshness. The verifier recomputes:

```text
equity = liquid vault value + collateral value - debt value
earned = current equity + confirmed claims - confirmed deposits
forecast = collateral-yield contribution - debt cost - explicit fees and drag
```

The API reports equity, not gross collateral. It also reports leverage, LTV,
health, current operation and withdrawal state. Realized APY is cash-flow
adjusted and distinct from forecast APY. Incomplete coverage returns nullable
realized performance plus `history_incomplete`; unknown values are never
fabricated as zero.

## Transaction engine properties retained

- literal hookless KLend and Jupiter trench transactions;
- exact current ProgramInteraction policy binding;
- signed bytes persisted before broadcast intent;
- one send per deterministic signature with `maxRetries: 0`;
- no blind resend after broadcast intent;
- rebuild only after conclusive expiry and signature absence;
- confirmed account and transaction reconciliation after every mutation;
- exact Token/Token-2022 identity, ALT resolution, and packet fit;
- complete source close before another strategy opens;
- repay funded by reconciled reverse-swap proceeds;
- one nonterminal operation per route;
- constrained delegate-only runtime and fee cap; and
- claimability and unwind completion no later than 600 seconds after request.

## Authoritative verifier order

The verifier stops on the first false condition:

1. **Contract identity:** this version is the sole production contract and the
   deployed/source revisions are explicit.
2. **Cheap architecture:** per-user derivation exists; fixed canary identity,
   obsolete pooled state, direct app policy writes, dual policy transports,
   generic executor, forbidden tables/endpoints/programs, hooks, guard, flash,
   and USX are absent.
3. **Schema and ownership:** only the minimal tables and writers above exist;
   uniqueness, compare-and-swap, and one-nonterminal-operation constraints hold.
4. **Targeted static/runtime checks:** projector and worker role probes are
   secret-free and mutation-free; touched Rust crates compile; touched loyal-app
   files pass scoped lint/type validation. A local frontend build is forbidden.
5. **Deployment identity:** mainnet genesis, confirmed configuration, current
   migrations, immutable worker images, Render services, and deployed loyal-app
   revision match inspected source.
6. **Policy projection:** independently derived ProgramInteraction accounts and
   semantics match the manifest; confirmed install/removal signatures and slots
   match the current row; replay is idempotent; no app confirmation exists.
7. **Prepared boundary:** authentication, closed actions, canonical unsigned
   messages, rejection of overrides, packet fit, action gating, and write-free
   repeated preparation all hold against the deployed API.
8. **Fresh mainnet lifecycle:** partial install -> ready -> deposit -> real open
   -> withdrawal -> unwind within 600 seconds -> claim -> close -> removed.
   RPC metadata, obligations, custodies, policies, operation evidence, and exact
   token deltas reconcile. Final debt/collateral are zero and no unexplained
   custody or nonterminal work remains.
9. **API/accounting truth:** deployed state and history recompute from the same
   confirmed inputs; earnings, realized APY, forecast, freshness, coverage, and
   state discriminants are honest.
10. **Terminal idempotency:** withdrawal replay and LaserStream overlap do not
    duplicate work, cash flows, policies, or rows. Final chain state stays closed.

Compilation, lint, simulation, prepared-but-unsigned messages, DB-only state,
LaserStream logs without account readback, historical pooled-vault evidence,
screenshots, or an open without unwind/claim/removal cannot produce PASS.

## Implementation loop

Run the verifier once, fix only its first useful failure, rerun the affected
cheap checks, and run the complete verifier once after exact revisions are
deployed and the authorized canary lifecycle is complete. An additional
strategy is advertised only after that same literal lifecycle passes for it;
do not create a generic strategy framework first.
