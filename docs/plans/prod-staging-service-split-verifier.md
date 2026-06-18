# Production/Staging Service Split Verifier

Use this document as the fixed verifier for separating production and staging
Yield services while keeping shared infrastructure intentionally shared. Do not
treat it as an implementation checklist. The implementation passes only when a
skeptical runner can verify every required condition below from repo files,
Render service configuration, Neon branch configuration, database readbacks, and
worker logs.

## Goal

Production and staging may both serve the Loyal Earn/yield-routing product, but
staging must be able to change Yield schemas, policy rows, worker behavior, and
experimental routing state without mutating the production Yield control plane
or production execution workers.

The target model is:

- `NEON_DATABASE_URL` is the Yield control-plane boundary and must be split into
  production and staging Neon branches.
- `TIMESCALEDB_URL` remains one shared physical TimescaleDB for Kamino market
  data, but Loyal ATA telemetry must use separate production and staging
  Timescale tables or schemas inside that database.
- The main Loyal smart-account/product database, usually `DATABASE_URL` in
  `loyal-apps`, remains shared and is not migrated or forked by this work.
- Workers that read or write Yield control-plane state are split by environment.
- Workers that only ingest shared Kamino market data stay single/shared.
- Any staging worker that can send transactions must be dry-run, disabled, or
  constrained to explicit staging/test accounts until it can prove it cannot
  affect production users or production policies.

Overall PASS is impossible if staging can write production `loyal_yield` rows,
if production workers can consume staging-only Yield rows, or if staging
experiments can execute against broad production accounts through the shared main
smart-account database.

## Expected Split Matrix

| Surface | Current repo evidence | Target decision | Why |
| --- | --- | --- | --- |
| Yield Neon control-plane DB | `NEON_DATABASE_URL`; `loyal_yield.*`; `yield-migrations` | Split: production branch and staging branch | Owns policies, vaults, balance-sweep targets, executions, current positions, APY snapshots, migration ledger, and experimental schemas. |
| Main Loyal smart-account/product DB | External `loyal-apps` `DATABASE_URL` | Keep shared | User/account product state is intentionally not part of this split. Staging execution must compensate with dry-run or allowlists. |
| Kamino Timescale market data | `TIMESCALEDB_URL`; `kamino.*`; `loyal-kamino-reserve-monitor` | Keep shared | Reserve/APY data is chain-derived market data and should feed both environments. |
| Loyal ATA Timescale telemetry | `TIMESCALEDB_URL`; current `loyal.balance_sweep_wallet_ata_observations` | Split tables/streams per env inside shared TimescaleDB | Rows contain branch-local Yield `target_id`s and are projected into `NEON_DATABASE_URL`; production and staging must not share one raw ATA stream. |
| `loyal-kamino-reserve-monitor` | Render worker; `TIMESCALEDB_URL` only | Keep single/shared | Ingests Kamino reserve data and supported reserves; does not touch Yield Neon. |
| `loyal-balance-sweep-ata-monitor` | Render worker; `NEON_DATABASE_URL` + `TIMESCALEDB_URL` | Split production/staging | Reads active Yield targets and writes Earn APY snapshots to Yield Neon. Also writes ATA observations that must be environment-safe. |
| `loyal-balance-sweep-ata-projector` | Render worker; `NEON_DATABASE_URL` + `TIMESCALEDB_URL` | Split production/staging | Projects raw ATA telemetry into branch-specific Yield current balance/event rows. |
| `loyal-balance-sweep-autodeposit-trigger` | Render worker; `NEON_DATABASE_URL`; executor command | Split production/staging | Claims lots, writes executions, and can execute policy-mediated pulls. Staging must be dry-run or allowlisted. |
| `loyal-same-mint-yield-monitor` | Render worker; `NEON_DATABASE_URL` + `TIMESCALEDB_URL` + `YIELD_ROUTER_KEYPAIR` | Split production/staging | Reads branch-specific active vaults/policies, writes rebalance decisions, and can execute routes. Staging must be dry-run or allowlisted. |
| `loyal-squads-policy-monitor` | Crate/README command; `NEON_DATABASE_URL` | Split if deployed | Writes policy and balance-sweep execution rows to Yield Neon. Production must not ingest staging-only policy activity as executable production state. |
| Manual scripts/binaries | `same-mint:*`, `yield:migrate`, `autodeposit:execute` | Environment-selected by env file/Render env | Any command that reads `NEON_DATABASE_URL` must be run against the intended branch only. |

