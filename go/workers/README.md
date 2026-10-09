# Loyal workers

See `../../docs/workers/facts.md` for the source of truth and the one writer of
each fact.

One module composes observer, retail engine and separately credentialed Backyard
engine. The initial source-preserving imports retain existing schemas and tests.
KLend instruction builders and the Earn domain application are Go; their byte
and row parity with the retired Rust workers is pinned by recorded goldens
(`testdata/klend`, `testdata/earn`). The Squads SVM proof in
`crates/squads-test-harness` remains authoritative for policy execution.

Run `make verify` for formatting, vet, race tests and the three binary builds.
Use `GOTOOLCHAIN=local`; the language baseline is Go 1.25.1 and branch CI uses
1.26.6. Default tests need no production secrets. Database and SBF tests skip
unless their explicitly disposable fixture inputs are supplied; an offline PASS
does not prove those gates.

`loyal-evidence -kind fleet-wave -snapshot saved.json` replays the runtime
planner with no database, signing or RPC.
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
`RETAIL_FAMILIES` names the families this process writes (`autodeposit`,
`fleet`, `multiply`, `lookup`). The
process holds each family's session advisory lock on `RETAIL_DATABASE_URL`,
which must therefore be the direct Neon DSN, and exits when a lock is lost.
Every family lands signed rows through one function, `solana.Land`: the
bytes are written to the operation row before the first send and resent until
they land or the finalized height passes their blockhash.
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

Backyard (`LOYAL_WORKER_SCOPE=backyard`) is the production Voltr/Kamino RWA
line ported from `origin/feat/voltr-rwa-selector`. It requires the credentials
`BACKYARD_DATABASE_URL` (the direct, non-pooler DSN: it also holds the family
lock), `BACKYARD_SOLANA_RPC_URL` and `BACKYARD_POLICY_KEYPAIR`; an optional
`JUPITER_API_KEY` credential selects the keyed Jupiter API. The selector
collector is off by default; `BACKYARD_RWA_SELECTOR_LIVE=1` enables it and then requires the
`BACKYARD_TIMESCALE_DATABASE_URL` credential. `BACKYARD_RWA_PILOT_CANARY_ENTRY`
keeps its existing meaning. One-shot operator commands run as
`loyal-engine backyard <command>` (for example `clear-hold --reason "<text>"`)
with the same credentials.

Backyard rejects unsigned reserve-refresh captures beyond the observation
freshness window with a retryable observation-unavailable error. Repeated late
captures do not increment the health-failure streaks or latch a manual stop;
only a fresh, valid capture can be used. Integrity checks run before lateness:
incomplete captures, slot regression, namespace/provenance drift, duplicates
and simulated fee-payer captures still fail closed, even when also late.
Kamino-rejected refreshes still latch after three consecutive failures. The
13-second observation window (32–64 slots), Kamino freshness limits, debt and
capital guards, and recovery behavior are unchanged.

Observer retains its reviewed transport configuration in
`internal/observer/config/config.go` and runs the Earn domain application
(`internal/observer/earn`) in process: policy projection, the durable Earn
reconciliation queue and the hourly Earn APY snapshots. Observer watches come
from the Yield database (`NEON_DATABASE_URL`) alone. Product read models stay with the Apps crons until
`OBSERVER_READ_MODELS_ENABLED=true` hands them over. The fixed watch catalog
verifies actual mainnet genesis before opening writers and on every watch
refresh.
Each family
owns SQL beside its lifecycle code. Shared packages provide concrete pool,
lease, amount and process-lifetime behavior; there is no workflow framework.

Disposable SQL verification uses `scripts/workers-v2-fixture.py`, with
`WORKERS_V2_DISPOSABLE=1` and a password-free loopback `workers_v2` bootstrap
database. It applies `migrations/yield` and `migrations/timescale` over the
pinned Apps baseline; the production-specific 0071 data activation and ledger
entry are excluded. Branch CI provides actual PostgreSQL 17 and Timescale.
Tests never apply schema to production.

`cmd/loyal-migrate` is the production runner (`-db yield|timescale status|up`).
It keeps the ledger the retired Rust runners wrote: version, name and the
SHA-256 of the file bytes. `status` is read-only and fails on drift.

Health is the four family facts on `/metrics` (`internal/engine/facts.go`);
alert rules live in `deploy/hetzner/monitoring/`. There is no readiness
endpoint. Recovery preserves possibly sent bytes, even after user
disablement; do not clear uncertainty by deleting rows.

The Docker image contains only the three Go binaries, runs as UID 65532, and
records
artifact checksums. Branch CI builds and probes it without publishing.
