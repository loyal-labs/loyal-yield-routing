# Backyard Voltr four-market router verifier and implementation plan

Status: active verifier v1. The one-vault/four-strategy graph and eight exact
runtime policies are installed; the fresh source-bound confirmed lifecycle and
durable withdrawal-restoration handoff remain the final proof boundary. The
verifier stays `FAIL` until every required artifact below is produced by the
maintained commands and independently reconciled.

This document extends, but does not rewrite, the completed single-Main
600-second proof in `backyard-voltr-partner-vault-verifier.md`. Historical Main
evidence remains valid for what it proved, but cannot satisfy this verifier.

## Fixed product decisions

- Backyard integrates one Voltr USDC vault and one LP mint.
- The withdrawal waiting period is exactly `600` seconds. Voltr permits its
  vault-level instant-withdraw instruction only when this value is `0`, so this
  route is request/claim-only. The separate
  locked-profit degradation period may remain `86_400` seconds.
- The normal Earn optimization interval is exactly `86_400` seconds per vault.
  This daily timer is independent of the 600-second withdrawal setting and never
  delays receipt scanning, liquidity restoration, or recovery of an in-flight
  movement.
- The Voltr manager is the Loyal Squads vault PDA. The delegated guardian is the
  only normal manager-operation signer.
- User deposits and user withdrawals remain user-signed Voltr operations.
  Squads controls only movement between Voltr idle USDC and a Voltr strategy.
- The exact approved Kamino USDC reserves are:

  | Strategy id | Reserve |
  | --- | --- |
  | `main` | `D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59` |
  | `prime` | `9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu` |
  | `onre` | `AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z` |
  | `maple` | `Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo` |

- POC limits remain a `10_000_000` raw-USDC vault cap, a `1_000_000`
  raw-USDC manager-operation cap, and zero fees.
- Confirmed commitment is sufficient for the POC and partner-validation loop.
  A signature alone is not: each transition needs a successful confirmed
  transaction plus an account read at `contextSlot >= transaction slot`.
- No new Solana program is part of this plan.

## Architecture decision

The public manager surface stays deliberately small:

```text
deposit(strategyId, amountRaw)
withdraw(strategyId, amountRaw)
```

Underneath it, use eight permanent Squads ProgramInteraction policies: one
deposit policy and one withdrawal policy for each strategy. Do not make policy
count a product concern.

The existing security-critical single-route policy-create packet is already
`1,221` bytes against Solana's `1,232`-byte packet limit. Four independent
instruction alternatives cannot fit in one policy. Combining reserve, market,
farm, receipt, and obligation allowlists independently would instead authorize
Cartesian mixed graphs. Therefore two physical policies are rejected as an
implementation target. Keep one exact route and one direction per policy.

There are only four runtime concepts:

1. one canonical route catalog;
2. eight active approved policy accounts whose bytes are rechecked before use;
3. one exact Voltr manager executor;
4. one receipt scanner that gives withdrawal liquidity priority over the Earn
   optimizer.

## Enforcement boundary

Be precise about what Squads can and cannot express.

- Each on-chain policy pins the exact Voltr program, manager, vault, strategy,
  adaptor and strategy receipts, authorities, USDC mint/accounts, reserve,
  lending market, obligation, Token Program, Farms program, K-Lend program,
  operation discriminator, adaptor discriminator, `additionalArgs = null`, and
  `0 < amountRaw <= 1_000_000` wherever those values occupy the deployed
  security-critical policy indexes.
- Deposit policy indexes remain
  `0,1,2,3,4,5,6,7,8,10,11,12,13,14,15,17,21,29,30`.
- Withdrawal policy indexes remain
  `0,1,2,3,4,5,6,7,8,9,11,12,13,14,15,17,21,26,27`.
- The complete SDK-built account vector, account order, signer/writable roles,
  data length, lookup table use, and wrapper shape are also pinned by the local
  canonical manifest and pre-send verifier.
