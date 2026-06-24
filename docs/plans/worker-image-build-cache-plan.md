# Worker Image Build Cache Plan

Recorded: 2026-06-24.

## Goal

Reduce routine `worker-images` workflow wall time by making Docker layer caching
and Cargo rebuild behavior predictable on GitHub-hosted runners.

The immediate target is to turn script-only or docs-only worker image rebuilds
from "recompile most of the Rust dependency graph" into "reuse dependency
layers, rebuild only local crates that Cargo actually considers dirty, then
package and push the runtime image."

This plan must preserve the repo's low-maintenance Dockerfile shape. We should
not add a long manual list of every source file or manifest that needs to be
copied whenever the repo grows. `cargo-chef` is the right primitive for that
because it computes the dependency recipe from the workspace instead of relying
on hand-maintained manifest copies.

## Verifier

Use this verifier after implementation. PASS requires every required section to
pass.

### Required: cache shape

- `Dockerfile.light-workers` and `Dockerfile.laserstream-workers` still use
  `cargo chef prepare` before `cargo chef cook`, and the `cook` layer is the
  dependency cache layer.
- The final Rust build no longer hides the `target` directory created by
  `cargo chef cook` behind a BuildKit cache mount.
- If BuildKit cache mounts remain in the Dockerfiles, the plan names how their
  data is persisted across GitHub-hosted runners. Otherwise, the Dockerfiles
  rely on exported BuildKit layers, not ephemeral cache mounts, for Cargo build
  reuse.
- Both worker images keep separate cache scopes or cache refs so one image does
  not overwrite the other's cache.

### Required: maintenance boundary

- The Rust builder stage does not use a hand-maintained list of every workspace
  crate source file.
- Any `.dockerignore` changes are broad and explainable, such as excluding
  non-build artifacts. They must not rely on remembering to add every future
  source path.
- Runtime image copies remain limited to artifacts that actually need to be in
  the runtime image: compiled binaries, required scripts, package metadata, and
  production JS dependencies.

### Required: benchmark evidence

Collect before/after evidence from at least two `worker-images` runs:

- one warm build after a non-Rust change, such as a docs or smoke-script change;
- one warm build after a Rust local-crate change.

For each run, record:

- total workflow wall time;
- `cargo chef cook` duration and whether it was cached;
- final `cargo build --release` duration;
- image export/push duration;
- cache export duration.

PASS target:

- non-Rust warm rebuild: final Rust build should not recompile third-party
  Solana/Kamino dependency graphs;
- Rust local-crate warm rebuild: final Rust build should reuse dependency
  artifacts and spend most compile time in changed local crates and final
  binaries;
- no cache export step should dominate the workflow for routine builds.

## Source Notes

- `cargo-chef` is designed for Docker builds: `prepare` creates a recipe from
  the workspace dependency shape, `cook` builds dependencies from that recipe,
  and the application source is copied only after the dependency layer. Its
  README also calls out that all stages must use the same Rust version and that
  `cook` and the final `cargo build` must run from the same working directory.
  Source: https://github.com/LukeMathWalker/cargo-chef
- Docker's GitHub Actions cache backend supports `cache-from: type=gha` and
  `cache-to: type=gha,mode=max`, but Docker documents the backend as
  experimental and separately notes that BuildKit cache mounts are not preserved
  in the GitHub Actions cache by default. Source:
  https://docs.docker.com/build/ci/github-actions/cache/
- Docker's `gha` cache backend uses `scope` to identify cache objects. If
  multiple images share the default scope, they overwrite each other's cache, so
  per-image scopes are required for matrix builds. Source:
  https://docs.docker.com/build/cache/backends/gha/
- GitHub Actions caches are branch-scoped and subject to cache access rules,
  retention, and repository size limits. Source:
  https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching

## Current Diagnosis

Workflow run `28097709842` spent almost all wall time in the matrix jobs'
`Build and push image` step:

- `laserstream-workers`: final `cargo build --release` took about 479.7 seconds.
- `light-workers`: final `cargo build --release` took about 280.4 seconds, then
  GitHub Actions cache export took about 71.2 seconds.

