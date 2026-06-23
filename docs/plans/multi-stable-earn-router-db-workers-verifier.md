# Multi-Stable Earn Router DB/Worker Verifier

Use this document as the verifier-first goal for making the Earn router and
orchestrator ready for non-USDC same-mint stablecoin routing.

Do not treat this as an implementation checklist. The work passes only when a
skeptical runner can verify every required condition below from repo files,
database schema/readbacks, worker logs, Render service state, and command
outputs.

## Goal

The Yield control-plane database and workers must be ready to track, plan, and
execute same-mint Kamino routing for every SDK-supported stablecoin that an
existing active policy already allows, while preserving today's USDC deposit and
autodeposit behavior.

The supported stablecoin universe for this verifier is:

- CASH
- USDG
- PYUSD
- USDC
- USDT
- USDS

Overall PASS is impossible if the implementation requires any on-chain policy
update, creates a new policy universe, performs cross-mint routing, or changes
the product deposit path away from USDC.

## Scope

Required:

- Add the database shape needed for generic token-account/mint tracking in the
  Earn balance-sweep/autodeposit path while keeping existing USDC rows working.
  The target v1 schema uses generic columns on the existing
  balance-sweep/autodeposit tables; a child `(target_id, mint)` table is
  deferred until a future policy update allows one sweep policy to track
  multiple deposit mints.
- Update same-mint worker planning so candidate discovery, policy filtering,
  planning, decision writing, and execution validation are liquidity-mint
  generic.
- Default the worker enabled-stablecoin universe to CASH, USDG, PYUSD, USDC,
  USDT, and USDS, with an optional environment allowlist for staged rollout.
- Keep same-mint candidate selection on Safe-basket reserves unless an existing
  active policy or explicit worker option says otherwise.
- Update affected workers/scripts to consume the new generic database shape
  without breaking the current USDC-only deposit path.
- Build and deploy new pinned Render worker images for the affected workers.
- Verify staging first, then production, with secret-safe database and Render
  readbacks.

Non-goals:

- No route-policy, setup-policy, init-obligation-policy, or policy SDK semantic
  updates.
- No migration of existing users into non-USDC deposits.
- No cross-mint swaps.
- No enabling non-USDC autodeposit pulls.
- No live non-USDC transaction is required for this verifier.

## Required Checks

### 1. No Policy Update Surface

PASS only if the final diff and verification run show that no policy semantic
surface was changed.

Required local evidence:

```sh
git diff --name-only
```

Required result:

- No change is needed to deployed on-chain policy accounts.
- No command in this verifier creates, updates, or removes a route policy for
  the purpose of enabling the six stablecoins.
- Existing active policies remain the source of truth for allowed route modes,
  markets, and liquidity mints.
- The workers never assume a mint is routeable just because it is in the global
  supported-stablecoin list; the policy arrays must still allow it.

If a code change touches SDK/policy files only to read or expose existing mint
constants, the verifier may still pass. If it changes policy bytes,
constraints, seed layout, route modes, account positions, or generated policy
universe semantics, this section is FAIL.

### 2. Database Migration Shape

PASS only if a new forward migration makes the balance-sweep/autodeposit schema
mint-generic without dropping or renaming existing USDC columns.

Required local evidence:

```sh
rg -n "wallet_usdc_ata|vault_usdc_ata|wallet_token_ata|vault_token_ata|source_token_ata|destination_token_ata|token_mint|DROP NOT NULL|source_mint|source_wallet_token_ata" crates/loyal-yield-orchestrator/migrations
```

```sh
rg -n "0006_generic_balance_sweep_token_accounts|primary key must be \\(target_id, mint\\)|must be nullable|wallet_token_ata|vault_token_ata|source_token_ata|destination_token_ata|source_mint|source_wallet_token_ata|NULLIF|COALESCE" crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs crates/loyal-yield-orchestrator/src/store.rs scripts/execute-autodeposit-policy.ts
```

Required schema outcome:

- Existing `wallet_usdc_ata` and `vault_usdc_ata` columns remain readable for
  backward compatibility.
- The schema contains generic columns on the existing target/current/event
  tables for the tracked mint and token accounts.
