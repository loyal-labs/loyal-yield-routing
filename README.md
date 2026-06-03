# Loyal Yield Routing

This repo experiments with yield-routing automation for Squads smart accounts.
It contains a small Next.js app shell, production-facing Loyal Action builders,
an on-chain Loyal Hub swap program, read/write orchestration storage, and a
LiteSVM test harness for proving delegated route policies.

At a high level, the system separates strategy, authorization, execution, and
verification:

- route planning decides whether a stablecoin route can be filled directly,
  through Loyal Hub inventory, through Jupiter, or through a combination of
  lanes;
- Loyal Actions construct narrow Squads `ProgramInteraction` policies for the
  delegated executor;
- external protocols and the Loyal Hub program still validate their own account
  relationships and token movements;
- Rust tests execute the resulting actions through LiteSVM, Squads, SPL Token,
  local protocol mocks, and the Loyal Hub SBF.

## Repo Map

| Path | Owns |
| --- | --- |
| `src/app` | Next.js App Router shell. The current page is intentionally minimal. |
| `src/features/yield-routing` | App-side domain helpers, currently the stable-swap lane planner. |
| `packages/loyal-actions` | TypeScript package for unsigned Loyal yield-route policy instruction builders. |
| `crates/loyal-actions` | Rust SDK for constructing delegated Squads route actions and Loyal Hub instructions. |
| `crates/loyal-hub-abi` | Generated Loyal Hub instruction tags, account indexes, data offsets, PDA seed bytes, and layout constants. Edit the schema, not generated output. |
| `crates/loyal-hub-swap-program` | Pinocchio SBF program that fills stablecoin swaps from lane-scoped Loyal Hub inventory. |
| `crates/squads-test-harness` | LiteSVM, Squads PDA/setup helpers, policy adapters, protocol seeding, and deterministic route scenarios. |
| `crates/mock-yield-protocols-program` | Test-only SBF mocks for Jupiter and Kamino behavior used by the harness. |
| `crates/loyal-yield-router` | Read-only TimescaleDB boundary for Kamino reserve updates and update streams. |
| `crates/loyal-yield-orchestrator` | Postgres-backed orchestration state, policy-match persistence, decision state transitions, and delegated signer loading. |
| `crates/loyal-squads-policy-monitor` | Helius websocket monitor that detects Loyal route-policy creations and emits or stores matches. |
| `scripts` | QEDGen/Kani verification wrappers and data-analysis helpers. |
| `docs` | Architecture notes, plans, verification reports, and Squads testing details. |

## Core Workflows

### App And TypeScript SDK

The Next.js app is Bun-managed and follows a vertical-slice structure. Keep
route files thin and put feature-specific UI, server code, domain logic, data
access, and integrations under `src/features/<feature-name>/`.

The TypeScript `@loyal/actions` package builds unsigned policy initialization
instructions for app or service consumers. It generates its Loyal Hub ABI mirror
from `crates/loyal-hub-abi/schema/loyal_hub_abi.schema` during package builds.

### Rust Action And Program Layer

The Rust `loyal-actions` crate owns production-facing route action
construction:

- Kamino withdraw/deposit actions scoped by whitelisted markets and liquidity mints
- swap actions scoped by whitelisted route mints
- all-in-one actions that can cover Kamino plus swap lanes

The Loyal Hub wire layout is centralized in `crates/loyal-hub-abi`; program
parsers, SDK builders, Squads policies, and verification tests should import
generated constants instead of duplicating byte offsets or account positions.

The `loyal-hub-swap-program` crate is the on-chain inventory leg. It does not
quote or choose routes. It validates the configured lane, mint pair, fee cap,
hub authorizer, canonical inventory accounts, and user vault accounts before
performing checked SPL Token transfers.

### Data And Orchestration

`loyal-yield-router` reads Kamino reserve history and live updates from the
existing Timescale schema. It should stay read-only and avoid strategy or
execution policy.

`loyal-yield-orchestrator` owns durable Postgres state for discovered policies
and route decisions. `loyal-squads-policy-monitor` streams Squads transactions
from Helius, detects Loyal route-policy creation, and writes matches through
the orchestrator store when a Neon URL is provided.

## Development

Install dependencies with Bun:

```bash
bun install
```

Run the Next.js app:

```bash
bun dev
```

Run the frontend build or lint checks:

```bash
bun run build
bun run lint
```

Build or validate the TypeScript action package:

```bash
bun run loyal-actions:build
bun run loyal-actions:typecheck
bun run loyal-actions:test
```

Run focused Rust checks for crates that do not need SBF builds:

```bash
cargo test -p loyal-yield-router
cargo test -p loyal-yield-orchestrator -p loyal-squads-policy-monitor
cargo test -p loyal-hub-abi -- --nocapture
```

