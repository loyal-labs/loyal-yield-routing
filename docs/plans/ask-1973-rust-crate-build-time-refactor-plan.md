# ASK-1973 — Rust crate refactor for build time and reuse

Plan of record for reducing `loyal-yield-routing` Rust build times and improving
crate reuse. All measurements below were taken on `main` at commit `4a0304b`.

## 1. Verified baseline

### Workspace shape

- `Cargo.toml` is a virtual workspace with `members = ["crates/*"]` and **no
  `default-members`**. Any untargeted root command selects all 20 crates.
- `Cargo.lock` holds **980 package versions across 819 unique names**; **161
  names are duplicated** at more than one major version.
- Local `target/` is **30 GB** — `target/debug` 28 GB, `target/release` 2.8 GB.

### Source weight

`loyal-yield-orchestrator` is 114,734 LOC, an order of magnitude larger than any
other crate. Roughly 74k of that is `src/bin/`:

| File | LOC | Notes |
| --- | --- | --- |
| `src/bin/same-mint-reserve-swap.rs` | 22,096 | production worker; tests start at 12,779 |
| `src/lookup_tables.rs` | 17,696 | lib; tests start at 4,202 |
| `src/bin/fleet-orchestration-verifier.rs` | 12,595 | operator-only |
| `src/bin/verify-reusable-alt-db.rs` | 7,153 | operator-only |
| `src/fleet_orchestration/queue.rs` | 6,079 | lib |
| `src/bin/fleet-orchestration-production-evidence.rs` | 5,629 | operator-only |
| `src/bin/route-lookup-table-provisioner.rs` | 5,351 | production worker |
| `src/store.rs` | 3,853 | lib |

`src/bin/` holds **17** auto-discovered binaries and there is no `autobins =
false`, so any `cargo check -p loyal-yield-orchestrator --all-targets` or
`cargo build -p loyal-yield-orchestrator` compiles all 17.

### Module dependency cones inside the orchestrator

This is the finding that makes the split cheap. External imports per module:

| Module | LOC | External deps |
| --- | --- | --- |
| `store.rs` | 3,853 | chrono, serde_json, sha2, **sqlx only** |
| `types.rs` | 853 | chrono, serde, serde_json |
| `domain.rs` | 473 | serde_json |
| `lookup_table_alerts.rs` | 1,871 | chrono, serde, serde_json, sha2, sqlx |
| `stable_mints.rs` | 122 | loyal-actions, sha2, thiserror |
| `signer.rs` | 245 | solana-sdk, thiserror |
| `rpc_safety.rs` | 202 | solana-sdk |
| `lookup_tables.rs` | 17,696 | solana-sdk, sqlx, sha2, serde |
| `shared_market_catalog.rs` | 553 | **klend-interface, solana-client (RPC)**, loyal-actions |
| `fleet_orchestration/**` | 13,238 | full graph |

`OrchestratorStore`, `OrchestratorConfig`, `OrchestratorError`, `NeonSqlClient`
all live in `store.rs`, which touches **no Solana, no reqwest, no klend, no
observability**. Everything heavyweight in the orchestrator's dependency graph
(`solana-client`, `solana-rpc-client`, `reqwest`, `klend-interface`,
`loyal-observability`/OTLP) enters through `shared_market_catalog.rs`,
`lookup_tables.rs`, `fleet_orchestration/**`, `signer.rs`, and the binaries.

### What the small crates actually use from the orchestrator

| Crate | Symbols used | Module they live in |
| --- | --- | --- |
| `balance-sweep-ata-observations` | `BalanceSweepTargetId`, `WalletAtaBalanceUpdateInput` | types/domain |
| `balance-sweep-ata-projector` | `OrchestratorConfig`, `OrchestratorError`, `OrchestratorStore`, `ProjectedWalletAtaBalanceUpdateInput` | store/types |
| `balance-sweep-ata-monitor` | `BalanceSweepTarget(Id)`, `OrchestratorConfig`, `OrchestratorError`, `OrchestratorStore` | store/types |
| `loyal-squads-policy-monitor` | `BalanceSweepExecutionInput`, `BalanceSweepPolicyMatchInput`, `PolicyMatchInput`, `OrchestratorConfig`, `OrchestratorError`, `OrchestratorStore` | store/types |
| `autonomous-vaults` | `decode_kamino_reserve_account`, `KaminoReserveCatalogAccount`, `keypair_from_env`, `policy_keypair_from_env`, `rpc_safety::validate_rpc_genesis_hash`, `NeonSqlClient`, `NeonSqlConfig`, `PolicyMatchInput`, `FIXED_KAMINO_MAIN_ROUTE_MODE` | catalog + signer + rpc_safety + store |

