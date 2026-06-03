# Same-Mint Yield Routing Implementation Plan

## Readiness Verdict

We have enough policy and state scaffolding to start a first plain same-mint yield router, but not enough production execution code to ship it without new pieces.

The existing code can express and detect the delegated Squads policy for a route, store managed vaults, snapshot positions, make a simple same-mint decision, load the yield-router signer, and read APY updates from TimescaleDB. The missing part is the production worker that reconciles real Kamino positions, builds real Kamino redeem/deposit instructions, batches Squads policy executions, submits transactions, confirms them, and records attempts.

The first implementation should be intentionally narrow:

- Same liquidity mint only.
- Full-position move from the user's current reserve to the current max-APY eligible reserve for that mint.
- No Jupiter, Loyal Hub, quote fetching, or cross-mint routing.
- One outer transaction may contain `N` independent Squads policy execution instructions.
- Each Squads policy execution contains exactly two inner instructions: Kamino redeem and Kamino deposit.
- No position splitting or portfolio optimizer.

## Existing Building Blocks

`crates/loyal-actions` already defines the route-policy surface. `YieldRouteActionSetup::same_mint_route()` coalesces withdraw and deposit into a `SameMintRoute`, and the all-in-one action topology builds one policy account whose constraint indexes are `[withdraw, deposit]`. In the current all-in-one shape, withdraw is constraint index `0` and deposit is `1 + swap_lanes.len()`.

`crates/loyal-actions/src/protocols.rs` already defines the policy constraints for Kamino redeem and deposit. These constraints pin Kamino Lend, the Squads vault, allowed Kamino markets, allowed liquidity mints, SPL Token ownership, vault token authority, and the Kamino instruction discriminator. This is the authorization boundary we need for same-mint routing.

`crates/loyal-actions/src/detection.rs` can detect route policies from Squads policy creation transactions. It records route modes, delegated signers, allowed stable mints, allowed Kamino markets, allowed Kamino liquidity mints, and swap-lane metadata.

`crates/loyal-squads-policy-monitor` consumes those detected policies and calls `record_policy_match`, so the orchestrator can learn which vaults are managed.

`crates/loyal-yield-router` is the read-only APY input boundary. It can read latest reserve rows, historical reserve rows, durable catch-up rows after a cursor, and `LISTEN` for Kamino reserve-update notifications. It deliberately does not own scoring, offset persistence, execution, or decisions.

`crates/loyal-yield-orchestrator` has the durable state base:

- `route_policies` and `managed_vaults` for discovered policies.
- `vault_position_snapshots` and `vault_reserve_positions_current` for reconciled state.
- `rebalance_decisions` and a decision state machine for planned, simulated, submitted, confirmed, failed, and abandoned decisions.
- `plan_same_mint_rebalance`, which locks one vault, checks for an active decision, reads current positions, and inserts a planned or skipped decision.
- `yield_router_keypair_from_env`, which loads the delegated signer keypair.

`crates/squads-test-harness` proves the route shape in LiteSVM. The important test path creates an all-in-one policy, deposits into one mock Kamino reserve, then submits one delegated policy execution containing mock Kamino withdraw and mock Kamino deposit to move from Main USDC to Prime USDC. This validates the Squads policy shape, not production Kamino instruction construction.

## Missing Pieces

The largest missing piece is real Kamino instruction construction. Today, the only withdraw/deposit builders are test helpers that write mock discriminators and mock account lists. Production needs a builder that derives or fetches the real reserve account graph, token accounts, collateral mint/accounts, lending market authority, oracle/sysvar/program accounts, and encodes the exact Kamino deposit and redeem instruction data.

We need to confirm the real amount semantics before coding the worker. The orchestrator stores `amount_raw`, but production Kamino redeem/deposit may differ between liquidity amount and collateral-share amount depending on the instruction and helper used. The first implementation must fail closed if it cannot quote or derive the redeemable liquidity/share conversion safely.

We need an on-chain position reconciler. The store can accept `ReconciledVaultState`, but no worker currently reads every managed vault's Kamino collateral balances, liquidity token accounts, reserve metadata, and current reserve state. The reconciler must also include zero-balance candidate reserves for the same mint so the planner can target reserves the user is not currently deposited into.

We need a reserve scorer that is global per liquidity mint. The current planner compares the largest valued current position to other positions already present in the vault snapshot. Intended behavior requires grouping eligible latest reserves by `liquidity_mint`, selecting the current max-APY reserve, and comparing every user's current reserve for that mint against that target.

We need active-vault enumeration and claiming. The store can lock one vault by id, but a batch worker needs to list active managed vaults for a cluster/policy mode/mint, claim work with `SKIP LOCKED` or equivalent leases, and avoid double-routing the same vault while another decision is active.

