# ASK-1973 build comparison

Both captures use Rust/Cargo 1.89.0 and build the production
`same-mint-reserve-swap` binary in release mode. The binary moved from the
`loyal-yield-orchestrator` package to `loyal-fleet-worker`; its runtime name and
Render command are unchanged.

| Scenario | Before | After | Change |
| --- | ---: | ---: | ---: |
| Cold release build | 115 s | 105 s | -10 s (-8.7%) |
| Warm no-op release build | 1 s | 0 s | below one-second capture resolution |
| Warm shared-store change | 26 s | 23 s | -3 s (-11.5%) |

Docker image timing is unavailable in both captures because this machine does
not have the `docker` command. The Dockerfile cook/build selections were
validated with the equivalent cargo selection instead.

## Selected normal dependency counts

The count is the unique-line result captured by `scripts/build-timings.sh`
using `cargo tree -p <package> -e normal --prefix none --no-dedupe`.

| Package | Before | After | Change |
| --- | ---: | ---: | ---: |
| `balance-sweep-ata-observations` | 529 | 134 | -395 (-74.7%) |
| `kamino-historic-data` | 705 | 527 | -178 (-25.2%) |
| `loyal-yield-orchestrator` | 528 | 532 | +4 (+0.8%) |

The orchestrator's transitive package count is intentionally flat because it
re-exports the extracted store and lookup-table surfaces for compatibility.
Its compile unit is substantially smaller: fleet worker implementation/tests,
lookup-table implementation/tests, migrations, signer/RPC helpers, and pure
Kamino decoding no longer live in that crate.

The raw timing JSONL, logs, metadata, duplicate trees, and all package counts
are in [`before`](before/) and [`after`](after/).

## Verification limitations

- `cargo fmt --all -- --check`, `cargo check --workspace --all-targets
  --locked`, `cargo clippy --workspace --all-targets --locked -- -D warnings`,
  `cargo test --workspace --locked`, the extracted-crate narrow builds,
  `bun run test:squads`, the historical `bun run test:squads:e2e` replay, the
  ABI/spec drift gate, SQLx offline metadata check, and lint pass.
- The configured QEDGen executable is not installed at
  `/Users/zotho/.agents/skills/qedgen/tools/qedgen`. A local source build of the
  repository-documented QEDGen 2.30.0 reaches 100% operation coverage but exits
  on three pre-existing P1 spec warnings: `set_lane_count` invariant
  preservation and the unbound `initialize_config` / `set_admin` auth names.
- `cargo public-api` confirms that the facade exports remain present, but its
  diff is not empty because rustdoc records moved definitions under their new
  owning crates. It therefore reports canonical ownership-path changes even
  though the orchestrator continues to re-export the source-level paths.
- Authoritative image verification remains unavailable on this host. Docker is
  absent; the only previously bootable Podman VM has a full root filesystem,
  and both fresh VMs fail their CoreOS Ignition user-creation step. The PR
  workflows now perform non-pushing linux/amd64 builds and runtime probes for
  all three images, so those checks must pass before merge.
