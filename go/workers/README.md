# Loyal workers v2

Isolated rewrite, kept off main and production until the infrastructure migration
is accepted and later merge/activation is authorized. See
`../../docs/workers-v2/contracts.md` for behavior and acceptance.

One module composes observer, retail engine and separately credentialed Backyard
engine. The initial source-preserving imports retain existing schemas and tests.
The observer may retain the reviewed Rust Earn bridge until Go application proof
passes; Rust ABI/SVM proof and the official KLend helper remain authoritative.

Run `make verify` for offline checks. Disposable-database verification is separate
and explicitly targets local fixtures. No default test needs production secrets.
Missing runtime wiring is an error, never a healthy inert worker. Saved evidence
replay must call the same decision functions as runtime execution.
