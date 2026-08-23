# Kamino Multiply production-engine simplification

**Date:** 2026-08-22
**Status:** Production deployed. Trusted `main` published and probed the immutable light-workers image at commit `e6cc09c22e85c4813ab485f016b6ccb6881b10f8`; Render service `srv-da56asrncjis73fu9psg` runs that exact image in the light-workers production environment.

## Outcome contract

```text
Objective:
  Produce the smallest releasable one-vault RWA Multiply worker: the mainnet-
  proven engine is packaged in the immutable light-workers image, declared as
  one Render worker, uses only the constrained policy delegate online, drains
  safely on shutdown, serializes withdrawal demand, and automatically redeploys
  residual pooled capital after a claim.

Scope:
  One fixed pooled Squads vault, one route row, one serial worker, two data-only
  strategy configurations, one exact-policy readback path, confirmed commitment,
  immutable-image and declarative Render wiring, and the existing frontend view.
  Preserve route_policies. No loyal-app API or frontend endpoints, per-user vault
  provisioner, optimizer, reserve monitoring, partial withdrawal, hooks, guard,
  registry, flash loan, new program, SVM, fixtures, new orchestration tables, or
  manual repair commands.

Verifier:
  op run --env-file=.env.1password -- bun scripts/verify-multiply-deployment.ts

External gates:
  Terminal-only 1Password environment, Render API access, current mainnet RPC,
  and Neon are required for live readback. The verifier never deploys or sends
  a transaction.

Verdict:
  PASS_DEPLOYED_RWA_MULTIPLY_WORKER
  FAIL_DEPLOYED_RWA_MULTIPLY_WORKER <first false condition and evidence>
  BLOCKED_DEPLOYED_RWA_MULTIPLY_WORKER <dependency and resume condition>
```

The verifier is independent of the worker planner, builders, transition code,
and frontend mapper. It may invoke compilation and read current DB/RPC state,
but it never sends or repairs anything.

## What remains non-negotiable

The refactor must preserve the properties already proven on mainnet:

- exact Squads ProgramInteraction constraints and current policy hashes;
- literal KLend and Jupiter transactions rather than synthetic atomic actions;
- signed bytes persisted before broadcast intent;
- one send for one deterministic signature with `maxRetries: 0`;
- compiled transaction fee no greater than 20,000 lamports before signing;
- no resend after broadcast intent; query the stored signature;
- rebuild only after conclusive blockhash expiry and signature absence;
- confirmed custody, obligation, farm, and policy reload after every mutation;
- exact Token/Token-2022 program identity and ALT resolution;
- complete source close before target open;
- withdrawal debt paid only from reconciled collateral-swap proceeds;
- equal-and-opposite confirmed vault/user claim deltas; and
- claimability no later than 600 seconds after the request.

## The production data model

Keep the existing `multiply_route_states` table, but migrate its JSON to a
small schema-v4 current-state document. It contains only:

```text
vault and route identity
cycle and desired outcome
current confirmed position or idle custody
current withdrawal request
current operation id, if any
manual recovery reason
generation and observation time/slot
```

Add exactly one child table, `multiply_operations`. An operation is not an
abstract saga or outbox: it is one literal Solana transaction. It owns:

```text
operation id, route id, cycle, action, strategy key
status and idempotency key
exact expected raw token/obligation effects
policy account/hash and message hash
signed wire, deterministic signature, blockhash expiry
broadcast intent, confirmed slot, reconciliation evidence
created/updated timestamps
```

The confirmed wallet-originated deposit is also one operation row. Because the
worker did not broadcast it, that row stores the observed signed-wire/message
hashes, signature, blockhash, confirmed slot, and equal wallet/vault deltas,
but truthfully has no policy binding, broadcast intent, or blockheight expiry.
Its globally unique signature prevents replay admission. A cycle begins at
deposit and spans deploy, strategy moves, unwind, and claim.

Enforce one nonterminal operation per route with a partial unique index. Move
completed operations to terminal rows instead of embedding an ever-growing
receipt array in the route JSON. Do not add any other Multiply table.

Raw token amounts and USD values are different types. Custody snapshots carry
raw amounts only. Optional USD values require explicit oracle/NAV evidence;
code must never copy a raw amount into a USD field.

## The production engine

The worker loop is one boring pipeline:

```text
lease route
-> recover the one current operation first
-> observe confirmed chain state
-> next_action(intent, snapshot)
-> prepare one operation row
-> build(action, strategy, amount)
-> ensure_exact_policy(action, built transaction)
-> simulate exact signed bytes
-> persist signed wire
-> persist broadcast intent
-> send once
-> reconcile confirmed effects
-> update current route state
-> repeat until Waiting, Blocked, ManualRecovery, or Complete
```

`next_action` is the only planner. It accepts current intent, current confirmed
snapshot, and current strategy configuration and returns one literal action:

```text
swap_claim_to_collateral
deposit_collateral
borrow_debt
swap_debt_to_collateral
withdraw_collateral
swap_collateral_to_debt
repay_debt
swap_collateral_to_claim
claim
```

Repeated borrow/swap/deposit and withdraw/swap/repay trenches reuse those same
actions. Source and target do not receive separate prepare/reconcile functions.
Amounts are derived from the deposit, desired leverage, current health, current
debt, and current quote; the production engine contains no `40/20/16/4` or
`843/20` canary schedule.

Two `StrategyConfig` values contain only facts that differ between the proven
syrupUSDC/USDC and syrupUSDC/PYUSD topologies: reserve, debt mint/program,
obligation, farm identities, custodies, and policy accounts. Instruction
builders use SDK/ABI builders plus explicit semantic assertions. Historical
Jupiter instruction bytes and ALTs are not production configuration.

Fresh packet measurement disproved the proposed combined Jupiter policy: it was
2,019 bytes and cannot fit Solana's 1,232-byte transaction limit. Keep the
smallest stable topology instead: one hookless policy per KLend primitive and
one per Jupiter direction. Each Jupiter policy pins the vault, source and
destination custodies, mint order, token-program identity, and
SharedAccountsRoute discriminator while leaving Jupiter's dynamic AMM tail
flexible. The worker separately requires ExactIn, one to four quote legs, 50
bps requested slippage, zero platform fee, no setup/cleanup/ledger/extra
instructions, the fresh quote threshold, resolved confirmed ALTs, and a signed
packet no larger than 1,232 bytes. It requests at most 32 inner-swap accounts
because the Squads wrapper occupies the rest of the packet. A failed signed
simulation is never broadcast; the prepared intent is canceled and rebuilt
from a fresh route. The worker verifies the deployed semantic contract before
each send and never mutates policy per operation. `repair-*` commands and
human rerun instructions are forbidden from the production CLI.

KLend instructions come from `klend-interface`, including dynamic
RefreshObligation reserve tails. Debt-aware unwind uses KLend's `u64::MAX`
maximum-safe withdrawal sentinel rather than a guessed percentage, then swaps
the confirmed proceeds and repays only what custody actually holds. A 50,000
raw debt-unit floor repays uneconomic swap dust; above that floor the 10 bps
value tolerance remains authoritative.

## Runtime and frontend boundaries

The worker binary only parses `run`, `deposit`, `move`, `withdraw`, `claim`, and
`status`; it delegates to the engine and exits successfully for ordinary
`Progressed`, `Waiting`, or `Complete` outcomes. `BLOCKED` is reserved for an
external prerequisite. `FAIL` is invariant or reconciliation failure.

The frontend view is derived from schema-v4 route state plus the current
operation status. It exposes stable business state, confirmed balances and
position metrics, withdrawal timing, and manual recovery. It never exposes
policy bytes, signed wires, internal action indexes, or repair instructions.

The named **Linus simplicity rubric** is a regression fence, not a claim that
line count alone makes code good: the CLI wiring is at most 250 nonblank lines,
the persisted Multiply domain is at most 1,000 nonblank lines, the production
engine modules total at most 3,500 nonblank lines, and no engine module exceeds
900 nonblank lines. Passing also requires every structural deletion and live
behavior check below, so splitting or minifying code cannot manufacture PASS.

## Authoritative verifier checks

The verifier stops on the first false condition in this order:

1. **Preflight:** expected source tree, terminal-only environment, confirmed
   mainnet RPC identity, Neon schema/version, disposable vault, and current
   policy inventory are present. Missing transaction authority is reported
   only when fresh lifecycle evidence is required.
2. **Architecture deletion:** production code contains no repair command,
   canary tranche schedule, historical Jupiter bytes/ALT, optimizer, capacity
   reservation, conflict lease, quote fence, embedded submission history, or
   raw-to-USD assignment. The CLI is thin and the verifier does not import the
   worker planner/build/transition modules.
