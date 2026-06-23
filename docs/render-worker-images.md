# Render worker image deployment

The worker deployment boundary is intentionally split into two Render projects/environments:

| Purpose | Render project/environment | Services | Image |
| --- | --- | --- | --- |
| LaserStream-heavy monitors | `loyal-yield-laserstream-workers` / `production` | `loyal-kamino-reserve-monitor`, `loyal-balance-sweep-ata-monitor` | `ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-<commit>` |
| LaserStream-heavy staging monitor | `loyal-yield-laserstream-workers` / `staging` | `loyal-balance-sweep-ata-monitor-staging` | `ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-<commit>` |
| Lightweight SQL/background workers | `loyal-yield-light-workers` / `production` | `loyal-balance-sweep-ata-projector`, `loyal-balance-sweep-autodeposit-trigger`, `loyal-same-mint-yield-monitor` | `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>` |
| Lightweight staging workers | `loyal-yield-light-workers` / `staging` | `loyal-balance-sweep-ata-projector-staging`, `loyal-balance-sweep-autodeposit-trigger-staging`, `loyal-same-mint-yield-monitor-staging` | `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>` |

Current live pre-split Render state, observed with `render services -o json` on 2026-06-10:

| Object | ID |
| --- | --- |
| Current combined Render project `loyal` | `prj-d8hjcnojs32c73998uu0` |
| Current combined production environment `Production` | `evm-d8hjcnojs32c73998uv0` |
| Current `loyal-kamino-reserve-monitor` service | `srv-d8h4i9a8pkls73bver00` |
| Current `loyal-balance-sweep-ata-monitor` service | `srv-d8j87m6q1p3s73ff8n8g` |
| Current `loyal-balance-sweep-ata-projector` service | `srv-d8kfqpjbc2fs73chlc00` |

Target split IDs, once created/imported in Render, must be recorded here before the deploy is considered verified:

| Object | ID |
| --- | --- |
| Heavy Render project | `prj-d8kgt3a8qa3s7382glb0` |
| Heavy production environment | `evm-d8kgt3a8qa3s7382glc0` |
| Light Render project | `prj-d8kgt4r7uimc73b1ul0g` |
| Light production environment | `evm-d8kgt4r7uimc73b1ul1g` |
| Heavy `loyal-kamino-reserve-monitor` service | `srv-d8h4i9a8pkls73bver00` |
| Heavy `loyal-balance-sweep-ata-monitor` service | `srv-d8j87m6q1p3s73ff8n8g` |
| Light `loyal-balance-sweep-ata-projector` service | `srv-d8kfqpjbc2fs73chlc00` |
| Light `loyal-balance-sweep-autodeposit-trigger` service | `srv-d8lplql7vvec73f1it6g` |
| Light `loyal-same-mint-yield-monitor` service | `srv-d8n7gqbbc2fs73emk610` |
| Heavy staging environment | `evm-d8plqfrtqb8s738actsg` |
| Heavy `loyal-balance-sweep-ata-monitor-staging` service | `srv-d8plrh9194ac739eulrg` |
| Light staging environment | `evm-d8plqhgjs32c738s1n70` |
| Light `loyal-balance-sweep-ata-projector-staging` service | `srv-d8plri36sc1c73cstumg` |
| Light `loyal-balance-sweep-autodeposit-trigger-staging` service | `srv-d8plrirsq97s7387q8og` |
| Light `loyal-same-mint-yield-monitor-staging` service | `srv-d8plrj8js32c738s2f80` |

Production and staging use separate 1Password Environments with matching
variable names. The target names are `loyal-yield-routing-production` and
`loyal-yield-routing-staging`. `NEON_DATABASE_URL` differs by environment and
points at the matching Yield Neon branch. `TIMESCALEDB_URL` may point at the
same physical TimescaleDB, but balance-sweep ATA workers must also set
`BALANCE_SWEEP_ATA_STREAM` to `production` or `staging`; that selector chooses
the environment-specific `loyal_prod` or `loyal_staging` ATA tables inside the
shared TimescaleDB. `loyal-kamino-reserve-monitor` does not need this selector
because it writes shared `kamino.*` market data only.

| 1Password Environment | ID | Local mount |
| --- | --- | --- |
| `loyal-yield-routing-production` | `2e463mizwetw6sbv3tiw7loxi4` | pending; 1Password local .env file limit was reached |
| `loyal-yield-routing-staging` | `zspmwsfuhomrlffpqp6wk7fbdu` | pending; 1Password local .env file limit was reached |