- Every constrained policy index must be exact. Squads does not constrain the
  omitted indexes: each omission must be enumerated and justified by a named
  Voltr/Kamino on-chain validation boundary plus mutation evidence before the
  policy gate can pass.
- Keep a machine-readable inventory of every account index omitted from the
  on-chain policy. For every omitted writable, token-destination, or value-bearing
  account, require independent evidence that the pinned Voltr/Kamino program
  derives or validates it against the constrained graph, plus named canonical
  mutation simulations. A finite mutation suite is not proof against every
  possible redirect. If the validation boundary cannot be established, or any
  named mutation succeeds, the no-custom-program design fails this verifier; do
  not paper over it with a local check.
- Squads SpendingLimit is not the capital cap because the asset source belongs
  to Voltr, not the Squads PDA. The instruction amount bound and Voltr vault cap
  are the relevant on-chain limits.

## Verifier prompt

Run this section cold against the checked-out repository, confirmed mainnet
state, the exact route catalog, and the supplied evidence manifest. Act as a
skeptic. Return a named PASS/FAIL result for every required gate and the smallest
next experiment for every failure. Do not infer a pass from source code,
simulation success, or a returned signature.

### Required gate 1 — one vault, four exact strategies

- Decode the current Voltr vault and require the exact vault, LP mint, USDC mint,
  admin/pending-admin, Squads manager PDA, zero-fee configuration,
  `allowAnyAdaptor = 0`, `maxCap = 10_000_000`, and
  `withdrawalWaitingPeriod = 600`.
- Decode exactly four active native-Kamino strategy receipts for the four reserve
  addresses above. For each one, independently bind the reserve to its current
  lending market, market authority, farm, reserve-owned token accounts, Scope,
  user metadata, obligation, obligation farm, Voltr strategy authority, strategy
  USDC ATA, adaptor receipt, and strategy receipt.
- Require classic USDC and the approved Voltr, native adaptor, K-Lend, Farms,
  Token, Associated Token, System, and Squads executable identities. Deployment
  drift or an inactive/hidden/wrong-mint reserve is `FAIL`.
- Require one canonical base route-spec hash covering the vault, 600-second
  config, 86,400-second normal optimization interval, idle floor, four complete
  strategy graphs, limits, programs, and lookup table. Separately require one
  effective route-authorization digest covering that base hash, the exact
  eight-policy catalog bytes and semantics, its strict approval envelope, and
  every authorization-bound source hash. The base route hash alone never
  authorizes a policy catalog. A singular `strategy: main` route cannot pass.

### Required gate 2 — authority and eight-policy surface

- Decode Squads Settings and manager PDA. Require the exact guardian,
  permissions mask `7`, threshold `1`, vault index, zero policy timelock, no
  unexpected expiry, and the expected policy rent collector.
- Require exactly one active deposit and one active withdrawal policy for each
  of `main`, `prime`, `onre`, and `maple`. Setup-only policies must be absent.
- Independently decode every policy account and its creation payload. Bind each
  policy's seed, PDA, direction, strategy id, full creation-artifact SHA-256,
  create-data SHA-256, route-spec SHA-256, guardian, account constraints, and
  data constraints before any send.
- After strategy initialization and before the first policy send, take one
  confirmed Settings snapshot and deterministically precompute the complete
  expected eight-policy catalog: retained Main pair plus the six contiguous
  next seeds, PDAs, canonical creation artifacts, and hashes. Freeze that catalog
  and strict approval envelope, derive one effective route-authorization digest,
  and authorize it once. Before every policy send, re-read Settings and require
  the next seed to equal the frozen expected seed. Concurrent seed consumption
  invalidates the whole unsent suffix and requires a newly generated catalog,
  approval envelope, and effective route-authorization digest; neither the base
  route hash nor effective digest is silently rewritten after installation.
- Require one Voltr instruction constraint per policy. Policy seeds must be the
  exact sequential seeds actually consumed from a fresh confirmed Settings
  read; no hard-coded stale seed is accepted.
