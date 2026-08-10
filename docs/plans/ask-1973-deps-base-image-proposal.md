# ASK-1973 follow-up — prebuilt dependency base image

Proposal only. Nothing here is implemented. Numbers are measured from the CI
runs recorded in [`../build-timings/comparison.md`](../build-timings/comparison.md).

## 1. The problem this solves

After the crate refactor, the `cargo chef cook` layer is the single largest
cost in every worker image, and it is *entirely* third-party code:

| Image | cook layer (cold) | crates compiled in the cook |
| --- | ---: | ---: |
| `laserstream-workers` | 421.0 s | 738 |
| `light-workers` | 274.4 s | 575 |
| `operator-tools` | 261.6 s | ~560 |

Reuse of that layer rests on one thing: the GitHub Actions cache. That cache is
already over budget.

```
GET /repos/loyal-labs/loyal-yield-routing/actions/cache/usage
  active_caches_size_in_bytes: 11502877445   # 10.71 GiB
  active_caches_count:         419
```

GitHub's per-repository limit is 10 GiB, and eviction is LRU. Observed
consequences today:

- The oldest surviving cache entry was last accessed **2026-08-04** — the
  effective retention window is about six days, not the nominal seven.
- `refs/heads/main` holds 8.4 GiB across two image scopes; PR #34 alone holds a
  further 2.3 GiB under `refs/pull/34/merge`. Every open PR that touches
  `crates/**` competes with main for the same budget.
- This branch adds a **third** scope (`operator-tools`). Once merged, main's
  steady-state footprint grows by roughly half again, against a budget that is
  already exceeded.

So the reuse the refactor depends on is real (see §2 of the comparison doc) but
it is not durable. When a scope is evicted, the next deploy pays the full cold
cook — 421 s on laserstream, 274 s on light — plus a re-export of the cache.
That is the difference between a 4-minute deploy and a 10-minute one, decided by
an LRU policy we do not control.

The cache export is itself expensive, and it is paid on *every* build, hit or
miss:

| Image | `exporting to GitHub Actions Cache` |
| --- | ---: |
| `light-workers`, cold | 205.3 s |
| `light-workers`, warm | 95.7 s |
| `laserstream-workers`, cold | 118.8 s |
| `operator-tools`, cold | 100.5 s |

On the warm `light-workers` run, cache export was 95.7 s of a 427 s job — 22% of
wall time spent writing a cache whose only purpose is to survive to the next run.

## 2. Proposal

Publish the cooked dependency tree as an image on GHCR, rebuilt only when the
dependency graph actually changes, and have the worker Dockerfiles `FROM` it.

### Tagging

Tag by a digest over the inputs that can change the cooked output:

```
ghcr.io/loyal-labs/loyal-yield-routing/deps-base-<image>:lock-<sha256[:12]>
```

where the digest covers `Cargo.lock`, `Cargo.toml`, every
`crates/*/Cargo.toml`, `rust-toolchain.toml`, and the image's own cook
selection. This is the same input set that `cargo chef prepare` reduces to
`recipe.json`, which we verified is byte-identical under source-only changes —
so the tag moves exactly when the cook would have had to rerun anyway.

### One base per image, not one shared base

Three tags, built by one workflow. A single union base would cook the
laserstream/Solana-3.1 graph into the base that `light-workers` pulls, undoing
the graph separation this refactor established and inflating the pull on the
image that builds most often. Per-image bases cost more to build (once, on a
lock change) and less to consume (on every build).

### Workflow

`deps-base-images.yml`, triggered on:

- `push` to `main` affecting `Cargo.lock`, `Cargo.toml`, `crates/*/Cargo.toml`,
  or `rust-toolchain.toml`
- `workflow_dispatch`

It computes the tag, checks whether it already exists on GHCR, and skips the
build if so. Content: the `chef` stage as it exists today (toolchain, apt
packages, `cargo install cargo-chef`), plus `cargo chef cook --release <the
image's selection>`, leaving the populated `/app/target` and `CARGO_HOME` in
place.

### Worker Dockerfile change

```dockerfile
ARG DEPS_BASE_TAG
FROM ghcr.io/loyal-labs/loyal-yield-routing/deps-base-light-workers:${DEPS_BASE_TAG} AS builder
WORKDIR /app
COPY Cargo.toml Cargo.lock rust-toolchain.toml ./
COPY .sqlx .sqlx
COPY crates crates
RUN cargo build --release <same selection as the base cooked> && cp ...
```

