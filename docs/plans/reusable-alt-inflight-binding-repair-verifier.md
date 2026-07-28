# Reusable ALT In-Flight Binding Repair Verifier

Use this document as the fixed done condition for ASK-1916. Do not weaken it to
match an implementation. Do not mutate production, deploy, push an image, or add
or run tests while satisfying it.

## Scope

Verify the implementation of
`docs/plans/reusable-alt-inflight-binding-repair-plan.md` for
`loyal-route-lookup-table-provisioner`.

The historical production shape to reproduce is:

```text
vault 1997 has multiple in-flight bindings for manifest 1901
```

The fixture must model two in-flight bindings for one
`(vault_id, family_id, binding_ordinal)` desired head:

- an older `preparing` binding with no operation; and
- a newer canonical `preparing` binding with a completed operation carrying
  finalized transaction evidence.

Use synthetic local identifiers. The verifier must never depend on production
row IDs or write to a production database.

## Required conditions

### 1. Reproduce the failure in an isolated database

- Start or use an explicitly disposable local PostgreSQL database.
- Apply the repository migrations.
- Temporarily model the pre-fix schema without the new in-flight uniqueness
  index.
- Insert a complete, constraint-valid fixture with two in-flight bindings.
- Prove the pre-repair diagnostic sees exactly one duplicate group and would
  produce the same `multiple in-flight bindings` invariant.

### 2. Prove the guarded SQL repair

- Run the repository SQL repair script in one transaction.
- Require an explicit expected group count and expected stale binding IDs.
- Lock the desired head and every in-flight binding in deterministic order.
- Revalidate that each expected stale binding is the older row, owns no
  operation, and is paired with exactly one newer canonical binding whose
  operation is complete and has finalized transaction evidence.
- Abort without changing rows when the expected set, group count, row count, or
  safety predicates differ.
- Update only the expected stale no-operation binding to `failed`.
- Preserve the canonical binding, operation, signature, slots, and timestamps.
- Perform no `DELETE`.
- Run the same repair a second time and prove it succeeds with zero updates.
- Add an unsafe fixture in which the older binding owns an operation and prove
  the repair aborts and rolls back the whole transaction.

### 3. Prove planner reconciliation

- Pending operation ownership in vault allocation must be filtered by
  `binding_id`; a pending operation on the same physical table but for another
  binding must not suppress work for the current binding.
- Before inserting, the planner must lock and reconcile matching in-flight
  bindings.
- Exactly one operation-owning binding is canonical. Safe duplicate rows with
  no operations may be marked failed. Multiple operation-owning rows must
  remain unchanged and fail closed.
- A signed operation must remain attached to its canonical binding and be
  returned to the normal finalized-chain reconciliation path; it must never be
  discarded or relabeled as a safe stale row.
- A completed/finalized canonical binding must be reused so activation can
  continue.

### 4. Prove uniqueness and conflict recovery

- After repair, apply the partial unique index on
  `(vault_id, family_id, binding_ordinal)` for lifecycle states `preparing` and
  `warming`.
- Prove PostgreSQL rejects a second in-flight row for the same key.
- Prove terminal rows do not occupy the partial uniqueness key.
- Every planner insert of an in-flight binding must handle a uniqueness conflict
  without poisoning the transaction, reload the canonical row, and either reuse
  it or fail closed if its identity is incompatible.

### 5. Prove repository integrity without tests

- The focused verifier must emit a named `PASS` or `FAIL` for every required
  condition and emit final `PASS` only when every condition passes.
- It must refuse database writes unless its database was created by the
  verifier itself or carries an explicit isolated/disposable marker.
- Run `cargo fmt --all -- --check`.
- Run focused `cargo check` for the orchestrator library, migration runner,
  provisioner, and focused verifier binary.
- Run `git diff --check`.
- Check the changed text for plaintext secret material and Cyrillic characters.
- Do not invoke `cargo test`, `bun test`, or any test target.

## Verdict

Report `PASS` only if all five required-condition groups pass. A skipped,
inconclusive, or environment-blocked required condition is `FAIL`, not a
partial pass. Record the command, timestamp in Asia/Yekaterinburg, database
class, and per-condition evidence in
`docs/plans/reusable-alt-inflight-binding-repair-verification-run.md`.