Four crates need **only the SQL layer**. `balance-sweep-ata-observations` is a
392-LOC crate that today compiles solana-client, solana-rpc-client, reqwest,
klend-interface, and the OTLP stack to get two struct definitions.

Same pattern one level over: `kamino-historic-data` depends on the
`kamino-reserve-monitor` **package** for `targets::SupportedReserveRecord` and a
few decoders, which drags in `helius-laserstream`.

### Where the duplicate Solana generation comes from

`cargo tree -i solana-pubkey@3.0.0` resolves cleanly to one root:

```
helius-laserstream v0.1.10
  └── laserstream-core-proto v9.0.2
        ├── solana-account-decoder v3.1.14
        ├── solana-transaction-status v3.1.14
        └── agave-feature-set / agave-reserved-account-keys v3.1.14
```

Our crates pin `solana-sdk 2.3.1` / `solana-program 2.3.0` via
`[workspace.dependencies]`. So the 2.3 and 3.1 generations coexist **only**
because of `helius-laserstream`. Selected duplicate sets:

| Package | Versions in lock |
| --- | --- |
| `solana-pubkey` | 2.4.0, 3.0.0, 4.2.0 |
| `solana-hash` | 2.3.0, 3.1.0, 4.4.0 |
| `solana-system-interface` | 1.0.0, 2.0.0, 3.2.0 |
| `prost` | 0.12.6, 0.13.5, 0.14.3 |
| `spl-token` | 7.0.0, 8.0.0 |
| `anchor-lang` | 0.31.1, 0.32.1 |
| `base64` | 0.12.3, 0.13.1, 0.21.7, 0.22.1 |
| `rand` | 0.7.3, 0.8.6, 0.9.4 |

`kamino-reserve-monitor` declares `solana-account-decoder = "2.3"` *and* pulls
`helius-laserstream`, so that one binary compiles both `solana-account-decoder`
2.3 and 3.1.14.

### Image binary selection

`Dockerfile.light-workers` builds and packages **17 binaries**. Cross-checking
against every `dockerCommand` and `preDeployCommand` in `render.yaml`:

| Binary | Used by a light-image service? |
| --- | --- |
| `balance-sweep-ata-projector` | yes (prod + staging) |
| `balance-sweep-autodeposit-trigger` | yes (prod + staging) |
| `loyal-yield-realtime` | yes (prod web) |
| `yield-migrations` | yes (every light predeploy) |
| `same-mint-reserve-swap` | yes (revalidator, executor, reconciler) |
| `fleet-opportunity-planner` | yes (prod) |
| `fleet-route-confirmer` | yes (prod) |
| `route-lookup-table-provisioner` | yes (prod) |
| `same-mint-yield-monitor` | staging only |
| `loyal-timescale-migrations` | **no** — only the laserstream image's predeploy (render.yaml:49, :94) uses it |
| `fleet-orchestration-verifier` | **no** — operator |
| `fleet-orchestration-production-evidence` | **no** — operator |
| `same-mint-monitor-e2e` | **no** — E2E |
| `route-lookup-table-shared-catalog` | **no** — operator |
| `route-lookup-table-alert-monitor` | **no** — operator |
| `route-lookup-table-legacy-import` | **no** — operator |
| `route-lookup-table-cleanup` | **no** — operator |

Eight binaries — about **28k LOC of codegen**, including the 12.6k-LOC verifier
and the 5.6k-LOC evidence tool — are compiled and shipped on every light image
build for no runtime reason.

### sqlx features

Only **one file in the whole workspace** uses compile-time sqlx macros:
`crates/loyal-yield-orchestrator/src/store.rs`. `.sqlx/` holds 18 cached query
files. But `macros` is enabled in `loyal-yield-orchestrator`,
`loyal-yield-realtime-core`, **and** `loyal-yield-realtime`. The latter two pull
`sqlx-macros` + `sqlx-macros-core` (which drags `dotenvy`, `sha2`, `syn`,
`proc-macro2` proc-macro codegen) for nothing.

