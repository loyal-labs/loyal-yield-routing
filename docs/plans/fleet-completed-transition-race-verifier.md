# Fleet completed-transition race verifier

Use this verifier as the standing done condition for the scoped fix to
`rebalance_queue_transition_failed` false positives in the fleet route
revalidator.

## Objective

Prove that a successfully handed-off fleet execution is treated as successful
when the same opportunity has already advanced monotonically from
`decision_created` to `completed`, without hiding identity mismatches or other
terminal states, and prove that an already-finished worker task is handled
before a simultaneously ready health tick.

## Allowed scope

The implementation may change only:

- `crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs`
- this verifier

Existing unrelated working-tree changes are outside the evaluation scope and
must remain untouched. Do not change database migrations, views, health
intervals, retry/fencing behavior, deployment configuration, or production
state.

## Required conditions

1. The execute-handoff completion check accepts `decision_created` for the
   exact leased opportunity when its durable decision link and route identity
   are intact.
2. The same check accepts `completed` as the idempotent monotonic successor
   only when the exact opportunity, durable decision link, route fingerprint,
   and requirements fingerprint still match the completed worker outcome.
3. The check rejects `completed` if its decision link is absent or either
   fingerprint differs. It also rejects every unrelated state, including
   `waiting_alt`, `revalidate`, `ready`, `leased`, `stale`, `superseded`,
   `failed`, and `cancelled`.
4. When a completed worker task and a health tick are both ready, the worker
   task is selected first. The proof must be a deterministic async test, not
   source-text inspection.
5. Existing non-execute completion behavior, lease-loss behavior, retry
   behavior, fencing, and the one-second health interval remain unchanged.
6. Focused tests cover the accepted `decision_created` and `completed` cases,
   the rejected identity/state cases, and task-over-health priority.
7. The target binary compiles, formatting is clean, and Clippy passes while
   allowing only the package's pre-existing lint categories listed in the
   command below. Do not broaden this fix to clean up unrelated lint debt.

## Verification commands

Run these commands literally from the repository root:

```sh
cargo test -p loyal-yield-orchestrator --bin same-mint-reserve-swap fleet_worker_completion --locked
cargo test -p loyal-yield-orchestrator --bin same-mint-reserve-swap ready_fleet_worker_task_preempts_ready_health_tick --locked
cargo check -p loyal-yield-orchestrator --bin same-mint-reserve-swap --locked
cargo fmt -p loyal-yield-orchestrator -- --check
cargo clippy -p loyal-yield-orchestrator --bin same-mint-reserve-swap --no-deps --locked -- -D warnings -A dead-code -A clippy::manual-range-contains -A clippy::too-many-arguments -A clippy::large-enum-variant -A clippy::explicit-auto-deref -A clippy::needless-borrow -A clippy::nonminimal-bool -A clippy::type-complexity -A clippy::vec-init-then-push -A clippy::op-ref -A clippy::needless-borrows-for-generic-args
git diff --check
git diff -- crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs docs/plans/fleet-completed-transition-race-verifier.md
```

Inspect the final diff and independently map it to each required condition.
Do not count pre-existing unrelated files as implementation scope.

## Nice-to-have conditions

- The state/identity rule is isolated in a small helper so unit tests exercise
  the same logic used by the production completion path.
- The scheduler priority is explicit at the selection site.

## Verdict format

Report:

- `PASS` or `FAIL` for each required condition, with the supporting test,
  command, or diff evidence.
- A separate note for each nice-to-have condition.
- `OVERALL: PASS` only when every required condition passes. Otherwise report
  `OVERALL: FAIL` and list the smallest remaining corrective action.
