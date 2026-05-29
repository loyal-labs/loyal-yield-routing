# Loyal Hub 16-Mint, 32-Lane Implementation Plan

## Context

This plan captures the current direction for scaling Loyal Hub swap inventory while preserving user custody and avoiding unnecessary Solana write-lock contention.

The target shape is:

- Raise Loyal Hub allowed mints from 8 to 16.
- Use 32 Hub lanes.
- Keep the all-in-one policy as the main delegated execution surface.
- Batch tiny deposits per vault instead of pooling user custody.
- Use Jupiter for residual, unsupported, or saturated long-tail routes.

Do not rush this change. Take the time to measure packet sizes, policy sizes, and test drift carefully. Quality is more important than one-shot speed here.

## Learnings From Design Review

Solana write locks are held only while a transaction executes, usually milliseconds, but transactions touching the same writable account cannot execute concurrently. For Loyal routes this means:

- Two routes touching the same user or vault token accounts still serialize.
- Two Hub swaps touching the same lane inventory accounts serialize.
- Different users can execute in parallel if they use different vault accounts and different Hub lane inventory accounts.
- Helius can improve delivery, but it does not bypass the leader's account-lock scheduler.

Lanes should be treated as shared liquidity shards, not per-user accounts:

- Per-vault route execution should be serialized.
- Hub lane selection should spread independent users across lanes.
- Rebalancing should happen out of the hot path and avoid active lanes when possible.

The all-in-one policy is favorable for this architecture:

- It creates one Squads ProgramInteraction policy for withdraw, swap lane(s), and deposit.
- Adding more Hub lane IDs does not multiply the policy constraints.
- The policy constrains the Hub program, config PDA, vault, allowed mints, hub authorizer, token program, and fee cap.
- The Hub program validates the selected `lane_id` and canonical lane inventory accounts at execution time.

Current constraints:

- Hub ABI currently has `MAX_ALLOWED_MINTS 8` in `crates/loyal-hub-abi/schema/loyal_hub_abi.schema`.
- Hub config stores `lane_count` as `u8`, so 32 lanes fits the current type shape.
- The current production stable preset lists 14 mints in `crates/loyal-actions/src/actions.rs`.
- Moving from 8 to 16 Hub mints adds 256 bytes to the Hub config mint list.
- 32 lanes x 16 mints implies 512 Hub inventory token accounts.

## Proposed Product Shape

Use a two-tier stablecoin universe:

- **All-in-one policy universe:** the mints a vault's policy may route across.
- **Hub hot universe:** up to 16 stable mints we are willing to inventory directly in Loyal Hub.
- **Jupiter universe:** residual, saturated, unsupported, and long-tail stable routes.

Recommended near-term config:

```text
MAX_ALLOWED_MINTS = 16
lane_count = 32
max Hub inventory accounts = 16 * 32 = 512
```

This gives enough room for the current 14 production stable mints plus near-term growth, without jumping to the operational sprawl of 32 mints.

## Implementation Steps

1. Update the Hub ABI schema.

   Change `MAX_ALLOWED_MINTS` from 8 to 16 in:

   ```text
   crates/loyal-hub-abi/schema/loyal_hub_abi.schema
   ```

2. Regenerate or rebuild the generated ABI crate.

   The generated Loyal Hub ABI crate is the byte-layout source of truth. Do not hand-maintain matching constants in the program, SDK, policies, or tests.

3. Update tests and snapshots that assume 8 allowed mints.

   Search for:

   ```text
   MAX_ALLOWED_MINTS
   LOYAL_HUB_MAX_ALLOWED_MINTS
   allowed_mints
   production_stable_mints
   ```

   Pay particular attention to tests that intentionally create too many mints or assert config lengths.

4. Verify all-in-one policy creation with 16 stable mints.

   Measure whether policy creation still fits comfortably, especially when combined with setup instructions. The policy pubkey table currently supports up to 240 custom pubkeys, so 16 mints should be fine, but packet size should be measured rather than assumed.

5. Re-measure route execution packing.

   Previous measured capacity was about:

   ```text
   7 same-mint routes per outer transaction
   4 cross-mint routes per outer transaction
   ```

   Treat those numbers as stale after changing policy shape or route account lists. Re-measure against current `PACKET_DATA_SIZE`.

6. Add or update lane-count tests for 32 lanes.

   Cover:

   - Valid lane IDs `0..31`.
   - Rejection of lane ID `32`.
   - Hub swap using a non-default lane.
   - Rebalance between lanes within the 32-lane config.
   - Rejection of wrong canonical inventory accounts.

7. Add orchestration design for tiny deposits.

   The scheduler should support:

   - One queue per vault.
   - At most one active route transaction per vault.
   - Deposit coalescing by threshold and timer.
   - Lane inventory reservation with a short TTL.
   - Lane selection by sufficient inventory and low in-flight load.
   - Fallback or residual routing through Jupiter.
   - Background lane rebalancing that avoids hot lanes.

8. Keep custody boundaries intact.

   Do not batch by pooling user funds unless the product explicitly changes custody semantics. The preferred model is:

   ```text
   batch per user/vault
   parallelize across users
   shard shared Hub liquidity by lane
   ```

## Verification Commands

Run the ABI/spec drift gate whenever the ABI schema changes:

```sh
bun run verify:hub-abi-spec-drift
```

Run the active QEDGen gates:

```sh
bun run verify:qedgen:check
bun run verify:qedgen:proptest
bun run verify:qedgen:probe
```

Or run the combined verification target:

```sh
bun run verify:qedgen
```

Run Squads route coverage:

```sh
bun run test:squads
```

Run the heavier replay when route policy composition, heap/compute assumptions, or replay-sensitive behavior changes:

```sh
bun run test:squads:e2e
```

## Open Questions

- Should all 14 current production stable mints be Hub-inventory mints, or should some remain Jupiter-only at launch?
- What is the initial per-mint inventory target per lane?
- What route threshold makes a tiny deposit worth moving on-chain?
- What lane reservation TTL should the scheduler use?
- Should lane inventory be rebalanced on a fixed cadence, threshold trigger, or both?

## Guidance For Next Codex

Take your time. This is a protocol shape change, not a quick constant bump.

Before editing code, read:

- `AGENTS.md`
- `crates/loyal-hub-abi/schema/loyal_hub_abi.schema`
- `crates/loyal-hub-swap-program/src/state.rs`
- `crates/loyal-actions/src/actions.rs`
- `crates/loyal-actions/src/protocols.rs`
- `crates/squads-test-harness/tests/loyal_hub_swap.rs`
- `crates/squads-test-harness/tests/usdc_pyusd_kamino_route.rs`

Measure first, then change. Do not assume previous packet-packing numbers still hold after changing mint capacity. Keep changes scoped, preserve vertical ownership, and let the tests tell you where the ABI, QED spec, SDK, policy builder, and harness expectations need to move together.