The changed commit only touched `scripts/mainnet-loyal-hub-tests.ts`, but both
Dockerfiles do `COPY . .` before the final Rust build. That invalidated the
final build instruction. The dependency `cargo chef cook` step showed as cached,
yet the final build still updated the crates.io index, updated the Kamino git
dependency, downloaded crates, and compiled third-party dependencies.

The likely reason is the current combination of `cargo-chef` and BuildKit cache
mounts:

```dockerfile
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    --mount=type=cache,target=/app/target \
    cargo chef cook --release ...
```

and then:

```dockerfile
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    --mount=type=cache,target=/app/target \
    cargo build --release ...
```

This undermines the standard `cargo-chef` Docker layer model on
GitHub-hosted runners. The `cook` Docker layer can be cached while the expensive
Cargo output lives in a cache mount that Docker says is not preserved in the
GitHub Actions cache by default. The final build then mounts `/app/target`
again, which hides any lower-layer target directory even if a previous layer
had populated one.

## Proposed Plan

### Phase 1: restore cargo-chef layer caching

Change both worker Dockerfiles to use `cargo-chef` in its standard layer-cache
shape:

- keep the `chef`, `planner`, `builder`, and `runtime` stages;
- keep `COPY . .` in the planner and builder stages so the workspace remains
  self-maintaining as crates and build files move;
- remove `--mount=type=cache,target=/app/target` from `cargo chef cook`;
- remove `--mount=type=cache,target=/app/target` from the final
  `cargo build --release`;
- strongly prefer removing the Cargo registry/git cache mounts from those two
  Rust build instructions as well, unless testing shows they help without
  hiding data needed by Cargo's fingerprinting.

Expected effect:

- `cargo chef cook` creates a real Docker layer containing the compiled
  dependency artifacts.
- The final `cargo build --release` sees `/app/target` from the dependency
  layer and can reuse it after `COPY . .`.
- A change outside Rust dependency manifests should still invalidate the final
  local source build instruction, but it should not force the full third-party
  dependency graph to rebuild.

Tradeoff:

- The exported BuildKit cache may be larger because compiled dependencies live
  in layers instead of transient mounts. This is acceptable for the first pass
  because it restores the documented `cargo-chef` behavior and makes cache hits
  explainable.

### Phase 2: move Docker cache storage to GHCR registry cache if GHA export is heavy

If Phase 1 improves compile reuse but GitHub cache export remains slow or
thrashes repository cache limits, switch Docker layer cache storage from
`type=gha` to per-image registry cache refs in GHCR:

```yaml
cache-from: type=registry,ref=${{ env.REGISTRY }}/${{ env.IMAGE_NAMESPACE }}/${{ matrix.image }}:buildcache
cache-to: type=registry,ref=${{ env.REGISTRY }}/${{ env.IMAGE_NAMESPACE }}/${{ matrix.image }}:buildcache,mode=max
```

Keep normal immutable runtime tags as:

```yaml
tags: ${{ env.REGISTRY }}/${{ env.IMAGE_NAMESPACE }}/${{ matrix.image }}:sha-${{ github.sha }}
```

Expected effect:

- Docker layer cache moves next to the existing private worker images in GHCR.
- The workflow avoids the GitHub Actions cache's repository-wide size and
  retention pressure for large Rust build layers.
- The cache remains naturally separated by worker image.

Tradeoff:

- GHCR retains extra build-cache artifacts. Add a cleanup policy only after
  confirming retention behavior and package visibility.

### Phase 3: only use cache mounts if we explicitly persist them

Do not rely on BuildKit cache mounts for cross-run Cargo reuse unless we also
add an explicit persistence mechanism.

Acceptable options:

- avoid cache mounts in the Rust build path and use BuildKit layer cache only;
- use Docker's documented `buildkit-cache-dance` workaround to extract and
  inject cache mounts into the GitHub Actions cache;
- move Rust compilation out of Docker and use `actions/cache` for
  `~/.cargo/registry`, `~/.cargo/git`, and `target`, then package prebuilt
  binaries into small runtime images.