Use the mounted 1Password env file for local non-critical secrets. Store the
delegated yield router signer as `YIELD_ROUTER_KEYPAIR`, using a hex
encoded private key. The orchestrator accepts either a 32-byte private seed or a
64-byte Solana keypair encoded as hex, and exposes
`yield_router_keypair_from_env()` so transaction code can load the signer
without writing key material to disk or logs.

## Squads Policy Monitor

Run the Helius Squads policy monitor with the Neon-backed sink:

```bash
op run --env-file=.env.1password -- sh -c 'cargo run -p loyal-squads-policy-monitor -- --postgres-url "$NEON_DATABASE_URL"'
```

The monitor also reads `NEON_DATABASE_URL` directly when `--postgres-url` is omitted.

For Rust SQLx validation against Neon, set `DATABASE_URL` from the same direct
Neon URL. Avoid the pooled `-pooler` URL for these tests because SQLx prepared
statements need a stable backend connection.

```bash
op run --env-file=.env.1password -- sh -c 'DATABASE_URL="$NEON_DATABASE_URL" cargo test -p loyal-yield-orchestrator -p loyal-squads-policy-monitor'
```

## Loyal Hub Verification

Treat `crates/loyal-hub-abi/schema/loyal_hub_abi.schema` as the byte-layout
source of truth and
`crates/loyal-hub-swap-program/verification/loyal_hub_swap.qedspec` as the
behavioral source of truth.

Run the ABI/spec drift gate whenever the schema or QEDGen spec changes:

```bash
bun run verify:hub-abi-spec-drift
```

Run the active QEDGen verification bundle with:

```bash
bun run verify:qedgen
```

Additional Kani wrappers are available through `bun run verify:qedgen:kani`,
`bun run verify:qedgen:kani-impl`, and `bun run verify:hub-kani-impl`.

## Squads Tests

Run the lean Squads test suite:

```bash
bun run test:squads
```

Run the ignored historical Kamino replay:

```bash
bun run test:squads:e2e
```

Run the ignored Loyal Hub hindsight replay:

```bash
bun run test:squads:hub-hindsight
```

The action SDK lives in `crates/loyal-actions`. The Squads test crate lives in `crates/squads-test-harness` and consumes the SDK through small test adapters:

```rust
let route_action_setup = create_three_step_yield_route_actions(
    loyal_action_context(context, wallet_b.pubkey()),
    yield_route_universe_from_mock_reserves(
        vec![USDC_MINT, PYUSD_MINT],
        vec![main_usdc, prime_usdc, main_pyusd],
    ),
    vec![mock_jupiter_swap_lane(true)],
    YieldRouteActionSeeds::default(),
)?;
```

The SDK returns delegated action accounts, create instructions, and named route actions. Route tests build executable Squads instructions through the fluent action surface instead of assembling Squads constraint indexes directly:

```rust
let deposit_ix = route_action_setup
    .deposit()?
    .build(delegated_signer, vault_index, deposit_instructions, deposit_accounts);
```

Swap actions use typed execution arguments, for example `.jupiter()?.build(JupiterSwapExecution { ... })` or `.hub()?.build(HubSwapExecution { ... })`. Swap-only tests can use `create_swap_yield_route_action()`.

Loyal Hub lane-load tests live under `crates/squads-test-harness/tests/loyal_hub_lane_simulation.rs`. That test module keeps its simulation support local: LiteSVM, Squads execution, SPL Token accounts, Loyal Actions, and the Hub SBF still run normally, while the support code derives expected balances, lane metrics, scheduling conflicts, and planner output from recorded simulation events.

### Test Crate Map

The Squads test crate is grouped by domain modules for onboarding.

| Module | Owns |
| --- | --- |
| `squads` | Squads PDA derivation, settings setup, smart-account instructions, payload basics |
| `runtime` | LiteSVM setup, funded contexts, program loading, heap-frame helpers, transaction sending |
| `policies` | Raw Squads policy families |
| `actions` | Adapters from funded contexts and mock reserves into `loyal-actions` inputs |
| `policies/program_interaction` | Low-level historical Squads ProgramInteraction helpers |
| `protocols` | Mock Jupiter/Kamino/Loyal Hub instruction data, SPL account seeding, SBF mock loading |
| `types` | Shared public test structs and crate-private Squads wire types |

New scenario tests can use `squads_test_harness::prelude::*` for runtime/mock helpers and import action builders from `loyal_actions`. Keep route action construction in `crates/loyal-actions`; keep mock protocol state in `protocols`.

See `docs/squads-testing.md` and `docs/plans/squads-yield-routing-policy.md` for the policy model and test coverage.