- Valid bounded deposit and withdrawal wrappers for all four strategies must
  simulate successfully. Mutations of guardian, manager, vault, strategy,
  reserve, market, farm, receipt, obligation, mint, program, account order,
  account role, discriminator, adaptor tail, zero amount, over-limit amount,
  mixed graph, extra instruction, or reordered instruction must be rejected by
  the named enforcement layer.
- The verifier must enumerate every unconstrained account index and the exact
  validation evidence/mutation cases for it. An unexamined omitted writable or
  value-bearing account is `FAIL`, even when the listed examples reject.

### Required gate 3 — exact logical manager API

- `deposit(strategyId, amountRaw)` and `withdraw(strategyId, amountRaw)` accept
  only the four strategy ids and amounts in `1..=1_000_000`.
- Resolution selects exactly one matching physical policy and one canonical
  SDK-derived graph. No caller may provide a policy PDA, reserve graph, remaining
  accounts, program id, or instruction bytes.
- The compiled Squads wrapper has exactly one guardian signer, the expected
  manager PDA, policy PDA, account table, inner program, inner account indexes,
  and inner data. It fits packet, compute, heap, call-depth, and lookup-table
  limits.
- A confirmed manager deposit decreases Voltr idle and increases only the named
  Kamino strategy. A confirmed manager withdrawal performs the inverse. Preserve
  requested and actual redeemed raw amounts separately and reconcile protocol
  rounding from transaction metadata.

### Required gate 4 — user deposit and withdrawal surface

- User deposit remains the canonical SDK instruction, signed only by the user.
  It moves the exact USDC amount into Voltr idle and mints the exact LP amount.
- Withdrawal quote uses the pinned Voltr SDK bigint math and a confirmed vault,
  LP-supply, locked-profit, fee, and idle snapshot. It returns raw asset amount,
  idle available, `instantAvailable = false`, and a claim time 600 seconds after
  the request.
- Build the canonical one-instruction `instantWithdrawVault` packet only as a
  no-broadcast compatibility probe. Even with sufficient idle, it must fail
  with exact Voltr `Custom 6015 / InstantWithdrawNotAllowed` while the decoded
  waiting period remains `600` and `disabledOperations = 0`. Its expected
  signature must never land. An admin update to waiting period `0` is diagnostic
  evidence only and is forbidden from the accepted lifecycle.
- Build the canonical request transaction for every withdrawal. The confirmed
  `RequestWithdrawVaultEvent`, receipt PDA, and receipt bytes must agree on user,
  vault, LP escrow, fixed-point asset quote, and
  `withdrawableFromTs - requestedTs = 600` exactly.
- Claim remains user-signed and is rejected before the deadline. At or after the
  deadline it pays the effective request quote, burns the escrowed LP, and
  closes or empties the expected request state with no unrelated movement.

### Required gate 5 — withdrawal-liquidity monitor

- The POC source of truth is a confirmed scan of Voltr
  `RequestWithdrawVaultReceipt` accounts filtered by discriminator and exact
  vault. Use the pinned SDK decoder and bigint math; do not convert raw balances
  or U80F48 values through JavaScript `number`.
- Poll every few seconds and again on process start. The 600-second live receipt
  is the active on-chain demand record, so a websocket, LaserStream notification,
  or Backyard callback may only wake a scan; it cannot create authoritative
  demand or replace a fresh account scan.
- Persist a request identity by `(request signature, event index)` when the
  origin is known. Otherwise use a generation fingerprint over receipt PDA,
  user, LP amount, fixed-point quote bits, deadline, bump, and version. A receipt
  PDA is not globally unique across time because the same vault/user PDA can be
  recreated after close. The current scanner reports an `observedContextSlot`
  for each confirmed scan; it does not claim that slot is the first-ever
  observation. Durable generation identity, request-origin/event-index binding,
  restart dedupe, and outbox persistence remain unproven until the existing
  orchestration boundary is wired in.