- Existing USDC rows are backfilled into the generic representation.
- Future non-USDC rows can be represented without storing non-USDC addresses in
  `*_usdc_ata` fields.
- Current wallet balance state can be keyed by mint when one logical target may
  observe more than one mint; `yield-migrations --check` must verify the
  current balance primary key is `(target_id, mint)`.
- Balance events, surplus lots, lot claims, and executions can be traced back
  to the mint that produced them.
- The pending surplus-lot read model exposes the source event mint/token ATA so
  worker/readback diagnostics do not have to infer the mint from legacy USDC
  columns.
- The migration is idempotent and can be run by `yield-migrations --apply`.
- No destructive migration, table rewrite, or data deletion is required.

Required staging/prod readback:

```sql
SELECT version, name, applied_at
FROM loyal_yield.schema_migrations
ORDER BY version DESC
LIMIT 5;
```

```sql
SELECT COUNT(*) AS target_count
FROM loyal_yield.balance_sweep_targets;
```

```sql
SELECT target_id, mint, COUNT(*) AS duplicate_count
FROM loyal_yield.balance_sweep_wallet_balances_current
GROUP BY target_id, mint
HAVING COUNT(*) > 1;
```

```sql
SELECT COUNT(*) AS generic_usdc_backfill_count
FROM loyal_yield.balance_sweep_targets
WHERE token_mint = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v'
  AND wallet_token_ata = wallet_usdc_ata
  AND vault_token_ata = vault_usdc_ata;
```

```sql
SELECT COUNT(*) AS generic_current_usdc_backfill_count
FROM loyal_yield.balance_sweep_wallet_balances_current
WHERE mint = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v'
  AND wallet_token_ata = wallet_usdc_ata;
```

```sql
SELECT ARRAY_AGG(a.attname ORDER BY cols.ordinality) AS current_balance_pkey
FROM pg_constraint c
CROSS JOIN LATERAL UNNEST(c.conkey) WITH ORDINALITY AS cols(attnum, ordinality)
JOIN pg_attribute a
  ON a.attrelid = c.conrelid
 AND a.attnum = cols.attnum
WHERE c.conrelid = 'loyal_yield.balance_sweep_wallet_balances_current'::regclass
  AND c.contype = 'p';
```

```sql
SELECT table_name, column_name, is_nullable
FROM information_schema.columns
WHERE table_schema = 'loyal_yield'
  AND (table_name, column_name) IN (
    ('balance_sweep_targets', 'wallet_usdc_ata'),
    ('balance_sweep_targets', 'vault_usdc_ata'),
    ('balance_sweep_wallet_balances_current', 'wallet_usdc_ata'),
    ('balance_sweep_wallet_balance_events', 'wallet_usdc_ata'),
    ('balance_sweep_executions', 'source_wallet_ata'),
    ('balance_sweep_executions', 'destination_vault_ata')
  )
ORDER BY table_name, column_name;
```

```sql
SELECT column_name
FROM information_schema.columns
WHERE table_schema = 'loyal_yield'
  AND table_name = 'pending_balance_sweep_surplus_lots'
  AND column_name IN ('source_mint', 'source_wallet_token_ata')
ORDER BY column_name;
```

Overall PASS requires proving row counts did not unexpectedly drop after the
migration, and that legacy compatibility token-account columns are nullable
where generic token-account columns now carry the authoritative value.

### 3. Same-Mint Monitor Is Mint Generic

PASS only if `same-mint-yield-monitor` no longer plans from a hardcoded USDC
universe.

Required local evidence:

```sh
rg -n "USDC_MINT|safe_usdc|load_safe_usdc|same_mint_usdc" crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs crates/loyal-yield-router/src/timescale/mod.rs
```

Required result:

- Any remaining USDC references are clearly compatibility defaults, test
  fixtures, or user-facing labels, not planner filters.
- Candidate loading runs for all enabled stablecoin mints or for an explicit
  enabled-mint allowlist that defaults to the supported stablecoin universe.
- Timescale candidate filters remain safety preserving:
  - active supported reserve;
  - selected risk basket;
  - fresh non-stale latest row;
  - minimum TVL threshold;
  - non-negative APY;
  - maximum sanity APY.
