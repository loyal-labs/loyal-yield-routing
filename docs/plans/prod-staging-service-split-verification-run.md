# Production/Staging Service Split Verification Run

Recorded: 2026-06-18.

Verifier: `docs/plans/prod-staging-service-split-verifier.md`.

## Evidence Captured

- `cargo fmt --package balance-sweep-ata-observations --package balance-sweep-ata-monitor --package balance-sweep-ata-projector --package loyal-timescale-migrations`
- `cargo check -p balance-sweep-ata-observations -p balance-sweep-ata-monitor -p balance-sweep-ata-projector -p loyal-timescale-migrations`
- `ruby -e 'require "yaml"; YAML.load_file("render.yaml"); puts "render.yaml parsed"'`
- `git diff --check`
- `op run --env-file=.env.1password -- sh -c 'cargo run -p loyal-yield-orchestrator --bin yield-migrations -- --check'`
- `op run --env-file=.env.1password -- sh -c 'cargo run -p loyal-timescale-migrations -- --apply'`
- `op run --env-file=.env.1password -- sh -c 'cargo run -p loyal-timescale-migrations -- --check'`
- Render API readbacks for service IDs, image paths, registry credential names,
  commands, environment IDs, and env-var names only. Secret values were not
  printed.
- 1Password MCP readbacks for environment names, IDs, variable names, and local
  mount paths only. Secret values were not read or printed.
- `cargo check -p loyal-yield-orchestrator`
- `cargo check -p balance-sweep-ata-observations -p balance-sweep-ata-monitor -p balance-sweep-ata-projector -p loyal-timescale-migrations -p loyal-yield-orchestrator`
- `rg -n "postgres(ql)?://|://[^\\s]*:[^\\s]*@|NEON_DATABASE_URL=.*://|TIMESCALEDB_URL=.*://|RENDER_API_KEY=|HELIUS_API_KEY=|YIELD_ROUTER_KEYPAIR=|POLICY_KEYPAIR=|SOLANA_TESTING_PK=|DEPLOYMENT_PK=" . -g '!target/**' -g '!node_modules/**' -S`
- `rg -n "or_else\\(\\|_\\| env::var\\(\"DATABASE_URL\"\\)|env::var\\(\"DATABASE_URL\"\\)" crates/loyal-yield-orchestrator/src/bin crates scripts -S`
- `op run --env-file=.env.1password -- sh -c 'render blueprints validate render.yaml -o json'`
- `.github/workflows/worker-images.yml` inspection: the workflow is manual and
  publishes `laserstream-workers` and `light-workers` as `sha-${GITHUB_SHA}`.
- Commit `ce5fe2ead0ab55bf3cac4a597cf6aac52232ee3a` was pushed to `main`.
  GitHub Actions workflow run `27732951674` completed successfully on
  2026-06-18, building and pushing both
  `laserstream-workers:sha-ce5fe2ead0ab55bf3cac4a597cf6aac52232ee3a` and
  `light-workers:sha-ce5fe2ead0ab55bf3cac4a597cf6aac52232ee3a`.
- 1Password fresh environment readback:
  `loyal-yield-routing-production` is `2e463mizwetw6sbv3tiw7loxi4` and
  `loyal-yield-routing-staging` is `zspmwsfuhomrlffpqp6wk7fbdu`; both currently
  have the same non-secret variable names.
- Render staging environments were created:
  `evm-d8plqfrtqb8s738actsg` for `loyal-yield-laserstream-workers` and
  `evm-d8plqhgjs32c738s1n70` for `loyal-yield-light-workers`.
- Render staging services were created and reached latest deploy status `live`:
  `loyal-balance-sweep-ata-monitor-staging` / `srv-d8plrh9194ac739eulrg`,
  `loyal-balance-sweep-ata-projector-staging` / `srv-d8plri36sc1c73cstumg`,
  `loyal-balance-sweep-autodeposit-trigger-staging` /
  `srv-d8plrirsq97s7387q8og`, and `loyal-same-mint-yield-monitor-staging` /
  `srv-d8plrj8js32c738s2f80`.