- Compute the vault-level target as:

  ```text
  requiredIdleRaw = configuredIdleFloorRaw
                  + sum(active request upper-bound quoteRaw)
  shortfallRaw = max(0, requiredIdleRaw - confirmedIdleRaw)
  ```

  For receipt fixed-point bits, calculate the conservative per-request value as
  `requestUpperBoundRaw = (bits + (1n << 48n) - 1n) >> 48n` and use that value in
  the sum. Flooring with `bits >> 48n` is not an acceptable liquidity target.
  The eventual claim verifier still reconciles the exact effective amount
  actually paid.

- If `shortfallRaw > 0`, withdrawal restoration outranks cooldown, active Earn
  decisions, and new allocation. The worker acquires the same vault/account
  conflict lease used by routing, re-reads receipts/idle/positions, and creates
  bounded strategy-withdraw legs only.
- Choose funded sources deterministically from the four approved strategies:
  lowest current net yield/lowest unwind cost first, then largest safely
  redeemable amount, with a stable strategy-id tie-break. Chunk every leg to the
  `1_000_000` raw cap.
- Persist signed restoration intent and expected signature in the existing
  orchestration outbox/execution record before broadcast. Reuse its dedupe key,
  lease/fencing, confirmation, and reconciliation contracts; an ad-hoc local
  file or a second scheduler cannot satisfy this gate.
- After each one-send transaction, require successful confirmed status and a
  `minContextSlot >= confirmedSlot` read. Recompute shortfall from actual idle;
  do not assume requested amount was returned. Stop when shortfall is zero or no
  approved redeemable position remains.
- Cancellation, claim, disappearance, restart, duplicate scan, lost RPC response,
  and a confirmed fork rollback must not cause a blind resend. If a request
  disappears, stop new legs; any harmless excess idle stays in Voltr and may be
  reallocated only after no active demand remains.

### Required gate 6 — reuse the Earn router, not its direct executor

- Main/Prime/OnRe/Maple observations come from the existing shared market catalog
  and confirmed reserve-observation pipeline. There is no second APY or reserve
  data source in `tools/backyard-voltr`.
- The route configuration fixes the normal optimization interval at `86_400`
  seconds. A timer tick may create at most one new economic movement for the
  vault; withdrawal restoration and recovery bypass this timer without creating
  a competing optimizer decision.
- Reuse the existing freshness, risk eligibility, capacity, net-yield,
  transaction-cost, hysteresis, cooldown, priority, lease/fencing,
  confirmation, and reconciliation semantics.
- Add a thin Voltr route adapter. The existing direct-Kamino executor must never
  execute a Backyard decision because Backyard positions are owned through
  Voltr strategy authorities and receipts.
- A normal rebalance is one durable movement with two separately confirmed and
  reconciled legs:

  ```text
  source Voltr strategy -> Voltr idle -> destination Voltr strategy
  ```

- The destination deposit may begin only after the source withdrawal's actual
  idle effect is confirmed and reconciled. Restart recovery continues the
  existing movement; it never creates a second decision or skips an ambiguous
  leg.
- Optimizer investable idle is
  `max(0, idle - configured floor - active withdrawal demand)`. A kill switch
  stops new allocations but must not disable recovery of already-broadcast
  movements or withdrawal-liquidity restoration.

### Required gate 7 — confirmed four-market mainnet lifecycle

One coherent evidence manifest must bind the same vault, LP mint, route-spec
hash, four strategies, eight policies, guardian, testing user, commitment, and
program identities. With tiny amounts it proves:

1. one user USDC deposit into the Voltr vault;
2. idle-to-Main manager deposit;
3. Main withdrawal to idle, then idle-to-OnRe deposit;
4. OnRe withdrawal to idle, then idle-to-Prime deposit;
5. Prime withdrawal to idle, then idle-to-Maple deposit;
6. Maple withdrawal back to idle or Main;
7. a canonical instant-withdraw simulation that fails with exact 6015 under the
   unchanged 600-second configuration, even while idle covers its quote;