- Fleet vault discovery selects policies by route mode, delegated signer, and
  overlap between policy mints and enabled mints. It must not require USDC.
- Candidate eligibility requires all of:
  - candidate market is in `route_policies.kamino_markets`;
  - candidate liquidity mint is in `route_policies.stable_mints`;
  - candidate liquidity mint is in `route_policies.kamino_liquidity_mints`;
  - candidate liquidity mint is in the worker enabled-mint set.
- Dry-run JSON includes enabled mints, candidate counts by mint, eligible
  candidate counts by mint, skipped mints if any, and the planned liquidity
  mint.

### 4. Planner Chooses Across Mints Safely

PASS only if planning considers same-mint opportunities per liquidity mint and
then chooses the best routeable opportunity for the vault.

Required behavior:

- A source reserve can only move to a target reserve with the same
  `liquidity_mint`.
- If a vault has positions in multiple mints, the planner evaluates each
  mint's best source/target edge instead of choosing the largest source first
  and ignoring better opportunities in smaller mints.
- The planned decision writes row-level `liquidity_mint`,
  `source_liquidity_mint`, and `target_liquidity_mint`, plus matching
  `execution_plan.liquidity_mint`, `execution_plan.source_liquidity_mint`, and
  `execution_plan.target_liquidity_mint`.
- Existing one-active-decision-per-vault behavior remains unless explicitly
  changed and verified; this verifier does not require parallel per-mint active
  decisions.
- A policy that currently only allows USDC continues to behave as USDC-only.
- A policy that already allows a non-USDC mint may dry-run and plan that mint
  without policy mutation.

Required focused evidence:

```sh
rg -n "routeable_positions|same_mint_candidate_exists|liquidity_mint|estimated_edge_bps" crates/loyal-yield-orchestrator/src/domain.rs crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs
```

Record the relevant planner code path and, if a dry-run artifact is available,
show a same-vault candidate set with more than one enabled mint and the selected
planned mint. Do not add or run Rust tests for this verifier unless the repo
test policy is explicitly changed.

### 5. Same-Mint Executor Accepts Planned Mint

PASS only if the execution path used by the worker is liquidity-mint generic.

Required local evidence:

```sh
rg -n "USDC_MINT|wallet_usdc_ata|vault_usdc_ata|SourceMintMismatch|TargetMintMismatch|neonAllowsUsdc" crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs
```

Required result:

- `--optimization-cycle` reads the planned liquidity mint from the decision or
  reconciled source/target rows.
- Vault token ATAs are derived from the planned liquidity mint, not always from
  USDC.
- Source and target reserves must have the same liquidity mint.
- Source and target reserve liquidity mint must equal the planned decision
  mint.
- The decoded policy or Neon policy row must allow the planned mint.
- Error messages and JSON diagnostics name the expected planned mint rather
  than hardcoded USDC.
- Compatibility aliases such as `--full-withdraw-main-usdc` may remain
  USDC-specific.

This verifier does not require sending a live non-USDC route. It does require a
dry-run or unit-level proof that the worker would not reject a valid non-USDC
same-mint plan solely because the mint is not USDC.

### 6. Autodeposit Remains USDC While Reading Generic Schema

PASS only if the autodeposit worker remains behaviorally USDC-only for deposits
but is not blocked by the generic database migration.

Required local evidence:

```sh
rg -n "prepareEarnUsdcAutodepositPull|wallet_usdc_ata|vault_usdc_ata|wallet_token_ata|vault_token_ata|liquidityMint" scripts/execute-autodeposit-policy.ts crates/loyal-yield-orchestrator/src/bin
```

Required result:

- Existing USDC autodeposit rows can still be loaded and executed.
- New generic mint/token-account fields are populated or read for USDC rows.
- The worker refuses non-USDC autodeposit pulls until a future verifier expands
  deposit policy and SDK support.
- The ATA monitor/projector path reads the generic target shape but only
  subscribes/projects USDC targets for this verifier.
- Surplus-lot projection, claim selection, claim completion/release, and
  executable-target discovery stay scoped to the target mint and, for this
  verifier, to USDC.
- `user_yield_positions`, deposits, and holding events continue recording
  `deposit_mint`, `initial_liquidity_mint`, and `current_liquidity_mint`.
