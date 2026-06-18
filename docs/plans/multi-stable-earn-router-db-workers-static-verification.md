# Multi-Stable Earn Router Static Verification Snapshot

Date: 2026-06-18

This snapshot records the current verifier status for
`multi-stable-earn-router-db-workers-verifier.md`. It includes local static
evidence plus live staging/production database readbacks and same-mint monitor
dry-runs. It does not claim overall PASS because image build, deploy, and
post-deploy log evidence are not present yet.

## Static Evidence Commands

```sh
git diff --name-only HEAD
git status --short
```

Result: implementation files are limited to worker/orchestrator/router surfaces
plus the autodeposit executor/test surface, the new migration, and verifier
docs. Two pre-existing staged docs
(`docs/plans/prod-staging-service-split-verification-run.md` and
`docs/render-worker-images.md`) are present in the worktree but are not part of
this verifier implementation. Use `git status --short` as the complete changed
file inventory because `git diff --name-only HEAD` omits untracked verifier and
migration files. No policy SDK, on-chain policy, ABI schema, route policy
builder, account-position, seed-layout, or policy constraint file is in the
implementation diff. The `loyal-squads-policy-monitor` diff is storage
ingestion compatibility only: it fills the new generic balance-sweep target and
execution columns from existing USDC policy-monitor events. The modified
`scripts/execute-autodeposit-policy.test.ts` file adds a source-level assertion
for root smart-account deposit recording, but it was not executed in this
static-only pass.

```sh
git diff --check
```

Result: PASS.

```sh
rg -n "safe_usdc|load_safe_usdc|no_fresh_safe_usdc|neonAllowsUsdc|SourceMintMismatch|USDC liquidity" crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs crates/loyal-yield-orchestrator/src/store.rs crates/loyal-yield-router/src/timescale/mod.rs
```

Result: no matches in implementation files.

```sh
rg -n "SUPPORTED|ENABLED|CASH|USDG|PYUSD|USDC|USDT|USDS|safe_stable|candidate_counts|eligible|liquidity_mint|kamino_liquidity_mints|stable_mints" crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs crates/loyal-yield-router/src/timescale/mod.rs
```

Result: PASS - the monitor imports all six supported stablecoin constants,
defaults `EARN_ROUTER_ENABLED_STABLE_MINTS` to the six-mint universe, loads
Safe candidates per enabled mint, filters by policy stable/Kamino mint arrays,
and emits per-mint candidate/eligible counts.

```sh
rg -n "expected-liquidity-mint|TargetMintMismatch|liquidity_mint|source_liquidity_mint|target_liquidity_mint|vault.*ata|stable_mints|kamino_liquidity_mints" crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs
```

Result: PASS - optimization-cycle execution now requires the expected planned
mint, validates source/target decision mint fields, validates the selected
policy allows the planned stable/Kamino mint, and derives the vault liquidity
ATA from that planned mint.

```sh
rg -n "DROP NOT NULL|must be nullable|NULLIF|COALESCE|source_mint|source_wallet_token_ata" crates/loyal-yield-orchestrator/migrations/0006_generic_balance_sweep_token_accounts.sql crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs crates/loyal-yield-orchestrator/src/store.rs scripts/execute-autodeposit-policy.ts
```

Result: PASS - migration, schema validation, store read/write paths, pending
surplus-lot view, and TypeScript autodeposit target loading all expose the
generic token-account shape while preserving nullable legacy compatibility
fields.

```sh
rg -n "USDC_MINT|token_mint|wallet_token_ata|vault_token_ata|balance_sweep_wallet_balance_events|balance_sweep_lot_claim_items|source_event_id|target_id|mint" crates/balance-sweep-autodeposit-trigger/src/main.rs
rg -n "tokenMint|walletTokenAta|vaultTokenAta|prepareEarnUsdcAutodepositPull|expectedUsdcMint|recordPullExecution|source_event_id|balance_sweep_lot_claim_items|token_mint|wallet_token_ata|vault_token_ata|USDC_MINT" scripts/execute-autodeposit-policy.ts
```

Result: PASS - the Rust trigger and TypeScript executor read/write the generic
target/current/event/execution columns while retaining explicit USDC target
guards. Lot claim, completion, release, current balance, and depletion queries
join back through target and source-event mint guards.