### Workflow

`.github/workflows/worker-images.yml` is `workflow_dispatch` with a hardcoded
2-entry matrix. Every dispatch builds both images. Cache is
`type=gha,mode=max`, which shares the repo-wide 10 GB GitHub Actions cache
budget across both scopes.

---

## 2. Target crate graph

New crates in **bold**.

```
loyal-hub-abi ──> loyal-actions ──┐
                                  ├─> loyal-kamino-codec (new)
klend-interface ──────────────────┘        │
                                           ├─> kamino-reserve-monitor  [laserstream]
                                           ├─> kamino-historic-data    [no laserstream]
                                           └─> autonomous-vaults

loyal-yield-store (new: store.rs + types.rs + domain.rs)
   deps: chrono, serde, serde_json, sha2, sqlx
      ├─> balance-sweep-ata-observations
      ├─> balance-sweep-ata-projector
      ├─> balance-sweep-ata-monitor
      ├─> loyal-squads-policy-monitor
      ├─> autonomous-vaults
      └─> loyal-yield-orchestrator

loyal-solana-env (new: signer.rs + rpc_safety.rs)
   deps: solana-sdk, thiserror
      ├─> autonomous-vaults
      └─> loyal-yield-orchestrator

loyal-yield-orchestrator (retains lookup_tables, lookup_table_alerts,
   fleet_orchestration, shared_market_catalog, stable_mints, all bins)
```

`loyal-yield-orchestrator` keeps re-exporting the moved symbols from its
`lib.rs` so the migration is mechanical and no call site changes in step 1.

---

## 3. Phased implementation

Each phase is independently shippable and independently measurable. Do not
combine phases in one commit — the acceptance criteria require per-change
timings.

### Phase 0 — Measurement harness (do first, no behavior change)

1. Add `scripts/build-timings.sh` capturing the four required scenarios:
   - cold: `cargo clean && cargo build --release <selection>`
   - warm no-op: re-run the same command
   - warm local change: `touch crates/<c>/src/lib.rs && cargo build --release …`
   - image phases: `docker buildx build --progress=plain` with per-step
     durations parsed out of the log
2. Record `cargo build --timings=json` output per scenario into
   `docs/build-timings/before/`.
3. Snapshot the graph:
   ```sh
   cargo tree --duplicates > docs/build-timings/before/duplicates.txt
   for p in $(cargo metadata --no-deps --format-version=1 | jq -r '.packages[].name'); do
     printf '%s %s\n' "$p" "$(cargo tree -p "$p" -e normal --prefix none --no-dedupe 2>/dev/null | sort -u | wc -l)"
   done > docs/build-timings/before/dep-counts.txt
   ```

This file **is** the deliverable for the "before/after comparison" and
"before/after timings" acceptance criteria. Everything after re-runs it into
`after/`.

### Phase 1 — Zero-risk selection and feature fixes

No code moves. Expect the largest ratio of win to risk.

1. **Prune the light image.** Drop these from both `cargo chef cook` and
   `cargo build` selections, from the `cp` list, and from the runtime `COPY`
   list in `Dockerfile.light-workers`: `loyal-timescale-migrations`,
   `fleet-orchestration-verifier`,
   `fleet-orchestration-production-evidence`, `same-mint-monitor-e2e`,
   `route-lookup-table-shared-catalog`, `route-lookup-table-alert-monitor`,
   `route-lookup-table-legacy-import`, `route-lookup-table-cleanup`. Keep
   `-p loyal-timescale-migrations` out of the package list too.
   Keep `same-mint-yield-monitor` — staging uses it.
   **Keep the cook and build selections byte-identical** — the existing comment
   in both Dockerfiles is correct and load-bearing: a narrower second
   invocation changes feature resolution for hyper/tower/tower-http and forces
   a full rebuild above them.
2. **Add `Dockerfile.operator-tools`** building exactly the eight dropped
   binaries, published on demand only. Reference it from
   `docs/render-worker-images.md`. If a tool is genuinely dead, delete it
   instead — decide per binary with the assignee.
3. **`autobins = false` + explicit `[[bin]]`** in
   `crates/loyal-yield-orchestrator/Cargo.toml` for all 17 binaries. This makes
   the binary set reviewable and stops a new `src/bin/*.rs` from silently
   entering every image build.
