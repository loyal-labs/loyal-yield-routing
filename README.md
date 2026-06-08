# Loyal Yield Routing

This repo experiments with yield-routing automation for Squads smart accounts. The current implementation centers on `loyal-actions`, a Rust SDK for constructing delegated route actions:

- Kamino withdraw/deposit actions scoped by whitelisted markets and liquidity mints
- swap actions scoped by whitelisted route mints
- all-in-one actions that can cover Kamino plus swap lanes

The Rust tests keep Squads authorization separate from protocol validation. Squads bounds the delegated signer to the vault, approved markets/mints, route mints, and instruction discriminators. Each external protocol still validates its own account relationships.

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

## Yield Policy Initialization

Create a Squads smart account for a user keypair, install the all-in-one Loyal
yield-routing policy, and upsert the resulting `route_policies` /
`managed_vaults` rows into Neon:

```bash
op run --env-file=.env.1password -- sh -c 'bun run yield-policy:init -- -k /path/to/user.json'
```

The command reads the user's Solana CLI-style keypair file from
`-k/--keypair <KP_FILE>`. It reads `SOLANA_RPC_URL`, `NEON_DATABASE_URL`, and
`YIELD_ROUTER_KEYPAIR` from the mounted 1Password environment. The router
keypair env accepts a 32-byte private seed or 64-byte Solana keypair encoded as
hex, base58, base64, or a Solana keypair JSON array. The initializer defaults
the persisted cluster label to `mainnet`; pass `--cluster devnet` with a devnet
RPC for testing. The created smart account uses threshold `1` with the user as
its only full-permission Squads signer. The user keypair signs smart-account
creation and policy creation; the installed yield-routing policy has the yield
router keypair as its single delegated signer, matching `loyal-actions`
behavior. The initializer submits a split setup sequence: one smart-account
creation transaction followed by one policy-creation transaction each for the
withdraw, swap, and deposit route actions. Each transaction gets its own
compute-budget setup and is checked against the Solana packet size limit before
submission.

Use `--dry-run` to sign locally, simulate over RPC, and print the derived
settings, vault, policy account, allowlists, route modes, transaction size,
fee estimate, rent estimate for newly created accounts, estimated total
lamports, payer balance, compute units, and simulation logs without submitting
the transaction or writing Neon rows. When the script is creating a new smart
account, dry-run simulates the smart-account creation and reports fee/size
estimates for the dependent policy transactions, but it skips policy
transaction simulation because Solana RPC simulation does not persist the
simulated smart-account account across later transactions.

```bash
op run --env-file=.env.1password -- sh -c 'bun run yield-policy:init -- -k /path/to/user.json --dry-run'
```

`mainnet` and `devnet` currently use the repo's shared hardcoded Loyal cluster
config from `packages/loyal-actions`: Squads smart-account, Jupiter V6, Loyal
Hub swap, Loyal Hub authorizer, and Kamino Lend program IDs are the same for
both cluster labels. Pass `--loyal-hub-authorizer <PUBKEY>` only when using the
Loyal Hub lane with a different configured Hub authorizer.

For Rust SQLx validation against Neon, set `DATABASE_URL` from the same direct
Neon URL. Avoid the pooled `-pooler` URL for these tests because SQLx prepared
statements need a stable backend connection.

```bash
op run --env-file=.env.1password -- sh -c 'DATABASE_URL="$NEON_DATABASE_URL" cargo test -p loyal-yield-orchestrator -p loyal-squads-policy-monitor'
```

## Kamino Timescale Migrations

Kamino market data lives in the separate `kamino_timescale` Neon database. Its
schema is managed by the Rust SQLx migration runner in
`crates/kamino-timescale-migrations`.

```bash
op run --env-file=.env.1password -- sh -c 'bun run timescale:migrate'
```

Use `bun run timescale:migrate:check` in the same wrapper to verify that no
migrations are pending.

The runner reads `TIMESCALEDB_URL` from 1Password, applies checked-in SQL files
under `crates/kamino-timescale-migrations/migrations`, and records applied
versions in `kamino.schema_migrations`.

## Squads Tests

Run the lean Squads test suite:

```bash
bun run test:squads
```

Run the ignored historical Kamino replay:

```bash
bun run test:squads:e2e
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
| `policies/program_interaction` | Low-level compact Squads ProgramInteraction helpers |
| `protocols` | Mock Jupiter/Kamino/Loyal Hub instruction data, SPL account seeding, SBF mock loading |
| `types` | Shared public test structs and crate-private Squads wire types |

New scenario tests can use `squads_test_harness::prelude::*` for runtime/mock helpers and import action builders from `loyal_actions`. Keep route action construction in `crates/loyal-actions`; keep mock protocol state in `protocols`.

See `docs/squads-testing.md` and `docs/plans/squads-yield-routing-policy.md` for the policy model and test coverage.
