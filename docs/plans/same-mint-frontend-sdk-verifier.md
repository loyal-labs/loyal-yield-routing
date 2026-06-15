# Same-Mint Frontend SDK Verifier

Use this document as the fixed verifier for porting the proven same-mint fleet
flow into the sibling `loyal-apps` SDK and Loyal web frontend. Do not treat it
as an implementation checklist. The work passes only when a skeptical runner can
verify every required condition below from sibling app code, this repo's Rust
oracle, the running frontend server, Neon control-plane rows, Timescale reserve
data, Solana chain state, and orchestrator logs.

Overall PASS requires an explicitly approved live mainnet E2E. Dry-run output is
required as a safety preview, but dry-run output alone is FAIL.

## Goal

The Loyal web frontend must be able to create or reuse a same-mint Safe USDC
route policy, deposit into Kamino Main USDC, let the fleet orchestrator move the
funds to the best eligible Safe USDC reserve, top up directly into the best
eligible reserve at request time, and fully withdraw from the current reserve
through frontend backend requests.

The product surface under verification is the sibling app. Telegram is out of
scope for this pass.

- `/Users/taequn/loyal/loyal-apps/packages/smart-account-vaults`
- `/Users/taequn/loyal/loyal-apps/packages/loyal-actions`
- `/Users/taequn/loyal/loyal-apps/sdk/loyal-smart-accounts*`, only as needed
- `/Users/taequn/loyal/loyal-apps/frontend/src/app/api/smart-accounts/yield-optimization/**`
- `/Users/taequn/loyal/loyal-apps/scripts/verify-earn-mainnet-flow.ts`

No Telegram mini-app changes are required for this verifier.

## Implementation Posture

This verifier should keep implementation aimed at observable behavior. Build
the smallest frontend and SDK path that can exercise the real lifecycle through
backend requests, then let dry-run output, DB readbacks, chain readbacks, and
server errors show what is still false. Mistakes during iteration are
acceptable when each one is surfaced in evidence and corrected instead of
becoming a hidden compatibility shim.

Update tests after the implementation shape is clear. Early work should favor
making the policy, reserve metadata, prepare/confirm contracts, and verifier
script actually move through the target flow. Once those contracts settle, add
or update focused tests for the SDK builders, route handlers, idempotent confirm
paths, packet/ALT guards, and inactive-vault filtering. The final verifier still
requires the focused static proof below before PASS.

Favor a fast iteration cycle. Work in small chunks that expose the next concrete
failure quickly: one prepare route, one confirm route, one SDK builder path, one
script phase, or one DB readback at a time. Prefer narrow dry-runs, simulations,
typechecks, and targeted route tests while the shape is changing. Broaden to the
full static proof and live E2E only after the smaller loop is producing useful
evidence instead of generic failures.

## Behavioral Oracle

This repo's same-mint Rust flow is the oracle. The sibling app must preserve the
same signer split, Safe USDC policy universe, active DB pickup rules, ALT and
packet-size constraints, and full-withdraw cleanup expectations already proven
by:

```sh
NO_DNA=1 cargo test -p squads-test-harness --test usdc_pyusd_kamino_route wallet_b_can_execute_same_mint_route_through_one_policy_call -- --nocapture
```

```sh
NO_DNA=1 cargo test -p squads-test-harness --test usdc_pyusd_kamino_route same_mint_route_execution_pack_size_is_packet_bound_by_measurement -- --nocapture
```

```sh
op run --env-file=.env.1password -- sh -c 'cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor -- --once --all-active-vaults --execute'
```

Use `docs/same-mint-reserve-swap.md` and
`docs/plans/same-mint-fleet-monitor-verifier.md` as the local runbooks for the
orchestrator side. If this frontend verifier conflicts with those oracle
documents, stop and resolve the mismatch before implementing.

## Secret And Signer Boundaries

Run secrets-backed commands through this 1Password wrapper:

```sh
OP='op run --env-file=.env.1password -- sh -c'
$OP '<command>'
```

Required signer split:

- frontend policy/deposit/top-up/withdraw user transactions are signed by the
  authenticated wallet, using `SOLANA_TESTING_PK` only inside the verifier
  script;
- app routes may use the existing deployment policy signer configuration needed
  to prepare frontend-owned policy operations;
- no frontend or backend app route may read or require `YIELD_ROUTER_KEYPAIR`;
- no fleet or orchestrator execution path may read or require
  `SOLANA_TESTING_PK`;
- same-mint fleet optimization and protected route execution use only
  `YIELD_ROUTER_KEYPAIR` as delegated signer and route payer.

PASS is impossible if these boundaries are proven only by convention. The
verifier must include code search evidence and live command environment evidence.

## Public Wire Contract

The frontend prepare and confirm contracts must carry reserve-specific metadata
for policy setup, initial deposit, top-up, and withdraw:

- target metadata: `reserve`, `market`, `liquidityMint`, reserve-specific vault
  collateral ATA, and supply APY evidence or explicit `null`;
- policy and vault metadata: policy account plus seed and app-facing policy id,
  delegated policy signer, settings, vault index, and vault pubkey;
- transaction metadata: lookup-table addresses and packet-size evidence when
  used.

The initial deposit target must be Kamino Main USDC. A top-up target must be the
top-ranked eligible reserve at prepare time. A hardcoded Main USDC target fails
the top-up check. A full withdrawal target must be discovered from the current
optimizer/current-position state and confirmed against chain state before
building the withdraw transaction.

Backend confirm routes must validate authenticated wallet/settings ownership,
confirmed signature and slot, policy metadata, reserve metadata, active/inactive
state transitions, and idempotency. A duplicate confirm must return the existing
record or an explicit idempotent response without creating duplicate
`route_policies`, `managed_vaults`, deposits, withdrawals, holding events, or
current-position rows.

## SDK Builder Checks

The sibling SDK builders pass when they support:

- creating a same-mint policy over the full eligible Safe USDC universe;
- reusing an existing active route policy without creating policy/vault spam;
- building reserve-specific Kamino deposit transactions for Main USDC and for
  the current highest-APY eligible Safe USDC reserve;
- building reserve-specific Kamino withdraw transactions from the current
  reserve;
- compiling v0 transactions with lookup tables where needed;
- failing before simulation/send when the serialized packet would exceed
  Solana's packet limit without sufficient ALT coverage.

The builders must not assume Main USDC except for the initial-deposit path.

## Static Proof

This section passes when the changed sibling surfaces pass their focused checks,
with exact commands and output recorded. Use package-native equivalents if scripts are
renamed. During the first implementation loop, record failing focused checks as
known work instead of blocking all code motion. Before final PASS, the checks
must be updated for the implemented behavior and pass:

```sh
cd /Users/taequn/loyal/loyal-apps
bun run --cwd packages/loyal-actions typecheck
bun run --cwd packages/loyal-actions test
bun run --cwd packages/smart-account-vaults typecheck
bun run --cwd sdk/loyal-smart-accounts-core typecheck
bun run --cwd sdk/loyal-smart-accounts typecheck
bun run frontend:lint
```

Also record:

```sh
rg -n "YIELD_ROUTER_KEYPAIR|SOLANA_TESTING_PK" frontend/src/app/api frontend/src/lib packages/smart-account-vaults packages/loyal-actions sdk/loyal-smart-accounts*
```

This search must show `YIELD_ROUTER_KEYPAIR` absent from app routes and
`SOLANA_TESTING_PK` absent from fleet/orchestrator execution paths. Expected
script-only verifier references are allowed when they are not imported by app
routes.

Run Slop Guard on this verifier document before closing the implementation:

```text
docs/plans/same-mint-frontend-sdk-verifier.md
```

## Frontend Server Proof

`scripts/verify-earn-mainnet-flow.ts` must be adapted so the live same-mint
phase drives an already-running frontend server with HTTP backend requests. It
must not import Next route handlers or call repository mutation functions to
prepare or confirm the core flow.

Required command shape:

```sh
cd /Users/taequn/loyal/loyal-apps
OP='op run --env-file=.env.1password -- sh -c'
$OP 'NEXT_PUBLIC_SOLANA_ENV=mainnet EARN_VERIFY_FRONTEND_BASE_URL=http://localhost:3000 EARN_VERIFY_PHASE=same-mint-frontend-sdk-live EARN_VERIFY_DRY_RUN=1 bun scripts/verify-earn-mainnet-flow.ts'
```

Approved live command, after explicit operator approval:

```sh
cd /Users/taequn/loyal/loyal-apps
OP='op run --env-file=.env.1password -- sh -c'
$OP 'NEXT_PUBLIC_SOLANA_ENV=mainnet EARN_VERIFY_FRONTEND_BASE_URL=http://localhost:3000 EARN_VERIFY_PHASE=same-mint-frontend-sdk-live bun scripts/verify-earn-mainnet-flow.ts'
```

The script may use direct Neon read-only queries for evidence, and it may use
SDK transaction helpers to sign and submit prepared operations. The prepare and
confirm product behavior must go through the running frontend API.

## Live Required Checks

### 1. Authenticated Setup

The script must authenticate the testing wallet against the running frontend
server. Every prepare/confirm request must resolve the same wallet, settings
PDA, and vault index from the server-side auth session. Requests with a
different wallet, settings PDA, or missing session must fail before any DB
mutation.

### 2. Initial Policy And Main Deposit

The initial deposit flow passes when it:

- creates or reuses one same-mint policy;
- records active `loyal_yield.route_policies` and
  `loyal_yield.managed_vaults` rows;