3. **One real state owner:** one schema-v4 route row per vault and exactly one
   `multiply_operations` child table; no decision, projection, outbox, policy
   event, or second orchestration table exists.
4. **One operation pipeline:** one unique nonterminal operation per route;
   every terminal receipt proves persisted wire hash, deterministic signature,
   policy/message binding, confirmed slot, and reconciliation hash.
5. **Automatic policy binding:** current-cycle operation history contains no
   operator repair step; every dynamic Jupiter operation binds the current
   policy hash and exact fresh route used by its signed transaction.
6. **Generic two-strategy behavior:** one `linus_v1` cycle uses the same action
   engine to open syrupUSDC/USDC, close to safe idle, then open the materially
   different Token-2022 PYUSD strategy. Strategy-specific code is data only.
7. **User-capital Down:** reconciled collateral-to-debt output covers each
   following repay, debt reaches zero, and remaining collateral becomes the
   configured claim mint without donated debt custody.
8. **Crash recovery:** no deterministic signature lands twice and no operation
   is broadcast without prior persisted intent. If an expiry occurred, it must
   be followed by a distinct reconciled retry; a release does not manufacture
   an expiry merely to make the verifier green.
9. **Claim contract:** a current-cycle confirmed deposit and claim have exact
   opposite vault/user deltas, and unwind plus claimability complete within
   600 seconds of the request.
10. **Frontend truth:** the DTO generation matches the route generation and
    confirmed observation; unknown values are null, never fabricated zero.
11. **Least privilege:** the always-on runtime loads only `POLICY_KEYPAIR`; it
    uses that constrained delegate as fee payer and never reads the stronger
    `SOLANA_TESTING_PK` setup authority. Every compiled message is rejected
    before signing if mainnet reports a fee above 20,000 lamports.
12. **Release topology:** `multiply-route-worker` is compiled, copied, and
    probed in the linux/amd64 light-workers image, and one disabled-by-default
    production Render worker declares migration predeploy, immutable image,
    confirmed RPC, Neon, policy delegate, and observability inputs.
13. **Operational lifecycle:** the image probe performs no network, secret,
    database, or transaction work; SIGTERM stops new leases after the current
    tick; a pending withdrawal cannot be overwritten; after a successful claim,
    residual pooled USDC is automatically redeployed to the prior target.

Only current `linus_v1` operation rows can prove checks 4–10. The previous
canary history is migration evidence, not release evidence.

## Implementation order

1. Extend this existing independent verifier with checks 11-13 and record its
   first cheap failure before changing runtime or deployment code.
2. Remove `SOLANA_TESTING_PK` from the Multiply runtime and add a secret-free,
   network-free role probe for the exact worker entrypoint.
3. Make withdrawal admission idempotent for an identical active request and
   reject replacement demand until the current request is claimed. After claim,
   retain the claimed receipt while setting the existing move goal when pooled
   USDC remains; do not add a queue or lifecycle table.
4. Install shutdown handling once at startup. A signal prevents the next tick,
   while the current tick keeps its persisted-wire/reconciliation contract and
   finishes before exit.
5. Add the binary to the light-workers build inventory, runtime image, and
   linux/amd64 workflow probes. Add one production Render worker using the same
   immutable image family, migration runner, and private registry convention.
6. Run the sole verifier through the mounted 1Password environment. It performs
   cheap source checks, the secret-free role probe, targeted compilation, and
   current confirmed mainnet/Neon reconciliation in that order.

## Confirmed production evidence

Cycle 3 admitted a confirmed wallet deposit, opened syrupUSDC/USDC, closed it
fully before opening syrupUSDC/PYUSD, reached the target strategy, then handled
withdrawal request `linus-v1-withdraw-20260822`. The request was confirmed as
claimed in about 335 seconds, with both obligations at zero and 1,784,538 raw
USDC remaining in vault custody after the exact 1,000 raw USDC claim.

The authoritative verifier reported
`PASS_RWA_MULTIPLY_PRODUCTION_ENGINE`: 176 recorded operations, 175 reconciled
transactions, one conclusively expired unsigned retry path, exact source-close
before target-open ordering, and a confirmed claim signature.

## Stop rule

Do not preserve a layer merely because the canary used it. Preserve only chain
safety properties and current evidence. On ambiguous send, unexpected delta,
unsafe withdrawal, policy authority drift, or unavailable transaction authority,
return `BLOCKED` or `ManualRecovery` with the exact resume condition. Never
weaken the verifier to make the refactor pass.