The planner and chef stages disappear from the worker Dockerfiles entirely, as
does `cargo install cargo-chef` (50.9 s when cold).

The **cook and build selections must stay byte-identical**, exactly as the
comment in the current Dockerfiles says — the base's cook and the worker's build
are still two invocations, so a narrower second call would re-fingerprint
hyper/tower and cascade a rebuild. Moving the cook into a base image does not
relax that constraint; it makes violating it more expensive, because the
mismatch would no longer be visible in the same log. The CI check should diff
the two selections and fail on drift.

### Cache configuration after this lands

`cache-to: type=gha,mode=max` can drop to `mode=min` or be removed. The
deps that justified `mode=max` would live in the base image; what is left is a
handful of thin application layers. That returns most of the 10 GiB budget and
removes cross-PR cache contention as a deploy-time variable.

## 3. Expected numbers

Per worker build, against measured layer costs:

`light-workers`, measured on the post-refactor branch:

| Phase | Today, cache hit | Today, cache miss | With deps base |
| --- | ---: | ---: | ---: |
| `cargo install cargo-chef` | CACHED | CACHED | — (in base) |
| `cargo chef prepare` (planner) | 1.2 s | 1.2 s | — (in base) |
| `cargo chef cook` | CACHED | 230.6–274.4 s | — (in base) |
| Pull base image | — | — | ~35 s (est.) |
| `COPY .sqlx` + `COPY crates` | 22.2 s | 24.0 s | ~22 s |
| `cargo build` | 141.5 s | 113.6–130.1 s | ~135 s |
| Export to image | 30.8 s | 31.3 s | ~31 s |
| Export gha cache | 38.4 s | 142.6 s | ~15 s (est.) |
| Job overhead | ~20 s | ~20 s | ~20 s |
| **Job wall** | **275 s** | **574–597 s** | **~260 s (est.)** |

**This is a weaker case than it looked before the warm numbers came in.**
Against a cache *hit*, the base image saves almost nothing — perhaps 15 s — and
it might even lose, because a ~2–3 GB base pull can cost more than the 38.4 s
cache export it replaces. The two estimates above are unmeasured and are the
whole risk:

- **Base pull ~35 s.** If it lands at 90 s, the hit case is a net regression.
- **Residual gha export ~15 s.**

The case rests entirely on the *miss*, which costs 574–597 s today against 275 s
warm — a 5-minute cliff that lands without warning whenever LRU eviction takes a
scope. The base image converts that cliff into a bounded, predictable pull.

So the honest recommendation is: **do not build this to make deploys faster.**
Build it if the eviction cliff is judged unacceptable for hotfix latency. If the
cliff is tolerable, the cheaper mitigations in §5 address the same risk for far
less work, and should be tried first.

## 4. Cheaper alternatives to try first

All three attack the eviction cliff without a new image or a new workflow:

1. **Move the cache off the 10 GiB budget.** Switch `cache-to` to
   `type=registry,ref=ghcr.io/loyal-labs/loyal-yield-routing/<image>:buildcache,mode=max`.
   Registry-backed BuildKit cache has no repo-wide cap and no LRU eviction we
   don't control. This is a two-line change per workflow and captures most of
   the base image's benefit — it is the option the refactor plan already listed
   under Phase 1 step 8, and the cache-usage measurement now justifies acting on
   it. **Recommended first move.**
2. **Stop caching what we don't reuse.** `mode=max` caches every intermediate
   layer of every stage. `mode=min` on the runtime stages, keeping `max` only
   where the cook lives, would cut the footprint materially.
3. **Prune PR caches on merge/close.** PR #34 alone holds 2.3 GiB. A cleanup job
   on `pull_request: closed` would return that to the budget immediately.

If (1) lands, the base image's remaining advantage is only the ~250 s of cook
work saved on a genuine `Cargo.lock` change, which is rare. That is likely not
worth the operational surface described in §5.

## 5. What it does not do

- It does not reduce `cargo build` — that is our own code, 130 s on
  `light-workers`, and only the Phase 4 crate splits move it.
- It does not remove the laserstream/Solana-3.1 duplicate generation. That
  blocker stands as recorded in the refactor plan.
- It adds an ordering constraint: a `Cargo.lock` change must publish a base
  before the worker build that consumes it. The tag-exists check plus a clear
  failure message handles this, but it is new operational surface.