After re-authenticating the 1Password MCP path against account
`V7U7OAXJBVEP5LQLVFNOKQ2GUE`, both target environments are visible and list no
local env mounts. The local mount retry still fails with the per-device local
`.env` file limit `max: 10`. The `op` CLI can reach the 1Password desktop app
when run outside this sandbox, and `op run --env-file=.env.1password` has a
passing secret-safe health check there; sandboxed CLI calls cannot reach the
desktop app IPC. MCP-visible local mounts account for five active env files
across the visible environments, so the slots to free may be hidden, disabled,
or associated with another configured account. Free local env-file slots or use
another approved secret-safe mounting path before relying on
`.env.1password.production` and `.env.1password.staging` locally.

Both new environments currently have the same non-secret variable names:
`BALANCE_SWEEP_ATA_STREAM`, `BALANCE_SWEEP_EXECUTE_ELIGIBLE`,
`BALANCE_SWEEP_UPDATE_SOURCE`, `BALANCE_SWEEP_TARGET_REFRESH_SECONDS`,
`EARN_APY_REFRESH_INTERVAL_SECONDS`, `EARN_APY_RISK_PROFILES`,
`KAMINO_UPDATE_SOURCE`, `KAMINO_API_BASE`, `LASERSTREAM_ENDPOINT`, and
`RUST_LOG`, `NEON_PROJECT_ID`, `NEON_BRANCH_ID`, and `NEON_BRANCH_NAME`.
Production sets `BALANCE_SWEEP_ATA_STREAM=production`,
`BALANCE_SWEEP_EXECUTE_ELIGIBLE=true`, `NEON_BRANCH_ID=br-damp-queen-aq3ixgw2`,
and `NEON_BRANCH_NAME=production`; staging sets
`BALANCE_SWEEP_ATA_STREAM=staging`, `BALANCE_SWEEP_EXECUTE_ELIGIBLE=false`,
`NEON_BRANCH_ID=br-old-wind-aq34quzh`, and `NEON_BRANCH_NAME=staging`.

Populate the remaining environment-specific secret values from the approved
operator source: `NEON_DATABASE_URL`, `TIMESCALEDB_URL`, `SOLANA_RPC_URL`,
`HELIUS_API_KEY`, `YIELD_ROUTER_KEYPAIR`, `POLICY_KEYPAIR`,
`SOLANA_TESTING_PK`, `RENDER_API_KEY`, `SF_API_TOKEN`, and `DEPLOYMENT_PK` where
that environment actually needs them. Do not create blank placeholder secret
values. Older duplicate shells were renamed to
`loyal-yield-routing-production-superseded`
(`yz6qehsjpi4rz44wxz2xpovtay`) and
`loyal-yield-routing-staging-superseded`
(`ntowznk6ogjprrgbrzaitw7opu`) after a duplicate
`BALANCE_SWEEP_ATA_STREAM` entry was detected.

Neon branch IDs are recorded below without connection strings. The currently
mounted local repo env has only the existing `NEON_DATABASE_URL` and
`TIMESCALEDB_URL`; it does not include `NEON_API_KEY`, `NEON_PROJECT_ID`, or
`NEON_BRANCH_ID`, so connection strings still need an operator-safe population
path.

| Neon Yield project/branch | ID | Notes |
| --- | --- | --- |
| Project `yield-optimization` | `purple-wave-56227231` | Org `org-noisy-cake-62775570`; proxy host `c-8.us-east-1.aws.neon.tech` |
| Production branch `production` | `br-damp-queen-aq3ixgw2` | Default branch |
| Staging branch `staging` | `br-old-wind-aq34quzh` | Created from production on 2026-06-18 |

Secret-safe fingerprint readback on 2026-06-18 showed production
`NEON_DATABASE_URL` fingerprint `9c788c60c1c3a2c0` and staging fingerprint
`63165d6a1066fe7a`. The staging isolation probe
`loyal_yield.staging_isolation_probe` exists on staging with one row and is
absent on production.

Render service env readback on 2026-06-18 showed the production split workers
all share `NEON_DATABASE_URL` fingerprint `ce0458839b5350ae`, and the staging
split workers all share fingerprint `3abff897e6f5cc84`. Host-only parsing
confirmed production uses `ep-ancient-grass-aqb5aalu.c-8.us-east-1.aws.neon.tech`
and staging uses `ep-calm-bonus-aq0yls0u.c-8.us-east-1.aws.neon.tech`; no
connection strings or credentials were printed.

