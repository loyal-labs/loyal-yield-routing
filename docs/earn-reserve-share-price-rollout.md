# Earn reserve share-price recorder

Migration 0084 creates only `loyal_yield.earn_reserve_share_prices` and its
sequence/indexes. Production already uses versions 0074–0083 for Backyard,
so the earlier 0078 recorder registration must not be applied.

Before applying, compare every registered migration checksum with the target
database ledger. The recorder must be the only pending migration. Verify the
production target independently; never infer it from a local environment name.
The standalone `yield-migrations --apply` runner retains its advisory lock and
checksum checks. The new DDL batch has a five-second lock timeout and a
30-second statement timeout. Both entrypoints verify the new table's columns,
positive finite price constraint, primary key, generated IDs and valid indexes
before recording success; checksum-current recorder schemas are also verified.

Run `yield-migrations --check` after applying and verify version 84's name and
checksum. The migration does not alter existing position tables or ledger
versions. On failure, inspect the catalog and retry after resolving the cause;
`IF NOT EXISTS` alone does not prove an existing object has the correct shape.

Deploy loyal-app #794 after schema verification. The production cron runs at
minute 05 each hour. Prices are stamped with the reserve's last-update block
time and slot; stale, future, more-than-three-hour-old or unverifiable states
are skipped. Missing block-time evidence produces no new observation. The
upsert keeps the newer state when cron runs overlap. Check recorder results and
price history freshness after release before enabling the APY consumer.

There is no historical backfill in this release. Preserve the table and all
recorded prices when rolling back the application. Stopping or reverting the
recorder is sufficient to stop new writes; do not drop history or rewrite
previous migration ledger entries as part of recovery.
