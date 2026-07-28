# Reusable ALT In-Flight Binding Repair Plan

## 1. Repair existing duplicate groups

- In one guarded transaction, lock each desired head and its preparing/warming bindings.
- Revalidate the older binding has no operation and the newer binding is the canonical completed binding.
- Mark only the stale no-operation binding failed; abort if the expected duplicate set or row count changes.

## 2. Fix planning and reconciliation

- Attribute pending operations to `binding_id`, not only the physical lookup table.
- Before inserting, lock and reconcile current in-flight bindings: reuse the canonical binding, fail a safe stale no-operation binding, and verify signed bindings against finalized chain evidence.
- Insert only when no valid canonical binding exists.

## 3. Enforce and handle the invariant

- Add the partial unique index only after existing duplicates are repaired.
- Treat uniqueness conflicts idempotently: reload and reuse the canonical binding.

Recommended:

```sql
UNIQUE (vault_id, family_id, binding_ordinal)
WHERE lifecycle_state IN ('preparing', 'warming')
```