4. **Drop `macros` from sqlx** in `loyal-yield-realtime-core` and
   `loyal-yield-realtime`. Keep it in `loyal-yield-orchestrator` (store.rs
   needs it).
5. **Add `default-members`** to the root `Cargo.toml`:
   ```toml
   default-members = [
     "crates/balance-sweep-ata-monitor",
     "crates/balance-sweep-ata-observations",
     "crates/balance-sweep-ata-projector",
     "crates/balance-sweep-autodeposit-trigger",
     "crates/kamino-reserve-monitor",
     "crates/loyal-actions",
     "crates/loyal-hub-abi",
     "crates/loyal-observability",
     "crates/loyal-squads-policy-monitor",
     "crates/loyal-timescale-migrations",
     "crates/loyal-yield-orchestrator",
     "crates/loyal-yield-realtime",
     "crates/loyal-yield-realtime-core",
     "crates/loyal-yield-router",
   ]
   ```
   Excluded on purpose: `loyal-hub-swap-program` and
   `mock-yield-protocols-program` (SBF targets, built via `cargo build-sbf`),
   `squads-test-harness` (has its own `bun run test:squads`),
   `autonomous-vaults`, `kamino-historic-data`, `loyal-hub-cli` (operator CLIs).
6. **Add dev profile tuning** to the root `Cargo.toml` — this is what attacks
   the 28 GB `target/debug`:
   ```toml
   [profile.dev]
   debug = "line-tables-only"

   [profile.dev.package."*"]
   debug = false
   opt-level = 1
   ```
   Do **not** add LTO or lower `codegen-units` on release; both trade image
   build time for runtime speed we have not shown we need.
7. **Split the workflow dispatch.** Add a `workflow_dispatch` input:
   ```yaml
   on:
     workflow_dispatch:
       inputs:
         images:
           type: choice
           options: [both, light-workers, laserstream-workers]
           default: both
   ```
   and gate each matrix entry with
   `if: inputs.images == 'both' || inputs.images == matrix.image`.
8. **Re-evaluate the buildx cache backend.** `type=gha,mode=max` on two scopes
   competes for one 10 GB repo budget while the release target tree is ~2.8 GB
   per image. Measure whether entries are being evicted between runs; if so,
   move to `type=registry,ref=ghcr.io/.../<image>:buildcache,mode=max`, which
   has no 10 GB cap.

### Phase 2 — Extract `loyal-yield-store`

1. `git mv` `store.rs`, `types.rs`, `domain.rs` into
   `crates/loyal-yield-store/src/` as `store.rs`, `types.rs`, `domain.rs` with a
   `lib.rs` re-exporting the same public surface (including `pub use sqlx;`).
   Manifest deps: `chrono`, `serde`, `serde_json`, `sha2`, `sqlx` with
   `chrono, json, macros, postgres, runtime-tokio-rustls`.
2. `loyal-yield-orchestrator` depends on it and keeps
   `pub use loyal_yield_store::*;` in `lib.rs`, so no downstream call site
   changes in this commit.
3. Move the `.sqlx` offline data consumer with it — verify `cargo sqlx prepare`
   still emits into the workspace-root `.sqlx/` (it does for a workspace; use
   `cargo sqlx prepare --workspace` if not).
4. Repoint the four small crates to `loyal-yield-store` and **remove** their
   `loyal-yield-orchestrator` dependency:
   `balance-sweep-ata-observations`, `balance-sweep-ata-projector`,
   `balance-sweep-ata-monitor`, `loyal-squads-policy-monitor`.

### Phase 3 — Extract `loyal-solana-env` and `loyal-kamino-codec`

1. `loyal-solana-env` ← `signer.rs` + `rpc_safety.rs`. Deps: `solana-sdk`,
   `thiserror`. Consumers: `autonomous-vaults`, `loyal-yield-orchestrator`.
2. `loyal-kamino-codec` ← the pure decode half of
   `shared_market_catalog.rs` (`decode_kamino_reserve_account`,
   `KaminoReserveCatalogAccount`) plus `kamino-reserve-monitor`'s
   `targets.rs` record types (`SupportedReserveRecord`, `ReserveTarget`) and
   `apy.rs`. Deps: `klend-interface`, `borsh`, `solana-sdk`, `loyal-actions`,
   `serde`, `sha2`, `thiserror`. **No `solana-client`, no `helius-laserstream`,
   no `reqwest`.** Keep the RPC-fetching wrappers in the orchestrator /
   monitor.
