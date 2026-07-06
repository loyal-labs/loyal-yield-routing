# ASK-1648 Staging Hub Readiness

Recorded: 2026-07-06

This PR only makes Loyal Hub route-policy readiness explicit. It does not add a
cross-mint planner, executor, staging Render worker, Hub admin mutation, DB
operator write, or live transaction path.

## Persisted Policy Contract

`loyal_yield.route_policies.route_modes` uses canonical strategy names:

- `same_mint_kamino`
- `cross_mint_loyal_hub`

Fresh policy-monitor ingestion normalizes legacy `same_mint` to
`same_mint_kamino`. Same-mint readers remain tolerant of older rows that still
contain `same_mint` so staging does not appear empty while historical rows wait
to be re-seen.

Hub-capable policies must include `cross_mint_loyal_hub` and at least one
`swap_lanes` entry with:

- `kind = loyal_hub`
- `hub_authorizer`
- `max_fee_bps`
- `action_account`
- `instruction_constraint_indexes`

The instruction indexes identify the protected withdraw, Hub swap, and deposit
constraints needed by a later executor. This PR only persists and reads those
values; it does not execute them.

## Readiness Boundary

A route policy is Hub-ready for the next ASK-1648 PR only when readback shows:

- `route_modes` supports `cross_mint_loyal_hub`
- a `loyal_hub` swap lane is present
- the lane includes the policy action account
- the lane includes three instruction constraint indexes

This is policy/readiness evidence, not operational readiness. Staging rollout
still requires separate read-only proof of Hub config, authorizer/rebalancer
bindings, lane inventory, staging DB/env scope, and dry-run worker behavior.

## Operator Prerequisites

Before a staging worker can plan or simulate Hub routes, operators should verify
without printing secret values:

- which staging signer is expected to act as the route delegated signer
- whether Hub `admin`, `hub_authorizer`, and `inventory_rebalancer` match the
  intended staging authority boundary
- whether lane inventory exists for the staging route pairs
- whether staging `NEON_DATABASE_URL` and `TIMESCALEDB_URL` point at staging
  control-plane and ATA state
- whether the worker command still omits live execution flags

Any Hub role binding, lane inventory seeding, Render rollout, or DB repair is an
operator action outside this PR.
