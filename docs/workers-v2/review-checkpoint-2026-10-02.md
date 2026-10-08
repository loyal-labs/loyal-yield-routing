# Review checkpoint

## Current continuation: 2026-10-03

The runner recovered. The subsequent work below is historical; it does not
describe the current implementation tree. Reviewed Autodeposit setup/control,
same-mint execution, Multiply and read-model increments are integrated on the
inactive rewrite branch. The combined offline verifier passed. Actual registered
PostgreSQL family tests passed, including cancellation, competing owners,
immutable attempt accounting, fresh capacity/ALT fences, unsigned admission
recovery and signed ambiguity holds. New additive migrations 0087 and 0088 are
registered only in this branch and applied only to task-owned disposable tests.

The precise latest state and remaining gates are in [plan.md](plan.md).
The local service uses PostgreSQL 17 on loopback port 51913 and task-owned data;
no migration target, production database, credential or chain sender was used.
The extension-backed Timescale gate remains branch CI's responsibility.

Independent SDK fixtures and actual Squads/SPL SVM claim proof now pass for
Multiply. The connected fleet lifecycle helper is being rebuilt after the
canonical Autodeposit policy builder increment. These fixture proofs do not
establish full mature KLend SBF or production readiness. Retail composition is
present but genuine family readiness and full cross-mint continuation/provisioning
are under implementation. The planner oracle exposes a concrete 47.19% cumulative
marginal-gain regret case and a synthetic 514MB cumulative allocation probe;
neither is hidden behind a claim of optimality or production sizing.

GLM source-transfer approval remains pending. Native supervised implementation
continues; no provider restriction was bypassed. Main/prod remain untouched.

## Historical checkpoints

The implementation goal remains active. No main merge, deployment, migration
resource change or worker activation has occurred.

Routing branch `codex/workers-v2` last committed and pushed head is `5e1045f0`.
Its isolated PostgreSQL/Timescale branch verification passed in Actions run
37097141373. Apps branch `codex/workers-v2-app-contract` is committed and pushed
at `49573b7e`; focused financial, repository and mobile GET tests passed. The
whole-web typecheck retains the pre-existing baseline errors.

Current Autodeposit controller changes are uncommitted review work. Root has
corrected the lease-before-claim ordering, separated sweep and Earn policies,
replaced the manual Solana compiler/parser with the existing SDK, preserved
the full signature, reused the fleet policy matcher, checked the destination
and exact official top-up builder before pull, and switched valuation to the
existing KLend conversion including borrowing and fee liabilities. Claims now
require a live release lease and unspent custody, exclude a live executor from
stale release, and re-read control limits under the target lock. Exact receipt
balances feed execution accounting. First submission simulates the persisted
wire before recording broadcast intent; simulation transport failure holds it.

An earlier focused Autodeposit/Backyard/fleet race run passed. Subsequent edits
have not been verified. Controller database tests have not yet run against the
registered disposable fixture. Missing-obligation setup, bootstrap/artifact
recovery, full retail composition and old/new custody acceptance remain required.
The official Rust KLend proxy built successfully with locked offline Cargo
inputs at the task-owned rust-target cache; ABI/SVM acceptance remains separate.

Fleet execution and Multiply drafts remain in their own worktrees and have not
been integrated. Fleet expiry/release proof must follow the actual Rust family
protocol and cannot infer unchanged effects from signature absence. Multiply
review found blocking planner, scaled-fraction, topology, policy encoding,
transaction composition, quote validation, stored-hash and recovery errors.
Native Codex repair lanes were dispatched with disjoint scopes, but stopped
when the tool environment reported network permission revoked. Confirm their
actual diffs before resuming; do not copy drafts as completed implementations.

Runner verification was temporarily interrupted: both ordinary and escalated
exec_command fail before starting a shell with `Too many open files (os error
24)`. No unrelated process was stopped or machine/server configuration changed.
The execution service subsequently recovered; local checks resumed. Native correction lanes are working again. This is historical runner evidence, not a current blocker.

Automatic approval review rejected resuming a GLM lane because private
repository source/context would be sent to Z.ai. A specific source-transfer
approval question is pending. Do not resume that provider or bypass the review
without authorization. Existing earlier-approved GLM lanes have finished.

Remaining goal work includes the complete reviewed Autodeposit family, fleet
execution/ALT/recovery, Multiply, scheduled observer read models, runtime
composition/readiness, immutable Linux helper/bridge packaging, independent
Rust/SVM proof, planner oracle/performance acceptance and disposable old/new
contention/recovery checks. The scaffold and partial branch proofs do not make
the goal complete.

Latest local review work: native Multiply math/planner/worker and wire/policy/
store corrections passed package races and vet in the separate Multiply
worktree. They remain unintegrated; independent ABI/SVM, registered DB, pending
intent eligibility and legacy custody proof are still required. Fleet execution
now has concrete same-mint collateral and idle no-effect reads, but fresh
admission, ALT and cross-mint/reconciliation integration remain incomplete.
Its final small corrections have not been formatted/tested after os24 recurred.

The routing read-model lane saved `telemetry.go` and `maintenance.go` in
`workers-v2-readmodels`. Its test-file patch failed twice with automatic
approval-review deadline timeouts; no live patch remains. This is a timeout,
not a stated safety rejection. Root has not reviewed or integrated the draft.
Independent on-chain price probing, derived stage health, payload parity and
registered SQL acceptance remain required before retiring App maintenance.

On the next continuation, command startup recovered briefly. Root formatted
`topup_proxy_test.go` and ran the full Autodeposit package with races and the
locally built official Rust KLend proxy: PASS (1.464s). This includes the
actual-builder/exact-policy top-up preflight regression. Controller database
tests still lacked a disposable database URL and were skipped; no DB or SVM
PASS is implied. The current-tree full verifier has not run.

The fleet execution repair lane also verified its latest package race run,
with database tests skipped. Native repair lanes then terminated with
`application network permission was revoked`; no subsequent implementation
success is claimed. Command startup again fails before launching a shell with
os24. A Terminal UI verification attempt was separately denied by the tool's
safety restriction; no command ran and no alternate UI bypass was attempted.
No late drafts have been committed or pushed as verified.
