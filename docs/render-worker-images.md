# Render worker and realtime image deployment

The worker/realtime deployment boundary is intentionally split into two Render
projects/environments:

| Purpose | Render project/environment | Services | Image |
| --- | --- | --- | --- |
| LaserStream-heavy monitors | `loyal-yield-laserstream-workers` / `production` | `loyal-kamino-reserve-monitor`, `loyal-balance-sweep-ata-monitor` | `ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-<commit>` |
| LaserStream-heavy staging monitor | `loyal-yield-laserstream-workers` / `staging` | `loyal-balance-sweep-ata-monitor-staging` | `ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-<commit>` |
| Lightweight SQL/background workers and realtime web service | `loyal-yield-light-workers` / `production` | `loyal-yield-realtime`, `loyal-balance-sweep-ata-projector`, `loyal-balance-sweep-autodeposit-trigger`, `loyal-same-mint-yield-monitor` | `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>` |
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
| Light `loyal-yield-realtime` service | `srv-d966hcpkh4rs73da0j4g` |
| Heavy `loyal-kamino-reserve-monitor` service | `srv-d8h4i9a8pkls73bver00` |
| Heavy `loyal-balance-sweep-ata-monitor` service | `srv-d8j87m6q1p3s73ff8n8g` |
| Light `loyal-balance-sweep-ata-projector` service | `srv-d8kfqpjbc2fs73chlc00` |
| Light `loyal-balance-sweep-autodeposit-trigger` service | `srv-d8lplql7vvec73f1it6g` |
| Light `loyal-same-mint-yield-monitor` service | `srv-d8n7gqbbc2fs73emk610` |
| Light `loyal-route-lookup-table-provisioner` service | pending creation and production readback |
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
The production autodeposit trigger also sets `SOLANA_WEEK_NOTIFY_ENDPOINT` for
the post-sweep Solana Week callback.

The staging ATA monitor also sets `DISABLE_EARN_APY_REFRESH=true`. Its
`TIMESCALEDB_URL` resolves to the production compute, so the Earn APY refresher
was rescanning 60 days of production `kamino.reserve_updates` once an hour
alongside the production monitor's own refresh. Staging tracks two ATA targets
and does not serve Earn APY history, so only production needs that refresh.
Keep `EARN_APY_REFRESH_INTERVAL_SECONDS` and `EARN_APY_RISK_PROFILES` on staging
so re-enabling it is a single flag flip. The flag is parsed by clap's strict
bool parser, so the only accepted values are `true` and `false`; anything else
fails argument parsing at startup.

Populate the remaining environment-specific secret values from the approved
operator source: `NEON_DATABASE_URL`, `TIMESCALEDB_URL`, `SOLANA_RPC_URL`,
`HELIUS_API_KEY`, `POLICY_KEYPAIR`,
`SOLANA_TESTING_PK`, `SOLANA_WEEK_NOTIFY_SECRET`, `RENDER_API_KEY`,
`SF_API_TOKEN`, and `DEPLOYMENT_PK` where that environment actually needs them.
Do not create blank placeholder secret values. Older duplicate shells were renamed to
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

CI builds the images in `.github/workflows/worker-images.yml` and tags them as
`sha-${GITHUB_SHA}`. Pull requests compile each image-family inventory, then
package and probe all three compiler-free Dockerfiles without publishing.

A trusted `main` push compiles the three image-family inventories in parallel and publishes all three immutable image families.
The family-scoped Cargo target caches retain compatible fingerprints and record the source revision
that produced them; CI marks only paths changed since that revision as newer before rebuilding.
The scheduled cache refresh rolls them forward once per UTC day. Main-branch image publication restores
those snapshots but never uploads multi-gigabyte Cargo state, so deployable images are not held behind cache compression.
The compiled family artifacts use low-level compression to reduce the handoff to the runtime-image jobs.
Publishing these images does not deploy them.
Deployment selects an already-published immutable SHA tag or digest; it never rebuilds Rust.
Render services should use those immutable references and must not use `latest`
as their only image reference.

Operator-only binaries are deliberately excluded from the production images.
`Dockerfile.operator-tools` is packaged from the same shared artifact and its
immutable `operator-tools:sha-<commit>` image is published on trusted `main`
pushes. It contains `loyal-timescale-migrations`, the fleet verifier and
production-evidence tools, `same-mint-monitor-e2e`, and the shared-catalog,
alert-monitor, legacy-import, and cleanup lookup-table tools. No Render service
is pinned to this image.

