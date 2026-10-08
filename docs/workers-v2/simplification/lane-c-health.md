# C — Multiply recovery health simplification

Read go/workers/AGENTS.md and docs/workers-v2/contracts.md. Edit only
multiply/worker.go, existing runtime_health tests and (if essential) new narrowly
named health-store helper/tests. Never edit multiply/policymatch.go; B owns it.
No schemas, state migration, cmd, dependency or unrelated controller changes.

Simplify runtimeRecoveryHealth's duplicated latest-snapshot and claim-coverage
logic. Keep a single coherent SQL statement and compute repeated predicates once.
Keep durable state authoritative; don't invent a new persisted readiness flag or
reuse a partial TickResult as proof that all routes are healthy. Preserve unknown
conditions, signed uncertainty, scope, manual/orphan holds, exact-operation live
signature exception, policy seed/readiness, freshness, claim coverage, zero
exposure and the narrow completed wallet-owned Claim freshness exception.
Preserve SQL NULL behavior and invalid cast/error fail-closed behavior. Avoid
ORDER BY LIMIT 1 that changes semantics for same-slot multiple rows.

Make the health decision readable without adding a parallel business state machine.
Moving unchanged SQL to another file is not an accepted simplification. Show the
specific repeated predicates/joins removed and why each guard still holds.
Inspect existing registered SQL health tests; preserve them and propose bounded
meaningful new negatives for any changed query boundary. No database access until
root grants a serialized disposable fixture window. Offline focused tests first.
Send an early query shape/deletion checkpoint; deliver scoped diff and evidence.
