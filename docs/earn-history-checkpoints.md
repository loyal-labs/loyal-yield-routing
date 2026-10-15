# Earn chart checkpoint invalidation

Migration `0115_earn_history_day_revisions` adds small per-vault/per-UTC-day
revision counters and row triggers on the existing Earn calculation sources.
It changes no principal, holdings or transaction values. The web/mobile shared
reader compares counters for completed days before reusing a calculation
checkpoint; new current-day history can be calculated incrementally.

Corrections, deletes and timestamp/scope moves update the affected old and new
days. Changes to initial position metadata invalidate all checkpoints through an
epoch-day counter. Snapshot current flags and position principal bookkeeping do
not invalidate old days. Idle-balance updates invalidate the old observation's
day because the last complete snapshot can carry that state.

Apply with `loyal-migrate -db yield -to 115 up` before enabling web checkpoints.
The application falls back to full verified calculation if the revision table
is absent or inaccessible. Existing history requires no backfill: revision zero
is the starting point and triggers track all subsequent changes atomically with
source writes. Do not disable these triggers for repairs; if maintenance bypasses
them, invalidate saved `_checkpoint` metadata before serving charts again.

Local contract verification (disposable PostgreSQL only): create a database named
`earn_checkpoint_test`, then run `psql -v ON_ERROR_STOP=1 -d earn_checkpoint_test
-f scripts/verify-earned-history-revisions.sql`. The script checks inserts,
corrections, deletes, timestamp and scope moves, and bookkeeping isolation, then
rolls back its fixtures and migration.