```sh
rg -n "user_yield_positions|deposit_mint|initial_liquidity_mint|current_liquidity_mint|holding|topUpLiquidityMint|pull\\.persistence\\.liquidityMint" scripts/execute-autodeposit-policy.ts crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs
```

Result: PASS - autodeposit still records user yield position mint fields and
holding events, and top-up execution checks the existing/current liquidity mint
against the pulled USDC mint before running the same-mint top-up.

```sh
rg -n "same-mint-yield-monitor|same-mint-reserve-swap|balance-sweep-autodeposit-trigger|balance-sweep-ata-monitor|balance-sweep-ata-projector|execute-autodeposit-policy|yield-migrations|worker-images|Dockerfile\\.light-workers|Dockerfile\\.laserstream-workers" Dockerfile.light-workers Dockerfile.laserstream-workers .github/workflows/worker-images.yml docs/render-worker-images.md render.yaml package.json
```

Result: PASS - affected worker binaries/scripts and `yield-migrations` are
included in the existing worker images, and Render service definitions use
image-runtime commands with migration predeploy hooks for every affected
schema-reading worker, including the laserstream ATA monitor and light workers.

## Verdict

No Policy Update Surface: STATIC PASS - implementation diff does not touch
policy SDK, on-chain policy, ABI schema, route-policy builder, account-position,
seed-layout, or policy constraint files. Existing active policy rows remain the
source of truth in worker code. Policy-monitor changes only populate generic
database columns from already-observed USDC balance-sweep policy/execution
events.

Database Migration Shape: STATIC PASS - migration
`0006_generic_balance_sweep_token_accounts.sql` adds generic target/current/event
and execution columns, preserves legacy USDC columns, backfills existing rows,
and rewrites current wallet balance primary key to `(target_id, mint)`.
`yield-migrations.rs` wires migration version 6 and validates the required
columns, nullable legacy compatibility columns, and the current-balance primary
key. The raw `OrchestratorStore::apply_migrations` path also includes migration
6, so local/admin executor flows that invoke store migrations see the same
schema shape. The migration discovers and drops the actual current primary-key
constraint name before adding the mint-keyed primary key, and it raises before
constraint changes if duplicate `(target_id, mint)` current rows exist. Legacy
USDC columns remain readable, but can be null for future non-USDC rows where
generic token-account columns carry the authoritative value; store writes treat
empty legacy compatibility strings as SQL null and read back legacy string
fields from generic token-account columns when needed. The TypeScript
autodeposit executor also coalesces legacy target ATAs from generic token ATAs
before applying the explicit USDC-only guard, so future non-USDC targets fail
closed for mint support rather than nullable legacy fields. The verifier
includes concrete staging/prod readback SQL for duplicate rows, the USDC generic
backfill, nullable legacy columns, pending surplus-lot source mint columns, and
the current-balance primary key. The pending surplus-lot view preserves the
existing view column order and appends `source_mint` and
`source_wallet_token_ata`, which keeps `CREATE OR REPLACE VIEW` compatible with
Postgres' existing-column order requirement.

Same-Mint Monitor Is Mint Generic: STATIC PASS - monitor defaults enabled mints
to CASH, USDG, PYUSD, USDC, USDT, and USDS, supports
`EARN_ROUTER_ENABLED_STABLE_MINTS`, loads Safe-basket candidates per enabled
mint, filters fleet discovery by active vault, active policy, delegated signer,
same-mint route mode, nonempty Kamino markets, and enabled-mint overlap across
both stable and Kamino liquidity mint arrays. Explicit vault mode remains
operator-selected and returns per-vault skip diagnostics when the active policy
is not route- or mint-eligible. Per-vault planning explicitly skips policies
missing the same-mint route mode, reconciles from policy-eligible candidates,
and emits enabled/candidate/eligible counts by mint, skipped mints, and planned
source/target liquidity mints. Recent-rebalance cooldown diagnostics include
the confirmed decision liquidity mint fields.

Planner Chooses Across Mints Safely: STATIC PASS - planner iterates routeable
positions, only compares same-liquidity-mint targets, and chooses the best
candidate across mints by edge, target APY, amount, and stable tie-breaks.
Freshly written decisions persist row-level `liquidity_mint`,
`source_liquidity_mint`, and `target_liquidity_mint`, and the JSON
`execution_plan` also records those liquidity-mint fields. The planned-decision
recording path now fails closed if source and target liquidity mints differ or
if the execution-plan mint fields are missing or do not match the row fields.

