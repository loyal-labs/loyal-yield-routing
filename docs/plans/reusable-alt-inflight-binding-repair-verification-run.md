# Reusable ALT In-Flight Binding Repair Verification Run

- Linear issue: ASK-1916
- Branch: `ASK-1916-fix-reusable-alt-inflight-bindings`
- Worktree:
  `/private/tmp/loyal-yield-routing-ASK-1916-inflight-binding-repair`
- Timestamp: 2026-07-28 23:49:54 +05
- Database class: two disposable local PostgreSQL databases created and removed
  by the verifier
- Production writes: none
- External RPC: none
- Tests added or run: none

## Command

```sh
bun run verify:reusable-alt-inflight-binding-repair
```

## Result

Overall verdict: **PASS**

| Required condition | Result | Evidence |
| --- | --- | --- |
| Reproduce duplicate failure | PASS | The isolated fixture emitted `vault 1 has multiple in-flight bindings for manifest 1` before repair. |
| Planner binding reconciliation | PASS | The planner failed only the stale no-operation row, reused the completed canonical binding, ignored an unrelated same-table pending operation, and reused its newly queued binding-owned operation on retry. |
| Signed and ambiguous operation safety | PASS | Multiple operation-owning rows failed closed without state changes. A single signed operation-owning row was preserved and returned through normal reconciliation while its no-operation duplicate was failed. |
| Repaired terminal supersession | PASS | The planner excluded the append-only repaired `permanent_failure` root from effective reconciliation, retained its completed finalized/reconciled successor as evidence, failed the superseded old binding, and continued planning the new manifest in the same transaction. |
| Guarded SQL repair | PASS | The first run updated exactly one expected stale row, the second run updated zero rows, canonical finalized evidence remained byte-for-byte unchanged, and an unsafe stale operation owner caused an atomic abort. |
| Partial uniqueness | PASS | Migration 32 rejected a second `preparing`/`warming` row for the same vault/family/ordinal while allowing a terminal `failed` row. |
| Migration and schema integrity | PASS | Migration apply/check and the complete reusable ALT schema verifier passed on fresh disposable databases. |
| Repository integrity | PASS | Focused Cargo checks, `cargo fmt --check`, `git diff --check`, plaintext-secret scan, and Cyrillic scan passed. |

The final verifier lines were:

```text
PASS planner_repaired_terminal_successor_does_not_poison_supersession
PASS repository_integrity_without_tests
PASS reusable_alt_inflight_binding_repair
```
