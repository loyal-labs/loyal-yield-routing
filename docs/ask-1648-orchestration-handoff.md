# ASK-1648 Orchestration Handoff

Recorded: 2026-07-06

Issue: `ASK-1648 Deploy Loyal Hub to staging`

## Proposed Solution

Implement Loyal Hub cross-mint routing as a staged extension of
`loyal-yield-orchestrator`, not as an immediate live staging transaction path.
The safe shape is:

- keep current same-mint behavior unchanged;
- make Hub-capable policy metadata and Hub readiness explicit;
- add a `cross_mint_loyal_hub` planner that produces its own
  `execution_plan.kind`;
- add a dry-run/simulation-first Hub executor for
  withdraw -> Hub swap -> deposit;
- wire staging as dry-run/blocked-unless-ready before any execution rollout.

The implementation should reuse existing Hub surfaces where possible:
`crates/loyal-actions`, `crates/loyal-hub-cli`,
`scripts/mainnet-loyal-hub-tests.ts`, and
`crates/loyal-yield-orchestrator/src/bin/loyal-hub-mainnet-route-files.rs`.

## Current Evidence

- Linear `ASK-1648` asks for integrating `loyal-hub-swap-program` into
  `loyal-yield-orchestrator`, adding a cross-mint strategy through Loyal Hub,
  and rolling it out to staging first.
- `docs/ask-1648-problem-analysis.md` records code-path evidence and a
  read-only mainnet Hub state probe.
- `docs/ask-1648-subtask-decomposition.md` records recommended PR boundaries.
- Read-only Render evidence from `render services --output json` shows there is
  no Hub orchestration staging worker today. Staging same-mint uses
  `loyal-same-mint-yield-monitor-staging` on pinned image
  `light-workers:sha-3668df10c02c23e3aff5a7be70c34500db6bdd96` and omits
  `--execute`.
- Read-only Render logs for `srv-d8plrj8js32c738s2f80` on 2026-07-06 show
  `execute: false`, `status: fleet_poll`, and `discoveredVaultCount: 0`.
- Read-only Hub state via `bun run hub:cli -- --url m --json state` shows Hub is
  initialized and unpaused, with `max_fee_bps: 50`, `lane_count: 4`, and
  `admin`, `hub_authorizer`, and `inventory_rebalancer` all set to
  `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`.
- The local `.env.1password` does not expose `NEON_DATABASE_URL`,
  `TIMESCALEDB_URL`, `SOLANA_RPC_URL`, or `YIELD_ROUTER_KEYPAIR`. Direct
  `op run --environment zspmwsfuhomrlffpqp6wk7fbdu` access failed because the
  local 1Password CLI could not connect to the desktop app. Therefore current
  thread did not obtain fresh Neon/Timescale readbacks.

## High-Level Plan

1. Land policy/readiness groundwork first. Make route-mode names, Hub lane
   metadata, and Hub config readiness checks explicit without sending
   transactions.
2. Add planner support next. Produce `cross_mint_loyal_hub` plans only when the
   active policy and Hub metadata allow it, and keep same-mint planning
   unchanged.
3. Add dry-run executor support. Build and simulate the protected route from a
   prepared Hub decision, with fail-closed checks for policy, Hub config,
   inventory, amount semantics, packet size, and lookup-table coverage.
4. Wire staging last. Add or adjust a staging worker in dry-run/blocked mode
   using pinned GHCR worker images. Do not enable live execution as part of the
   first rollout.
5. Treat Hub admin binding, inventory seeding, and any live route execution as
   separate operator-approved actions with read-only before/after proof.

## PR Split Decision

Use multiple easy-to-review PRs.

A single PR would mix policy ingestion, planner math, transaction construction,
DB decision lifecycle, Render staging changes, and signer/admin readiness. The
minimum reviewable stack is:

1. `feat(yield-routing): persist Loyal Hub route readiness`
2. `feat(yield-routing): plan Loyal Hub cross-mint routes`
3. `feat(yield-routing): dry-run Loyal Hub route execution`
4. `feat(yield-routing): enable Loyal Hub strategy in staging`

Start with PR 1. Do not touch existing PRs or branches that may implement a
similar fix.

## Thread Instructions

Implementation and review threads should use Codex threads, not subagents. PR
descriptions and review comments should format code names in backticks and
should not include verification command examples.
