# Neon to loyal-db-primary: replica and cutover

Rehearsed, not executed. The freeze needs the owner's go in `gates.md`. `cutover.sh preflight` ran green on Nov 5.

## What runs today

Three databases on loyal-db-primary (10.92.1.1) follow Neon prod.

| Hetzner database | Neon source | Owner role on Hetzner | How it moves |
| --- | --- | --- | --- |
| `loyal_app` | Loyal project `curly-glade-19640752`, `neondb` | `loyal_app_owner` | subscription `loyal_hz_app`, publication `loyal_hz` FOR ALL TABLES |
| `loyal_yield_prod` | yield-optimization `purple-wave-56227231`, `neondb` | `loyal_yield_prod_owner` | subscription `loyal_hz_yield`, publication `loyal_hz` FOR ALL TABLES |
| `kamino_timescale` | yield-optimization, `kamino_timescale` | `kamino_timescale_owner` | plain tables: subscription `loyal_hz_timescale`, publication `loyal_hz` FOR TABLE (8 tables). Hypertables: `replica.sh backfill` then `catchup` |

`replica.sh` runs on the box as `/usr/local/sbin/loyal-db-replica`. `remote.sh` runs on the Mac, installs the script and runs one subcommand. Secrets stay root-only in `/etc/loyal-db/replica` (Neon URLs, `pg_service.conf`, one `<owner>.password` per owner role). Work state is in `/var/lib/loyal-db-replica`.

```sh
deploy/hetzner/db/remote.sh urls        # Neon URLs onto the box through op run, never printed
deploy/hetzner/db/remote.sh setup       # idempotent; a rerun prints "setup: 0 change(s)"
deploy/hetzner/db/remote.sh backfill    # starts loyal-ts-backfill.service; resumes per day
deploy/hetzner/db/remote.sh catchup     # copies hypertable rows inserted since the last floor
deploy/hetzner/db/remote.sh sequences   # setval on every sequence from Neon
deploy/hetzner/db/remote.sh verify      # per table counts, max PK, sync state, slot lag
deploy/hetzner/db/remote.sh check       # exit 1 if a Neon slot is inactive or lags > 5 GiB
deploy/hetzner/db/remote.sh neon-cu 6   # hourly average CU per Neon project, last 6 h
```

### Decisions

- Schema comes from `pg_dump --schema-only` of Neon. Ownership is dropped and re-owned by the database's owner role through `pg_restore --no-owner --role`. Grants to `loyal_web_stats_reader` and `loyal_vault_demo_observation` restore as written; both roles exist on Hetzner as NOLOGIN. Neon's `cloud_admin` default ACLs and the `pg_repack` extension are not restored.
- Owner roles reuse the names already on the box (`loyal_app_owner`, `loyal_yield_prod_owner`, `kamino_timescale_owner`), now LOGIN with generated passwords. The `*_runtime` roles and the `pg_hba.conf` lines for them stay untouched.
- Hypertables do not ride the publication. Their chunks would not replicate through the root. `kamino.reserve_updates` and `loyal_prod.balance_sweep_wallet_ata_observations` are append-only: the only writers insert (`go/workers/internal/observer/kamino/store.go:134`, `go/workers/internal/observer/ata/handler.go:193`). No code path updates or deletes them. `backfill` copies the last 62 days one day at a time in binary COPY. Each day deletes its own range first and is marked done only after its COPY commits, so a rerun resumes. `catchup` copies rows with `event_id` above a floor into a temp table and inserts the ones not already present. `event_id` comes from a sequence on every insert, late historic imports included, so it is the watermark. `observed_at` is not one, because historic imports insert old `observed_at`. The floor keeps a 10000-id margin for transactions in flight.
- Both dedupe tables ride logical replication in full: `kamino.reserve_update_dedupe` is 11 GB and `loyal_prod.balance_sweep_wallet_ata_observation_dedupe` is 954 MB. They are keyed state, and their primary key makes logical replication exact through the freeze with no watermark. Their job is "never insert the same observation twice". A 62-day trim would let a replayed older slot insert a duplicate. Keeping them costs about 12 GB.
- The initial copy writes WAL faster than pgbackrest archives it to the Storage Box. On Nov 5 the copy generated about 2.3 GB/min and the archiver pushed about 2 GB/min. The `.ready` backlog sits in `pg_wal` on the 492 GB volume. `backfill` waits whenever more than 512 segments (8 GB) are still unarchived, so it only runs while the archiver keeps up. `verify` prints the backlog on its first line. The pgbackrest repo is the Storage Box `u679742` (1 TB, 820 GB free on Nov 5), not the volume.
- Sync workers are bounded at 2 per subscription (`max_sync_workers_per_subscription = 2` in `conf.d/zz-replica.conf`). Parallel apply is off (`streaming = on`, spill to disk). This also keeps the Neon yield project inside `max_replication_slots = 10`: 2 Neon internal slots, 2 subscription slots, up to 4 tablesync slots.

## Migration rule while the replica runs