- Production/shared services were redeployed to the split images and reached
  latest deploy status `live`: `dep-d8ploj3tqb8s738abhj0`,
  `dep-d8plooojs32c738s09p0`, `dep-d8plooog4nts7383rtu0`,
  `dep-d8plos8g4nts7383s26g`, and `dep-d8plop4m0tmc73b1ae0g`.
- Render `NEON_DATABASE_URL` fingerprint readback showed production split
  workers all share fingerprint `ce0458839b5350ae`, staging split workers all
  share `3abff897e6f5cc84`; host-only parsing confirmed production uses
  `ep-ancient-grass-aqb5aalu.c-8.us-east-1.aws.neon.tech`, while staging uses
  `ep-calm-bonus-aq0yls0u.c-8.us-east-1.aws.neon.tech`.
- Render non-secret env readback showed production ATA monitor/projector use
  `BALANCE_SWEEP_ATA_STREAM=production`, staging ATA monitor/projector use
  `BALANCE_SWEEP_ATA_STREAM=staging`, production autodeposit uses
  `BALANCE_SWEEP_EXECUTE_ELIGIBLE=true`, and staging autodeposit uses
  `BALANCE_SWEEP_EXECUTE_ELIGIBLE=false`.
- `loyal-apps/app` Vercel project readback used project
  `prj_DMp23ZuBz7apUcQbBRjwJyCSFTVq` / org
  `team_CWDtWDIyqqcfgsOfzt4AWU5w`. `vercel env ls` showed existing
  `DATABASE_URL` and `DATABASE_URL_UNPOOLED` entries for Production and Preview
  branch `staging`, but no `NEON_DATABASE_URL` entry. Adding
  `NEON_DATABASE_URL` to Vercel production was not performed because the
  approvals reviewer requires explicit user approval for that sensitive
  production config write.
- Production Render service readback showed `loyal-kamino-reserve-monitor`,
  `loyal-balance-sweep-ata-monitor`, `loyal-balance-sweep-ata-projector`,
  `loyal-balance-sweep-autodeposit-trigger`, and
  `loyal-same-mint-yield-monitor` are `not_suspended` with latest deploy status
  `live`.
- Neon CLI metadata readback:
  project `yield-optimization` is `purple-wave-56227231`; production branch is
  `production` / `br-damp-queen-aq3ixgw2`; staging branch is `staging` /
  `br-old-wind-aq34quzh`.
- Secret-safe Neon branch fingerprint and isolation probe:
  production fingerprint `9c788c60c1c3a2c0`, staging fingerprint
  `63165d6a1066fe7a`; `loyal_yield.staging_isolation_probe` exists on staging
  with one row and is absent on production.
- Yield migration checks passed against both production and staging branch
  connection strings captured only inside the shell.
- Staging-only inactive policy/target marker
  `staging-probe-20260618-prod-split` was inserted in staging and read back as
  absent from production.
- Local staging worker probes:
  `balance-sweep-ata-projector --ata-stream staging --once --batch-limit 10`,
  `balance-sweep-autodeposit-trigger --once --batch-limit 10` with
  `BALANCE_SWEEP_EXECUTE_ELIGIBLE=false`, and
  `same-mint-yield-monitor --once --all-active-vaults` without `--execute`.
  The same-mint dry-run reported `execute=false`, `candidateCount=4`, and
  `discoveredVaultCount=0`.
- 2026-06-18 re-check: `cargo fmt`, `git diff --check`, YAML parsing, combined
  `cargo check`, secret-pattern scan, `loyal-timescale-migrations --check`, and
  `yield-migrations --check` all passed. The secret-pattern scan matched only
  empty placeholders in `.env.example`. Sequential `op run` was required for DB
  checks after a parallel `op run` attempt produced transient env parsing/URL
  shape failures.
