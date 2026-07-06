# ASK-1648 Subtask Decomposition

Issue: `ASK-1648 Deploy Loyal Hub to staging`

Goal: integrate `loyal-hub-swap-program` into `loyal-yield-orchestrator`, add a cross-mint orchestration strategy via Loyal Hub alongside the existing same-mint route, and roll it out to staging first.

This decomposition is based on repo inspection only. No blockchain, database, API writes, live transactions, PRs, commits, or branch changes were performed.

## Current Repo Boundary

- `scripts/mainnet-loyal-hub-tests.ts` is the working mainnet E2E surface for a split user route: Kamino withdraw route files, one Loyal Hub swap instruction, and Kamino deposit route files.
- `package.json` already exposes `hub:mainnet-test`, `hub:mainnet-route-files`, `hub:cli`, and `hub:squads-ops`.
- `crates/loyal-actions/src/actions.rs` already models `SwapLane::LoyalHub` and exposes `loyal_hub_route()` for a withdraw -> Hub swap -> deposit route.
- `crates/loyal-actions/src/detection.rs` and `crates/loyal-squads-policy-monitor/src/lib.rs` can detect and persist route policies with `DetectedYieldRouteMode::CrossMintLoyalHub` and `swap_lanes`.
- `loyal_yield.route_policies` already stores `route_modes`, `stable_mints`, `kamino_markets`, `kamino_liquidity_mints`, and `swap_lanes`.
- `crates/loyal-yield-orchestrator/src/domain.rs` currently plans only same-mint Kamino moves. It explicitly returns `SkipReason::CrossMintOnly` when a vault has value but no same-mint target exists.
- `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs` currently selects fleet vaults by the `same_mint_kamino` route mode and shells into `same-mint-reserve-swap`.
- `render.yaml` already has production and staging `loyal-same-mint-yield-monitor` workers. Production passes `--execute`; staging omits `--execute` and is therefore dry-run by current config.
- Hub is deployed to mainnet, but the issue says it is not configured. Treat staging Hub config/admin binding as an operational prerequisite, not something an implementation PR should silently perform.

## Recommendation

Use multiple PRs. A single PR would mix policy ingestion semantics, planner scoring, transaction construction, DB decision semantics, worker deployment, and Hub admin readiness. That is too large to review safely because staging rollout correctness depends on each layer preserving fail-closed behavior.

Recommended shape: four implementation PRs plus one staging operations PR/runbook. PR 1 and PR 2 can be reviewed and merged without enabling execution. PR 3 should remain dry-run/simulation-first. PR 4 is the staging worker rollout. PR 5 is optional for production hardening and should wait until staging evidence exists.

## PR 1: Normalize Hub Route Policy Ingestion and Readiness

Branch: `ASK-1648-hub-policy-ingestion-readiness`

Title: `feat(yield-routing): persist Loyal Hub route readiness`

Purpose: make the control plane reliably recognize Hub-capable route policies and expose enough read-only readiness evidence for staging without creating or updating policies.

Likely changes:

- Keep `loyal-squads-policy-monitor` as the source of truth for detected policy accounts.
- Audit and normalize route-mode names so orchestrator readers can distinguish `same_mint_kamino`, `cross_mint_loyal_hub`, and any legacy `same_mint` values without silently skipping eligible policies.
- Ensure `swap_lanes` for Hub policies persist the `hubAuthorizer`, `maxFeeBps`, action account, and constraint indexes needed by the executor.
- Add read-only Hub readiness helpers or reports that decode Hub config, `hub_authorizer`, `inventory_rebalancer`, lane count, paused state, and lane inventory.
- Document the staging operator prerequisites: which public key must be bound as Hub admin/authorizer/rebalancer, which env key supplies the orchestrator signer, and which checks prove staging can only dry-run until explicitly enabled.

Suggested files:

- `crates/loyal-actions/src/detection.rs`
- `crates/loyal-squads-policy-monitor/src/lib.rs`
- `crates/loyal-yield-orchestrator/src/store.rs`
- `crates/loyal-yield-orchestrator/src/types.rs`
- `crates/loyal-hub-cli/src/main.rs`
- `docs/render-worker-images.md`
- `docs/ask-1648-staging-hub-readiness.md`