- A top-up into an existing active position still enforces mint consistency.

Required focused evidence:

```sh
rg -n "tokenMint|walletTokenAta|vaultTokenAta|prepareEarnUsdcAutodepositPull|expectedUsdcMint|recordPullExecution|source_event_id|balance_sweep_lot_claim_items|token_mint|USDC_MINT" scripts/execute-autodeposit-policy.ts crates/balance-sweep-autodeposit-trigger/src/main.rs crates/balance-sweep-ata-monitor/src/main.rs crates/balance-sweep-ata-observations/src/lib.rs
```

Confirm from the script and any dry-run output that the autodeposit worker reads
the generic columns, still calls the USDC pull path, and rejects a target whose
generic token mint is not USDC. Also confirm lot/claim queries trace through
`balance_sweep_wallet_balance_events.source_event_id` and only act on lots whose
event mint matches the target mint, and that ATA monitoring only subscribes
USDC targets until non-USDC deposits are separately enabled. Do not run the
autodeposit test suite for this verifier unless separately requested.

### 7. Worker Images Contain Updated Binaries

PASS only if the affected worker images are rebuilt from the updated commit and
the image contents match the worker surfaces under verification.

Affected images:

- `laserstream-workers`, because `loyal-balance-sweep-ata-monitor` reads active
  balance-sweep targets and must have `yield-migrations` available for its
  predeploy schema gate.
- `light-workers`, because it contains `yield-migrations`,
  `balance-sweep-ata-projector`, `balance-sweep-autodeposit-trigger`,
  `same-mint-yield-monitor`, `same-mint-reserve-swap`, and
  `scripts/execute-autodeposit-policy.ts`.

Required local evidence:

```sh
rg -n "same-mint-yield-monitor|same-mint-reserve-swap|yield-migrations|execute-autodeposit-policy.ts|balance-sweep-ata|preDeployCommand: .*yield-migrations --apply" Dockerfile.light-workers Dockerfile.laserstream-workers .github/workflows/worker-images.yml render.yaml
```

Required CI/deploy evidence:

- The `worker-images` GitHub Actions workflow succeeds for the implementation
  commit.
- Both images are pushed with immutable `sha-<commit>` tags.
- No worker is deployed from `latest`.
- Render services use `runtime: image` with the private GHCR registry
  credential, not Render Docker builds.
- Every affected worker that reads or writes the Yield control-plane schema runs
  `/usr/local/bin/yield-migrations --apply` in its Render predeploy command.

Record CI/deploy evidence in this shape:

```text
implementation_commit=<git sha>
worker_images_workflow=<workflow run URL or run id>
laserstream_workers_image=ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-<commit>
laserstream_workers_digest=<digest>
light_workers_image=ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>
light_workers_digest=<digest>
```

### 8. Staging Database And Worker Verification

PASS only if staging proves the migration and worker behavior before production
is changed.

Required staging steps:

```sh
op run --env-file=<staging-env-file> -- sh -c 'yield-migrations --apply'
```

```sh
op run --env-file=<staging-env-file> -- sh -c 'yield-migrations --check'
```

```sh
op run --env-file=<staging-env-file> -- sh -c 'same-mint-yield-monitor --once --all-active-vaults'
```

Required staging evidence:

- Migration version is present in staging.
- Existing USDC balance-sweep target rows are still readable.
- Generic USDC backfill rows or columns are populated.
- `same-mint-yield-monitor` logs enabled mints and candidate counts by mint.
- Policies that only allow USDC are reported as USDC-eligible only.
- No staging worker sends transactions unless it was already explicitly
  configured to do so outside this verifier.
- No plaintext secrets are printed.

### 9. Production Database And Worker Deployment

PASS only if production is migrated and the affected workers are deployed to the
new pinned images after staging passes.

Required production steps:

```sh
op run --env-file=<production-env-file> -- sh -c 'yield-migrations --apply'
```

```sh
op run --env-file=<production-env-file> -- sh -c 'yield-migrations --check'
```

Required Render service evidence:

- `loyal-balance-sweep-ata-monitor` is on the new `laserstream-workers`
  `sha-<commit>` image.
