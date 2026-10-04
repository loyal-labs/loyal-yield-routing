# Measured simplification results

This is the first bounded simplification pass after the original workers v2
implementation. It stays on `codex/workers-v2-simplify`, based on routing
`4c57a712cfd1b067de05f9a7a51b55bc6e206c0b`. Three implementation lanes used
native `gpt-6.1-sol` at low reasoning in isolated worktrees. Root integrated and
reviewed their diffs, requested corrections, and owns connected verification.
A fourth lane reviewed the changes and made bounded test-isolation edits.

## Deleted mechanisms

| Area | Removed | Retained behavior |
| --- | --- | --- |
| Autodeposit | Twelve numeric exit constants, outcome-to-exit-to-outcome translation, nullable integer adapters and numeric tally mapper | Direct typed family result; separate pull/deposit transactions; durable custody, recovery decisions, classified operator remedies and error-based readiness holds |
| Squads policy | Fleet's three duplicate constraint representations and three parser functions; Multiply's duplicate header traversal and decoding mechanics | One concrete bounded decoder; Fleet's prefix acceptance, 128-element/256-byte limits and legacy-first fallback; Multiply's full payload, authority, timing, hook/spending, tight-table and ambiguity checks |
| Multiply readiness | Repeated current-operation/claimability expressions and duplicate exact Claim coverage/exposure predicate | One coherent SQL statement, all latest-slot rows, route scope, manual/orphan/pending holds, exact live-operation exception, fresh snapshot rules and narrow completed Claim exception |

Multiply keeps type aliases for existing family consumers; these reference the
shared types and do not create another representation. Its semantic policy
matcher stays in the family. Fleet's strict swap tail parser also stays in the
family. Shared byte decoding grants no signing or execution authority.

Unknown Autodeposit results continue to hold readiness. They now produce a
generic operator alert, including the zero value, rather than retaining the old
unclassified numeric process-success silence. Known outcomes accompanied by an
error keep their outcome tally and separately hold readiness.

## Measurement

`baseline.json` and `after.json` count physical non-test Go lines under
`go/workers/internal`, including comments and SQL: **57,301 → 57,149**, a net
deletion of **152 lines**, with 186 → 187 source files. New rejection tests and
verification tooling are additional code. This modest reduction does not prove
the broader lean-codebase objective complete. The useful result is removal of
actual duplicate representations and translation paths. No SQL performance
improvement is claimed: PostgreSQL can inline the factored expressions.

## Corrections exposed by fresh verification

Actual registered-schema Autodeposit testing exposed two existing query defects:
destination setup ownership referenced a nonexistent target column instead of
joining its exact claim token; unknown cluster eligibility yielded SQL NULL
when scanned into a bool. Setup ownership now uses the claim relation, and
unknown eligibility becomes false. Existing custody and namespace holds remain.

Fixtures also needed actual active-policy binding, claim-token setup lookup,
canonical SVM floor-test identity, and an explicit control reconciliation request.
The positive namespace readiness fixture now requires control work to occur and
runs the actual projector before asserting readiness. It does not directly set
bootstrap proof or relax runtime census rules.

Connected same/cross-mint tests use two new explicit disposable database names.
The producer helpers and retained Rust test consumer accept those exact names;
existing loopback/role/RPC guards remain. A new PostgreSQL instance could not
initialize because of host shared-memory resource exhaustion. Root instead used
separate databases on the task-owned local service, preserving previous terminal
evidence databases. Failed admissions were reset only in the two new databases.

## Proof boundaries

The combined local verifier passes formatting, vet, uncached race tests and all
three Go command builds. With fixture variables absent, opt-in database/bank
tests are skipped; their separate enabled results must be recorded below.

Autodeposit's full registered-schema suite with the local bank passes in 10.815s.
Multiply's full registered-schema suite with the local bank passes in 27.210s;
the corrected readiness negatives also pass separately under the race detector.
Current Go same-mint connected execution passes in 3.471s; current Go retail
cross-mint connected execution passes in 18.442s. These use actual locally
created policy accounts through the shared decoder and retain the explicit
local protocol-mock boundary. The Rust test consumer was rebuilt offline and
locked from this worktree before both final runs.
Policy tests cover synthetic legacy/compact boundaries, malformed/truncated
input, authority and timing negatives, width/endian matching and strict swap
tails. Synthetic examples complement independent bank-created policy evidence.

Financial bank tests execute actual Squads and SPL programs with explicit local
protocol fixtures. Fixed-price KLend/Jupiter mocks are not production protocol,
oracle, liquidity or consensus proof. No mainnet transaction, service deployment,
database cutover, service retirement or main merge is authorized by these results.

The branch workflow now checks this branch and forces `go test -race -count=1`.
CI uses actual registered Yield/Timescale schemas and probes the local Linux image
without publishing or deployment permissions. Exact revision results are recorded
after completion; earlier v2 CI success is not substituted for this pass.
