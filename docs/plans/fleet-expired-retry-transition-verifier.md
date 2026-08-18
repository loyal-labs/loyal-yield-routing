# Fleet expired-retry transition verifier

Use this verifier as the standing done condition for the scoped fix to the
`rebalance_queue_transition_failed` false positive emitted when an effect-free
fleet retry loses its opportunity to optimizer-epoch expiry before its durable
queue transition is persisted.

## Objective

Prove that the durable store atomically distinguishes an applied transition,
an expired opportunity, and a fenced lease; that the fleet worker treats only
an expired effect-free `Retry` as successfully handled without emitting the
paging operational error; and that every state with possible effects or an
uncertain durable outcome remains loud.

## Allowed scope

Implementation changes may touch only:

- `crates/loyal-yield-store/src/fleet_orchestration/queue.rs`
- `crates/loyal-fleet-worker/src/lib.rs`
- `crates/loyal-fleet-worker/src/cross_mint.rs` only if adapting the typed store API
- `crates/loyal-yield-orchestrator/src/bin/fleet-orchestration-verifier.rs`
- this verifier

Do not add a migration, change queue schema, change retry delays, weaken the
commit-lifetime fences, change completed-transition identity checks, or mutate
production state. Existing unrelated changes outside this isolated worktree are
out of scope.

## Required conditions

1. `advance_rebalance_opportunity` returns a public typed outcome with distinct
   `Applied`, `Expired`, and `Fenced` variants. Database, decoding, and invariant
   faults remain `Err`; callers must not classify outcomes from error strings.
2. The store decides `Expired` or `Fenced` while holding the opportunity row
   lock and using database time/current durable state. There is no worker-side
   `Utc::now()` precheck that predicts whether expiry will win the write.
3. The store returns `Expired` when either:
   - the exact lease/opportunity has reached its durable lifetime boundary; or
   - the expiry sweep has already moved the same identity to `stale` with
     `terminal_reason = 'optimizer_epoch_expired'`, no decision, and no active
     signed-route effect.
   An identity-divergent, non-expiry terminal state or mismatched live lease is
   `Fenced` or `Err`, never `Expired`.
4. `finish_fleet_worker_task` treats `Expired` as success only for an outcome
   whose state is `Retry` and whose existing effect flags say it wrote no
   decision and sent no transaction. It emits one non-operational structured
   status and returns `Ok`, so the caller does not emit
   `rebalance_queue_transition_failed`.
5. `Expired` for any non-`Retry` or potentially effectful outcome, `Fenced`, and
   every store/database error still reach the existing paging failure path.
   The accepted `decision_created`/`completed` handoff path and its exact
   identity checks remain unchanged.
6. Focused deterministic unit tests exercise the worker decision for:
   - `Applied`;
   - `Expired` plus effect-free `Retry`;
   - `Expired` plus a non-retry or effectful result; and
   - `Fenced`.
7. The isolated database verifier proves both authoritative classifications:
   - after the expiry sweep retires an effect-free leased opportunity, the
     stale row is classified `Expired`; and
   - a live lease with a different owner/fencing token is classified `Fenced`.
   Its JSON subchecks must be required for the isolated-database verdict.
8. The target crates compile, focused tests pass, formatting is clean, Clippy
   reports no new warnings, the isolated database verifier passes, and the
   final diff contains only the allowed scope.

## Verification commands

Run these commands literally from the isolated worktree root:

For the database command, first export `FLEET_VERIFY_DATABASE_URL` for a
disposable, fully migrated local PostgreSQL database whose name contains
`fleet_verify`. Never set it from `NEON_DATABASE_URL` or any production branch.

```sh
cargo test -p loyal-fleet-worker --lib fleet_worker_advance_outcome --locked
cargo test -p loyal-fleet-worker --lib fleet_worker_completion --locked
cargo test -p loyal-yield-store --lib advance_rebalance_opportunity --locked
cargo check -p loyal-fleet-worker --bin same-mint-reserve-swap --locked
cargo check -p loyal-yield-orchestrator --bin fleet-orchestration-verifier --locked
cargo fmt --all -- --check
cargo clippy -p loyal-fleet-worker --bin same-mint-reserve-swap --no-deps --locked -- -D warnings
cargo clippy -p loyal-yield-store --lib --no-deps --locked -- -D warnings
test "$(psql "$FLEET_VERIFY_DATABASE_URL" -X -Atqc "SELECT current_database() LIKE '%fleet_verify%'")" = t
bun run fleet:verify -- --isolated-database
git diff --check
git diff --name-only origin/main
git status --short
```

Inspect the implementation and command output independently. In particular,
confirm that the quiet path is selected from the typed store outcome and
effect-free retry state, not from timing guesses or substring matching.

## Nice-to-have conditions

- Existing callers that require an applied record use one small conversion
  helper rather than repeating `Expired`/`Fenced` error construction.
- The expected-expiry status is low-cardinality and contains no raw external
  error or secret material.

## Verdict format

Report `PASS` or `FAIL` for each required condition with the supporting test,
command, JSON subcheck, or diff evidence. Report nice-to-have conditions
separately. Emit `OVERALL: PASS` only when all eight required conditions pass;
otherwise emit `OVERALL: FAIL` and name the smallest remaining correction.