The shared TimescaleDB has migration `4 split_balance_sweep_ata_streams`
applied. Live readback on 2026-06-17 confirmed these six relations exist:
`loyal_prod.balance_sweep_wallet_ata_observations`,
`loyal_prod.balance_sweep_wallet_ata_observation_dedupe`,
`loyal_prod.latest_balance_sweep_wallet_ata_observations`,
`loyal_staging.balance_sweep_wallet_ata_observations`,
`loyal_staging.balance_sweep_wallet_ata_observation_dedupe`, and
`loyal_staging.latest_balance_sweep_wallet_ata_observations`. Both split streams
were empty immediately after creation.

CI builds both images in `.github/workflows/worker-images.yml` and tags them as `sha-${GITHUB_SHA}`. Render services should use those immutable SHA tags or image digests. Do not use `latest` as the only service image reference.

The production/staging stream-selector code in this change is not active on
Render until the repo change is committed, the `worker-images` workflow builds
`laserstream-workers` and `light-workers` for that commit, and the Render
services are repointed to those resulting `sha-<commit>` images or digests.
Existing live image pins predate this work unless explicitly updated in a later
verification run. Do not create or start the staging ATA monitor/projector on
older images; older binaries do not honor `BALANCE_SWEEP_ATA_STREAM` and can use
the legacy shared `loyal` ATA stream.

The split-stream code was pushed to `main` at commit
`ce5fe2ead0ab55bf3cac4a597cf6aac52232ee3a`. GitHub Actions workflow run
`27732951674` completed successfully on 2026-06-18 and pushed both images:
`ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-ce5fe2ead0ab55bf3cac4a597cf6aac52232ee3a`
and
`ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-ce5fe2ead0ab55bf3cac4a597cf6aac52232ee3a`.
Render production and staging services should use those tags until a later
workflow run intentionally replaces them.

Safe rollout order:

1. Keep `render.yaml` pinned to the verified image tags above.
2. Keep staging `NEON_DATABASE_URL` pointed at branch `br-old-wind-aq34quzh`.
3. Verify staging services remain in dry-run/disabled posture before enabling
   any broad staging execution.

Secret-safe Neon URL generation can be done inside a shell without printing the
URL:

```sh
STAGING_URL="$(neonctl connection-string staging --project-id purple-wave-56227231 --database-name neondb --ssl require)"
PRODUCTION_URL="$(neonctl connection-string production --project-id purple-wave-56227231 --database-name neondb --ssl require)"
```

Use those values only in an approved secret store or API request body. Do not
paste them into docs, chat, shell history, logs, or command-line arguments.
Do not use `vercel env add` with piped stdin for these database URLs on Vercel
CLI 41.3.2: during the approved `loyal-apps` production binding attempt, the
CLI echoed the stdin value while prompting even with `--sensitive`. Treat that
production Neon credential as exposed until the production Neon role password is
rotated/reset and Vercel Production `NEON_DATABASE_URL` is overwritten through
the Vercel UI or another non-echoing secret path. The Preview branch `staging`
`NEON_DATABASE_URL` binding was added through the non-echoing Vercel REST API
path and verified by env-name readback.

The light worker image contains the Rust projector/trigger binaries, same-mint monitor/executor binaries, Bun production dependencies, and `scripts/execute-autodeposit-policy.ts`. The autodeposit trigger invokes that in-image executor through `BALANCE_SWEEP_EXECUTOR_COMMAND`; it should not depend on a sibling checkout at runtime. During the June 16 same-mint amount-semantics incident response, keep the same-mint monitor in fleet dry-run mode until the incident regression, DB guardrail, local checks, and explicit operator approval pass. The current safety command is `/usr/local/bin/same-mint-yield-monitor --all-active-vaults --poll-interval-seconds 300 --rebalance-cooldown-seconds 300`. That service does not include `SOLANA_TESTING_PK`; live optimization execution, when re-enabled, uses `YIELD_ROUTER_KEYPAIR` as the route payer and delegated signer. Monitor logs should report `execute: false`, `pollIntervalSeconds: 300`, and `rebalanceCooldownSeconds: 300`.

Staging worker posture is fail-closed until staging proves it cannot affect
production users or production policies:

- `loyal-balance-sweep-autodeposit-trigger-staging` omits `--execute-eligible`
  and sets `BALANCE_SWEEP_EXECUTE_ELIGIBLE=false`. It intentionally omits
  transaction-signing env vars in Render while staging execution remains
  disabled.
