# Earn MAX three-policy production contract

**Version:** `earn-max-v4`
**Date:** 2026-08-24
**Status:** Authoritative verifier-first implementation and release contract. This replaces the six-account `earn-max-v1` policy manifest and the `earn-max-v3` readiness contract.

## Outcome

```text
Objective:
  At exact deployed loyal-apps and loyal-yield-routing revisions, one
  allowlisted mainnet testing user installs exactly three fresh earn-max-v2
  Squads ProgramInteraction policy accounts and completes, at confirmed
  commitment:

  install -> route ready -> deposit/open -> top up -> cancel -> partial
  unwind/claim/redeploy -> full Max unwind/claim -> close.

  LaserStream, Neon, authenticated APIs, the client SDK and the hidden or
  allowlisted UI reconcile to current chain state. The terminal state has zero
  strategy and custody residue, all three policy PDAs absent, and policy rent
  returned.

Scope:
  Mainnet at confirmed commitment. One user-owned Squads Settings account,
  vault index 0, the exact seven-strategy catalog below, and the existing
  worker/projector/read model. No SVM fixture is production authority.

  Required catalog:
    ONyc/USDC, ONyc/USDS
    PRIME/USDC, PRIME/PYUSD, PRIME/USDS
    syrupUSDC/USDC, syrupUSDC/PYUSD

  CASH, USDG, AUTO and USDe are absent. USDS remains even when current
  utilization makes a lane temporarily capacity-blocked.

Hard constraints:
  Exactly three physical policy accounts at fresh consecutive seeds:
    base+0 CollateralLifecycle: deposit collateral, withdraw collateral
    base+1 DebtLifecycle: borrow debt, repay debt
    base+2 SwapRoutes: one exact directed lane in each direction for every
                       catalog pair (14 constraints total)

  Each KLend transaction places permissionless refreshes at top level and
  exactly one signer-bearing terminal mutation inside Squads. Each directed
  swap lane pins the vault plus exact source and destination custody and the
  Jupiter SharedAccountsRoute discriminator. Dynamic mint, token-program and
  route-tail accounts are accepted only because Jupiter validates them against
  the pinned token accounts and a mutation matrix proves they cannot redirect
  value. The client and worker independently require the exact expected mint,
  token-program and route semantics before submission.

  CollateralLifecycle and DebtLifecycle pin the vault and finite catalog sets
  of obligations, reserves, custody accounts and token programs. Farm user and
  farm state accounts are not pinned because the fully pinned payload cannot
  fit one policy; KLend validates farm derivation from the selected reserve and
  obligation. The compact format cannot express relationships between two
  allowlisted account positions: a same-market catalog cross-product can be
  accepted by KLend. That residual case cannot redirect value outside the
  vault's finite catalog custodies, is never built by the worker, and must be
  detected as obligation-topology drift during reconciliation. Outside-catalog,
  cross-market and invalid-farm mutations must reject with no custody delta.
  This exact KLend/Jupiter validation boundary was owner-approved on 2026-08-24.

  Every legacy policy-create wire is below 1232 bytes and within the deployed
  Squads constraint limit. Packet overflow is BLOCKED; it is not permission for
  a fourth policy or weaker pins.

  The verifier reads and hashes the deployed Squads ProgramData account before
  any lifecycle check. Compact PolicyCreate and PolicyUpdate are usable only
  when that exact binary hash is allowlisted after an independent deployed
  simulation proves both compact variants deserialize. A legacy binary that
  returns InstructionDidNotDeserialize is BLOCKED even when the compact packet
  itself fits; raw payload overflow never permits weaker constraints.

  Installation may use multiple confirmed transactions. Each missing PDA is
  first created with a minimal exit-safe legacy payload, then compact-updated
  in place to the complete family catalog. Readiness remains incomplete until
  all three full semantic payloads match. Setup transaction count never changes
  the exactly-three physical-account invariant.

  No hooks, guards, flash loans, wrapper program, new on-chain program, new
  policy/event ledger, app database writes, direct projector repairs,
  dual-version worker, new mutation API, generic mint executor, or second
  projection owner. earn-max-v1 is historical and close-only.

  Exact and Max withdrawal intents remain distinct. Max resolves from actual
  confirmed post-unwind custody, never a stale snapshot, and cannot redeploy a
  remainder. Active positions receive periodic observations; stale or
  incomplete coverage is never displayed as zero.

  The projector ignores or quarantines foreign/malformed events without
  dropping the stream, is process-supervised, uses bounded overlap dedupe,
  persists canonical (slot, transaction index, instruction index) ordering,
  replays idempotently, and exposes liveness and lag. Page visibility and worker
  admission use the same fail-closed testing allowlist.

Verifier:
  op run --env-file=.env.1password -- bun run verify:earn-max:production

External gates:
  Mounted 1Password environments, current mainnet RPC/Jupiter/Kamino state,
  funded testing identities, Render/GitHub deployment access, and an exact
  transaction-specific approval before each mainnet write. The verifier is
  read-only and never signs, submits, deploys, restarts or repairs.

Verdict:
  PASS_EARN_MAX_THREE_POLICY_PRODUCTION_READY
  FAIL_EARN_MAX_THREE_POLICY_PRODUCTION_READY <first false condition and evidence>
  BLOCKED_EARN_MAX_THREE_POLICY_PRODUCTION_READY <dependency and resume condition>
```

`verify:earn-max:production` is the only product-readiness authority. Source,
byte, simulation, submission, deployment, reconciliation and live-behavior
checks are evidence rungs, not competing PASS conditions.

## Minimal architecture