Dependencies: none.

Required evidence:

- Local policy-detection tests cover a policy with `CrossMintLoyalHub` and persisted Hub `swap_lanes`.
- Store tests or DB readbacks show Hub route metadata survives `record_policy_match`.
- Read-only Hub config evidence shows current mainnet Hub admin, `hub_authorizer`, `inventory_rebalancer`, pause state, fee cap, and lane inventory.
- Staging secret boundary is documented without plaintext secrets.
- No PR code path sends a transaction or performs an admin binding.

Review risk:

- Route-mode name drift can make staging appear empty even when policies exist.
- Persisting incomplete `swap_lanes` would force the executor to rediscover policy shape from chain every run.

## PR 2: Add a Cross-Mint Loyal Hub Planner

Branch: `ASK-1648-cross-mint-hub-planner`

Title: `feat(yield-routing): plan Loyal Hub cross-mint routes`

Purpose: let the orchestrator decide when a Hub route is preferable or required, while preserving the current same-mint behavior.

Likely changes:

- Add a planner path beside `draft_same_mint_decision`, for example `draft_cross_mint_loyal_hub_decision`.
- Keep same-mint as the default first strategy unless a route policy, worker option, or feature flag enables Hub cross-mint planning.
- Require the active policy to include `cross_mint_loyal_hub` and a Hub `swap_lanes` entry before producing a Hub plan.
- Score source and target reserves across stable mints, using existing `stable_mints`, `kamino_markets`, `kamino_liquidity_mints`, and current positions.
- Represent Hub plans with a distinct `execution_plan.kind`, such as `cross_mint_loyal_hub`, including source reserve, target reserve, source mint, target mint, route amount semantics, Hub lane/authorizer, fee cap, and quote/fill assumptions.
- Add skip reasons or decision reasons that distinguish "Hub route not policy-allowed", "Hub not configured", "no Hub lane", "Hub fee too high", and "no cross-mint edge" instead of collapsing everything into `CrossMintOnly`.
- Keep `record_planned_rebalance_decision` usable for externally produced Hub plans if the first executor PR still prepares decisions outside the automatic planner.

Suggested files:

- `crates/loyal-yield-orchestrator/src/domain.rs`
- `crates/loyal-yield-orchestrator/src/types.rs`
- `crates/loyal-yield-orchestrator/src/store.rs`
- `crates/loyal-yield-orchestrator/migrations/*`
- `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs` or a new generic route monitor module

Dependencies: PR 1.

Required evidence:

- Unit tests prove same-mint planning is unchanged for same-mint candidates.
- Unit tests prove a cross-mint-only opportunity can produce a `cross_mint_loyal_hub` plan only when the policy and Hub lane metadata allow it.
- Unit tests prove Hub planning fails closed when the policy lacks `cross_mint_loyal_hub`, when `swap_lanes` lacks `loyal_hub`, or when amount semantics are unsupported.
- Migration/readback evidence shows any new decision reasons are idempotently available in `loyal_yield.decision_reason`.
- Planner output contains enough deterministic fields for an executor to verify the route before sending anything.

Review risk:

- Incorrect amount semantics across mints can turn collateral units into liquidity units.
- A planner that treats Hub as universally trusted can bypass the existing route-policy constraints.

## PR 3: Add a Dry-Run Loyal Hub Route Executor

Branch: `ASK-1648-hub-route-executor`

Title: `feat(yield-routing): dry-run Loyal Hub route execution`

Purpose: turn a persisted `cross_mint_loyal_hub` decision into a verifiable transaction plan, without enabling staging execution yet.

Likely changes:

- Add a new binary rather than stretching `same-mint-reserve-swap`, for example `loyal-hub-reserve-swap` or `cross-mint-hub-reserve-swap`.
- Reuse route-file generation from `loyal-hub-mainnet-route-files` for Kamino withdraw/deposit route legs.
- Reuse the working Hub swap construction from `scripts/mainnet-loyal-hub-tests.ts`, but move shared logic into a maintainable surface instead of making the E2E script a production dependency.
- Load a prepared decision whose `execution_plan.kind` is `cross_mint_loyal_hub`.
- Verify policy constraint indexes, active policy, delegated signer, route-mode, Hub authorizer, fee cap, source/target reserves, source/target mints, and amount semantics against the DB decision and on-chain policy account before simulation.
- Build the split route as withdraw -> Hub swap -> deposit and clear the Squads vault PDA signer bit where required.
- Produce a dry-run JSON result that includes transaction simulation outcome, expected token deltas, required signers, lookup-table coverage, and the exact reason execution was blocked if anything is missing.
- Keep live submission behind explicit flags and disabled from staging worker config until PR 4.

Suggested files:

- `crates/loyal-yield-orchestrator/src/bin/cross-mint-hub-reserve-swap.rs`
- `crates/loyal-yield-orchestrator/src/bin/loyal-hub-mainnet-route-files.rs`
- `crates/loyal-yield-orchestrator/src/store.rs`
- `crates/loyal-yield-orchestrator/src/types.rs`
- `scripts/mainnet-loyal-hub-tests.ts`
- `package.json`
- `Dockerfile.light-workers`

Dependencies: PR 1 and PR 2.

Required evidence:

- Local checks prove the new executor compiles in the light-worker image.
- A dry-run fixture or test proves a Hub decision cannot be executed as a same-mint decision.
- Simulation evidence proves the executor verifies route policy, Hub policy lane, signers, fee cap, and ALT coverage before any send path.
- Failure evidence includes policy missing, Hub lane missing, admin/authorizer mismatch, route-file mismatch, and lookup-table coverage missing.
- `scripts/mainnet-loyal-hub-tests.ts` remains usable as an E2E proof harness after shared logic is extracted.

Review risk:

- This is the highest-risk implementation slice because it is closest to transaction construction.
- Do not merge this PR if the executor can infer or mutate policy state to make execution pass.

## PR 4: Wire Staging Worker Strategy and Render Rollout

Branch: `ASK-1648-hub-staging-worker`

Title: `feat(yield-routing): enable Loyal Hub strategy in staging`

Purpose: run the Hub strategy in staging with explicit dry-run/allowlist controls and pinned worker images.

Likely changes:

- Add a generic route monitor or extend `same-mint-yield-monitor` behind an explicit strategy option such as `same_mint_kamino,cross_mint_loyal_hub`.
- Keep existing production `loyal-same-mint-yield-monitor` behavior unchanged.
- Add or modify only staging Render service config first. Staging should remain dry-run unless an explicit execution flag and staging allowlist are both present.
- Add env controls for enabled route strategies, enabled stable mints, execution mode, Hub program id, Hub config address if needed, and signer/authority expectations.
- Keep worker images pinned to immutable GHCR `sha-*` tags.
- Ensure `NEON_DATABASE_URL` points to staging Yield control-plane state for staging workers.
- Include a staging readiness gate that refuses to plan or simulate Hub routes if Hub admin/authorizer/rebalancer bindings do not match the staging orchestrator expectation.

Suggested files:

- `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs`
- `crates/loyal-yield-orchestrator/src/bin/cross-mint-hub-reserve-swap.rs`
- `render.yaml`
- `Dockerfile.light-workers`
- `docs/render-worker-images.md`
- `docs/ask-1648-staging-rollout.md`

Dependencies: PR 1, PR 2, and PR 3.

Required evidence:

- Render config diff proves production same-mint execution remains unchanged.
- Staging worker logs show the enabled strategies and `execute` state at startup.
- Staging DB readback proves Hub plans and dry-run outcomes write only to the staging `loyal_yield` schema.
- Read-only Hub config readback proves the staging orchestrator signer matches the configured Hub role, or the worker refuses to run Hub strategy with an explicit blocked status.
- Staging logs show no live transaction signatures when execution is disabled.
- A pinned staging worker image is built from the exact merged commit.

Review risk:

- Accidentally enabling `--execute` on staging without allowlists would make a shared-mainnet dry run become a live mainnet route.
- Render/image drift can make the repo diff look correct while staging runs an older binary.

## PR 5: Production Guardrails After Staging Evidence

Branch: `ASK-1648-hub-prod-guardrails`