8. a user request while idle is deliberately insufficient;
9. automatic receipt discovery and one explicitly named, policy-verified
   `managerMainRestorationWithdraw` lifecycle transaction that restores the
   exact scanned shortfall (the general worker may select/chunk any approved
   funded strategy, but this tiny proof deliberately makes Main sufficient);
10. exact pre-deadline claim rejection and successful claim at or after 600
    seconds; and
11. wrong-route, mixed-graph, wrong-signer, zero, and over-limit policy
    simulations that fail without broadcast.

Every successful step requires the confirmed transaction, successful metadata,
exact signer/program/account/data shape, token and lamport deltas, and a
post-state read at or after its slot. The final read must reconcile Voltr idle,
all four strategy positions, LP supply, active receipts, vault total value, and
no unexplained token destination.

The manifest contains thirteen unique confirmed transactions plus seven exact
proof artifacts. Every transaction retains the exact ordered 42-account
pre/post images, recomputable row/data/state hashes, a user-or-guardian
pre-send Ed25519 attestation persisted before send, and a linked settlement
attestation. Its protected-state chain is literal and gap-free: request
poststate bytes equal the named restoration-withdrawal prestate bytes, and
restoration poststate bytes equal claim prestate bytes. A restoration artifact
may reference that exact transaction; it may not introduce an additional
hidden send between two manifest checkpoints.

This is signer-attested evidence of confirmed-provider account observations,
not independent historical account replay: ordinary RPC transaction metadata
cannot reconstruct all 42 account images at past slots. Exact transaction
metadata remains the independent proof of landed signers, instructions, token
rows, lamport rows, and events.

The fallback path must additionally bind the exact request-origin signature and
event index to the receipt generation that triggered restoration. If origin
recovery is unavailable, bind the generation fingerprint defined in gate 5 and
prove that its complete decoded receipt bytes match the scanned demand before
the first restoration leg.

### Required gate 8 — execution safety and partner boundary

- Mainnet genesis, signer identity, route hash, artifact hash, amount, policy,
  deployment identities, protected prestate, fee/rent ceiling, fresh blockhash,
  and simulation are checked before signer use or send.
- Each signed transaction is persisted before broadcast, sent at most once with
  transport retries disabled, and recovered only by its precomputed signature.
- Secrets are loaded through the mounted 1Password environment and never enter
  source, artifacts, command arguments, logs, or chat.
- The current testing admin may remain only while the vault cap is 10 USDC and
  partner validation is explicitly labeled POC. It is not used by the runtime
  router. Raising caps requires a separate production verifier and transfer of
  admin authority to approved governance.
- Backyard needs only the vault, LP mint, deposit builder, withdrawal quote,
  request/claim builders, and status. Backyard never signs Squads or
  Kamino instructions and never chooses arbitrary strategy accounts.

## Nice-to-have gates — not required for POC PASS

- Repeat the coherent lifecycle at finalized commitment.
- Add a LaserStream transaction wakeup while retaining receipt scans as truth.
- Deploy the monitor/executor as pinned Render worker images with alerts and
  service-level objectives.
- Transfer Voltr admin to a separate higher-threshold Squads governance account,
  use a production signer boundary, and raise caps only under a new approval.
- Support concurrent withdrawal requests with batched restoration economics.

## Verdict and evidence format

The future literal verifier command is:

```sh
cd tools/backyard-voltr
bun run check
bun run verify:structure
bun src/cli.ts verify four-market \
  --commitment confirmed \
  --evidence ../../docs/evidence/backyard-voltr-four-market/confirmed-lifecycle-v1.json
```

Policy/action changes also run the repository's existing proof surface after the
live fail-fast probe:

```sh
bun run test:squads
bun run test:squads:e2e
cargo check -p loyal-actions -p loyal-yield-orchestrator -p loyal-yield-store
```

Do not add broad mock-heavy tests. Add only the smallest proof-surface checks
needed to protect policy bytes, canonical action construction, mutation
rejection, and persisted queue contracts that could compile while wrong.

