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
- 1Password fresh environment readback:
  `loyal-yield-routing-production` is `2e463mizwetw6sbv3tiw7loxi4` and
  `loyal-yield-routing-staging` is `zspmwsfuhomrlffpqp6wk7fbdu`; both currently
  have the same ten non-secret variable names.
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
- `gh auth status` failed for the configured GitHub accounts, so the manual
  `worker-images` workflow still needs a valid GitHub auth path before it can be
  triggered from this machine.

## Verdict

Repo Inventory: PASS - repo ownership matches the verifier; `render.yaml` keeps
`loyal-kamino-reserve-monitor` shared and splits the NEON-backed workers in the
Blueprint.

Neon Branch Isolation: FAIL - production and staging Neon branches exist, have
distinct IDs, distinct secret-safe connection fingerprints, and the staging
schema probe is absent from production. This section still fails because split
Render services and `loyal-apps` deployments are not yet bound to branch-specific
`NEON_DATABASE_URL` values.

Render Service Shape: FAIL - production/shared services are live and pinned to
private GHCR images through `loyal-ghcr`; staging services are present in
`render.yaml` but were absent from the live Render service list. The current
worktree is not committed, so no GHCR images exist yet for the stream-selector
code in this verifier run. Staging Render service creation was intentionally
deferred because creating ATA monitor/projector services on older images would
risk using the legacy shared `loyal` ATA stream.

Environment Variable Boundaries: FAIL - live production ATA monitor/projector
now expose `BALANCE_SWEEP_ATA_STREAM`; fresh production/staging 1Password envs
have matching non-secret variable names and environment-specific stream,
execution, and Neon branch metadata values, but still need environment-specific
secret values. The Yield-oriented same-mint and migration binaries no longer
fall back from `NEON_DATABASE_URL` to `DATABASE_URL`. The old duplicate
1Password shells were renamed with a `-superseded` suffix.

Shared Timescale DB, Separate ATA Streams: PASS - migration 4 applied and
readback confirmed `loyal_prod` and `loyal_staging` observation tables, dedupe
tables, and latest views exist in the shared TimescaleDB.

Worker Behavior By Environment: FAIL - code and Blueprint route monitor/projector
traffic by stream and branch, and local staging one-shots/dry-runs passed for
the projector, autodeposit trigger with execution disabled, and same-mint fleet
monitor without `--execute`. This section still fails because staging Render
workers are not live, and existing live production workers are still pinned to
previously built images that must be repointed to images built from the commit
that contains this split-stream code.

Loyal Apps Binding: FAIL - no production/staging `loyal-apps` deployment
readback has been captured in this run.

Staging Mutation Does Not Affect Production: FAIL - the staging-only schema
probe and inactive policy/target marker were created in staging and read back as
absent from production. Local staging worker probes also completed without live
execution. This section still fails because production worker logs have not been
checked for the staging identity after live staging services are deployed.

Production Still Works: FAIL - Yield and Timescale migration checks pass, and
production services are not suspended with latest deploy status `live`, but
production worker logs/freshness were not fully verified after the live env-var
updates and services are not yet running images built from this worktree.

Documentation And Operator Handoff: FAIL - service/1Password/Timescale docs are
updated, but Neon branch IDs, staging Render service IDs, and final live
verification commands remain pending. Render Blueprint validation currently
fails only on private GHCR image visibility, which matches the documented
registry-credential caveat for these image-backed services.

Overall Verdict: FAIL

## Next Required Moves

1. Populate both 1Password Environments with the remaining secret variable names
   and environment-specific values through a secret-safe operator path.
2. Free at least two local 1Password env mounts or use another safe mounting
   path for `.env.1password.production` and `.env.1password.staging`.
3. Commit this repo change, run the `worker-images` GitHub Actions workflow for
   that commit, and repoint production/staging Render services to the resulting
   `sha-<commit>` GHCR images before claiming worker behavior PASS.
4. Create/import the staging Render services from `render.yaml` only after the
   new images exist; verify their service IDs, env-var names, pinned images, and
   dry-run/disabled posture.
5. Bind `loyal-apps` production/staging `NEON_DATABASE_URL` to the matching
   Yield Neon branches while leaving the main product `DATABASE_URL` shared.
6. Run staging-only policy/target and ATA stream probes, then production readbacks/log
   checks proving staging state does not appear in production.