## Required Checks

### 1. Repo Inventory

PASS only if repo inspection finds the same worker/database ownership described
above, or this verifier is intentionally revised before implementation work
continues.

Required local evidence:

```sh
rg -n "name: loyal-|NEON_DATABASE_URL|TIMESCALEDB_URL|dockerCommand|preDeployCommand" render.yaml docs/render-worker-images.md
```

```sh
rg -n "env = \"NEON_DATABASE_URL\"|env::var\\(\"NEON_DATABASE_URL\"\\)|env = \"TIMESCALEDB_URL\"|env::var\\(\"TIMESCALEDB_URL\"\\)" crates scripts
```

Required result:

- `loyal-kamino-reserve-monitor` uses `TIMESCALEDB_URL` and not
  `NEON_DATABASE_URL`.
- Every Render worker except `loyal-kamino-reserve-monitor` uses
  `NEON_DATABASE_URL` directly or through an executor/predeploy command.
- `yield-migrations` reads `NEON_DATABASE_URL`.
- `loyal-timescale-migrations` reads `TIMESCALEDB_URL`.
- `render.yaml` must not introduce app/product `DATABASE_URL` as the Yield
  worker state store.

### 2. Neon Branch Isolation

PASS only if production and staging have distinct Yield Neon branches and every
split service is bound to the correct branch.

Required evidence:

- The production Yield branch and staging Yield branch have distinct Neon branch
  names or IDs recorded in an operator-safe place. Do not put plaintext
  connection strings in this repo, logs, or chat.
- A secret-safe fingerprint readback proves the production and staging
  `NEON_DATABASE_URL` values differ.
- `bun run yield:migrate:check` passes against production.
- `bun run yield:migrate:check` passes against staging, or staging intentionally
  reports pending experimental migrations that are absent from production.
- A staging-only schema probe can be created in staging and is absent from
  production.

Example fingerprint command shape:

```sh
op run --env-file=.env.1password -- sh -c 'printf %s "$NEON_DATABASE_URL" | shasum -a 256 | cut -c1-16'
```

Example migration check shape:

```sh
op run --env-file=<prod-env-file> -- sh -c 'bun run yield:migrate:check'
op run --env-file=<staging-env-file> -- sh -c 'bun run yield:migrate:check'
```

Example isolation probe:

```sql
CREATE TABLE IF NOT EXISTS loyal_yield.staging_isolation_probe (
    id BIGINT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Overall PASS requires showing the probe exists only in staging.

### 3. Render Service Shape

PASS only if Render has separate production and staging service instances for
every split worker, and a single shared instance for every shared worker.

Required split workers:

- `loyal-balance-sweep-ata-monitor`
- `loyal-balance-sweep-ata-projector`
- `loyal-balance-sweep-autodeposit-trigger`
- `loyal-same-mint-yield-monitor`
- `loyal-squads-policy-monitor`, if it is deployed as a managed service

Required shared workers:

- `loyal-kamino-reserve-monitor`

Acceptable Render organization:

- one Render project with `production` and `staging` environments; or
- separate production and staging projects; or
- the existing heavy/light project split with both environments represented.

Required service readback:

- Service names or metadata make the environment unambiguous.
- Split services have distinct service IDs for production and staging.
- Split services use the intended environment's `NEON_DATABASE_URL`.
- Shared `loyal-kamino-reserve-monitor` has no staging duplicate unless the
  implementation deliberately creates a separate market-data plane.
- Worker services continue to use `runtime: image` with pinned
  `ghcr.io/loyal-labs/loyal-yield-routing/...:sha-...` images or digests.
- Private GHCR pull credentials stay attached through Render registry
  credentials, not plaintext config.

### 4. Environment Variable Boundaries

PASS only if env var ownership matches this model:

- Production `NEON_DATABASE_URL` points only at the production Yield Neon branch.
- Staging `NEON_DATABASE_URL` points only at the staging Yield Neon branch.
- `TIMESCALEDB_URL` may be identical across environments only because ATA
  telemetry is separated by a required production/staging table or schema
  selector.
- ATA monitor/projector services have an explicit environment selector for the
  ATA Timescale stream. The selector must be constrained to known values such as
  `production` and `staging`, not an arbitrary SQL identifier from untrusted
  input.
- `DATABASE_URL` for the main smart-account/product DB is not used as a
  substitute for `NEON_DATABASE_URL` in Render workers.
- Production and staging transaction-signing secrets are either distinct or
  staging execution is disabled/allowlisted.
- No plaintext secrets are written to repo files, shell history, command
  arguments outside the `op run --env-file ... -- sh -c '<command>'` pattern,
  logs, or chat.

If `same-mint-yield-monitor`, `same-mint-reserve-swap`, or `yield-migrations`
fall back from `NEON_DATABASE_URL` to `DATABASE_URL`, PASS requires proving the
Render/app env does not accidentally set `DATABASE_URL` to the shared product DB
for those processes.

### 5. Shared Timescale DB, Separate ATA Streams

PASS only if shared Timescale use cannot cause staging events to update
production Yield rows or production events to update staging Yield rows.

`kamino.*` market data may remain shared without additional isolation because it
is read-only market data for the Yield control plane.

`loyal.*` ATA telemetry must be split into separate production and staging
streams inside the shared TimescaleDB. Acceptable shapes include:

- separate schemas, such as `loyal_prod.balance_sweep_wallet_ata_observations`
  and `loyal_staging.balance_sweep_wallet_ata_observations`; or
- separate tables with the same schema, such as
  `loyal.balance_sweep_wallet_ata_observations_prod` and
  `loyal.balance_sweep_wallet_ata_observations_staging`; or
- one physical table partitioned by an explicit `environment` column only if the
  dedupe key, sequence/cursor semantics, latest view, and every monitor/projector
  query are scoped by that environment.

The preferred implementation is separate schemas or separate tables, not a
shared raw table plus best-effort filtering.

Current-schema warning: `loyal.balance_sweep_wallet_ata_observations` contains
`target_id`, and `loyal_yield.balance_sweep_wallet_balance_events` references
`balance_sweep_targets(id)`. A shared physical Timescale database is not enough
by itself; the verifier must prove the production and staging ATA streams are
physically or logically separated before projection.

Required evidence:

- The Timescale migration creates production and staging ATA observation
  streams, including each stream's observation table, dedupe table, sequence or
  event id semantics, indexes, and latest-observation view.
- Production `loyal-balance-sweep-ata-monitor` writes only to the production ATA
  stream.
- Staging `loyal-balance-sweep-ata-monitor` writes only to the staging ATA
  stream.
- Production `loyal-balance-sweep-ata-projector` reads only from the production
  ATA stream and writes only to the production Yield Neon branch.
- Staging `loyal-balance-sweep-ata-projector` reads only from the staging ATA
  stream and writes only to the staging Yield Neon branch.
- A staging-only target appears in the staging ATA stream and never in the
  production ATA stream.
- A production-only target appears in the production ATA stream and never in the
  staging ATA stream.
- Projector cursor state is scoped by environment so one environment cannot
  advance or replay the other's stream cursor.

### 6. Worker Behavior By Environment

PASS only if each split worker reads and writes only its intended Yield branch.

`loyal-balance-sweep-ata-monitor`:

- Production reads production `balance_sweep_targets`.
- Staging reads staging `balance_sweep_targets`.
- Earn APY snapshots are written to the same branch that supplied the worker's
  `NEON_DATABASE_URL`.
- ATA observations are written to the Timescale stream selected for the same
  environment as the worker's `NEON_DATABASE_URL`.

`loyal-balance-sweep-ata-projector`:

- Production advances only production `loyal_yield.projection_offsets` and
  writes only production balance/current/event rows.
- Staging advances only staging `loyal_yield.projection_offsets` and writes only
  staging balance/current/event rows.
- Production reads only the production ATA Timescale stream.
- Staging reads only the staging ATA Timescale stream.
- Consumer names or stream selectors prevent one environment from stealing or
  replaying the other's cursor semantics.

`loyal-balance-sweep-autodeposit-trigger`:

- Production uses production Yield rows for lot creation, claims, executions,
  and execution completion.
- Staging uses staging Yield rows only.
- Staging does not execute against broad production accounts. PASS requires
  dry-run mode, execution disabled, or a positive allowlist check before any
  transaction send.

`loyal-same-mint-yield-monitor`:

- Production fleet mode discovers only production active vault/policy rows.
- Staging fleet mode discovers only staging active vault/policy rows.
- Staging execution is dry-run, disabled, or allowlisted.
- Production execution is not enabled merely because staging needs it.

`loyal-squads-policy-monitor`, if deployed:

- Production writes to the production Yield branch.
- Staging writes to the staging Yield branch.
- Production does not treat staging-only policies as active production policies.
  Acceptable proof includes signer/authority filtering, staging-only signer
  separation, or no staging on-chain policy writes.

### 7. Loyal Apps Binding

PASS only if the app deployments point at the intended Yield branch while keeping
the shared main smart-account DB unchanged.

Required readback:

- Production `loyal-apps` `NEON_DATABASE_URL` points at the production Yield
  branch.
- Staging `loyal-apps` `NEON_DATABASE_URL` points at the staging Yield branch.
- Production and staging `loyal-apps` may share the same main
  smart-account/product `DATABASE_URL`.
- No Yield schema migrations are run against the main product `DATABASE_URL`.
- Staging UI/API paths that can prepare or send live transactions are disabled,
  dry-run, or constrained to staging/test accounts until staging execution
  isolation is proven.

### 8. Staging Mutation Does Not Affect Production

PASS only if a staging-only mutation can be performed and production remains
unchanged.

Required staging-only probes:

- Create a staging-only `loyal_yield` schema object or migration marker.
- Insert or update a staging-only policy/target row with clearly staged test
  identity, or use an existing staging-only row.
- Run the relevant staging worker in one-shot or dry-run mode.

Required production readbacks after the staging probe:

- Production `loyal_yield.schema_migrations` is unchanged.
- Production policy/target/current-position rows for the staging identity are
  absent.
- Production worker logs do not show the staging identity as discovered or
  executed.
- Production execution state and active decision counts are unchanged.

### 9. Production Still Works

PASS only if production workers remain able to run against the production Yield
branch after the split.

Required evidence:

- `yield:migrate:check` passes for production.
- Production `loyal-kamino-reserve-monitor` continues writing fresh
  `kamino.reserve_updates` or `kamino.supported_reserves` in the shared
  Timescale DB.
- Production split workers start successfully with production env vars.
- Production `loyal-same-mint-yield-monitor` remains in the intended mode
  (currently dry-run unless explicitly approved for execution).
- Production `loyal-balance-sweep-autodeposit-trigger` execution mode is exactly
  the intended production setting and is not changed by staging rollout.

### 10. Documentation And Operator Handoff

PASS only if the final docs make the service boundary obvious to the next
operator.

Required docs:

- A current service matrix listing production/staging/shared status.
- The production and staging Yield Neon branch names or IDs, without connection
  strings.
- Render service IDs for each split service in each environment.
- The shared Timescale decision, including the production/staging ATA table or
  schema names and the proof that each worker is bound to the correct stream.
- The staging execution policy: disabled, dry-run, or allowlisted.
- The exact commands used for the final verification run.

## Verdict Format

For each verification run, report:

```text
Repo Inventory: PASS|FAIL - note
Neon Branch Isolation: PASS|FAIL - note
Render Service Shape: PASS|FAIL - note
Environment Variable Boundaries: PASS|FAIL - note
Shared Timescale DB, Separate ATA Streams: PASS|FAIL - note
Worker Behavior By Environment: PASS|FAIL - note
Loyal Apps Binding: PASS|FAIL - note
Staging Mutation Does Not Affect Production: PASS|FAIL - note
Production Still Works: PASS|FAIL - note
Documentation And Operator Handoff: PASS|FAIL - note
Overall Verdict: PASS|FAIL
```

Overall verdict is `PASS` only when every required section passes. If any
section fails, keep this verifier unchanged and plan the smallest next change
needed to make the failing section pass. Revise this verifier only if it
misstates the real goal, and state the reason before changing it.
