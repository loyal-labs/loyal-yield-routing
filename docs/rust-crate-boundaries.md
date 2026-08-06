# Rust crate boundaries

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

The earlier build plan anticipated two Anchor generations through
`klend-interface` and the Meteora `commons` SDK. The refreshed dependency tree
contains one `anchor-lang` generation, 0.31.1, so there is no current Anchor
duplicate to fix. `autonomous-vaults` remains excluded from `default-members`
and all worker images because it is an operator CLI with the Meteora SDK graph.
Recheck with:

```sh
cargo tree -p autonomous-vaults | rg 'anchor-(lang|client|spl)'
```