The verifier writes JSON with the route hash, evidence hash, commitment, named
gates, observed/expected values, smallest next experiment for failures, and
`failedGateCount`. Overall verdict is
`BACKYARD_VOLTR_FOUR_MARKET_CONFIRMED_PASS` only when every Required gate passes.
Nice-to-have failures are reported separately and do not change the POC verdict.

## Current baseline verdict

Expected result today: `FAIL` until the fresh lifecycle manifest is complete.

- The maintained RouteSpec contains all four exact reserve graphs and the live
  vault has eight active deposit/withdraw policies.
- The confirmed receipt scanner, liquidity-priority planner, durable restoration
  bridge, and shared Earn replay adapter are implemented but still need one
  coherent source-frozen mainnet evidence chain.
- Live simulation proved the 600-second route rejects the canonical instant
  packet with 6015. The verifier now treats that as the required compatibility
  result instead of asking for an impossible successful transaction.
- The direct Earn executor still does not own Backyard positions; the thin Voltr
  manager adapter is the only accepted execution boundary.

## Fail-fast implementation plan

### Chunk 0 — live compatibility matrix, no writes

Start with OnRe, then Prime, then Maple; Main is the known-good control.

1. Across one monotonic confirmed context chain—reserve batch, support state,
   ALT, deployments, then blockhash—require every later read to be at or after
   the prior slot. Decode all four reserve accounts and resolve exact current
   lending market, authority, farm, reserve vaults/mints, Scope, status,
   liquidity mint, and deployment fingerprints. This is not represented as a
   single-bank snapshot.
2. Reject a route immediately if it is inactive, hidden, non-USDC, unsupported
   by the native Voltr adaptor, or has an unrepresentable account graph.
3. Use the pinned Voltr SDK to generate initialize/deposit/withdraw instructions
   and full canonical graphs for all four strategies.
4. Measure bare and Squads-wrapped packet bytes, ALT coverage, compute, heap, and
   simulation behavior. Produce a four-row compatibility artifact.
5. Produce one compile-only artifact showing that a four-alternative policy
   exceeds packet size; then stop exploring the two-policy design and proceed
   with eight.
6. Before RPC access, require the operator-confirmed SHA-256 of a separate,
   strict approval envelope. It must bind the exact route/catalog hashes,
   baseline policy artifact, and all verifier, builder, decoder, wrapper, and
   narrow Rust policy-compiler sources. Source drift invalidates the approval.

Exit: all four exact graphs are representable, or the plan stops before any
router/database work with the first concrete incompatibility.

### Chunk 1 — one catalog and generic builders

1. Replace singular `strategy` with a closed `strategies` catalog and stable
   `main|prime|onre|maple` ids.
2. Generalize reserve loading, Voltr account derivation, setup, manager builders,
   policy manifests, runtime intents, and verifiers to take `strategyId`—never a
   caller-supplied graph.
3. Keep one base route hash and one full graph fingerprint per strategy. Bind
   every manager execution intent to those semantics and to the separately
   derived effective route-authorization digest for the exact policy catalog.
4. Add exact bigint withdrawal quote construction and a no-broadcast canonical
   `instantWithdrawVault` rejection probe for the 600-second compatibility gate.

Exit: unsigned canonical artifacts regenerate deterministically for every
strategy and all mutation gates fail before signer loading.

### Chunk 2 — initialize the three additional strategies

Reuse the existing atomic setup pattern once per strategy:

```text
set manager to exact setup admin
initialize one exact strategy with zero funds
restore manager to exact Squads PDA
```

All three instructions are in one transaction, so failure rolls the manager
change back. Confirm and read back OnRe before Prime, and Prime before Maple.
Create/verify each strategy-owned USDC ATA separately. No temporary runtime
initialize policy remains active.

Exit: four strategy receipts and four zero/known positions exist, and manager is
the Squads PDA after every transaction.

### Chunk 3 — install the six missing runtime policies

1. Re-verify the existing Main pair rather than recreating it if its exact bytes
   and deployments still match.
