# Earn golden generator

Produces the Rust side of the Go Earn parity tests:

- `policy-detection.golden.json`: Squads settings instructions built by
  `loyal-actions` and the store inputs `loyal-squads-policy-monitor` emits for
  them, plus the canonical Earn MAX create/update instruction bytes.
- `store-rows.golden.json`: the table rows `loyal-yield-store` writes when it
  applies `store-script.json` to an empty registered fixture schema.

```sh
cp ../../../../../Cargo.lock .
CARGO_TARGET_DIR=/tmp/earn-golden cargo run --offline -- detect > ../policy-detection.golden.json
CARGO_TARGET_DIR=/tmp/earn-golden cargo run --offline -- store ../store-script.json \
  postgresql://workers_v2@127.0.0.1:55437/<fresh observer fixture> > ../store-rows.golden.json
```