DDL does not replicate. Apply any migration to `loyal_yield_prod` or `kamino_timescale` on Hetzner first when it adds columns or tables, then on Neon. Apply drops on Neon first, then on Hetzner. A new table in yield or app needs `ALTER SUBSCRIPTION loyal_hz_<src> REFRESH PUBLICATION` on Hetzner after it exists on both sides. A new plain table in Timescale also needs `ALTER PUBLICATION loyal_hz ADD TABLE` on Neon, which is the owner's call. A new hypertable needs a line in `HYPERTABLES` in `replica.sh`. A migration that breaks apply stops the subscription. It shows as `last_msg` growing in `verify` and in the Postgres log.

## Neon slot risk

Neon removes a logical slot that has not acknowledged `flush_lsn` progress for about 40 hours ([Neon docs, Unused replication slots](https://neon.com/docs/guides/logical-replication-neon#unused-replication-slots)). Until then, the stopped slot pins WAL and Neon storage grows with every write. When Neon removes it, the subscription cannot resume. The fix is to drop the database on Hetzner and rerun `setup`, which copies everything again. Hypertable state in `/var/lib/loyal-db-replica/backfill` survives that, but the database it describes does not, so remove that directory too. While a subscriber stays connected, the Neon compute stays awake and does not scale to zero.

Prometheus on loyal-workers pages the Telegram alerts group through `loyal-db-replica metrics` (a textfile on the DB box, written every minute): `LoyalDbReplicaSubscriptionDown` after 5 minutes without an apply worker, `LoyalDbReplicaSubscriptionStale` after 15 minutes without a message, `LoyalDbNeonSlotInactive` after 10 minutes, `LoyalDbNeonSlotLag` above 5 GiB, plus volume under 100 GiB free and `pg_wal` over 60 GB (`deploy/hetzner/README.md`, Replica guards). Neon's 40 hours leave most of the window to act in. `remote.sh check` (`replica.sh` `cmd_check`) is the manual form of the slot check and exits 1 when a slot is inactive or lags more than 5 GiB.

`cutover.sh switch` disables the subscriptions on purpose. `cutover.sh freeze` first adds a 7-day Alertmanager silence on `alertname=~"LoyalDbReplica.*|LoyalDbNeon.*"`. `cutover.sh pause-neon` drops the subscriptions. From then on, `replica.sh metrics` stops writing subscription and slot series for them, so those alerts resolve and the rule group can stay.

## Cutover

| The lie underneath | The one fix | What gets deleted |
| --- | --- | --- |
| "Every new DSN is direct, because the families hold session advisory locks." Only Yield clients hold session locks or LISTEN (`go/workers/internal/engine/family.go:38`, `go/workers/internal/backyard/store.go:115`, `go/workers/internal/observer/worker/watch_refresh.go:41`, `go/workers/internal/autodeposit/worker.go:361`), and they already use Neon's direct endpoint. App and Timescale clients use Neon's pooler and only `pg_advisory_xact_lock` (`go/workers/internal/webapp/crons.go:49`). | Each URL keeps its shape. A direct Neon host maps to `10.92.1.1:5432`, and a `-pooler` host maps to pgbouncer `10.92.1.1:6432` in transaction mode. `replica.sh pgbouncer-test` proves named prepared statements work through 6432. | The direct-only rule and the `verify-full` option. Every URL is `sslmode=require`. pgx and sqlx/rustls do not check the certificate at `require`, the same as on Neon today. |
| The credential table was written by hand, and it missed telegram, the web crons, admin's Yield and Timescale URLs, and backyard's Timescale URL. | `cutover.sh inventory` decrypts every credential on the hosts and writes `inventory.tsv` (credential, unit, Neon endpoint, db, role, pooler or direct). Every later step reads that file. `coverage` fails if a running `loyal-*` unit is missing from it, or if a database reader is missing from `ENGINES`/`SERVICES`. | The hand-written table and the "check list-timers on both hosts" step. |
| "Owner role with a password is enough." Out of the box the Hetzner owner roles had `TimeZone=UTC` and `statement_timeout=900000`, while Neon's `neondb_owner` has `GMT` and `0`. Neither had a pg_hba line or a pgbouncer user. | `replica.sh access` adds the hba lines and pgbouncer users. `replica.sh grants --fix` copies Neon's per-database role settings. `grants` diffs every table, sequence, schema, function and database privilege. `schema` diffs `pg_dump` of both sides. Each one is idempotent and runs in every preflight. | Freeze step "append the staged hba lines". |

### Consumers (inventory of Nov 5, 22:45Z)

All 19 live bindings are on loyal-workers, and all of them log in as `neondb_owner`. loyal-eu-1, robinx-prod and loyal-agent-memory hold no Loyal Neon URL. `loyal-clickstack` and `loyal-clickstack-relay` have no database.

| Database | Readers | Neon shape, maps to |
| --- | --- | --- |
| `loyal_app` | loyal-admin, loyal-stats, loyal-telegram, loyal-web, loyal-web-cron@ | pooler, maps to 6432 |
| `loyal_yield_prod` | loyal-admin, loyal-backyard, loyal-observer, loyal-realtime (inside `realtime-envelope`), loyal-retail, loyal-stats, loyal-web, loyal-web-cron@ | direct, maps to 5432 |
| `kamino_timescale` | loyal-admin, loyal-backyard, loyal-observer, loyal-retail, loyal-web, loyal-web-cron@ | pooler, maps to 6432 |

`/etc/credstore.encrypted/loyal-web-app-db.a347` and `.owner` are on disk but no unit references them, so they are not switched. The Vercel projects (`loyal-frontend`, `loyal-app`) still have 104 route handlers that reach a database. They are not switched: only 2 non-cron requests landed after 12:45 PST on Nov 5. A Vercel write after switch lands on Neon, and the write counters catch it (`verify` warns, `pause-neon` refuses).

### Kit

`cutover.sh` runs on the Mac. It reuses `remote.sh`'s `box` and `install`, and every database step is a `replica.sh` subcommand on the box. Credentials are staged, tested, switched and rolled back on loyal-workers by `/etc/loyal-cutover/creds.py`. Passwords go box to workers over a pipe and are sealed straight into `systemd-creds` files. Nothing prints a URL, user or password. Every subcommand can be rerun.

| Step | What it does |
| --- | --- |
| `preflight` | Inventory, coverage, hba and pgbouncer access, grants, schema, pgbouncer test, staging every credential into `/etc/loyal-cutover/stage`, a read through each staged URL, Neon sessions, lag-wait, catchup, sequences, write counters, a replica verify, both Neon endpoints over the API, unit stop/start times from 14 days of journal, and the estimate. Neon sees only read-only sessions. No running unit is touched. |
| `freeze` | Silence, then stop: web-cron timers, wait for running crons, engines one at a time (each must stop with `Result=success`), then services. Neon sessions, lag-wait, catchup, sequences, a `freeze` write snapshot, and a replica verify that must show no DIFF. |
| `switch` | lag-wait, `ALTER SUBSCRIPTION ... DISABLE`, catchup, sequences, a `switch` write snapshot. Then it saves each live Neon credential to `/etc/loyal-cutover/neon/<path>` and installs the staged one. Services start, and each must answer its health path. Engines stay stopped. |
| `verify` | Reads through every live URL, health on each service, write counters since `switch`. Fails if Hetzner took no write. Warns if Neon took one. |
| `unfreeze` | `verify`, then the timers, then the engines (each must answer `/metrics`), then health on everything after 60 s. |
| `rollback` | Stop the writers, put the saved Neon credentials back, start everything. Re-enables the subscriptions only if Hetzner took 0 writes since `switch`. Refuses once `pause-neon` has run. |
| `pause-neon` | Refuses if Neon took writes since `switch` (override with `PAUSE_ACCEPT_NEON_WRITES=1`). Drops the subscriptions and their Neon slots, sets `disabled: true` on `ep-dry-forest-ah27inp9` and `ep-ancient-grass-aqb5aalu`, and suspends them if they are active. Neither project is deleted. Branch endpoints are left alone. |
| `status` | Which side each credential points at, plus whether a staged copy and a saved Neon copy exist. |

### Freeze plan, measured

From the preflight of Nov 5, 22:45Z (`proof/hz-db5-cutover/preflight-20261105T2245Z.txt`). Stop and start times are the journal maxima over 14 days: stops at most 3.75 s (loyal-web), starts at most 13.59 s (loyal-observer).

1. `cutover.sh preflight`, any time before. It must exit 0 (295 s).
2. `cutover.sh freeze`, about 114 s. Catchup is most of it (66 s).
3. `cutover.sh switch`, about 108 s.
4. `cutover.sh verify`, about 26 s. If it fails, run `cutover.sh rollback`.
5. `cutover.sh unfreeze`, about 65 s.
6. `cutover.sh pause-neon` once the rollback window closes.

Writes stop for about 5.2 minutes, from freeze to unfreeze. loyal-web, admin, stats, telegram and realtime are down from freeze until switch starts them, about 3.7 minutes. Engines are down the whole 5.2 minutes.

### Rollback

Reverse replication from Hetzner to Neon is not set up. It needs `wal_level=logical` on the box, which means a Postgres restart. It also keeps Neon awake through the window, while the plan pauses Neon. And hypertables do not replicate through their root.

- Before `unfreeze`: `rollback` loses nothing the engines wrote. Services may have written during `verify`, and those writes stay only on Hetzner.
- After `unfreeze`, fix forward. Hetzner's pgbackrest PITR covers Hetzner. Neon keeps its own history until the project is deleted.
- If `switch` stops after disabling the subscriptions but before the `switch` snapshot, `rollback` leaves them disabled. Run `remote.sh verify`, then `replica.sh subscriptions enable` on the box.

## After

- `pause-neon` drops the three subscriptions and their Neon slots.
- Timescale retention is `loyal-db-retention.timer` on loyal-db-primary. Daily at 04:30 UTC it runs `loyal-db-replica retain`, which drops hypertable chunks older than 62 days (`timescaledb.max_background_workers = 0`, so no policy job). It keeps running after cutover.
- Rescale to cpx51 (owner call D2). It needs a stop of the box. Do it outside a copy, and before the freeze rather than after.