- `loyal-balance-sweep-ata-projector` is on the new `light-workers`
  `sha-<commit>` image.
- `loyal-balance-sweep-autodeposit-trigger` is on the new `light-workers`
  `sha-<commit>` image.
- `loyal-same-mint-yield-monitor` is on the new `light-workers`
  `sha-<commit>` image.
- Staging counterparts are also on the new images or intentionally documented
  as deferred.
- Production `loyal-same-mint-yield-monitor` remains in its approved execution
  posture. If no separate approval is given, that posture is dry-run.
- Production autodeposit remains USDC-only.

Record Render readback evidence in this shape for every affected staging and
production service:

```text
service=<render service name>
service_id=<render service id>
environment=<staging|production>
runtime=image
image=<ghcr sha tag>
image_digest=<digest>
command=<configured command>
registry_credential=loyal-ghcr
deploy_id=<deploy id>
deploy_status=<live/active status>
```

Required post-deploy log evidence:

- Same-mint monitor logs a fleet poll without crashing.
- Same-mint monitor logs enabled mints and candidate counts by mint.
- Autodeposit trigger logs no schema/read errors.
- ATA monitor/projector logs no schema/read errors.
- `SOLANA_TESTING_PK` is not required by `loyal-same-mint-yield-monitor`.
- No worker logs plaintext secrets.

### 10. Local Static Checks

PASS for this working pass only if focused local static readbacks support the
implementation shape and the affected Rust crates/bins pass formatting and
compile checks. Bun/autodeposit test commands are intentionally deferred because
the operator explicitly requested no testing for this verifier pass; this is
acceptable only with a narrow replacement that combines source-level
autodeposit guard readbacks, mint-scoped SQL readbacks, and live autodeposit
worker log evidence showing USDC-only, no-failure scans.

Required commands:

```sh
git diff --check
```

```sh
cargo fmt --all -- --check
```

```sh
cargo check -p loyal-yield-orchestrator -p loyal-yield-router -p balance-sweep-autodeposit-trigger
```

```sh
rg -n "safe_usdc|load_safe_usdc|no_fresh_safe_usdc|neonAllowsUsdc|SourceMintMismatch" crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs crates/loyal-yield-router/src/timescale/mod.rs
```

```sh
rg -n "tokenMint|walletTokenAta|vaultTokenAta|prepareEarnUsdcAutodepositPull|expectedUsdcMint|recordPullExecution" scripts/execute-autodeposit-policy.ts
```

Do not run Bun or autodeposit test commands for this pass unless the operator
explicitly allows testing again. The replacement must be called out in the
verifier result and must include enough evidence to prove that autodeposit still
loads existing USDC rows, refuses non-USDC pulls, and has no schema/read errors
after deployment.

## Verdict Format

For each verification run, report:

```text
No Policy Update Surface: PASS|FAIL - note
Database Migration Shape: PASS|FAIL - note
Same-Mint Monitor Is Mint Generic: PASS|FAIL - note
Planner Chooses Across Mints Safely: PASS|FAIL - note
Same-Mint Executor Accepts Planned Mint: PASS|FAIL - note
Autodeposit Remains USDC While Reading Generic Schema: PASS|FAIL - note
Worker Images Contain Updated Binaries: PASS|FAIL - note
Staging Database And Worker Verification: PASS|FAIL - note
Production Database And Worker Deployment: PASS|FAIL - note
Local Static Checks: PASS|FAIL - note
Overall Verdict: PASS|FAIL
```

Overall verdict is PASS only if every required section passes. If any section
fails, keep this verifier unchanged and plan the smallest next change needed to
make the failing section pass. Revise this verifier only if it misstates the
real goal, and state the reason before changing it.

## Frozen Decisions

- Generic balance-sweep/autodeposit support uses columns on existing tables for
  v1. A child `(target_id, mint)` model is deferred.
- Enabled stablecoins default to the six-mint verifier universe, with an
  optional environment allowlist for staged rollout.
- Candidate selection remains Safe-basket by default.
- Production same-mint execution posture is not changed by this verifier unless
  separately approved.
- Non-USDC proof for this verifier is dry-run or unit-level only; live non-USDC
  transaction proof is deferred until deposit/policy rollout.
