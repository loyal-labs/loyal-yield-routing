# ASK-1648 Problem Analysis: Deploy Loyal Hub to Staging

Recorded: 2026-07-06

## Issue interpretation

Linear `ASK-1648` is not asking for a new frontend surface yet. The issue asks to add a cross-mint yield orchestration strategy in `loyal-yield-orchestrator` using the already-deployed `loyal-hub-swap-program`, then roll it out to staging alongside the existing same-mint strategy.

Read-only Linear evidence: `ASK-1648` is titled `Deploy Loyal Hub to staging`, priority High, status In Progress, and says same-mint rebalance already works on prod/staging. It names `scripts/mainnet-loyal-hub-tests.ts` as the working E2E and says Loyal Hub is deployed to Mainnet but may need admin instructions to bind it to staging orchestrator keys.

Scope conclusion: this pass did not find evidence that `loyal-app` is required for the core issue. The relevant ownership is in `loyal-yield-routing`: policy detection/storage, `loyal-yield-orchestrator`, `loyal-actions`, Hub CLI/admin tools, and Render staging worker configuration.

## Read-only evidence

- `bun run hub:cli -- --url mainnet --json state` succeeded after network approval and performed no signing or transaction submission. Mainnet Hub is initialized at program `LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH`, config `8BkE1rD3Xgb8DHrWffgVB3zfJYgEerxDxbxHSdRin8wb`, `paused: false`, `max_fee_bps: 50`, `lane_count: 4`, and `admin`, `hub_authorizer`, and `inventory_rebalancer` are all currently `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`.
- The same read showed six allowed mints and mostly empty lane inventory. Lanes 0 and 1 have zero-balance inventory accounts for `2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo` and USDC `EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v`; most other lane inventory accounts do not exist yet.
- `bun run hub:mainnet-test -- --help` is local read-only evidence that the current Hub E2E defaults to simulation, requires both `--execute` and `CONFIRM_MAINNET=1` for live execution, supports `--simulate-only`, `--simulate-all`, `--allow-authority-handoff`, `--cleanup-only`, and route-file generation flags.

## Current same-mint orchestration path

Same-mint is implemented as a fleet monitor plus a dedicated executor:

- `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs:31` defines the runtime mode as `same_mint_kamino`.
- `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs:780` through `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs:813` loads active vaults whose policy has the delegated signer, contains `same_mint_kamino` in `route_modes`, and overlaps enabled stable mints/Kamino liquidity mints.
- `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs:292` through `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs:346` executes only when `--execute` is set and no active decision exists.
- `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs:374` through `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs:421` shells into `same-mint-reserve-swap` with `--optimization-cycle`, `--reconcile-from-chain`, and `--execute`.
- `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs:512` through `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs:535` only picks a reconcile pair where source and target reserves share the same `liquidity_mint`.

The same-mint planner is also explicitly same-mint:

- `crates/loyal-yield-orchestrator/src/domain.rs:45` through `crates/loyal-yield-orchestrator/src/domain.rs:155` plans only source/target pairs with the same `liquidity_mint`; if a valuable position has no same-mint target it returns `CrossMintOnly`.
- `crates/loyal-yield-orchestrator/src/store.rs:967` through `crates/loyal-yield-orchestrator/src/store.rs:1006` wraps that planner in `plan_same_mint_rebalance`.
- `crates/loyal-yield-orchestrator/src/store.rs:1079` through `crates/loyal-yield-orchestrator/src/store.rs:1135` prepares an executable decision with `same_mint_execution_plan`.
- `crates/loyal-yield-orchestrator/src/store.rs:2271` through `crates/loyal-yield-orchestrator/src/store.rs:2288` writes `execution_plan.kind = "same_mint"` and same-mint route steps.
- `crates/loyal-yield-orchestrator/src/store.rs:1150` through `crates/loyal-yield-orchestrator/src/store.rs:1265` confirms same-mint by projecting source to zero and target to `amount_raw` unless a post-chain snapshot is supplied.

The same-mint executor is likewise hard-bound:

- `crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs:6384` through `crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs:6645` builds a route execution plan from Kamino withdraw and Kamino deposit, optionally inline init-obligation setup.
- `crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs:4781` through `crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs:4832` rejects any prepared decision whose `execution_plan.kind` is not `same_mint`.
- `crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs:6865` through `crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs:7001` simulates, sends, confirms, reconciles post-chain state, and calls `confirm_same_mint_rebalance`.
- `crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs:4373` through `crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs:4452` implements fail-closed durable ALT reuse under a `same_mint_kamino:<settings>:<vault>:<source>:<target>` route scope.

## Existing policy and Hub surfaces

The repo already has policy metadata capable of representing cross-mint Hub policies:

- `crates/loyal-yield-orchestrator/migrations/0001_loyal_yield_orchestration.sql:53` through `crates/loyal-yield-orchestrator/migrations/0001_loyal_yield_orchestration.sql:70` stores `route_modes`, `stable_mints`, Kamino universe fields, and `swap_lanes` on `loyal_yield.route_policies`.
- `crates/loyal-yield-orchestrator/migrations/0001_loyal_yield_orchestration.sql:233` through `crates/loyal-yield-orchestrator/migrations/0001_loyal_yield_orchestration.sql:259` stores `rebalance_decisions.execution_plan`.
- `crates/loyal-squads-policy-monitor/src/lib.rs:488` through `crates/loyal-squads-policy-monitor/src/lib.rs:603` emits detected yield-route policy matches into the orchestrator store.
- `crates/loyal-squads-policy-monitor/src/lib.rs:671` through `crates/loyal-squads-policy-monitor/src/lib.rs:675` maps detected policy modes to `same_mint`, `cross_mint_jupiter`, and `cross_mint_loyal_hub`.
- `crates/loyal-yield-orchestrator/src/store.rs:1490` through `crates/loyal-yield-orchestrator/src/store.rs:1565` persists those modes and swap lanes into `route_policies`.

The Rust action SDK already models Hub lanes:

- `crates/loyal-actions/src/ids.rs:16` through `crates/loyal-actions/src/ids.rs:53` defines the mainnet Hub program ID and ABI tags/seeds.
- `crates/loyal-actions/src/actions.rs:75` through `crates/loyal-actions/src/actions.rs:82` defines `SwapLane::LoyalHub { hub_authorizer, max_fee_bps }`.
- `crates/loyal-actions/src/actions.rs:234` through `crates/loyal-actions/src/actions.rs:252` exposes `same_mint_route`, `jupiter_route`, and `loyal_hub_route` indexes.
- `crates/loyal-actions/src/actions.rs:347` through `crates/loyal-actions/src/actions.rs:410` creates or updates an all-in-one route policy with swap lanes.
- `crates/loyal-actions/src/actions.rs:821` through `crates/loyal-actions/src/actions.rs:875` builds all-in-one constraints around withdraw, swap lane(s), deposit, and init/refresh constraints.
- `crates/loyal-actions/src/actions.rs:985` through `crates/loyal-actions/src/actions.rs:1010` turns `SwapLane::LoyalHub` into a `loyal_hub_constraint`.
- `crates/loyal-actions/src/actions.rs:1555` through `crates/loyal-actions/src/actions.rs:1608` tests a compiled policy containing same-mint, Jupiter, and Hub routes, with Hub constraint indexes `[0, 2, 3]`.
- `crates/loyal-actions/src/detection.rs:130` through `crates/loyal-actions/src/detection.rs:144` defines `DetectedYieldRouteMode::CrossMintLoyalHub` and `DetectedSwapLane::LoyalHub`.
- `crates/loyal-actions/src/detection.rs:242` through `crates/loyal-actions/src/detection.rs:313` detects Hub swap constraints and adds `CrossMintLoyalHub`.
- `crates/loyal-actions/src/detection.rs:597` through `crates/loyal-actions/src/detection.rs:652` validates Hub swap constraints against program ID, config PDA, vault/user accounts, mints, authorizer, and token program.

The TypeScript SDK and test runner are the working Hub E2E surface:

- `packages/loyal-actions/src/cluster.ts:14` through `packages/loyal-actions/src/cluster.ts:27` defines shared mainnet/devnet config with Hub program `LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH` and authorizer `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`.
- `packages/loyal-actions/src/sdk.ts:41` through `packages/loyal-actions/src/sdk.ts:103` builds a yield route policy whose `routes` include `sameMint` and, depending on `swapLanes`, `jupiter` or `loyal`.
- `packages/loyal-actions/src/internal/protocols.ts:149` through `packages/loyal-actions/src/internal/protocols.ts:180` builds the Hub policy constraint for `SwapExactIn`.
- `packages/loyal-actions/src/internal/protocols.ts:281` through `packages/loyal-actions/src/internal/protocols.ts:287` derives the Hub config and lane authority PDAs.
- `scripts/mainnet-loyal-hub-tests.ts:271` through `scripts/mainnet-loyal-hub-tests.ts:298` defaults to `mainnet-beta`, resolves cluster config, loads state, and blocks live mainnet execution unless `CONFIRM_MAINNET=1`.
- `scripts/mainnet-loyal-hub-tests.ts:3153` through `scripts/mainnet-loyal-hub-tests.ts:3215` documents the guarded mainnet E2E flow and flags.
- `crates/loyal-yield-orchestrator/src/bin/loyal-hub-mainnet-route-files.rs:35` through `crates/loyal-yield-orchestrator/src/bin/loyal-hub-mainnet-route-files.rs:39` defines default route/setup JSON files for the mainnet runner.
- `crates/loyal-yield-orchestrator/src/bin/loyal-hub-mainnet-route-files.rs:138` through `crates/loyal-yield-orchestrator/src/bin/loyal-hub-mainnet-route-files.rs:172` generates policy setup, route withdraw, and route deposit instruction files.

Hub admin logic exists but is currently test/admin tooling, not orchestration:

- `crates/loyal-hub-cli/src/main.rs:131` through `crates/loyal-hub-cli/src/main.rs:190` exposes `state`, `initialize-config`, `set-admin`, `set-hub-authorizer`, and `set-inventory-rebalancer`.
- `crates/loyal-hub-cli/src/main.rs:950` through `crates/loyal-hub-cli/src/main.rs:1050` reads live Hub program/config state.
- `crates/loyal-hub-swap-program/src/processor.rs:199` through `crates/loyal-hub-swap-program/src/processor.rs:293` implements admin, authorizer, and inventory rebalancer mutation handlers.
- `scripts/mainnet-loyal-hub-tests.ts:1569` through `scripts/mainnet-loyal-hub-tests.ts:1572` requires `--allow-authority-handoff` before a Hub authorizer handoff.
- `scripts/mainnet-loyal-hub-tests.ts:1819` through `scripts/mainnet-loyal-hub-tests.ts:1842` restores admin, authorizer, and inventory rebalancer during cleanup.
- `scripts/mainnet-loyal-hub-tests.ts:1917` through `scripts/mainnet-loyal-hub-tests.ts:1934` only sets inventory rebalancer or Hub authorizer directly when current Hub admin is the system key.

## Staging deployment posture

Staging exists and is currently intentionally dry-run for same-mint:

- `render.yaml:227` through `render.yaml:247` defines `loyal-same-mint-yield-monitor-staging` with command `/usr/local/bin/same-mint-yield-monitor --all-active-vaults --poll-interval-seconds 300 --rebalance-cooldown-seconds 300`; there is no `--execute`.
- `docs/render-worker-images.md:178` through `docs/render-worker-images.md:188` explicitly says staging is fail-closed, autodeposit staging omits execution, same-mint staging is fleet dry-run only, and staging ATA monitor/projector use the staging Timescale stream.
- `render.yaml:161` through `render.yaml:182` shows production same-mint uses the same binary with `--execute`.

This means ASK-1648 should probably add a new staging worker or a new strategy mode behind dry-run first, rather than enabling live cross-mint execution in the existing staging service immediately.

## Likely integration boundary

The narrowest useful boundary is inside `loyal-yield-orchestrator`, reusing `crates/loyal-actions` as the policy/action builder and the existing Neon decision lifecycle as the persistence boundary.

Recommended boundary:

1. Add a new cross-mint strategy that searches current positions and candidate reserves where source and target `liquidity_mint` differ, source is routeable, target is supported by the policy's `stable_mints` and Kamino universe, and the active policy has `cross_mint_loyal_hub` plus a Hub lane in `swap_lanes`.
2. Persist decisions with a new `execution_plan.kind`, for example `cross_mint_loyal_hub`, rather than overloading `same_mint`.
3. Build a route execution plan with three protected steps: Kamino withdraw, Hub `SwapExactIn`, and Kamino deposit. Reuse the decoded policy's `loyal_hub_route()` constraint indexes where possible.
4. Keep execution fail-closed like same-mint: validate delegated signer, policy constraints, token accounts, Hub config authorizer, inventory availability, packet fit, simulation success, and durable ALT coverage before any DB decision write or route send.
5. Add explicit setup/admin commands for staging Hub binding and inventory bootstrap, separate from the recurring strategy worker. Admin instructions should remain operator-invoked and read back Hub config before/after.
6. Add a staging Render service or staging-only command mode that starts in dry-run/simulate posture and emits enough JSON to compare planned cross-mint moves, Hub inventory sufficiency, route packet size, and policy indexes before any `--execute` rollout.

## Main risks

- Policy mode name mismatch: detected policies store `cross_mint_loyal_hub`, while same-mint runtime constants currently use `same_mint_kamino`. Any new worker must query the exact persisted mode and not assume the TS route name `loyal`.
- Decision lifecycle mismatch: `load_prepared_same_mint_decision` rejects non-`same_mint` kinds, and `confirm_same_mint_rebalance` projects same-mint balances. Cross-mint needs its own loader, validator, and confirmation semantics.
- Amount semantics: same-mint relies on redeemable liquidity and same mint equality. Cross-mint must model `amount_in`, `amount_out`, `min_out`, fee bps, and target deposit amount without confusing collateral and liquidity units.
- Hub state risk: live Hub config is initialized and unpaused, but current lane inventory is mostly empty/zero from read-only state. A staging strategy must block or report `hub_inventory_insufficient` before route send.
- Authority risk: current mainnet Hub admin, authorizer, and inventory rebalancer are all `GTpq...`. Binding to staging orchestrator keys requires admin mutations. Those should be explicit, audited, and reversible, not a background worker side effect.
- ALT and packet risk: same-mint route execution already fails closed on missing durable ALT coverage. Cross-mint adds Hub accounts and may need its own route scope such as `cross_mint_loyal_hub:<settings>:<vault>:<source>:<target>:<lane>`.
- Rollout risk: staging Render currently omits `--execute` by design. A new Hub worker should preserve this posture until dry-run evidence, Hub config readback, inventory readback, simulation, and DB readback are all clean.

## Concise proposed shape

Implement `loyal-hub-yield-monitor` and `loyal-hub-reserve-swap` only if the same-mint binary would become too branch-heavy; otherwise factor shared planner/executor helpers and add a `--strategy loyal-hub-cross-mint` mode. Prefer a separate staging worker at first because it keeps logs, commands, and failure modes easy to inspect.

Suggested first PR:

1. Add `CrossMintLoyalHubInput`, `CrossMintLoyalHubResult`, `prepare_cross_mint_loyal_hub_rebalance`, and `confirm_cross_mint_loyal_hub_rebalance` beside the same-mint store path.
2. Add a planner that turns `CrossMintOnly` opportunities into `execution_plan.kind = "cross_mint_loyal_hub"` when the active policy supports `cross_mint_loyal_hub`.
3. Add a route builder that creates/validates withdraw, Hub `SwapExactIn`, and deposit instructions from decoded policy indexes and live Hub state.
4. Add dry-run JSON output first: `wouldWriteDecision`, `wouldBuildRoute`, `wouldExecuteRoute`, `hubState`, `hubInventory`, `policyPreflight`, `routeExecution`, and `lookupTableProvisioning`.
5. Add a staging Render service in dry-run mode, with `YIELD_ROUTER_KEYPAIR` present only if needed for signer identity checks and no live send until an operator flips an explicit `--execute` command.

No PR or commit was created for this analysis.