Recommended first choice: avoid cache mounts in the Rust build path. It is less
clever, but it matches `cargo-chef`'s model and needs fewer moving pieces.

### Phase 4: add workflow observability

Make cache behavior visible in every worker image run:

- set BuildKit progress to plain output if needed for easier log parsing;
- add a final summary step that prints elapsed time for each major build
  phase, or store a small markdown note in the GitHub job summary;
- after each build, record whether `cargo chef cook` was `CACHED`, how long the
  final `cargo build` took, and how long cache export took.

This keeps future investigations from requiring a full raw-log scrape.

### Phase 5: optional workflow controls, not cache correctness

After cache correctness is fixed, add small operator conveniences:

- a `workflow_dispatch` input for `image` with values `all`,
  `light-workers`, and `laserstream-workers`;
- a `workflow_dispatch` input for `push` or `dry-run` if local validation of
  Docker cache behavior is useful.

These controls should not be the main fix. They reduce unnecessary work when an
operator knows only one image is needed, but they do not solve slow warm builds.

## Non-Goals

- Do not switch Render workers back to Render Docker builds.
- Do not replace immutable `sha-<commit>` runtime tags.
- Do not maintain a long Dockerfile allowlist of every Rust source path.
- Do not optimize by excluding files from the build context unless there is a
  clear, low-risk category such as docs, local outputs, or unrelated generated
  artifacts.
- Do not introduce secret material into workflow logs or Docker build args.

## Implementation Checklist

1. Patch `Dockerfile.light-workers` and `Dockerfile.laserstream-workers` to
   restore standard `cargo-chef` dependency-layer behavior.
2. Run local syntax/build sanity checks that do not require secrets:
   `docker buildx build --file Dockerfile.light-workers --target builder --load .`
   and the corresponding laserstream builder target if local Docker is
   available.
3. Trigger `worker-images` on a non-Rust change and record timings in a follow-up
   note under this file or a dated run document.
4. Trigger `worker-images` on a small Rust local-crate change and record timings.
5. If compile reuse is good but cache export is still slow, switch to GHCR
   registry cache refs and repeat the two benchmark runs.
6. If final local-crate rebuilds are still too slow, open a separate plan for
   runner-native Rust builds plus small packaging images or `sccache`.

## Review Notes

Reviewer thread: append review points below this line. Use `No further review
points.` when the plan is acceptable as written.

No further review points.

Second review pass: No further review points.

## First Fix Step

The minimal Dockerfile change is to remove the `/app/target` BuildKit cache
mount from both Rust build steps in both worker Dockerfiles:

- `Dockerfile.light-workers`: remove the `/app/target` mount from the
  `cargo chef cook --release` step and the final `cargo build --release` step.
- `Dockerfile.laserstream-workers`: remove the `/app/target` mount from the
  `cargo chef cook --release` step and the final `cargo build --release` step.

Patch shape:

```diff
 RUN --mount=type=cache,target=/usr/local/cargo/registry \
     --mount=type=cache,target=/usr/local/cargo/git \
-    --mount=type=cache,target=/app/target \
     cargo chef cook --release ...

 COPY . .
 RUN --mount=type=cache,target=/usr/local/cargo/registry \
     --mount=type=cache,target=/usr/local/cargo/git \
-    --mount=type=cache,target=/app/target \
     cargo build --release ...
```

Why this helps: `cargo chef cook` should leave compiled dependency artifacts in
the Docker layer. With `/app/target` mounted as a BuildKit cache, the expensive
Rust target output is written outside the layer, so a cached cook layer does not
contain the compiled dependencies. The final `cargo build` also mounted
`/app/target`, which would hide any target artifacts present in the image layer.

Keep `COPY . .` as-is so the Dockerfiles do not need a hand-maintained source
allowlist whenever this repository changes. The slightly more robust variant is
to remove the Cargo registry/git mounts too, but the critical minimal fix is
removing only the `/app/target` cache mounts.

Execution status: applied as the first implementation step, leaving the Cargo
registry and git cache mounts in place.
