# Loyal Yield Router

`loyal-yield-router` is the read-only TimescaleDB boundary for Loyal yield-routing inputs.

It connects to the existing Kamino Timescale schema, reads `kamino.reserve_updates` and `kamino.latest_reserve_updates`, exposes typed reserve rows, and streams updates from `LISTEN kamino_reserve_updates`. Durable catch-up reads use the `(observed_at, slot, reserve)` cursor.

This crate should stay narrow. Do not put quant policy here. Keep eligibility rules, scoring code, decision records, execution behavior, and offset persistence in separate strategy or router crates that consume `timescale::ReserveUpdateRow`.

## Checks

```sh
cargo test -p loyal-yield-router
cargo check -p loyal-yield-router
```

Ignored live tests require `TIMESCALEDB_TEST_URL` pointing at the Kamino TimescaleDB.