3. Repoint `kamino-historic-data` from `kamino-reserve-monitor` to
   `loyal-kamino-codec` and delete the `kamino-reserve-monitor` dependency.
   This is the change that removes the entire Solana 3.1 / agave / laserstream
   / prost-0.14 subgraph from that crate.
4. Repoint `autonomous-vaults` to `loyal-kamino-codec` + `loyal-solana-env` +
   `loyal-yield-store`; drop `loyal-yield-orchestrator`.

### Phase 4 — Split the `same-mint-reserve-swap` monolith

22,096 LOC in a single binary crate is one codegen unit and the critical path of
three production services. It is also 9.3k lines of tests that
`cargo check --all-targets` recompiles on any touch.

1. Move the fleet worker logic (revalidate / execute / reconcile), which is the
   part `render.yaml` drives via `--fleet-worker` and `--fleet-reconciler`,
   into a `loyal-fleet-worker` library crate alongside its tests.
2. Leave `src/bin/same-mint-reserve-swap.rs` as a thin arg-parse + dispatch
   shell.
3. Same treatment for `lookup_tables.rs` (17,696 LOC, tests from line 4,202) →
   `loyal-route-lookup-tables` crate, consumed by the orchestrator and the
   provisioner binary.

Do this last: it is the highest-churn, highest-conflict phase, and Phases 1–3
already deliver most of the image-build win.

### Phase 5 — Version alignment and blocker record

1. Align what is trivially alignable inside our own manifests: promote
   `solana-client`, `solana-account-decoder`, `solana-pubsub-client`,
   `reqwest`, `sqlx`, `chrono`, `clap`, `tokio`, `serde`, `tracing`,
   `anyhow`, `thiserror`, `sha2`, `base64`, `futures-util` into
   `[workspace.dependencies]` and make every crate use `.workspace = true`.
   This does not remove duplicates by itself but stops new drift and makes the
   next audit one file.
2. Drop `solana-account-decoder = "2.3"` from `kamino-reserve-monitor` if the
   code actually consumes the 3.1 decoder that laserstream provides — check
   which one the `use` sites bind to.
3. **Record the blocker.** `helius-laserstream 0.1.10` forces the Solana 3.1 /
   agave 3.1.14 generation. We pin 2.3 workspace-wide. Options:
   (a) accept it and keep it isolated to the two laserstream crates, both of
   which live in the laserstream image — this is the recommendation;
   (b) migrate the entire workspace to Solana 3.x, which is a separate epic and
   touches every Squads/Kamino/Hub call site. Write this into
   `docs/rust-crate-boundaries.md` with the `cargo tree -i` evidence so nobody
   re-derives it.
   `anchor-lang` 0.31/0.32 duplication comes in via `klend-interface` and the
   Meteora `commons` SDK (`autonomous-vaults`) — not fixable from our side;
   note it and confirm `autonomous-vaults` stays out of `default-members` and
   out of every image.

### Phase 6 — Documentation

Add `docs/rust-crate-boundaries.md` covering:

- The crate graph above and the rule that produced it: **depend on a library
  crate, never on a package that also ships binaries or a stream client.**
- The rule that new shared types go in `loyal-yield-store` / `loyal-actions` /
  `loyal-kamino-codec`, not in `loyal-yield-orchestrator`.
- Recommended targeted commands:

  | Task | Command |
  | --- | --- |
  | Fast loop on one crate | `cargo check -p <crate>` |
  | Loop including its tests | `cargo check -p <crate> --all-targets` |
  | Everyday whole-tree check | `cargo check` (now honors `default-members`) |
  | One binary | `cargo check -p loyal-yield-orchestrator --bin <name>` |
  | Proof surface | `bun run test:squads` |
  | ABI/spec drift | `bun run verify:hub-abi-spec-drift` |
  | Never in the inner loop | `cargo check --workspace --all-targets` |

- The `worker-images` dispatch input and when to pick each image.
- The laserstream/Solana-3.1 blocker.

