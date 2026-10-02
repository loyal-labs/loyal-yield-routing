# Earn fleet allocation recorder

Migration 0086 creates only `loyal_yield.earn_fleet_allocations_hourly`. It
does not touch existing position tables, snapshots or ledger versions. Version
0085 is the complete-snapshot history index, which is applied separately.

The table holds one row per cluster and UTC hour: the Earn fleet's redeemable
liquidity per Kamino reserve, its idle capital, and how many vaults the sample
covers. loyal-app writes it from the hourly
`/api/cron/earn-reserve-share-prices` job, next to the share prices from
migration 0084, and reads it to weight realized Earn APY by the allocation
held at the time. This replaces rebuilding a month of fleet history from
`vault_position_snapshots` on every request.

The sample covers active Earn vaults only. Each one counts in exactly one
group: included, missing (no complete snapshot yet), invalid (unknown amount
units or unknown idle balance) or stale (funded, with a complete snapshot
older than six hours). Only included vaults contribute to the amounts.
`excluded_amount_raw` keeps the last-known raw amounts of invalid and stale
vaults so a reader can bound what a sample leaves out; its units are
unverified. loyal-app measures an hour only while that excluded amount stays
within 1% of the included capital. Missing vaults have no known capital and
do not block an hour.

Before applying, compare every registered migration checksum with the target
database ledger. Verify the production target independently; never infer it
from a local environment name. The DDL batch has a five-second lock timeout
and a 30-second statement timeout. Both entrypoints verify the table's
columns, check constraints and primary key before recording success, and
verify them again when the ledger is already current. Run
`yield-migrations --check` after applying and confirm version 86's name and
checksum.

The recorder reads each vault's latest complete snapshot through the 0085
history index (`vault_position_snapshots_complete_history_idx`). Confirm that
index exists and is valid in the target database before deploying the
recorder: without it the read has to scan each vault's snapshot history and
may not finish inside its 10 second limit.

Deploy the loyal-app recorder after schema verification. Recording is forward
only: there is no historical backfill, and a missed hour stays missing. A
rerun inside the same hour replaces that hour's row only with a newer sample.

Preserve the table and its rows when rolling back the application. Stopping
or reverting the recorder is enough to stop new writes; do not drop history or
rewrite ledger entries as part of recovery.