- `gh auth status` failed for the configured GitHub accounts, but
  `gh workflow run worker-images.yml --ref main` and `gh run watch 27732951674`
  succeeded through the available GitHub CLI path.

## Verdict

Repo Inventory: PASS - repo ownership matches the verifier; `render.yaml` keeps
`loyal-kamino-reserve-monitor` shared and splits the NEON-backed workers in the
Blueprint.

Neon Branch Isolation: FAIL - production and staging Neon branches exist, have
distinct IDs, distinct secret-safe connection fingerprints, and the staging
schema probe is absent from production. Render split workers are now bound to
distinct production/staging branch endpoints. This section still fails because
`loyal-apps` production/staging deployment bindings have not been captured.

Render Service Shape: PASS - production/shared services and staging split
services are live with distinct service IDs. `loyal-kamino-reserve-monitor`
remains single/shared. All services use pinned
`sha-ce5fe2ead0ab55bf3cac4a597cf6aac52232ee3a` GHCR images through Render
registry credential `loyal-ghcr`.

Environment Variable Boundaries: FAIL - Render split workers now have distinct
production/staging `NEON_DATABASE_URL` fingerprints, production/staging ATA
stream selectors, and staging execution-disabled posture. Fresh production and
staging 1Password Environments have matching non-secret variable names and
environment-specific metadata, but still need environment-specific secret values
populated or mounted through a durable operator path.

Shared Timescale DB, Separate ATA Streams: PASS - migration 4 applied and
readback confirmed `loyal_prod` and `loyal_staging` observation tables, dedupe
tables, and latest views exist in the shared TimescaleDB.

Worker Behavior By Environment: FAIL - code, Blueprint, and live Render env
route monitor/projector traffic by stream and branch. Local staging
one-shots/dry-runs passed, and staging Render workers are live with dry-run or
execution-disabled posture. This section still fails until post-live worker logs
and DB readbacks prove staging activity is absent from production state/logs.

Loyal Apps Binding: FAIL - no production/staging `loyal-apps` deployment
readback has been captured in this run. Vercel env-name readback shows
`DATABASE_URL` is already scoped for Production and Preview branch `staging`,
but `NEON_DATABASE_URL` is absent and still requires explicit approval to add.

Staging Mutation Does Not Affect Production: FAIL - the staging-only schema
probe and inactive policy/target marker were created in staging and read back as
absent from production. Local staging worker probes also completed without live
execution. This section still fails because production worker logs have not been
checked for the staging identity after live staging services are deployed.

Production Still Works: FAIL - Yield and Timescale migration checks pass, and
production services are not suspended with latest deploy status `live` on the
split images. This section still fails because production worker logs/freshness
were not fully verified after the live image updates.

Documentation And Operator Handoff: FAIL - service/1Password/Timescale docs are
updated with Neon branch IDs, staging Render service IDs, live image tags, and
staging execution posture. This section still fails until `loyal-apps` binding
and final post-live mutation/log verification commands are captured. Render
Blueprint validation currently fails only on private GHCR image visibility,
which matches the documented registry-credential caveat for these image-backed
services.

Overall Verdict: FAIL

## Next Required Moves

1. Populate both 1Password Environments with the remaining secret variable names
   and environment-specific values through a secret-safe operator path.
2. Free at least two local 1Password env mounts or use another safe mounting
   path for `.env.1password.production` and `.env.1password.staging`.
3. Bind `loyal-apps` production/staging `NEON_DATABASE_URL` to the matching
   Yield Neon branches while leaving the main product `DATABASE_URL` shared.
   This is blocked on explicit approval for the Vercel production config write.
4. Run staging-only policy/target and ATA stream probes, then production readbacks/log
   checks proving staging state does not appear in production.
5. Verify production worker logs/freshness after the split-image deploys.
