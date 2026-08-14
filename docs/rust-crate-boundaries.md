# Rust crate boundaries

## Dependency rule

Depend on the smallest library crate that owns the contract. Never depend on a
package merely because it also happens to ship a useful type when that package
owns binaries or a stream client.

The current boundaries are:

```text
loyal-actions ────────────────> loyal-kamino-codec
klend-interface ─────────────> loyal-kamino-codec
                                      │
                                      ├─> loyal-kamino-data
                                      │      ├─> kamino-reserve-monitor
                                      │      └─> kamino-historic-data
                                      ├─> autonomous-vaults
                                      └─> loyal-yield-orchestrator

loyal-yield-store ───────────> loyal-route-lookup-tables
       │                              │
       ├─> small SQL workers          └─> loyal-yield-orchestrator
       ├─> autonomous-vaults                    │
       └─> loyal-yield-orchestrator             └─> loyal-fleet-worker

loyal-solana-env ────────────> autonomous-vaults
       ├─────────────────────> loyal-route-lookup-tables
       └─────────────────────> loyal-yield-orchestrator
```

Shared durable SQL types and state belong in `loyal-yield-store`. Route action,
policy-byte, account-planning, and protocol topology contracts belong in
`loyal-actions`. Pure Kamino account decoding and APY calculations belong in
`loyal-kamino-codec`. Do not add new shared contracts to
`loyal-yield-orchestrator`.

`loyal-yield-store` also keeps its `[dev-dependencies]` free of Solana, RPC, and
observability crates, so its tests stay as cheap to build as the crate itself.
Where a store invariant needs a mint address, keep the address in
`domain::SUPPORTED_IDLE_DEPOSIT_MINTS` as a base58 string rather than importing
the typed constant from `loyal-actions`. `stable_mints` in
`loyal-yield-orchestrator` stays the typed source of truth and owns the parity
test that pins the two lists together.

`loyal-kamino-data` is the non-stream integration layer for the Kamino HTTP
catalog and Timescale sink. It exists so `kamino-historic-data` can reuse those
integrations without depending on the LaserStream monitor package.

`loyal-route-lookup-tables` owns reusable ALT planning, persistence, and the
compatibility `NeonSqlClient` wrapper. `loyal-yield-orchestrator` re-exports its
public surface so existing callers remain source-compatible.

`loyal-fleet-worker` owns the large same-mint executor/revalidator/reconciler
implementation and its tests. Its `same-mint-reserve-swap` binary is a thin
shell over the library entrypoint. The binary moved out of the orchestrator
package because leaving a shell there would create a Cargo package cycle:
the worker needs the orchestrator library, so the orchestrator cannot depend
back on the worker library.

## Build loops

Use the narrowest command that exercises the code being changed:

| Task | Command |
| --- | --- |
| Fast loop on one crate | `cargo check -p <crate>` |
| One crate including tests and binaries | `cargo check -p <crate> --all-targets` |
| Everyday whole-tree check | `cargo check` |
| One orchestrator binary | `cargo check -p loyal-yield-orchestrator --bin <name>` |
| Fleet worker | `cargo check -p loyal-fleet-worker --bin same-mint-reserve-swap` |
| Proof surface | `bun run test:squads` |
| ABI/spec drift | `bun run verify:hub-abi-spec-drift` |
| Full release gate, not an inner loop | `cargo check --workspace --all-targets` |

The root `default-members` list makes plain `cargo check` an intentional daily
workspace check. Operator-only packages such as `autonomous-vaults`,
`kamino-historic-data`, and `loyal-hub-cli` are excluded.

## Worker images

The `worker-images` workflow has an `images` dispatch input:

- `workers` packages both production worker images and is the default.
- `light-workers` builds only the SQL/realtime/fleet worker image.
- `laserstream-workers` builds only the two LaserStream monitor services.
- `operator-tools` builds only the operator image.
- `all` builds all three image families.

All selections first run one shared Cargo invocation in
`scripts/build-rust-image-binaries.sh`. Pull requests restore the trusted Cargo
cache, then package its binary artifact with compiler-free Dockerfiles. Main
pushes refresh that Cargo cache without packaging images; only manual dispatch
publishes images. Operator and verification binaries live in
`Dockerfile.operator-tools` and are not copied into either production worker
image.

## Upstream version blockers

The workspace pins its direct Solana dependencies to the 2.3 generation. The
two LaserStream consumers remain an intentional exception:

- `balance-sweep-ata-monitor`
- `kamino-reserve-monitor`

`helius-laserstream 0.1.10` brings `laserstream-core-* 9.0.2`, which in turn
brings the Solana/Agave 3.1.14 generation. The current evidence commands are:

```sh
cargo tree -i helius-laserstream@0.1.10
cargo tree -i solana-pubkey@3.0.0
```

Keep that dependency generation isolated to the two LaserStream crates and the
`laserstream-workers` image. Migrating the whole workspace to Solana 3.x is a
separate project because it changes every Squads, Kamino, and Loyal Hub call
site.

`kamino-reserve-monitor` also keeps its direct workspace-pinned
`solana-account-decoder` dependency. Its WebSocket source directly uses the
2.3 `UiAccount`, `UiAccountData`, and `UiAccountEncoding` types alongside the
2.3 `solana-client` and `solana-pubsub-client`; the 3.1 decoder hidden inside
LaserStream is not a compatible replacement for that boundary.

The dependency tree also contains two upstream-pinned Anchor generations.
Meteora `commons` brings `anchor-lang`/`anchor-spl` 0.31.1, while Squads v4
brings 0.32.1. They cannot be aligned from this repository. Keep
`autonomous-vaults` excluded from `default-members` and all worker images
because it is an operator CLI with the Meteora SDK graph. Recheck with:

```sh
cargo tree --workspace --duplicates | rg -C 3 'anchor-(lang|spl) v0\.(31|32)'
```