```text
wallet-signed Solana transaction
             |
             v
existing confirmed LaserStream owner
             |
             v
earn_max_policy_sets + multiply_route_states
+ multiply_operations + multiply_position_snapshots
             |
             v
typed summary + activity GETs -> Earn MAX UI adapter
```

The application does not write policy, intent, deposit, claim or operation
facts to Neon. Solana is authoritative and the existing LaserStream path is the
only ingestion owner. Keep the existing tables, worker, cursor and two
authenticated read endpoints. A narrow migration may add canonical chain
position fields and change snapshot idempotency, but it must not add another
ledger, event bus, command table, scheduler, saga, outbox or repair table.

## Policy and execution contract

The three accounts contain the bounded seven-strategy terminal catalog above.
The approved compact KLend constraints use finite account allowlists and rely
on KLend for reserve/market and farm coherence. No account outside the catalog
is admitted. A same-market combination assembled from individually approved
positions is a known delegated-signer residual: mutation verification must
prove that it cannot leave approved vault custody, the canonical worker cannot
construct it, and reconciliation fails closed if obligation reserves differ
from the selected strategy.
Rust worker and TypeScript client implementations are independent and must
produce identical family order, seeds, PDAs, constraints and semantic hashes.

The KLend envelope is:

```text
ComputeBudget
RefreshReserve(collateral)       # top-level, permissionless
RefreshReserve(debt)             # top-level, permissionless
RefreshObligation               # top-level, permissionless
ExecuteProgramInteraction(
  one of the three policy PDAs,
  exactly one terminal instruction,
  exactly one constraint index
)
```

The policy pins every caller-controlled endpoint that can redirect value or
authority within the measured packet envelope. Stable Jupiter data fields are
constrained where the deployed policy format can express them. The worker additionally requires ExactIn, a fresh
quote, bounded slippage, zero platform fee, no setup/cleanup/ledger instruction,
and confirmed output reconciliation. Anything not statically enforceable is an
explicit delegated-signer residual risk covered by a testing AUM cap and a
prebuilt exit-only update of the same three accounts.

## Projection, accounting and application contract

Policy projection is `incomplete` after one or two exact accounts, `ready` only
after all three exact accounts, `incomplete` for any mismatch, and `removed`
only after all three are absent. Each binding carries family, seed, PDA,
semantic hash, live data hash, existence and match state. A ready set becomes
depositable only after the existing worker transactionally creates its route;
the summary exposes `routeReady` and projection freshness.

Withdrawal state stores `Exact(u64) | Max`. Exact claims the requested amount
and may redeploy a confirmed remainder. Max claims actual confirmed custody
after full unwind, requires zero obligation/collateral/debt/custody residue and
never redeploys.

Idle active routes are observed at a bounded cadence by the existing worker.
Snapshots are idempotent by chain observation rather than route generation.
Earned value is current equity plus confirmed claims minus confirmed deposits.
Realized APY uses cash-flow-neutral ordered returns or reports incomplete
coverage. Forecast components and freshness are explicit; missing or stale data
is never rendered as zero.

The deployed application keeps exactly two authenticated, write-free endpoints:

```text
GET /api/smart-accounts/earn-max/summary
GET /api/smart-accounts/earn-max/activity
```

Client builders create/install/close policies and create deposit, withdrawal,
cancel and claim transactions. After confirmed submission the client performs
bounded polling until the projected signature or canonical chain position is
visible. `earn-max-v1` policy sets are never resumed or updated as v2.

## Authoritative verifier order

The verifier stops at the first false condition:

1. Contract hash/version and forbidden-artifact inventory.
2. Exactly three semantic policy families, exactly seven strategy keys and
   exactly fourteen directed swap lanes;
   Rust/TypeScript parity; no six-account or two-account executable manifest.
3. Deployed Squads program and ProgramData identity, exact binary hash, and
   signed-unsent proof that compact PolicyCreate and PolicyUpdate deserialize.
4. Three exit-safe legacy creation packets and three full compact update packets
   below 1232 bytes and within constraint limits; update readback must prove the
   final full payload on the same three PDAs.
5. Canonical trace and mutation matrix for every KLend allowlist cross-product,
   omitted farm account, and directed Jupiter lane. Outside-catalog,
   cross-market and invalid-farm KLend tuples must reject. Any admitted
   same-market catalog cross-product must remain in exact approved custody and
   trigger the worker/reconciliation fail-closed checks. Every mutated Jupiter
   route must reject or be byte-and-economically inert with no endpoint
   diversion.
6. Current-chain signed-unsent simulation with fresh Settings, seeds, quotes,
   reserves, obligations, packet sizes and final signer/payer topology.
7. Confirmed install transitions `incomplete -> ready`, all required operation
   directions, user deposit/top-up/cancel/partial/Max lifecycle, reconciliation,
   policy removal, final zero and rent return.
8. Malformed-event survival, supervised projector health, canonical same-slot
   ordering, bounded dedupe and measured restart/replay idempotency.
9. Two fresh same-generation position observations, honest APY coverage,
   authenticated API truth, bounded frontend convergence and withdrawal SLA.
10. Exact immutable app and worker revisions/images deployed before the sole PASS.

Static compilation, an old lifecycle, an unsigned packet, a simulated signature,
an unauthenticated 401, deployment health alone, or a DB-only repair cannot
produce PASS.

## Implementation loop

Run the sole verifier once, fix its first useful failure without changing this
constraint set, rerun the affected cheap check, and run the complete verifier
against the exact deployed revisions and fresh confirmed lifecycle. Keep Earn
MAX fail-closed until that final PASS.
