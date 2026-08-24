# Earn MAX production contract

**Version:** `earn-max-v3`
**Date:** 2026-08-24
**Status:** Authoritative simplified implementation and release contract. This replaces the v2 multi-owner projection and three-endpoint contract. The unchanged on-chain policy manifest remains `earn-max-v1`.

## Outcome

```text
Objective:
  At exact deployed loyal-apps and loyal-yield-routing revisions, one
  authenticated mainnet user completes this production-shaped lifecycle:

  install -> deposit -> real hookless Multiply open -> top up -> cancel one
  withdrawal request -> partial request -> full unwind -> partial claim ->
  automatic redeploy of the remainder -> full request -> full unwind -> full
  claim -> policy removal.

  The browser creates and submits the user's transactions. All confirmed Solana
  policy, intent, deposit, claim, and delegated-operation facts are admitted by
  the existing LaserStream owner.
  Neon state, operations, snapshots, read APIs, wallet/vault deltas, deployed
  images, and application revision reconcile independently. A bounded projector
  restart/replay leaves row and cash-flow cardinality unchanged.

Scope:
  One existing user Squads Settings account, Earn MAX vault index 0, policy
  manifest earn-max-v1, and the production-proven syrupUSDC/USDC strategy.
  Mainnet at confirmed commitment. PYUSD and USX are excluded until a second
  production lifecycle proves a real need. No SVM or fixture proof, hook, guard, flash loan, pooled
  shares, new on-chain program, generic executor, second database, event ledger,
  command table, scheduler, saga, outbox, or app confirmation endpoint.

Verifier:
  op run --env-file=.env.1password -- bun run verify:earn-max:production

External gates:
  Current mainnet RPC, Neon, LaserStream, Render, deployed loyal-app read access,
  immutable exact revisions, and the authorized funded disposable user. The
  verifier is read-only: it never signs, sends, deploys, restarts, or repairs.

Verdict:
  PASS_EARN_MAX_SIMPLIFIED_PRODUCTION_READY
  FAIL_EARN_MAX_PRODUCTION_READY <first false condition and evidence>
  BLOCKED_EARN_MAX_PRODUCTION_READY <dependency and exact resume condition>
```

`verify:earn-max:production` is the only product-readiness authority. Compile,
deployment, RPC, database, API, and lifecycle checks are subchecks; none may
emit a competing production PASS.

## The whole architecture

```text
wallet-signed Solana transaction
             |
             v
existing confirmed LaserStream reconciliation
             |
             v
earn_max_policy_sets + multiply_route_states
+ multiply_operations + multiply_position_snapshots
             |
             v
typed summary + activity/history endpoints -> one Earn MAX UI adapter
```

There is no application write path. Solana owns policy existence, user intents,
token balances, and protocol balances. LaserStream is the only confirmed-chain
ingestion owner; exact confirmed transaction/account readback is proof. The
worker persists attempts but never independently discovers user deposits or
claims. Neon stores only current policy/route state, literal operations,
performance snapshots, and the existing durable projection cursor.

Do not add Earn MAX policy-event, decision, command, job, saga, outbox,
allocation, registry, confirmation, or repair tables. A confirmed root-signed
withdraw/cancel Memo is stored as a reconciled `multiply_operations` row keyed
by `(transaction_signature, source_instruction_index)` and advances the existing
route state in the same transaction. Reprocessing the same chain location is a
no-op. Worker decisions remain on the operation that enacted them.

## Chain and worker contract

The browser may construct only these closed client actions:

```text
install policies
deposit or top up USDC
request withdrawal of an exact amount or max
cancel the current request
claim the reconciled available amount
remove policies after full close
```

All Settings, vault, ATA, obligation, farm-user, policy, program, reserve, mint,
token-program, destination, and seed identities are derived from authenticated
Settings plus frozen release data. Caller-selected topology is impossible.
Withdrawal and cancel use exact bounded `loyal:earn-max:v1:*` Memo bytes inside
a root-signed Squads vault execution. The Memo names the vault as signer and a
wallet-derived USDC ATA as destination. The projector accepts the Memo only from
the watched vault's successful confirmed transaction.

The worker retains the proven literal hookless KLend/Jupiter trench graph. It
never signs user deposits, requests, cancels, claims, or closes, and it does not
scan transaction history for them. The LaserStream reducer admits confirmed
deposit/claim token deltas atomically with the route and operation transition.
A top-up is another confirmed deposit; any idle claim balance is deployed even
when an active obligation already exists.