The production/staging stream-selector code in this change is not active on
Render until the repo change is committed, the `worker-images` workflow
publishes `laserstream-workers` and `light-workers` for that commit, and the
Render services are repointed to those resulting `sha-<commit>` images or
digests.
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

The light worker image contains the Rust projector/trigger/realtime binaries,
the fleet planner/confirmer and same-mint monitor/executor binaries,
`yield-migrations`, `route-lookup-table-provisioner`, Bun production
dependencies, and `scripts/execute-autodeposit-policy.ts`. The shared-catalog,
legacy-import, cleanup, and alert-monitor lookup-table tools are operator-only
and exist exclusively in the `operator-tools` image described above.
`loyal-yield-realtime` runs from the same immutable image as a Render Web
Service with command `/usr/local/bin/loyal-yield-realtime`, health path
`/healthz`, direct `NEON_DATABASE_URL`, and `REALTIME_AUTH_SECRET` from the
secret store. The autodeposit trigger invokes the in-image executor through
`BALANCE_SWEEP_EXECUTOR_COMMAND`; it should not depend on a sibling checkout at
runtime. After the June 16 same-mint amount-semantics incident response, the
production same-mint monitor must return to fleet execution only after the
incident regression, DB guardrail, local checks, fixed immutable image, and
explicit operator approval pass. The approved production command is
`/usr/local/bin/same-mint-yield-monitor --all-active-vaults --execute --poll-interval-seconds 300 --rebalance-cooldown-seconds 300`.
That service does not include `SOLANA_TESTING_PK`; live optimization and
idle-vault deposit execution uses `POLICY_KEYPAIR` as the route payer and
delegated signer. The monitor never mutates Address Lookup Tables during live
execution. Missing durable coverage fails before a decision/send, seals an
idempotent request, and is served by the separate continuously deployed,
budgeted provisioner using `POLICY_KEYPAIR`; the next monitor cycle retries.
The provisioner must stay on the same pinned light-worker image as the monitor
while using its own Render worker command. Its positive max-lamport ceiling and
budget-window seconds must be explicit; reservations are PostgreSQL-backed and
cluster-wide so a restart or overlapping Render instance cannot reset spend.
Monitor logs should report
`execute: true`, `pollIntervalSeconds: 300`, and
`rebalanceCooldownSeconds: 300`.

The durable-v2 ALT production order is strict:

1. Apply and verify migrations `0017`, `0018_earn_activity_realtime`,
   `0019_legacy_lookup_table_imports`, and
   `0020_demand_driven_shared_market_catalog`, then
   `0021_reusable_alt_production_controls`.
2. Import legacy ALTs for audit/refund accounting, bootstrap both v2 families,
   publish the complete signerless shared catalog, and run the provisioner
   until the exact shared generation is finalized, warm, and active. Keep
   routing fail closed during this work.
3. Pin the provisioner and monitor to the same newly built immutable
   `light-workers:sha-<commit>` image. Stop and drain the old monitor, prove no
   prepared decision/send remains, then start the no-legacy monitor and the
   separately budgeted `POLICY_KEYPAIR` provisioner.
4. Perform the finalized-RPC-verified, atomic global `reusable_only` switch. Do
   not run a vault fleet backfill. The first genuine missing-vault attempt must
   defer before decision creation/send, seal one request, be packed by the
   provisioner, and retry on the next monitor cycle.
5. Do not call the deployment healthy until Render logs, database evidence,
   and finalized RPC readback prove at least one eligible deposit/rebalance was
   compiled with reusable v2 ALTs, simulated, confirmed, and reconciled. A
   running service or empty queue alone is insufficient.
6. Only after the deployed no-legacy image and zero-reference proof are current
   may old tables be retired. Cleanup must exhaustively match the approved
   standard-policy fleet count/hash, simulate immediately before every
   deactivate and close, verify finality, wait the observed SlotHashes cooldown,
   repeat the exhaustive zero-reference proof before close, and prove rent
   returned to the standard policy account.

Record the provisioner service ID, immutable image digest, command, bounded
lamport/operation settings, and final monitor/provisioner deploy IDs in this
document before the production migration verdict is marked PASS.

Realtime V2 hardening added two secret-safe verifier commands:

```sh
op run --env-file=.env.1password -- sh -c 'bun run verify:realtime:render-config'
op run --env-file=.env.1password -- sh -c 'bun run verify:realtime:sse'
```

The first checks the Render service shape, immutable image tag, direct Neon host
fingerprints, required env names, and autodeposit executor safety. The second
signs a short-lived token, emits a safe durable event, verifies live SSE delivery,
and verifies `Last-Event-ID` replay. Neither command prints secrets, tokens, full
database URLs, or private keys.

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