2. From one confirmed Settings snapshot, precompute the six contiguous expected
   seeds/PDAs/artifacts, freeze the full eight-policy catalog and approval
   envelope, derive the effective route-authorization digest, then obtain one
   exact authorization for that frozen surface.
3. Before each new policy, require confirmed Settings to expose that policy's
   precomputed next seed; any mismatch invalidates the unsent catalog.
4. Compile, independently decode, hash-bind, simulate, send once, confirm, and
   read back one policy at a time.
5. Install deposit/withdraw pairs for OnRe, Prime, and Maple. Require the
   resulting catalog to match the frozen catalog exactly and retain the unchanged
   base route hash and effective route-authorization digest.
6. Run valid wrappers and the complete mutation matrix for each pair.

Exit: exactly eight permanent policies pass Required gate 2.

### Chunk 4 — prove the two logical manager methods

Use tiny mainnet amounts to call deposit/withdraw for each strategy independently.
Reconcile actual idle and strategy effects at confirmed commitment. Do not build
the optimizer yet. This isolates Voltr/Kamino/policy failures from scheduling
failures.

Exit: all eight manager operations have successful confirmed evidence and exact
post-state reconciliation.

### Chunk 5 — prove the request-only user withdrawal contract

1. Add a public-safe quote and exact user transaction builders.
2. Prove the canonical instant packet fails with exact 6015 while wait remains
   600 seconds, and independently prove its expected signature never landed.
3. Prove a 600-second request, pre-deadline rejection, and post-deadline claim
   manually while the manager executor is known-good.

Exit: user behavior is proven independently of monitoring.

### Chunk 6 — add the smallest automatic liquidity restorer

1. Add a confirmed receipt scan using the SDK discriminator/vault memcmp filters.
   It must use bounded `minContextSlot` retries and fail closed unless the
   receipt and idle reads align to the same confirmed slot. It reports an
   observation slot, not a durable first-observed slot.
2. Reuse existing one-send confirmation/reconciliation and account-conflict
   semantics. Persist the signed intent/expected signature through the existing
   orchestration outbox and execution-record boundary. For the POC, one local
   worker process is enough; do not introduce LaserStream, an ad-hoc file, or a
   new general scheduler.
3. Put the withdrawal guard before optimizer active-decision and cooldown checks.
4. Restore aggregate idle shortfall with bounded per-strategy withdrawals,
   re-reading state after every leg.
5. Crash/restart the worker during a proof and show it recovers the exact pending
   signature or recomputes from chain without double-withdrawing.

Exit: a confirmed request with insufficient idle automatically becomes
claimable liquidity without operator selection of a strategy.

### Chunk 7 — connect the existing Earn planner through a thin adapter

1. Reuse shared catalog observations and economic planning; filter the candidate
   universe to the four route-spec reserves.
2. Add a Voltr execution kind that resolves the planner's source/destination to
   the two logical manager methods. Do not call the direct-Kamino executor.
3. Persist one movement identity across source withdrawal, idle custody, and
   destination deposit. Advance only after confirmed reconciliation.
4. Apply idle reservations and withdrawal priority before normal allocation.
5. Fix the normal planner interval at 86,400 seconds while keeping receipt scans
   and movement recovery event-driven/fast.
6. First run dry, then execute one tiny Main -> OnRe -> Prime -> Maple cycle.

Exit: the existing Earn decision code chooses the opportunity, while the Voltr
adapter alone constructs and executes custody movement.

### Chunk 8 — run and package the coherent verifier

Run the complete Required gate 7 lifecycle against one immutable base RouteSpec
hash and its derived effective route-authorization digest; the base hash alone
never authorizes the policy catalog or lifecycle evidence. Use one evidence
manifest and preserve every confirmed signature, slot, exact transaction/meta,
post-state read, artifact hash, and negative simulation. Hand Backyard the one
vault/LP configuration plus user builders and the verifier result.

Only after `BACKYARD_VOLTR_FOUR_MARKET_CONFIRMED_PASS` should work begin on
production admin transfer, higher caps, finalized repetition, Render deployment,
or richer monitoring.
