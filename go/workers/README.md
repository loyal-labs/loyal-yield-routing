# Loyal workers v2

Isolated rewrite, kept off main and production until the infrastructure migration
is accepted and later merge/activation is authorized. See
`../../docs/workers-v2/contracts.md` for behavior and acceptance.

One module composes observer, retail engine and separately credentialed Backyard
engine. The initial source-preserving imports retain existing schemas and tests.
The observer may retain the reviewed Rust Earn bridge until Go application proof
passes; Rust ABI/SVM proof and the official KLend helper remain authoritative.

Run `make verify` for formatting, vet, race tests and the three binary builds.
Use `GOTOOLCHAIN=local`; the language baseline is Go 1.25.1 and branch CI uses
1.26.6. Default tests need no production secrets. Database and SBF tests skip
unless their explicitly disposable fixture inputs are supplied; an offline PASS
does not prove those gates.

`loyal-evidence -kind fleet-wave -snapshot saved.json` replays the runtime
planner with an explicit `evaluatedAt` clock and no database, signing or RPC.
It also supports single `fleet` and `backyard` decisions. This is a modeled
decision, not a realized return. Fixture provenance and proof limits are listed
in `testdata/manifest.json`.

The engine selects `LOYAL_WORKER_SCOPE=retail` or `backyard`; each instance
requires `LOYAL_WORKER_INSTANCE` and immutable `LOYAL_IMAGE_VERSION`. Retail requires
explicit `RETAIL_MODE=active` and scoped `RETAIL_DATABASE_URL`,
`RETAIL_TIMESCALE_DATABASE_URL`, `RETAIL_TIMESCALE_SCHEMA`,
`RETAIL_SOLANA_RPC_URL`, `RETAIL_HTTP_ADDRESS`, `RETAIL_SLOT_DURATION`,
`RETAIL_KLEND_PROXY_PATH`, `RETAIL_KLEND_PROXY_SHA256`,
`RETAIL_DELEGATE_KEYPAIR` and `RETAIL_FEE_PAYER_KEYPAIR`. The current Autodeposit
and same-mint packet contracts require the latter two keys to be identical.
Cross-mint flags currently fail before network access while that runtime is
being integrated. Do not enable this rewrite against shared resources.

Observer retains its reviewed transport configuration in
`internal/observer/config/config.go` and starts the separately supervised,
unsigned Rust Earn bridge with an explicit environment allowlist. Each family
owns SQL beside its lifecycle code. Shared packages provide concrete pool,
lease, amount and process-lifetime behavior; there is no workflow framework.

Disposable SQL verification uses `scripts/workers-v2-fixture.py`, with
`WORKERS_V2_DISPOSABLE=1` and a password-free loopback `workers_v2` bootstrap
database. It resolves the actual migration registry and pinned Apps baseline;
the production-specific 0071 data activation and ledger entry are excluded.
Branch CI provides actual PostgreSQL 17 and Timescale. No migration executable
is packaged in the worker image, and tests never apply schema to production.

`/readyz` opens only after every configured family reports a healthy, fresh
cycle using real chain slots and its custody/recovery census. It closes when
any family stalls or exits. Investigate that family's classified error and
durable journal before admitting more work. Recovery preserves possibly sent
bytes, even after user disablement; do not clear uncertainty by deleting rows.

The Docker image includes the official locked Rust KLend builder and retained
Earn bridge alongside the three Go binaries, runs as UID 65532, and records
artifact checksums. Branch CI builds and probes it without publishing. Main
merge, migration acceptance and family writer activation remain separate gates.
