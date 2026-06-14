# Render worker image deployment

The worker deployment boundary is intentionally split into two Render projects/environments:

| Purpose | Render project/environment | Services | Image |
| --- | --- | --- | --- |
| LaserStream-heavy monitors | `loyal-yield-laserstream-workers` / `production` | `loyal-kamino-reserve-monitor`, `loyal-balance-sweep-ata-monitor` | `ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-<commit>` |
| Lightweight SQL/background workers | `loyal-yield-light-workers` / `production` | `loyal-balance-sweep-ata-projector`, `loyal-balance-sweep-autodeposit-trigger`, `loyal-same-mint-yield-monitor` | `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>` |

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
| Light `loyal-balance-sweep-autodeposit-trigger` service | `srv-d8lplql7vvec73f1it6g` |
| Light `loyal-same-mint-yield-monitor` service | `srv-d8n7gqbbc2fs73emk610` |

CI builds both images in `.github/workflows/worker-images.yml` and tags them as `sha-${GITHUB_SHA}`. Render services should use those immutable SHA tags or image digests. Do not use `latest` as the only service image reference.

The light worker image contains the Rust projector/trigger binaries, same-mint monitor/executor binaries, Bun production dependencies, and `scripts/execute-autodeposit-policy.ts`. The autodeposit trigger invokes that in-image executor through `BALANCE_SWEEP_EXECUTOR_COMMAND`; it should not depend on a sibling checkout at runtime. The same-mint monitor starts in dry-run mode unless its Render command is intentionally changed to pass `--execute`.

As of 2026-06-14, `loyal-same-mint-yield-monitor` is deployed in dry-run mode with explicit settings/vault flags for the verified test vault. The live service uses `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-bf9546d906c6f71a3520b40e74af4bf7a4969ffc`, Render deploy `dep-d8n7h03bf2bc73c2b0n0`, and image digest `sha256:0584af52a6c789756ac9e9fccc355dbd52c1c18557467beacb7c211f876ffc3f`.

Render's current Blueprint validator rejects `registryCredential` on these `runtime: image` worker services. Keep the GHCR images private and attach the required private registry pull credentials in the Render Dashboard/API outside this Blueprint.

The live services use private GHCR images through Render registry credential `loyal-ghcr` (`rgc-d8kic4bs9h5c73d37l40`). As of 2026-06-10, Render's Blueprint validator still reports private GHCR image refs as `image ... not found` because `runtime: image` private registry credentials cannot be represented in this Blueprint. The live service config is applied through the Render API with `image.registryCredentialId`.

The monitor services deliberately remain separate Render services even though they share the same heavy image. They override the image command independently, so a restart, deploy, or failure of one monitor does not share a runtime process with the other.