Same-Mint Executor Accepts Planned Mint: STATIC PASS - optimization-cycle
execution requires `--expected-liquidity-mint`, validates source/target reserve
mints against the planned mint, derives vault liquidity ATAs from the planned
mint, validates newly written execution-plan mint fields when present while
remaining compatible with older plans that only recorded `liquidity_mint`, and
keeps USDC-specific aliases for setup/deposit/full-withdraw paths. The executor
also keeps policy preflight generic by checking the planned mint against both
`stable_mints` and `kamino_liquidity_mints` rather than checking only USDC.

Autodeposit Remains USDC While Reading Generic Schema: STATIC PASS - Rust
trigger and TypeScript executor read generic target/token columns while
requiring USDC. The TypeScript executor fails closed if the SDK USDC target mint
differs from its SQL helper guard. Lot selection/completion/release joins back
to source wallet events and requires event mint to match target mint; Rust claim
completion/release and TypeScript claim completion only advance claim status
when mint-matched lots are still present, so completion is safe to retry after
idempotent execution-lot insertion. Claim item reads also require the source lot
to belong to the claim target. Current-balance runtime reads are mint-scoped:
projection reads by `(target_id, mint)`, and autodeposit joins by
`balance.target_id = target.id` plus `balance.mint = target.token_mint` before
the USDC target guard. ATA monitor reads active targets, filters to USDC target
mints, and maps subscriptions from the generic token ATA columns for this
verifier. User yield position and holding-event persistence continues to use
`deposit_mint`, `initial_liquidity_mint`, and `current_liquidity_mint`; top-ups
fail closed when the existing current liquidity mint differs from the pulled
USDC mint.

Worker Image Packaging Shape: STATIC PASS - Dockerfiles and `worker-images`
workflow include the affected binaries/scripts and publish immutable
`sha-<commit>` image tags. The laserstream image also contains
`yield-migrations`; every changed worker that reads or writes the Yield
control-plane schema now has a Yield migration predeploy gate. The ATA monitor
services also retain the existing Timescale migration gate before startup.
Render config currently uses pinned image-runtime references, not Render Docker
builds. This does not prove new images were built or deployed; that remains
part of the external CI/deploy gates below. The verifier now includes structured
CI image and Render service readback fields for those external gates.

Staging Database And Worker Verification: PASS - staging was checked before
apply with migrations through version 5 and generic columns absent, then
`yield-migrations --check` correctly reported one pending migration. Staging
`yield-migrations --apply` applied migration 6
`generic_balance_sweep_token_accounts`, and a follow-up check reported all
migrations up to date. Staging readback showed 20 balance-sweep targets, 20
generic USDC target backfills, 12 generic current-balance USDC backfills, zero
duplicate `(target_id, mint)` current rows, current-balance primary key
`{target_id,mint}`, nullable legacy compatibility columns, and pending
surplus-lot `source_mint` / `source_wallet_token_ata` columns present. Staging
same-mint monitor dry-run with `--once --all-active-vaults` returned
`execute: false`, all six enabled stablecoin mints, 18 total candidates, per
mint candidate counts for CASH, USDG, PYUSD, USDC, and USDS, USDT in
`skippedMints`, and no discovered vaults/results.

Production Database And Worker Deployment: PARTIAL PASS - production was
checked before apply with migrations through version 5, 27 balance-sweep
targets, and generic columns absent. Production `yield-migrations --apply`
applied migration 6, and production `yield-migrations --check` now reports all
migrations up to date. Production schema readback showed 27 balance-sweep
targets, 27 generic USDC target backfills, 20 generic current-balance USDC
backfills, zero duplicate `(target_id, mint)` current rows, current-balance
primary key `{target_id,mint}`, and pending surplus-lot generic source columns
present. Production same-mint monitor dry-run with `--once --all-active-vaults`
returned `execute: false`, all six enabled stablecoin mints, 16 total
candidates, per-mint candidate counts for CASH, USDG, PYUSD, USDC, and USDS,
USDT in `skippedMints`, and no discovered vaults/results. This section remains
partial because new pinned worker images have not yet been built/deployed and
post-deploy logs have not yet been inspected.

Local Static Checks: PASS - static checks above were run. Cargo, Bun, and
autodeposit test commands, including the modified autodeposit test file, were
intentionally not run because the operator requested no testing for this pass.

Overall Verdict: FAIL - image build/deploy and post-deploy log evidence remain
required before the verifier can pass.