Title: `chore(yield-routing): guard Loyal Hub production rollout`

Purpose: prepare production for a later, explicit Hub rollout without enabling it during ASK-1648 staging validation.

Likely changes:

- Add production-disabled config entries or docs that make the Hub strategy visible but off.
- Add alert/log fields for Hub strategy disabled, blocked, simulated, submitted, confirmed, and failed states.
- Add a post-staging checklist document with required evidence before production can enable `cross_mint_loyal_hub`.
- Add read-only dashboards or SQL snippets in docs if operators need them, but keep plaintext secrets out.

Suggested files:

- `render.yaml`
- `docs/render-worker-images.md`
- `docs/ask-1648-production-readiness.md`
- Optional observability/logging files touched by the monitor/executor

Dependencies: staging evidence from PR 4.

Required evidence:

- Production config shows Hub strategy disabled by default.
- Production worker startup logs prove the disabled state after deploy.
- Staging evidence is linked from the production readiness doc.
- No production route plans or live Hub signatures are created by this PR.

Review risk:

- This PR should not add new planner or executor behavior. If it does, split that work back into PR 2 or PR 3.

## Operational Prerequisites Outside PRs

These may require live transactions and must not be hidden inside implementation PRs:

- Bind or confirm the Hub `admin`, `hub_authorizer`, and `inventory_rebalancer` roles needed by the staging orchestrator key.
- Seed or rebalance Hub lane inventory for the staging route pairs.
- Confirm Hub fee cap, pause state, lane count, and inventory are compatible with staging tests.
- Confirm staging `NEON_DATABASE_URL`, `TIMESCALEDB_URL`, and signer envs are scoped to staging expectations.
- Confirm policy monitor ingestion is running against the staging Yield database if staging relies on live policy detection.

Each operational action should have a separate read-only preflight, an explicit approval step, and a read-only after-state proof. ASK-1648 planning should assume no operational write has happened until that proof exists.

## Dependency Graph

1. PR 1 establishes that the DB can see Hub-capable policies and that operators can prove Hub readiness.
2. PR 2 adds the planner, consuming the metadata from PR 1 but not executing.
3. PR 3 adds the simulator/executor, consuming planned Hub decisions from PR 2.
4. PR 4 deploys the dry-run strategy to staging with explicit Render/env gates.
5. PR 5 adds production rollout guardrails only after PR 4 produces staging evidence.

Parallel work:

- PR 1 readiness docs and PR 2 planner design can begin in parallel if route-mode naming is agreed early.
- PR 3 should wait for the `execution_plan.kind` and required fields from PR 2.
- PR 4 should wait for PR 3 because staging needs the real binary/image to verify.

## Single PR vs Multiple PRs

Multiple PRs are better.

A single PR is only defensible if ASK-1648 is reduced to "document staging readiness and do not deploy Hub orchestration." For the actual issue text, a single PR would be too broad because reviewers would need to validate policy ingestion, cross-mint planner math, Hub transaction construction, DB decision semantics, Render staging gates, and signer/admin readiness all at once.

The minimum viable reviewable stack is:

1. Ingestion/readiness.
2. Planner.
3. Dry-run executor.
4. Staging rollout.

Do not combine planner and executor unless the implementation is throwaway behind a disabled feature flag. Do not combine staging Render rollout with production enablement.

## Acceptance Criteria for ASK-1648 Staging

ASK-1648 staging is complete when all of these are true:

- Staging Yield DB has Hub-capable route policy rows with `cross_mint_loyal_hub` and usable `swap_lanes`.
- Hub mainnet config is read back and matches the expected staging orchestrator role bindings, or the worker refuses to plan Hub routes with a clear blocked reason.
- The planner can produce a `cross_mint_loyal_hub` decision for an eligible staging vault and can still choose same-mint for same-mint opportunities.
- The executor can simulate the split withdraw -> Hub swap -> deposit route from a prepared decision and prove policy/Hub/ALT/signature preconditions before any send path.
- The staging worker runs from a pinned image and records dry-run Hub outcomes only in staging `loyal_yield` state.
- Production worker behavior remains unchanged and Hub strategy remains disabled for production.
