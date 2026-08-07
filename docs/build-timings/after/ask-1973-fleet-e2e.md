# ASK-1973 verification evidence

The isolated merge verifier passed at `2026-08-07T07:53:35Z` with:

```sh
op run --env-file=/Users/zotho/Dev/loyal/.env.1password.loyal-noncritical-env -- \
  bun run verify:ask-1973-fleet-e2e
```

Evidence directory: `/tmp/ask1973-fleet-e2e-evidence.cW19m7`

## Successful lifecycle proof

The verifier no longer treats six clean process exits as proof that all roles
worked. `successful-role-lifecycle.json` requires both the exact side-effect-free
entrypoint probe and role-specific successful-work evidence for every role:

- planner: seven deterministic ordered planning rounds over 10,000 vaults;
- revalidator: published opportunity economics were revalidated and bound to a
  durable decision, with ready/revalidate/waiting lanes isolated;
- executor: a decision-linked submission reached the explicit confirming and
  completed transitions, while production transaction code compiled,
  simulated, signed, mock-sent twice byte-identically, and performed its
  post-confirm read with no `minContextSlot` violation;
- confirmer: the authoritative confirmation poll and exact conflict lease
  reclaim/renew paths passed;
- reconciler: exact conflict lease reclaim/renew and exactly-once reconciled
  volume accounting passed;
- priority provisioner: one typed dry-run plan and one reusable-v2 ALT plan
  executed with zero stale-fence commits.

All six `entrypoint` and `successfulWork` fields were `true`. The controlled
transaction probe made no external network call and sent no production
transaction.

## Load and failure isolation

- Migrations 1 through 32 applied to disposable PostgreSQL.
- The isolated database verifier claimed 4,160 runnable jobs alongside 10,000
  ALT-cold and 10,000 inert jobs. Ready-claim p95 was 970,162 microseconds at
  baseline and 994,388 microseconds with the ALT-cold cohort.
- The run observed 64 nonoverlapping leases, zero database deadlocks, zero
  duplicate active-vault movements, and zero overlapping-lane violations.
- Planner p95 was 19,470 microseconds against a 10,000,000-microsecond limit.
- A separate negative control ran only the real revalidator and executor over
  4,160 deliberately incomplete jobs. Every job durably terminalized, no lease
  remained, and the RPC audit saw exactly the two expected genesis checks and
  no account, fee, status, or transaction method.
- The contention harness reduced status-view backend time from 52,175 to 8,533
  milliseconds and duty cycle from 86% to 14%, with no victim-pool timeout.

## Image packaging gate

Both image workflows now run on relevant pull requests. They build without
pushing, target `linux/amd64`, exercise cargo-chef and every final stage, assert
the runtime image platform and executable set, run all six fleet role probes in
`light-workers`, and run the declared operator-tools command. Manual dispatch
keeps the existing immutable GHCR push behavior and probes the pulled image.

The equivalent local command is:

```sh
bun run verify:ask-1973-images
```

Only a clean tracked checkout on `linux/amd64` is labeled authoritative in the
emitted `summary.json`; native-platform overrides are labeled supplementary.

The local host could not supply authoritative `linux/amd64` proof. Its arm64
Podman VM segfaulted in x86_64 `rustc -vV` under QEMU. A supplementary native
`linux/arm64` build then exhausted the shared 40 GiB VM store during `bun
install`. No shared images were pruned. These are local infrastructure failures,
not image passes; the native Linux/amd64 pull-request jobs remain the mandatory
packaging gate.

## Safety boundary

The passing isolated verifier establishes source-level merge safety for the
crate refactor. Merge still requires the new pull-request image jobs to pass on
the committed revision.

Deployment safety is deliberately separate. It additionally requires immutable
registry image identities, pinned Render service revisions, a fresh source-bound
production evidence artifact, Neon lifecycle transitions, ClickStack role
evidence, and finalized Solana evidence. No image was pushed, no service was
deployed, and no production database or RPC was mutated by this verification.

## Two-phase deployment gate

After the pull-request image jobs pass and an operator publishes both immutable
images from one source commit, pin those exact references in `render.yaml` and
commit the pin. From that clean checkout, capture the read-only pre-deploy
runtime proof and production baseline:

```sh
op run --env-file=/Users/zotho/Dev/loyal/.env.1password.loyal-noncritical-env -- \
  bun run verify:ask-1973-deployment pre-deploy \
    --light-image ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<image-source-commit> \
    --heavy-image ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-<image-source-commit> \
    --evidence-dir /tmp/ask1973-pre-deploy
```

The pre-deploy phase pulls and inspects the native Linux/amd64 registry
manifests and provenance, probes the images, runs the deterministic and isolated
database evidence on disposable PostgreSQL, and requires implementation Checks
1-7 to pass. The image source commit may precede the pin commit only by the
verifier's explicit source-binding allowlist; arbitrary source drift fails.

Only after an explicit deployment order, record the UTC cutover timestamp and
run the post-deploy phase. Load the ClickStack access key into the environment
without putting it in command arguments:

```sh
set -a
. /Users/zotho/Dev/loyal/.env.clickstack
set +a
op run --env-file=/Users/zotho/Dev/loyal/.env.1password.loyal-noncritical-env -- \
  bun run verify:ask-1973-deployment post-deploy \
    --runtime-evidence /tmp/ask1973-pre-deploy/runtime-evidence.json \
    --baseline /tmp/ask1973-pre-deploy/production-baseline.json \
    --cutover-at <UTC-RFC3339> \
    --evidence-dir /tmp/ask1973-post-deploy
```

This phase is read-only. It requires the clean Blueprint, the six live Render
roles, immutable manifest digests and env boundaries, migrations, current
Timescale data, post-cutover Neon lifecycle transitions, finalized Solana
effects, and Checks 1-11 to pass. It also queries the deployed ClickStack v2 API
for post-cutover error/fatal, panic, transition, join, and recovery-required
signals for every role.

ClickStack's Loyal OTLP log channel intentionally exports operational errors
only. Therefore zero matching ClickStack rows is a negative error gate, not
successful-work evidence. Successful planner-to-reconciler and provisioner work
must come from the independent Neon and finalized-chain end-state measurements.
The Render gate now requires all four observability env keys on every fleet role
and validates the fixed non-secret values; the ingestion key is presence-checked
but never copied to evidence.

Neither phase builds or pushes images, deploys services, mutates production
data, signs, or sends transactions. Existing output files are never overwritten.
