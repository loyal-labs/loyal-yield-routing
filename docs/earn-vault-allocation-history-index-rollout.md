# Earn allocation history index (migration 85)

## Purpose and release boundary

The Earn consumer reads complete vault snapshots in a time window and a latest
opening snapshot per vault. The existing `(vault_id, observed_slot DESC,
observed_at DESC)` index does not provide that time ordering/range efficiently.
Migration 85 adds a nonunique partial B-tree on `(vault_id, observed_at DESC,
observed_slot DESC, id DESC)` where `context->>'publication_scope' =
'complete_product_vault'`. Both ascending window traversal and descending seed
selection can use it. The consumer predicate and tie-breaking order must match.

**Do not merge/deploy this migration before the approved build and live validation.**
Normal worker pre-deploy runs `yield-migrations --apply`; merging makes the
expensive build eligible for a subsequent worker deployment. This is separate
from the small, already-applied recorder migration 84. Preserve all older SQL
bytes and ledger entries, including production versions 74–83.

## Production assessment (2026-10-02 UTC)

Catalog/statistics inspection, with no snapshot scans, found approximately
44 million live rows, a 24.7 GB heap and 6.3 GB of indexes. The slot-first index
is 4.3 GB. Plain EXPLAIN of the consumer query selected per-vault bitmap heap
scans and sorts for both seed and window reads. The snapshot-position join
already has its required `(snapshot_id, has_value)` access path.

Context most-common-value statistics cover only 1.55% of rows; complete-vault
values account for 0.0733%. The remaining population is unclassified, so those
statistics do not establish the partial index's actual size. An index may range
from small to several GB. Do not assume it is cheap because it is partial.

The authenticated Neon dashboard showed project `purple-wave-56227231`, default
production branch `br-damp-queen-aq3ixgw2`, approximately 1044 GB project storage,
Launch usage-based billing ($0.35/GB-month storage), and one-day restore history.
Production compute was configured for 0.25–8 CU, with roughly 1–2 CPU cores in
use in the displayed recent hour. A named `pg_settings` read verified
`neon.max_cluster_size=16777216 MB` (16 TiB), well above the current logical
size, and `neon.max_file_cache_size=26214 MB`. Official Neon documentation
allocates temporary disk as the greater of 20 GiB and 15 GiB times maximum CU,
so the existing 8-CU maximum implies 120 GiB allocation. Current free temporary
disk bytes were not available. `temp_file_limit` is unlimited and the existing
role lacks its SET privilege. The existing-role option does not set that
parameter; the bounded-temp option fails before DDL under this role. Select only the option covered by the operational approval. Do not create credentials or change permissions. The mounted environment had no Neon API credential, and
no Neon CLI was available. These observations do not prove physical headroom.

A concurrent build still reads the heap twice (roughly 50 GB before other IO),
evaluates the JSON predicate, and may spill its sort or compete with cache and
production queries. Existing maintenance work memory is 64 MB. Index bytes,
temporary files, restore history/WAL and extra compute contribute cost. Four GB
of persistent index alone would cost approximately $1.40 per month at the
observed rate; build/restore/compute costs and actual size remain uncertain.

## Explicit options and preflight

Builds are disabled by default when the index does not yet exist. Explicitly
select `EARN_HISTORY_INDEX_BUILD_MODE` only after the operational decision:

- `existing-role`: single process, five-second lock timeout and 30-minute
  statement timeout. It leaves the role's current temp-file limit unchanged
  (currently unlimited); actual temporary free space is unknown. This option
  can use the existing mounted role, with explicit acceptance of that risk.
- `bounded-temp`: the same guards plus an eight-GB per-process temp-file limit.
  It requires an already-authorized operator connection with existing SET
  privilege. The mounted role lacks that privilege. A failed privilege check
  does not fall back to existing-role mode.

No mode or an unrecognized value fails before DDL. Once the index is valid and
migration 85 is recorded, normal deployments need no build-mode override.
The read-only `yield-migrations --preflight-earn-history-index` reports index
existence/validity and temp-file SET capability without running DDL or requiring
an already-applied migration. Run it with read-only session options and a
bounded statement timeout. It does not approve the build or validate free disk.

## Preconditions and controlled execution

