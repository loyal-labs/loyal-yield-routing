# ASK-1973 build comparison

Two sources of evidence:

- **Cargo timings**, captured locally on an ARM laptop with Rust/Cargo 1.89.0,
  building the production `same-mint-reserve-swap` binary in release mode. The
  binary moved from `loyal-yield-orchestrator` to `loyal-fleet-worker`; its
  runtime name and Render command are unchanged.
- **Image timings**, pulled from GitHub Actions BuildKit logs. These are the
  numbers that matter for deploy latency; the cargo numbers are a local sanity
  check only.

## 1. Image build times (GitHub Actions)

All jobs ran on `ubuntu-latest`, 4 vCPU, `linux/amd64`, `cache-from`/`cache-to`
`type=gha,mode=max`.

### Runs used

| Run | Commit | Layout | Trigger | Cook cache |
| --- | --- | --- | --- | --- |
| [31182890142](https://github.com/loyal-labs/loyal-yield-routing/actions/runs/31182890142) | `e7b830f` (main) | before | dispatch | cold |
| [31414446958](https://github.com/loyal-labs/loyal-yield-routing/actions/runs/31414446958) | `5125f7f` (main) | before | dispatch | warm |
| [31174060737](https://github.com/loyal-labs/loyal-yield-routing/actions/runs/31174060737) / [31174062792](https://github.com/loyal-labs/loyal-yield-routing/actions/runs/31174062792) | `950f3a1` (PR 34) | after | pull_request | cold |
| [31418377209](https://github.com/loyal-labs/loyal-yield-routing/actions/runs/31418377209) / [31418377110](https://github.com/loyal-labs/loyal-yield-routing/actions/runs/31418377110) | `e5b01ce` (PR 34) | after | pull_request | cold |
| [31419389656](https://github.com/loyal-labs/loyal-yield-routing/actions/runs/31419389656) / [31419389699](https://github.com/loyal-labs/loyal-yield-routing/actions/runs/31419389699) | `dc58c47` (PR 34) | after | pull_request | warm |

Two caveats on cross-layout wall times. Dispatch runs on `main` push to GHCR
(`push: true`, `provenance: mode=max`); PR runs on the branch load into the
local daemon (`load: true`) and then run image probes. And `main`'s workflow
predates the probe steps. **Per-layer numbers are directly comparable; job wall
times are only roughly so.** The probes themselves cost 1–2 s.

### Per-layer split

Seconds, from `#N DONE` in the BuildKit log.

#### `light-workers`

| Layer | before, cold | before, warm | after, cold | after, warm |
| --- | ---: | ---: | ---: | ---: |
| `cargo install cargo-chef` | CACHED | CACHED | CACHED | CACHED |
| `cargo chef cook` | 285.7 | **CACHED** | 230.6–274.4 | **CACHED** |
| `cargo build` | 224.0 | 239.2 | **113.6–130.1** | **141.5** |
| Export image | 35.7 | 33.3 | 31.3–33.1 | 30.8 |
| Export gha cache | 205.3 | 95.7 | 116.0–142.6 | 38.4 |
| **Job wall** | **786** | **427** | **574–597** | **275** |

#### `laserstream-workers`

| Layer | before, cold | before, warm | after, cold | after, warm |
| --- | ---: | ---: | ---: | ---: |
| `cargo chef cook` | 425.1 | **CACHED** | 421.0–426.8 | **CACHED** |
| `cargo build` | 65.4 | 75.1 | 66.9–69.3 | 77.0 |
| Export image | 6.1 | 4.6 | 3.0–4.6 | 3.1 |
| Export gha cache | 118.8 | 16.4 | 88.0–111.1 | 15.9 |
| **Job wall** | **657** | **148** | **617–649** | **161** |

#### `operator-tools` (new image, no "before")

| Layer | after, cold | after, warm |
| --- | ---: | ---: |
| `cargo install cargo-chef` | 50.9 (first run) / CACHED | CACHED |
| `cargo chef cook` | 261.6–274.4 | **CACHED** |
| `cargo build` | 109.7–133.2 | 122.0 |
| Export image | 3.1–3.2 | 3.4 |
| Export gha cache | 95.5–100.5 | 20.4 |
| **Job wall** | **550–565** | **206** |

### What actually changed

The refactor's effect is confined to the `cargo build` layer of
`light-workers`, which is exactly what was predicted: the cook layer compiles
the same third-party set either way, and `laserstream-workers` was already
narrow.

| Measure | before | after | change |
| --- | ---: | ---: | ---: |
| `light-workers` `cargo build` (cold cook both sides) | 224.0 s | 130.1 s | **−42%** |
| `light-workers` `cargo build` (warm cook both sides) | 239.2 s | 141.5 s | **−41%** |
| `light-workers` job wall, warm | 427 s | 275 s | **−36%** |
| `laserstream-workers` `cargo build` | 65.4 s | 66.9 s | flat (by design) |
| `laserstream-workers` job wall, warm | 148 s | 161 s | flat |

`laserstream-workers` wall going *up* by 13 s is not a regression in the build:
its cook and build layers are unchanged, and the after-run additionally loads
the image into the local daemon and runs content/label/command probes that the
before-run's workflow did not have.

Eight operator binaries — roughly 28k LOC including the 12.6k-line verifier and
the 5.6k-line evidence tool — moved out of the production image and into
`operator-tools`, which is built on demand rather than on every release.

## 2. Cook-layer reuse (the acceptance criterion)

**Result: confirmed. A source-only change hits the cooked layer, and nothing in
the Agave/Kamino/laserstream graph recompiles.**

Three independent pieces of evidence.

### Local: `recipe.json` does not move under source-only changes

`cargo chef prepare` (cargo-chef 0.1.77) on the branch tip, before and after
appending to `crates/loyal-fleet-worker/src/lib.rs` and
`crates/loyal-yield-store/src/store.rs`:

```
before: sha256 3257335b557529e2…  356261 bytes
after:  sha256 3257335b557529e2…  356261 bytes   → byte-identical
```

The cook layer's cache key is the `COPY --from=planner recipe.json` plus the
`RUN` string. Neither moves when only crate source changes.

### CI, negative control: a manifest change *does* invalidate it

Commit `e5b01ce` added a `[[bin]]` entry to
`crates/loyal-yield-orchestrator/Cargo.toml` (registering `signer-balance-monitor`,
which arrived from main). `recipe.json` moved, and all three cook layers
rebuilt: 230.6 s, 426.8 s, 274.4 s. This is the intended behaviour — the
explicit `[[bin]]` list is what makes a new binary a visible, cache-invalidating
event rather than a silent one.

### CI, positive test: a source-only change reuses it

Commit `dc58c47` changed exactly one file — `crates/loyal-kamino-codec/src/lib.rs`,
a doc comment, no manifest and no lockfile:

```
#35 [builder 2/6] RUN … cargo chef cook --release … CACHED     (light-workers)
#23 [builder 2/6] RUN … cargo chef cook --release … CACHED     (laserstream-workers)
#21 [builder 2/6] RUN … cargo chef cook --release … CACHED     (operator-tools)
```

Every crate compiled in the subsequent `cargo build` layer is first-party —
15 on `light-workers`, 14 on `laserstream-workers`, 10 on `operator-tools`, all
under `/app/crates/`:

```
Compiling loyal-yield-store v0.1.0 (/app/crates/loyal-yield-store)
Compiling loyal-kamino-codec v0.1.0 (/app/crates/loyal-kamino-codec)
Compiling loyal-fleet-worker v0.1.0 (/app/crates/loyal-fleet-worker)
…
```

No `helius-laserstream`, `laserstream-core-proto`, `agave-*`, `solana-*`,
`klend-*`, `tonic`, `prost`, `hyper`, or `tower` unit appears in any of the three
build-layer logs. The third-party graph compiled once and was reused, which is
the property the whole refactor rests on.

Cache export collapses too, because only thin application layers are left to
write: `light-workers` 142.6 s → 38.4 s, `laserstream-workers` 111.1 s → 15.9 s,
`operator-tools` 95.5 s → 20.4 s.

### Why the feature-resolution hazard does not bite

The known failure mode — narrowing the second invocation so hyper/tower
re-fingerprint and everything above them rebuilds — is avoided structurally: all
three Dockerfiles pass a byte-identical `-p`/`--bin` selection to `cargo chef
cook` and to `cargo build`. Verified mechanically:

```
Dockerfile.light-workers       IDENTICAL selection (14 entries)
Dockerfile.laserstream-workers IDENTICAL selection (8 entries)
Dockerfile.operator-tools      IDENTICAL selection (10 entries)
```

The load-bearing comment above each cook step says so; keep it.

## 3. Where the cook time goes

Ranked from the `laserstream-workers` cook log (738 crate compilations, 421 s,
4 vCPU). Grouped by family; "gap" is the elapsed time between one compilation
starting and the next, which is where the build serialises.

| Family | Crates | Sum of gaps |
| --- | ---: | ---: |
| unclassified (regex, brotli, num-bigint, az, clap, …) | 366 | 159.6 s |
| solana/agave **2.x** | 125 | 64.7 s |
| solana/agave **3.x** (laserstream's generation) | 55 | 38.2 s |
| opentelemetry | 11 | 37.2 s |
| tonic / prost / tower / hyper | 31 | 26.8 s |
| spl | 31 | 20.2 s |
| crypto (curve25519, ed25519, ring, rustls, …) | 35 | 13.7 s |
| reqwest / tls | 12 | 11.6 s |
| kamino / klend | 3 | 4.7 s |
| helius-laserstream itself | 2 | 0.5 s |

Longest single poles: `opentelemetry_sdk` 23.1 s, `solana-account-decoder@3.1.14`
13.1 s, `az` 12.5 s, `reqwest@0.13.1` 9.7 s.

Two conclusions, and the second one is the important one:

1. The expectation that laserstream/Agave/Kamino dominate is **half right**.
   They dominate by *count* — `helius-laserstream` is only two crates itself,
   but it is the sole reason 55 Solana-3.x crates and a second tonic/prost
   generation exist in the graph at all (as recorded in the refactor plan). They
   do not dominate by *individual* cost.
2. There is no single crate worth attacking. 738 compilations in 421 s on 4
   vCPU is about 2.3 core-seconds each — the cook is **throughput-bound, not
   long-pole-bound**. No dependency removal short of deleting laserstream
   changes the shape, and that is blocked. The only lever that moves this number
   is *not compiling the set at all*.

Which makes reuse the whole game — and reuse currently rests on a cache that is
over its limit:

```
GET /repos/loyal-labs/loyal-yield-routing/actions/cache/usage
  active_caches_size_in_bytes: 11502877445    # 10.71 GiB, limit 10 GiB
  active_caches_count:         419
```

`refs/heads/main` holds 8.4 GiB across two scopes; `refs/pull/34/merge` holds
another 2.3 GiB. The oldest surviving entry was last touched six days ago.
Eviction is already happening, and this branch adds a third scope.

The proposal for a prebuilt deps base image on GHCR — rebuilt only on
`Cargo.lock` changes, with the worker Dockerfiles `FROM` it — is in
[`../plans/ask-1973-deps-base-image-proposal.md`](../plans/ask-1973-deps-base-image-proposal.md).
It is a proposal with expected numbers, not an implementation.

## 4. Is a ~7 minute deploy reachable?

Yes, and on the warm path it is already beaten. The end-to-end budget for a
single-service hotfix, measured:

| Stage | Time | Source |
| --- | ---: | --- |
| CI image build, `light-workers`, warm cook | 4 m 35 s | run 31419389656 |
| CI image build, `laserstream-workers`, warm cook | 2 m 41 s | run 31419389656 |
| GHCR push (inside the export step on dispatch runs) | ~33 s | run 31414446958 |
| Render pull + swap, per worker | 20–50 s | Render API, last 6 deploys per service |

Render deploy durations from the API, for the services that consume these
images: `loyal-balance-sweep-ata-projector` 18–25 s,
`loyal-kamino-reserve-monitor` 29–44 s, `loyal-fleet-route-executor` 25–47 s
(one 4 m 34 s outlier on 2026-07-30). **Render is not the bottleneck and never
was** — the ~20 minute figure is essentially all image build.

Where the ~20 minutes went before, and where the remaining minutes go now:

| | Before | After (warm) |
| --- | ---: | ---: |
| `cargo chef cook` (third-party) | 285.7 s, or CACHED | CACHED |
| `cargo build` (our code) | 224–239 s | **141.5 s** |
| Image export + GHCR push | ~35 s | ~31 s |
| gha cache export | 95.7–205.3 s | **38.4 s** |
| Render pull + swap | ~30 s | ~30 s |
| **Total, warm** | **~7.5 min** | **~5.1 min** |
| **Total, cold cook** | **~13.6 min** | **~10.1–10.5 min** |

The remaining minutes on the warm path, as a share of the 275 s wall (layers
overlap, so these do not sum to 100%):

1. **`cargo build`, 141.5 s (51%).** This is our own code and it is now the
   largest single item. Plan Phase 4 already splits the two monoliths that
   dominate it; `loyal-fleet-worker` and `loyal-route-lookup-tables` are
   extracted, but `same-mint-reserve-swap`'s 9.3k lines of tests still sit in
   the same package as the binary, and release builds still codegen one large
   unit per binary. This is where the next real win is.
2. **gha cache export, 38.4 s (14%).**
3. **Image export, 30.8 s (11%).** Mostly fixed cost.
4. **Source `COPY` layers, 22.2 s (8%).** `COPY .sqlx` alone is 19.1 s, which is
   suspicious for 18 small JSON files and worth a look.
5. **Runner and job overhead, ~20 s (7%).**

The caveat on all of this is the cold-cook cliff: 574–597 s instead of 275 s
whenever the GitHub Actions cache has evicted a scope. That is not a build
problem, it is a cache-budget problem, and it is quantified in §3.

## 5. Cargo timings (local, ARM laptop)

Sanity check only. This machine is not the build environment and its numbers do
not predict image build time.

| Scenario | Before | After | Change |
| --- | ---: | ---: | ---: |
| Cold release build | 115 s | 105 s | −10 s (−8.7%) |
| Warm no-op release build | 1 s | 0 s | below one-second capture resolution |
| Warm shared-store change | 26 s | 23 s | −3 s (−11.5%) |

## 6. Dependency counts

Unique-line result from `cargo tree -p <package> -e normal --prefix none
--no-dedupe`, captured by `scripts/build-timings.sh`.

| Package | Before | After | Change |
| --- | ---: | ---: | ---: |
| `balance-sweep-ata-observations` | 529 | 134 | −395 (−74.7%) |
| `kamino-historic-data` | 705 | 527 | −178 (−25.2%) |
| `loyal-yield-orchestrator` | 528 | 532 | +4 (+0.8%) |

The orchestrator's transitive package count is intentionally flat because it
re-exports the extracted store and lookup-table surfaces for compatibility. Its
compile unit is substantially smaller: fleet worker implementation/tests,
lookup-table implementation/tests, migrations, signer/RPC helpers, and pure
Kamino decoding no longer live in that crate.

Raw timing JSONL, logs, metadata, duplicate trees, and all package counts are in
[`before`](before/) and [`after`](after/).

## 7. Verification status

- `cargo fmt --all -- --check`, `cargo check --workspace --all-targets --locked`,
  `cargo clippy --workspace --all-targets --locked -- -D warnings`,
  `cargo test --workspace --locked`, the extracted-crate narrow builds,
  `bun run test:squads`, the historical `bun run test:squads:e2e` replay, the
  ABI/spec drift gate, SQLx offline metadata check, and lint pass.
- A local source build of the repository-documented QEDGen 2.30.0 passes the
  active ABI drift, 25/25-operation coverage, Pinocchio probe, and generated
  proptest gates after aligning the specification's authorization and lane
  invariants with the implementation.
- `bun run verify:ask-1973-public-api` compiles 488 pinned legacy orchestrator
  facade paths. `cargo public-api` remains an ownership audit rather than a
  source-compatibility gate because rustdoc attributes re-exported definitions
  to their new canonical crates.
- Image verification runs in CI, not locally: this ARM host has no `docker`, and
  a Podman VM reproducibly failed in `rustc -vV` under QEMU on Rust 1.89 before
  any repository code compiled. The PR workflows build all three images for
  linux/amd64 and probe their contents, command, labels, and every fleet role
  entrypoint; those checks are green on `dc58c47`.
