# A — typed Autodeposit execution outcomes

Read go/workers/AGENTS.md and docs/workers-v2/contracts.md. Edit only
internal/autodeposit/*.go and associated tests in your assigned worktree.
Root owns cmd and all other packages; send any required caller edit request.

Replace TargetExecutor.Execute returning (*int,error) with a small typed family
outcome. Remove Controller's conversion through numeric legacy exit codes,
ExecutorResultFromExitCode, and alert/tally remapping where no actual subprocess
consumer requires them. Search all callers before choosing names. A successful
function return is not proof of financial completion. Preserve completed,
deferred, recovery-pending, not-actionable, noop, classified failures and runtime
unresolved-health behavior. Keep existing operator alert codes/remedies/retry
classification. Unknown outcomes must never be counted as completed or healthy.

Adapt existing real controller/recovery tests rather than adding mirrors of enums.
Preserve lease cancellation, frozen destination, pull/top-up ordering, desired
pause recovery, immutable attempt and custody protocols. Do not alter SQL or
financial branching to make the result conversion easier. Retain a numeric adapter
only if a real external caller needs it; keep it out of the Go execution path.

Deliver an early concrete signature/deletion proposal, implemented diff, package
semantic/race results and before/after removed representations/physical lines.
Do not access database fixtures; root runs connected SQL/SVM after review.