The engine exposes only syrupUSDC/USDC. There is no strategy-switch goal,
generic target selection, or dormant second topology. A second strategy earns
an abstraction only after its own production-shaped lifecycle is observed and
mechanically compared with this one.

A request has a 30-second cancellation grace enforced by the existing route
selector. Cancel is valid only while the request is still `requested` and no
worker operation exists. After that, unwind proceeds immediately. `readyBy` is
the ten-minute SLA deadline, not an artificial claim delay; claim becomes
available as soon as the full unwind is reconciled.

Partial withdrawal deliberately performs the same full unwind as full
withdrawal. Claim transfers only the requested amount. Any confirmed remainder
in claim custody sets the existing goal to `deploy` and reuses the same open
graph. This is smaller and safer than partial KLend arithmetic. A full claim
leaves claim, collateral, debt, and equity at zero before policy removal.

## Application contract

The deployed slice has exactly two authenticated, write-free endpoints:

```text
GET /api/smart-accounts/earn-max/summary
GET /api/smart-accounts/earn-max/activity
```

There are no prepare, confirmation, withdrawal mutation, policy mutation, or
generic transaction endpoints. The summary response exposes only read-only client
configuration and projected state. The client uses shared transaction builders
and the existing wallet submission primitive, confirms at `confirmed`, then
refreshes projections. The server never receives private keys or unsigned user
intent to persist.

The summary response is a shared typed contract used directly by the client;
activity/history remains separate for pagination. One Earn MAX view model
adapts those two responses into:

- current equity, earned amount, forecast APY, realized APY, and coverage;
- confirmed performance points and confirmed activity;
- policy/install, position, busy, error, and freshness state; and
- request amount, `readyBy`, cancellation, claim, and close availability.

Unknown or incomplete realized performance is `history_incomplete`, never a
fabricated zero. Equity is liquid claim value plus collateral value minus debt
value. Earned is current equity plus confirmed claims minus confirmed deposits.
The displayed realized APY is the same intentionally simple approximation used
by the product: annualized net earnings over total confirmed deposits and the
observed coverage duration; it is not presented as money-weighted IRR. Forecast
is collateral yield minus debt cost using confirmed reserve curves. No regular
Earn domain state, mocked balance, mocked APY, or no-op action is used.

## Authoritative verifier order

The verifier stops on the first false condition:

1. **Contract identity:** this hash/version is the sole readiness contract.
2. **Cheap architecture:** one projector, four existing tables, two typed GETs,
   one client adapter, and one proven strategy; no worker-side user transaction
   scanner, persisted frontend projection, forbidden program, endpoint, table,
   direct mutation, mock, fixed user identity, hook, guard, flash loan, PYUSD,
   USX, or second authority.
3. **Static boundaries:** exact Memo grammar and vault signer binding; chain
   `(signature,index)` idempotency; confirmed readback; top-up, 30-second cancel
   grace, partial-claim redeploy, ten-minute SLA, and final-zero gates exist.
4. **Targeted checks:** touched Rust crates compile; Actions builds; the scoped
   Earn MAX TypeScript gate passes. No local frontend build and no new SVM test.
5. **Deployment identity:** mainnet genesis, migrations through 64, immutable
   worker image SHAs, live Render services, exact app revision, v2 response
   headers, authentication, and exact remote endpoint inventory match source.
6. **Fresh lifecycle:** two confirmed deposits, one confirmed canceled request,
   one confirmed partial request/claim followed by a real redeploy, one confirmed
   full request/claim, literal hookless open/unwind operations, policy removal,
   ten-minute SLA, and final zero reconcile on chain and in Neon.
7. **Accounting/API truth:** deployed summary and activity recompute
   from those confirmed operations/snapshots with honest coverage and freshness.
8. **Replay:** after the lifecycle, a bounded LaserStream restart/replay is newer
   than the last projected operation; `(signature,index)`, operation, deposit,
   claim, and policy cardinalities remain unique and unchanged.

Compilation, a UI render, prepared bytes, an unsigned simulation, a DB-only row,
logs without account readback, a historical lifecycle, or an open without
top-up/cancel/partial/full close/replay cannot produce PASS.

## Implementation loop

Run the sole verifier, fix its first useful failure, rerun the affected cheap
check, and run it once more only after exact revisions are deployed and the
authorized confirmed lifecycle plus replay are complete. Batch local checks and
publish each image/application revision once. Do not generalize to a second
strategy before this one passes literally.
