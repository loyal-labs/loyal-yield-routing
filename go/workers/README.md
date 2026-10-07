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

Secrets are read only from systemd credential files
(`$CREDENTIALS_DIRECTORY/<NAME>`, see `deploy/hetzner/`); everything else is
environment. The release identity is stamped at link time
(`-X .../internal/engine.Release=sha-<commit>`). Every process serves
`/metrics` on `LOYAL_METRICS_ADDRESS`.

The engine selects `LOYAL_WORKER_SCOPE=retail` or `backyard`; each instance
requires `LOYAL_WORKER_INSTANCE`. Retail requires
explicit `RETAIL_MODE=active`, `RETAIL_TIMESCALE_SCHEMA`, `RETAIL_SLOT_DURATION`,
and the credentials
`RETAIL_DATABASE_URL`, `RETAIL_TIMESCALE_DATABASE_URL`, `RETAIL_SOLANA_RPC_URL`,
`RETAIL_JUPITER_API_KEY`, `RETAIL_DELEGATE_KEYPAIR` and `RETAIL_FEE_PAYER_KEYPAIR`. The current Autodeposit
and same-mint packet contracts require the latter two keys to be identical.
`RETAIL_CROSS_MINT_ENABLED=true` opts the planner/controller into fresh cross-mint
work; it defaults off. Existing signed recovery and custody continuation stay
available with rollout off. Source database controls are checked independently.
Jupiter uses scoped `RETAIL_JUPITER_BUILD_URL` and the `RETAIL_JUPITER_API_KEY` credential;
optional `RETAIL_CROSS_MINT_MAX_SLIPPAGE_BPS` and
`RETAIL_CROSS_MINT_MAX_VALUE_LOSS_BPS` each default to 50 and accept 1..1000.
An inherited legacy cross-mint flag cannot grant fresh authority.
ALT packet recovery defaults to `RETAIL_LOOKUP_MODE=reconcile-only` and never
loads a manager key. Fresh ALT mutations require `RETAIL_LOOKUP_MODE=active`,
the distinct `RETAIL_LOOKUP_MANAGER_KEYPAIR` matching the source standard policy
authority, and an explicit positive `RETAIL_LOOKUP_MAX_LAMPORTS`. The optional
`RETAIL_LOOKUP_BUDGET_WINDOW` defaults to `24h` and accepts whole seconds between
one minute and 365 days. Source pause/family/table controls and durable budget
reservations still fence each operation. Route delegate and fee-payer keys do
not grant lookup authority. The unsigned planner and writer are joined retail
lanes with independent readiness from actual finalized RPC evidence. Catalog
reconciliation runs at a one-minute idle cadence; the one-second queue loop
handles provisioning, mature binding publication and expired rollback cleanup.
Packing retains the source defaults: eight reserved growth addresses per vault
and at most sixteen vaults per shard. Reconcile-only mode repairs observed
catalog state and publishes proved existing bindings; fresh provisioning and
cleanup packets require active mode and independently fenced writer admission.
Do not enable this rewrite against shared resources.

Observer retains its reviewed transport configuration in
`internal/observer/config/config.go` and starts the separately supervised,
unsigned Rust Earn bridge with an explicit environment allowlist. Each family
requires its own database boundary: observer watches use `NEON_DATABASE_URL`
for Yield and explicit `OBSERVER_APPS_DATABASE_URL` for Apps identities. The
Apps connection is read-only in the observer and is excluded from the bridge
environment. The fixed watch catalog verifies actual mainnet genesis before
opening writers and on every watch refresh.
Each family
owns SQL beside its lifecycle code. Shared packages provide concrete pool,
lease, amount and process-lifetime behavior; there is no workflow framework.

Disposable SQL verification uses `scripts/workers-v2-fixture.py`, with
`WORKERS_V2_DISPOSABLE=1` and a password-free loopback `workers_v2` bootstrap
database. It resolves the actual migration registry and pinned Apps baseline;
the production-specific 0071 data activation and ledger entry are excluded.
Branch CI provides actual PostgreSQL 17 and Timescale. No migration executable
is packaged in the worker image, and tests never apply schema to production.

Health is the four family facts on `/metrics` (`internal/engine/facts.go`);
alert rules live in `deploy/hetzner/monitoring/`. There is no readiness
endpoint. Recovery preserves possibly sent bytes, even after user
disablement; do not clear uncertainty by deleting rows.

The Docker image includes the official locked Rust KLend builder and retained
Earn bridge alongside the three Go binaries, runs as UID 65532, and records
artifact checksums. Branch CI builds and probes it without publishing. Main
merge, migration acceptance and family writer activation remain separate gates.