Also update `CLAUDE.md` / `AGENTS.override.md` with a pointer to that doc.

---

## 4. Verification

### Per-phase correctness gates

Run after every phase; all must stay green.

```sh
cargo check --workspace --all-targets          # nothing lost in the moves
cargo clippy --workspace --all-targets -- -D warnings
bun run verify:hub-abi-spec-drift
bun run verify:qedgen
bun run test:squads
bun run lint
```

Phase 4 additionally requires `bun run test:squads:e2e`, since it touches route
policy composition paths (per the Rust Test Policy in `AGENTS.override.md`).

Per the same policy, **add no new Rust tests** for this refactor. It is a pure
code-motion and build-configuration change; the existing suites moving with
their code is the regression guard. The one exception worth considering is a
`loyal-hub-abi`-style assertion only if a schema/spec file moves, which it
should not.

### Behavior-preservation checks specific to code motion

- After Phase 2, confirm the public surface is byte-identical:
  ```sh
  cargo public-api -p loyal-yield-orchestrator --diff-git-checkouts main HEAD
  ```
  (or, without that tool, diff `cargo doc --no-deps` JSON output). The expected
  diff is **empty** — the orchestrator re-exports everything it moved.
- After Phase 2/3, confirm `.sqlx` is unchanged:
  ```sh
  cargo sqlx prepare --workspace --check
  git diff --exit-code .sqlx
  ```
- Confirm every `dockerCommand` / `preDeployCommand` path in `render.yaml`
  exists in the image it is pinned to:
  ```sh
  docker run --rm --entrypoint sh ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<sha> \
    -c 'for b in <list>; do test -x /usr/local/bin/$b || echo MISSING $b; done'
  ```
  Do this for both images before any deploy is requested. This is the one check
  that catches a bad binary prune, and it must pass before shipping Phase 1.

### Acceptance-criteria evidence

| Criterion | How it is proven |
| --- | --- |
| Dependency-graph before/after | `docs/build-timings/{before,after}/duplicates.txt` and `dep-counts.txt` diff. Expect `kamino-historic-data` and `balance-sweep-ata-observations` to drop by hundreds of packages. |
| Warm non-Rust rebuild recompiles no third-party deps | Touch `docs/`, rebuild image with `--progress=plain`, assert cook and build layers report `CACHED`. |
| Local-crate change is mostly local | `touch crates/loyal-yield-store/src/store.rs && cargo build --release --timings`; assert in the HTML/JSON that the changed crate + dependents dominate and no third-party crate rebuilds. |
| Light image compiles only live binaries | Diff the `--bin` list in `Dockerfile.light-workers` against the `render.yaml` audit table above; the operator set lives in `Dockerfile.operator-tools`. |
| One image without the other | Dispatch `worker-images` with `images: light-workers`; assert the laserstream job is skipped. |
| Timings recorded | `docs/build-timings/after/` for all four scenarios plus per-image compile/export/cache-export phases pulled from `--progress=plain`. |
| Proof-surface checks pass | `bun run test:squads`, `bun run verify:qedgen`, `bun run verify:hub-abi-spec-drift`. |
| Boundary rules documented | `docs/rust-crate-boundaries.md`. |

### Expected direction of the numbers

Stated as expectations to falsify, not promises:

- Light image compile: ~28k LOC of orchestrator binaries removed, so the
  orchestrator's own codegen should drop meaningfully. Third-party cook time is
  unchanged in Phase 1 — the dependency set is the same.
- `kamino-historic-data` cold build: the largest single graph win, since the
  laserstream/Solana-3.1/agave/prost-0.14 subgraph leaves entirely.
- `balance-sweep-ata-observations` / `-projector`: should stop compiling
  solana-client, solana-rpc-client, reqwest, klend-interface, and OTLP.
- `target/debug`: `debug = "line-tables-only"` plus `debug = false` on
  dependencies should cut the 28 GB substantially. Measure before claiming it.
- Total lock package count should fall from 980; the 161 duplicate names will
  fall only partially, since laserstream keeps the 3.1 generation alive for the
  two monitor crates.

## 5. Deploy discipline

Nothing here ships without an explicit order. Phase 1 changes both Dockerfiles
and the workflow, so the first order should be a `light-workers` build with the
binary-presence check above run against the resulting tag before any Render
service is repinned.