We need a production route-execution builder outside the test harness. It should take a `SameMintRoute`, signer, vault index, real Kamino redeem compiled instruction/accounts, and real Kamino deposit compiled instruction/accounts, then return the Squads policy execution instruction. The test adapter already shows this shape, but it lives in `crates/squads-test-harness`.

We need Solana RPC execution dependencies and code. `loyal-yield-orchestrator` currently depends on `solana-sdk`, not `solana-client`, so it cannot fetch blockhashes, simulate, submit, poll status, or confirm transactions. This should probably live in a new worker crate or binary that depends on `loyal-yield-router`, `loyal-yield-orchestrator`, `loyal-actions`, and Solana RPC crates.

We need batch attempt state. `rebalance_decisions` can store a signature and slots, but a transaction batching `N` users needs batch-level and per-decision attempt records: simulated at, submitted at, blockhash, signature, packed decisions, failure reason, retry count, and whether failure invalidates all included decisions.

We need account setup policy. Same-mint routing assumes the vault has the source collateral token account, target collateral token account, and liquidity token account required by Kamino. We need either an onboarding/pre-create flow or a worker preflight that skips with a clear reason when accounts are missing.

We need production batching measurements. Existing tests measure route packing with mock accounts and recent Jupiter batch experiments focus on swap size. Same-mint production accounts should be measured separately with real Kamino account metas, v0 transactions, and any address lookup tables.

## Proposed First Implementation

### 1. Reserve Target Loop

Add a worker loop that consumes `loyal-yield-router` latest/catch-up data and recomputes one target reserve per liquidity mint.

For the first version, eligibility should be explicit and conservative:

- Reserve is in an allowed Kamino market from active policies.
- Liquidity mint is in active route-policy allowlists.
- Reserve row is not stale.
- Supply is above a configured minimum.
- APY is finite and inside a configured sanity range.
- Current target changes only when the edge exceeds configured minimum bps and cooldown.

Persist either a `reserve_score_snapshots` table or a compact current-target table keyed by `(cluster, liquidity_mint)`. The planner should receive the target reserve, target APY, score timestamp, and source row id or cursor.

### 2. Managed Vault Scan

Add a store method that returns claimable active managed vaults for a cluster, route mode `same_mint`, and liquidity mint. It should join active policy metadata and exclude vaults with active decisions.

The worker should process all managed vaults whose policy permits the target market and liquidity mint. "All users" should mean all active `managed_vaults` discovered by the policy monitor unless product wants a narrower enrolled-user table.

### 3. Position Reconciliation

Before planning, reconcile each claimed vault from chain:

- Read the active policy and vault pubkey.
- Read candidate Kamino reserves for the mint from the active route universe.
- Read source collateral balances and target collateral account existence.
- Read vault liquidity token account existence and balance.
- Write `ReconciledVaultState` with one row per candidate reserve, including zero-balance targets.
- Store account pubkeys and reserve metadata in `planning_metadata` so the executor does not rediscover a different account set than the planner used.

This reconciler is the boundary where we decide whether a vault has exactly one current source reserve for the mint. If multiple reserves have value for the same mint, skip in the first version unless product explicitly wants merge behavior.

### 4. Planning

Refactor or extend `draft_same_mint_decision` so the source is the vault's current valued reserve for the selected liquidity mint and the target comes from the current max-APY reserve target, not only from other current position rows.

Skip when:

- The vault has no value in that mint.
- The current source is already the target.
- The target reserve is not allowed by the active policy.
- Required token/collateral accounts are missing.
- The APY edge is below threshold.
- The reserve score is stale.
- There is already an active decision for the vault.

For the plain version, plan `amount_raw` as the full source position amount after the amount-unit question is settled.

### 5. Instruction Build

Introduce a production same-mint route builder:

1. Build real Kamino redeem/withdraw inner instruction for the source reserve.
2. Build real Kamino deposit inner instruction for the target reserve.
3. Convert both to Squads compiled-instruction payloads plus account metas.
4. Use the active policy's `SameMintRoute` metadata to build one Squads policy execution instruction.

This builder should validate that the inner instructions match the same policy constraints recorded for the vault before the transaction is sent.

### 6. Batch Assembly

Collect ready decisions into batches by cluster, fee payer, delegated signer, and compatible transaction settings.

Each outer transaction should look like:

```text
transaction:
  compute budget / priority fee instructions
  policy execution for vault A:
    inner: Kamino redeem source reserve
    inner: Kamino deposit target reserve
  policy execution for vault B:
    inner: Kamino redeem source reserve
    inner: Kamino deposit target reserve
  ...
```

Do not combine multiple users into one Squads policy execution. The policy execution is vault-scoped, so batching means many independent policy calls in one Solana transaction.