- deposits the requested amount into Kamino Main USDC;
- records Main USDC reserve, market, liquidity mint, vault collateral ATA,
  policy account, policy seed/id, delegated signer, vault info, signature, and
  confirmed slot;
- reconciles chain state into `vault_reserve_positions_current`;
- makes the active vault discoverable by
  `same-mint-yield-monitor --once --all-active-vaults`.

No Neon position row may be written before the corresponding chain transaction
is confirmed.

### 3. Orchestrator Pickup

The active frontend-created vault must be picked up by the fleet monitor:

```sh
$OP 'cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor -- --once --all-active-vaults --execute'
```

The proof must show the monitor chose the top-ranked eligible reserve with a
positive edge, wrote a confirmed same-mint `rebalance_decisions` row, submitted
a confirmed route signature, and reconciled the final current reserve position.
Directly calling `same-mint-reserve-swap` for the optimization move leaves this
section failed.

### 4. Top-Up

A frontend top-up after orchestrator pickup passes when it:

- queries fresh Safe USDC candidates at prepare time;
- selects the top-ranked eligible reserve at that moment;
- builds a reserve-specific Kamino deposit transaction for that reserve;
- records the top-up holding state with reserve, market, liquidity mint,
  collateral ATA, confirmed signature, confirmed slot, and policy metadata;
- does not create duplicate active policy or managed-vault rows;
- remains compatible with a later orchestrator move from the top-up reserve.

If the top-up reserve is the same as the current reserve, the proof must say so
explicitly and show the candidate ranking that made it true.

### 5. Full Withdrawal

Full withdrawal passes when it:

- discovers the current reserve from optimizer/current-position state;
- verifies the current reserve against chain obligation and collateral ATA
  state before preparing the transaction;
- withdraws from that reserve. A Main-only withdrawal path fails;
- recovers vault USDC to the wallet;
- closes the route policy and closeable token/obligation accounts where
  supported, or records an explicit non-closeable reason;

Cleanup checks:

- marks the selected `route_policies` and `managed_vaults` rows inactive;
- reconciles zero `vault_reserve_positions_current` rows for the vault;
- records wallet USDC return, rent refund/close evidence, signatures, and
  confirmed slots;
- proves a post-cleanup fleet poll no longer discovers the vault.

If the policy account, a closeable ATA, or a closeable obligation remains open
without an explicit reason, this section is FAIL.

### 6. Idempotency

Repeating every successful confirm request with the same body and signature must
leave row counts stable for active policies, managed vaults, current positions,
deposit rows, withdrawal rows, and holding events. Replaying a confirm with
mismatched reserve or policy metadata must fail.

## Failure Cases

The verifier must include explicit negative evidence for these cases:

- no fresh Safe USDC candidates: exit before setup, policy creation, deposit,
  or DB mutation;
- no positive APY edge from Main USDC: exit before setup, policy creation,
  deposit, or DB mutation;
- missing destination obligation: block before optimizer execution, policy
  mutation, decision write, or route submission;
- stale DB/chain disagreement: block prepare or confirm before send/finalize;
- oversized packet without ALT coverage: report packet blocker and send no
  transaction;
- duplicate confirms: idempotent result without duplicate rows;
- inactive policy or inactive vault: absent from fleet discovery.

## Evidence Record

The final verifier record must include run context and dry-run/live evidence:

- frontend base URL, commit SHAs for both repos, and command outputs;
- dry-run server proof with `sendsTransactions: false`;
- approved live run output with every signature and confirmed slot;
- direct Neon readbacks after initial deposit, orchestrator pickup, top-up, and
  full withdrawal;

It must also include market evidence plus the chain cleanup proof:

- Timescale candidate rows used for initial precondition and top-up target
  selection;
- chain readbacks for wallet USDC, vault USDC ATA, reserve collateral ATA,
  obligation, policy account, and rent refund/close evidence;
- packet-size and lookup-table evidence for policy, deposit, top-up, route, and
  withdrawal transactions;
- post-cleanup fleet poll output showing no discovery for the selected vault.

## Verdict Format

For each verification run, report:

```text
Static Proof: PASS|FAIL - note
Frontend Server Proof: PASS|FAIL - note
Authenticated Setup: PASS|FAIL - note
Initial Policy And Main Deposit: PASS|FAIL - note
Orchestrator Pickup: PASS|FAIL - note
Top-Up: PASS|FAIL - note
Full Withdrawal: PASS|FAIL - note
Idempotency: PASS|FAIL - note
Failure Cases: PASS|FAIL - note
Evidence Record: PASS|FAIL - note
Overall Verdict: PASS|FAIL
```

Overall verdict is PASS only when every required section passes. If any section
fails, keep this verifier unchanged and plan the smallest next change needed to
make the failing section pass. Revise this verifier only if it misstates the real
goal, and state the reason before changing it.