- `loyal-same-mint-yield-monitor-staging` omits `--execute`; it is fleet
  dry-run only.
- Staging ATA monitor/projector use `BALANCE_SWEEP_ATA_STREAM=staging`, which
  maps to the `loyal_staging` Timescale schema.

Live Render readback on 2026-06-18 showed all production/shared services and all
four staging services are `runtime: image`, use private registry credential
`loyal-ghcr`, and have latest deploy status `live`. Production deploy IDs for
the image update were `dep-d8ploj3tqb8s738abhj0`
(`loyal-kamino-reserve-monitor`), `dep-d8plooojs32c738s09p0`
(`loyal-balance-sweep-ata-monitor`), `dep-d8plooog4nts7383rtu0`
(`loyal-balance-sweep-ata-projector`), `dep-d8plos8g4nts7383s26g`
(`loyal-balance-sweep-autodeposit-trigger`), and
`dep-d8plop4m0tmc73b1ae0g` (`loyal-same-mint-yield-monitor`). Staging deploy
IDs were `dep-d8plrhh194ac739eumb0`, `dep-d8plrib6sc1c73cstuv0`,
`dep-d8plrj3sq97s7387q9ag`, and `dep-d8plrjgjs32c738s2fg0`.

As of 2026-06-15, `loyal-same-mint-yield-monitor` previously ran in fleet dry-run mode on `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-d3497113aed8fedb83dbaa3ea398f40ac58aab37`. Render deploy `dep-d8nom81kh4rs73fe3td0` was live with image digest `sha256:b34feb49ef99616b91570f248fde65cc257523b45c8a3f606c3249c908adfa5b`. The service env-var names were `NEON_DATABASE_URL`, `TIMESCALEDB_URL`, `SOLANA_RPC_URL`, `YIELD_ROUTER_KEYPAIR`, and `RUST_LOG`; `SOLANA_TESTING_PK` was absent. The first post-deploy dry-run log at `2026-06-15T05:20:03Z` reported `status: fleet_poll`, `execute: false`, `allActiveVaults: true`, `candidateCount: 4`, and `discoveredVaultCount: 0`, which matched the verified post-withdraw cleanup state.

On 2026-06-15, service `srv-d8n7gqbbc2fs73emk610` was patched through the Render API to use `/usr/local/bin/same-mint-yield-monitor --all-active-vaults --execute --poll-interval-seconds 300`, then redeployed as `dep-d8nsdfpkh4rs73fhlc90` on the same pinned image and digest. Render service readback showed `runtime: image`, registry credential `loyal-ghcr`, image `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-d3497113aed8fedb83dbaa3ea398f40ac58aab37`, digest `sha256:b34feb49ef99616b91570f248fde65cc257523b45c8a3f606c3249c908adfa5b`, and the 300-second fleet execution command. Fresh instance `srv-d8n7gqbbc2fs73emk610-k6wfp` logged `status: fleet_poll`, `execute: true`, `allActiveVaults: true`, `candidateCount: 4`, `discoveredVaultCount: 0`, and `pollIntervalSeconds: 300` at `2026-06-15T09:33:28Z`. The next release should update the same service to the current target command above and verify logs include the explicit cooldown field.

On 2026-06-17, the same service was patched back to dry-run mode during the amount-semantics incident response. Render API readback showed command `/usr/local/bin/same-mint-yield-monitor --all-active-vaults --poll-interval-seconds 300 --rebalance-cooldown-seconds 300`, `runtime: image`, registry credential `loyal-ghcr`, and image `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-6862dba08508e8a67ab38de85cfd3044d56162b2`. The dry-run redeploy `dep-d8p2aotckfvc739ug7d0` finished live on the same image digest `sha256:e7828c61d2eaaba7cd54e40561959fbbc09387536289853ae57fab09a4a35dbf`.

Render's current Blueprint validator rejects `registryCredential` on these `runtime: image` worker services. Keep the GHCR images private and attach the required private registry pull credentials in the Render Dashboard/API outside this Blueprint.

The live services use private GHCR images through Render registry credential `loyal-ghcr` (`rgc-d8kic4bs9h5c73d37l40`). As of 2026-06-10, Render's Blueprint validator still reports private GHCR image refs as `image ... not found` because `runtime: image` private registry credentials cannot be represented in this Blueprint. The live service config is applied through the Render API with `image.registryCredentialId`.

The monitor services deliberately remain separate Render services even though they share the same heavy image. They override the image command independently, so a restart, deploy, or failure of one monitor does not share a runtime process with the other.
