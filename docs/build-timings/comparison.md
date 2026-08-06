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

- `cargo check --workspace --all-targets --locked`, the extracted-crate tests,
  `bun run test:squads`, the historical `bun run test:squads:e2e` replay, the
  ABI/spec drift gate, and lint pass.
- The repository was not strict-Clippy-clean before this refactor. The required
  workspace command still stops on existing `needless_borrow`,
  `explicit_auto_deref`, `items_after_test_module`, `too_many_arguments`, and
  `large_enum_variant` findings in code moved without behavioral edits.
- `bun run verify:qedgen` reaches the ABI/spec drift pass, then cannot start
  because the configured local executable
  `/Users/zotho/.agents/skills/qedgen/tools/qedgen` is absent.
- Docker image timings cannot run because the local `docker` command is absent.
