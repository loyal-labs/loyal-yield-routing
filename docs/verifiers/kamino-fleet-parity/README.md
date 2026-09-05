# Kamino planner and revalidator local verification

The Go service is intended to replace the Rust opportunity planner and route
revalidator, not the retained executor, confirmer, reconciler, health projector,
or ALT provisioner. Local verification is necessary but is **not deployment
approval**. Follow the parallel-shadow rollout in
`go/kamino-fleet-planner/README.md` before stopping either Rust service.

## Run all local gates

```sh
scripts/verify-kamino-planner-revalidator-parity.sh --audit-current
```

The audit clears inherited credentials, disables online dependency resolution,
and sets HTTP proxies to an unavailable loopback port (local fixture endpoints
are exempt). Dependencies must already be cached. It creates disposable
PostgreSQL databases, runs Go vet and all Go tests with the race detector,
requires every named integration test to pass, and rejects skipped fleet tests.
Missing dependencies or failed/skipped checks fail the audit rather than being
reported as successful verification. The audit also requires `cargo build-sbf`
and its cached toolchain to rebuild the mock protocol program used by LiteSVM.

The blank-database migration runner accepts only the existing documented 0071
production-bound Backyard activation failure; it validates the required fleet
schema. Production is never migrated by this verifier.

## Evidence boundaries

1. **Market epoch parity:** independently generated Go/Rust immutable epoch JSON.
2. **Go integration tests:** actual planner publication, bound cross-mint policy
   claims, ALT request/readmission, fused capacity/economics handoff, and database
   lease fences. Shadow executes under PostgreSQL `default_transaction_read_only`.
   Expiry tests run the real sweep with live/expired leases, row locks, cluster
   isolation, new-epoch recovery, and unresolved submission ownership. For the
   defensive orphan-submission cases only, test setup injects legacy/partial
   rows with transaction-local trigger bypass; sweep execution never bypasses
   triggers. These fixtures do not simulate a successful signed handoff.
3. **Go route negative tests:** execute validation/preparation functions with
   missing ALTs, oversized packets, simulation errors, and changed identities.
4. **Actual KLend proxy:** Go invokes the compiled, digest-verified Rust binary
   to build independent withdrawal/deposit legs for all six stable target mints.
   Both sides reject wrong-lane requests. This exercises the new cross-mint
   operation at the formerly same-mint-only boundary, not canned proxy output.
5. **Retained Rust lifecycle:** existing isolated-database verifier exercises
   durable transition methods. Eleven named cross-mint recovery checks are
   mandatory, including crash windows, source recovery, target fallback,
   ambiguous effects, admission, pause/revocation, and manual-closure fencing.
   All three custody/capacity, policy-catalog, and opt-in store tests also execute
   against disposable PostgreSQL; missing/skipped results fail the audit.
   Side-effect-free role probes load retained role boundaries. These are not
   live on-chain executor/confirmer/reconciler runs.
6. **Squads/LiteSVM:** rebuild the mock SBF, then run the existing generalized
   cross-mint policy test that creates policies, reads them back, executes swaps,
   and rejects adversarial mutations. Require its named non-skipped PASS.
7. **Schema-v2 deterministic artifact parity:** independent Rust planner,
   official KLend/Squads builders and Solana compiler versus Go planner/proxy
   preparation. Compare actual opportunity plans/keys, route fingerprint, and
   complete unsigned message/wire bytes. No database or RPC is used by these
   two artifact producers. Go preparation uses a simulation stub solely to
   obtain compiled bytes; no simulation result is emitted as evidence.

The old schema-v1 artifacts claimed negative outcomes and lifecycle success
using literals and an unrelated table of state names. Those claims and the
fake lifecycle table have been removed. The comparator rejects legacy schemas,
extra lifecycle/simulation assertions, missing outputs, and differing bytes.

## Individual commands

```sh
scripts/verify-kamino-fleet-planner-e2e.sh
scripts/verify-kamino-market-epoch-parity.sh
scripts/verify-kamino-route-parity.sh
scripts/verify-kamino-planner-revalidator-parity.sh --self-test
python3 scripts/verify-kamino-go-test-evidence.py --self-test
scripts/verify-kamino-planner-revalidator-parity.sh --compare rust.json go.json
```

Comparator mutation controls test the comparator, not runtime failure recovery.
The separate test-evidence self-test rejects skipped/missing/failed/incomplete
test runs. Neither synthetic control is a substitute for the integration suite.

These are separate execution-backed gates, not one connected Go `Cycle` through
Rust execution/confirmation/reconciliation. That connected local handoff remains
an evidence gap; do not relabel the component gates as full service E2E.
Live Jupiter/RPC behavior, cross-mint end-to-end execution, throughput,
production alert delivery, and safe cutover remain deployment gates. A local
PASS must not be described as proof that both Rust services can be stopped.
