# Disposable Backyard route-lease contract

Run from the assigned deployed+identity-patch worktree:

```sh
python3 scripts/hetzner-backyard-lease-fixture/run.py \
  --ack-isolated --pg-bindir /opt/homebrew/opt/postgresql@17/bin
```

The harness creates a fresh PostgreSQL cluster with only a private Unix socket,
creates `backyard_route_lease_fixture`, applies `lease.sql`, runs the existing
`TestRouteLeaseAgainstDatabase` and new
`TestRouteLeaseIsolatedContentionAndStaleFencing` through actual `Database` Store
methods with `go test -race`, then stops/removes the fixture. No production URL
is accepted. Inherited DB, OP, signer/provider and telemetry variables are omitted;
Go module downloads and automatic toolchain downloads are disabled. Local
PostgreSQL shared memory may require an authorized sandbox escalation.

Source revision: `f821a78f6a5c0507eb1dada10fd300df1277f639` plus the parent's current
identity patch. No shared Store algorithms or accounting files are changed.

## Exact tested schema surface

`lease.sql` preserves the lease-relevant column types/defaults and route primary
key, unique vault binding/FK, positive state version, nonnegative fencing token,
JSON-object and owner/expiry coherence constraints from deployed migrations 0051
and 0053. Its `managed_vaults` table is only an FK anchor. It deliberately omits
unqueried operation/wire columns and later financial state-shape constraints.
This is a scoped Store lease fixture, not the entire deployed financial schema.
It does not apply or alter migration 71.

The contracts exercise legacy Render and valid provider-neutral owners in both
handoff directions, plus identical-owner takeover to isolate fencing-token checks.
They prove same-owner non-reentrancy, exclusion of an active contender, active
refresh without fence change, forced DB expiry, expired refresh rejection,
successor fence increment, stale predecessor release/refresh/assert rejection,
successor-row preservation and exact current-owner release. A barrier starts two
distinct contenders concurrently: exactly one wins and one is excluded. Invalid
owner/expiry pairs and negative token/zero version are rejected by PostgreSQL.
Expiry is forced only inside the disposable DB, avoiding timing-dependent sleeps.

stdout is one JSON supporting-evidence object with the shared collector fields.
PASS=0, FAIL=1, BLOCKED=2. Missing local prerequisites are BLOCKED; failed Go
contracts are FAIL. Parent remains authoritative for integration and financial
handoff. No worker startup, signing, transaction broadcast, unresolved-operation
recovery, reservation/custody preservation, receipt reconciliation or broader
financial recovery is proven by this lease-only result.

Validation on 2026-09-29: actual PostgreSQL 17.11 and Go 1.26.6 passed both
top-level tests and all six new subtests under the race detector. Check constraints
were confirmed by PostgreSQL SQLSTATE 23514, rather than accepting any SQL error.
Python compilation and `git diff --check` also passed. This is local isolated
evidence only; parent owns financial integration and production handoff gates.