Start with a conservative `N = 1` or `N = 2`, measure packet/compute, then raise it behind config. Use v0 transactions and address lookup tables only after the non-ALT path is proven and measured.

### 7. Simulate, Submit, Confirm, Reconcile

For each batch:

1. Mark decisions `simulating`.
2. Simulate the full transaction.
3. Mark decisions `ready` if simulation passes.
4. Submit with the yield-router signer as delegated policy signer and likely fee payer.
5. Mark decisions `submitted` with signature and submitted slot.
6. Poll confirmation until finalized or expired.
7. Reconcile each vault post-confirmation.
8. Mark confirmed decisions with post snapshots; mark failures with classified error reasons.

Retries should rebuild from fresh state, not blindly resubmit stale instructions after a blockhash expiration or APY target change.

## Suggested Data Model Additions

Add only the tables needed for safe operation:

- `reserve_targets_current`: current best eligible reserve per `(cluster, liquidity_mint)`, with APY, observed cursor, freshness, and filters used.
- `reserve_target_snapshots`: optional append-only history for replaying why a target changed.
- `worker_cursors`: durable Timescale cursor and loop state.
- `rebalance_batches`: one row per submitted/simulated Solana transaction.
- `rebalance_batch_decisions`: join table from batch to decision, with per-decision outcome.
- `rebalance_attempts`: optional if we want attempts without committing to a batch abstraction yet.
- `vault_execution_accounts`: optional cache of vault token/collateral account pubkeys and reserve metadata discovered during reconciliation.

If we want the smallest possible migration, start with `worker_cursors`, `reserve_targets_current`, `rebalance_batches`, and `rebalance_batch_decisions`; keep account metadata in snapshot `planning_metadata` until it proves too awkward.

## Testing Plan

Unit-test reserve grouping and target selection: same mint, APY changes, stale rows, min supply, edge threshold, cooldown, and already-on-target skips.

Unit-test the planner change: source current reserve versus target max reserve, zero-balance target candidates, multiple valued source reserves, missing policy allowlist, and active-decision skips.

Add orchestrator DB tests for active-vault listing/claiming, batch rows, attempt rows, and idempotent decision transitions.

Move or duplicate the test-harness same-mint route builder into a production-safe helper, then test it with the existing LiteSVM mock Kamino path.

Add a multi-vault LiteSVM test that creates several managed vaults/policies and submits one outer transaction containing multiple same-mint policy executions.

Add a packet/compute measurement for same-mint batches using the closest real Kamino account list we can construct locally. Update this doc with the measured `N`.

Before mainnet execution, run shadow mode for at least one week: compute targets, reconcile positions, plan decisions, build instructions, simulate if possible, but do not submit.

## Questions Before Implementation

1. What exactly counts as "all of our users": every active `managed_vaults` row discovered by the policy monitor, a separate enrolled-user list, or only vaults with a specific current policy preset?

2. Which APY input should drive the max-APY target: raw latest Timescale `supply_apy`, five-minute data, twenty-minute EWMA, one-hour mean, or another smoothed value?

3. What minimum edge, cooldown, and liquidity filters should the first version use? Existing docs mention conservative filtering and cooldown, but the exact numbers need product/ops approval.

4. Should the first version skip vaults with more than one valued reserve for the same mint, or merge multiple source reserves into the target?

5. What is the canonical production Kamino SDK/source for building deposit and redeem instructions in Rust? If none exists in this repo, should we add a small internal builder from Kamino IDL/layouts or depend on an external KLend SDK?

6. What are the exact amount units for the real redeem instruction we will call: liquidity amount, collateral amount, or "redeem all" semantics? We should not assume the mock `amount_raw` behavior matches production.

7. Is `YIELD_ROUTER_KEYPAIR` guaranteed to be the delegated signer on every policy we intend to route, and should it also be the fee payer?

8. Who funds missing vault token accounts, target collateral accounts, rent, priority fees, and any address lookup table setup?

9. Should missing target accounts be auto-created by the worker, pre-created during onboarding, or treated as a skip reason?

10. Which cluster is first: LiteSVM-only, mainnet shadow mode, or a devnet flow with mocked/cloned Kamino accounts?

11. Should a batch fail atomically for all included users, or should the worker use `N = 1` until we have enough confidence in account setup and compute stability?

12. What confirmation level is required before marking a decision confirmed, and how should the worker treat transactions that land after the APY target has changed again?

13. Is route-policy churn acceptable if a newly-max reserve is outside a user's active policy allowlist, or should the first version only route within already-authorized reserves?

14. Do we need user-visible notification or consent before moving an existing managed vault from one reserve to another?

15. Where should the production worker live: a new crate, a binary under `loyal-yield-orchestrator`, or a higher-level app/service package that composes the existing crates?