1. Verify the production target and current ledger again. All registered
   checksums must agree, version 85 must be the only pending migration, and any
   same-name existing object must have the exact required definition. Reconfirm
   version 85 remains free before release if another rollout occurred.
2. Obtain the build decision after reviewing cost, available storage and current
   compute/load. Do not change the plan, autoscaling, security or retention to
   make the build fit. A quieter window or deferral is a valid option.
3. Check active index builds and long transactions using catalog views, not
   table scans. PostgreSQL permits one concurrent index build per table; old
   transactions can delay its phases. Avoid starting while long transactions or
   unrelated maintenance are active.
4. Use the existing standalone migration runner and mounted secret environment.
   It retains the global session advisory lock and checksum fence. The shared
   helper uses a dedicated physical connection, runs one concurrent CREATE
   statement in autocommit, sets `lock_timeout=5s` and
   `statement_timeout=30min` and `max_parallel_maintenance_workers=0`.
   Bounded-temp mode also sets `temp_file_limit=8GB` after its privilege check.
   All settings changed by the chosen mode are restored. The single process
   reduces CPU contention; the optional temp-file limit bounds temporary
   sort/hash storage, not persistent index bytes or cumulative IO. It closes
   a connection whose limits cannot be restored. Neither entrypoint wraps this
   CREATE in a transaction or a multi-statement batch.
5. Capture the emitted index-build backend PID. Monitor only that build through
   `pg_stat_progress_create_index` and its corresponding `pg_stat_activity`
   record; compare production latency/load and temporary-file growth. The
   timeout is a bound, not an ETA or assurance that the build will finish.
6. On material production degradation, the operator may cancel only the
   identified migration backend with `pg_cancel_backend`, after re-verifying
   PID, database and command/index identity. Do not terminate other sessions.
   Cancellation can leave an invalid index, so follow recovery below.

The SQL file contains only one `CREATE INDEX CONCURRENTLY IF NOT EXISTS`.
Lock/session settings live in the shared Rust helper used by both migration
entrypoints. The catalog validator requires the exact relation, B-tree method,
four key names/order/directions, predicate, no included/expression keys, and
`indisvalid=true` / `indisready=true`. A same-name wrong/invalid index fails
closed. The runner records migration success only after validation.

## Interrupted build / recovery

Do not rely on `IF NOT EXISTS`: an interrupted concurrent build can leave an
invalid index that PostgreSQL ignores for reads but still maintains for writes.
The helper does **not** automatically drop any index. The version-85 ledger
entry remains absent when validation fails.

Inspect the exact new index definition, validity/readiness and active build
state. If it is the expected index, invalid, and no build remains active, a
separately reviewed recovery may run
`REINDEX INDEX CONCURRENTLY loyal_yield.vault_position_snapshots_complete_history_idx`
in autocommit with bounded session settings. This repeats expensive work and
must be covered by the operational decision; it is not an automatic retry.
If its definition is wrong, stop and investigate instead of dropping/replacing
an unknown object. Once valid, rerun the existing migration runner to validate
and record its checksum. Never write a success ledger row manually.

## Verification and fallback

Verify the version-85 checksum and exact catalog contract. Plain EXPLAIN should
use the new partial index for seed and window access without per-vault sorts.
Only after the operator permits a bounded representative query should runtime
be measured; an index does not guarantee that the entire fleet aggregation
fits the consumer's ten-second budget. Keep the consumer's database-enforced
read timeout and honest unavailable-data fallback. Empty-price preflight is a
safe immediate shortcut while prices are absent, not proof of future history
query performance. Preserve all source snapshots/history when deferring or
rolling back the consumer.

Local PGlite fixtures verify the actual single concurrent statement/rerun,
catalog drift rejection, indexed forward/backward query plans, and allocation
query equivalence. Rust compilation and existing migration tests cover both
registrations; an environment-contract test verifies that build-mode selection
is explicit and rejects default/unknown values. These tests do not establish live build duration or capacity.

Reference: [PostgreSQL concurrent index behavior](https://www.postgresql.org/docs/current/sql-createindex.html#SQL-CREATEINDEX-CONCURRENTLY).

Reference: [Neon compute temporary disk allocation](https://neon.com/docs/manage/computes).
