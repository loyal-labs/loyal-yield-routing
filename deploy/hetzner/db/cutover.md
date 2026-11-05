# Neon to loyal-db-primary: replica and cutover

Rehearsed, not executed. The freeze needs the owner's go in `gates.md`. Times are PST.

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

Freeze step 5 disables the subscriptions on purpose. Before it, silence `alertname=~"LoyalDbReplica.*|LoyalDbNeon.*"` in Alertmanager (`amtool silence add` on loyal-workers) for the rollback window, and delete the `loyal-db-replica` rule group when the subscriptions are dropped.

## Before the freeze

1. `remote.sh verify` shows every table in state `r` for all three subscriptions, and no `DIFF` lines beyond rows written since the read.
2. `remote.sh backfill` has finished (`backfill: done from <cutoff>` in `/var/lib/loyal-db-replica/backfill.log`). Run `remote.sh catchup` a few times beforehand so that the run during the freeze only covers the last interval.
3. Rehearse `remote.sh sequences`. It is safe on the replica and the final run during the freeze overwrites it.
4. Stage the new credentials without installing them. Each one is a direct DSN to `10.92.1.1:5432`, not pgbouncer, because the families hold session advisory locks (`deploy/hetzner/README.md:49`). Use `sslmode=verify-full` with the box CA `/etc/loyal-db/tls/ca.crt`, or `sslmode=require`. The user is the owner role, with the password from `/etc/loyal-db/replica/<owner>.password`.
5. Stage the `pg_hba.conf` lines. They go in front of the `host all all 0.0.0.0/0 reject` line:
   ```
   hostssl loyal_app        loyal_app_owner        10.92.0.0/16 scram-sha-256
   hostssl loyal_yield_prod loyal_yield_prod_owner 10.92.0.0/16 scram-sha-256
   hostssl kamino_timescale kamino_timescale_owner 10.92.0.0/16 scram-sha-256
   ```
6. Confirm that Vercel shows 0 requests on the routes that write: the Vercel web app (loyal-app) and the telegram app. Standing order 5 puts the DB move after that.

## Freeze

Expected length is 10 to 15 minutes. Most of it is installing credentials.

1. Stop the writers. On loyal-workers run `systemctl stop loyal-retail loyal-backyard loyal-observer loyal-realtime loyal-web loyal-admin loyal-stats`. On both hosts, check `systemctl list-timers 'loyal-*'` and stop the timers that write, such as `loyal-squads-watch-tick` and `loyal-observer-push-check`. The Vercel web and telegram apps must already be at 0 requests. If not, stop here.
2. Confirm that Neon has no writers left. The only sessions as `neondb_owner` should be the `loyal_hz_*` walsenders and your own:
   ```sql
   select application_name, state, count(*) from pg_stat_activity where usename = 'neondb_owner' group by 1, 2;
   ```
3. Lag 0. Run `remote.sh verify` until every slot shows `lag_bytes` at or near 0 and stops moving. Neon writes a little WAL on its own, so look for no change across two runs.
4. `remote.sh catchup`, then `remote.sh sequences`.
5. On Hetzner, run `ALTER SUBSCRIPTION loyal_hz_<src> DISABLE` for each of `app`, `yield` and `timescale`. Keep the subscriptions and their Neon slots for the rollback window.
6. Append the staged hba lines and run `systemctl reload postgresql@18-main`. Run `ANALYZE` in each database.
7. Flip the URLs. Every file is a systemd credential on loyal-workers, encrypted with `systemd-creds encrypt --with-key=host --name=<NAME>` (`deploy/hetzner/README.md:44`). Keep the old file as `<file>.neon` before writing the new one.
   | Service | Credential name and file | Database |
   | --- | --- | --- |
   | loyal-retail | `RETAIL_DATABASE_URL`, `/etc/credstore.encrypted/loyal-retail/RETAIL_DATABASE_URL` (`deploy/hetzner/systemd/loyal-retail.service:19`) | loyal_yield_prod |
   | loyal-retail | `RETAIL_TIMESCALE_DATABASE_URL`, same directory (`loyal-retail.service:20`) | kamino_timescale |
   | loyal-backyard | `BACKYARD_DATABASE_URL`, `/etc/credstore.encrypted/loyal-backyard/BACKYARD_DATABASE_URL` (`loyal-backyard.service:19`) | loyal_yield_prod |
   | loyal-observer | `NEON_DATABASE_URL` and `TIMESCALEDB_URL`, `/etc/credstore.encrypted/loyal-observer/` (`loyal-observer.service:20-21`) | loyal_yield_prod, kamino_timescale |
   | loyal-realtime | `NEON_DATABASE_URL` inside the `realtime-envelope` JSON credential (`loyal-realtime.service:29-33`, `deploy/hetzner/README.md:195`) | loyal_yield_prod |
   | loyal-admin | `ADMIN_APP_DATABASE_URL`, `/etc/credstore.encrypted/loyal-admin-app-db` (`loyal-admin.service:31`), plus any optional DB credential in its drop-ins | loyal_app |
   | loyal-stats | `STATS_APP_DATABASE_URL` and `STATS_YIELD_DATABASE_URL`, `/etc/credstore.encrypted/loyal-stats-{app,yield}-db` (`loyal-stats.service:26-27`) | loyal_app, loyal_yield_prod |
   | loyal-web | `WEB_APP_DATABASE_URL`, `/etc/credstore.encrypted/loyal-web-app-db` if installed (`deploy/hetzner/README.md:354`) | loyal_app |
   | Vercel loyal-app web and telegram | `DATABASE_URL` in Vercel project env | unchanged: they are at 0 requests and get deleted in teardown |
   | 1Password env files used by local tooling | `NEON_DATABASE_URL`, `TIMESCALEDB_URL`, `DATABASE_URL` | the owner updates them after the freeze |
8. Start the services in reverse order of step 1. Then check them: each unit is `active`, the realtime `/ready` endpoint answers, and one write lands in each database. On the box, `select max(inserted_at) from kamino.reserve_updates` should advance.

## Rollback

Within the window, before Hetzner takes writes worth keeping:

1. Stop the services again and move each `<file>.neon` back over its credential.
2. Start the services. Neon was never written by the cutover and still holds the truth.
3. To resume the replica, drop the rows written on Hetzner after the flip. In practice that means rerunning setup from an empty database. Then run `ALTER SUBSCRIPTION ... ENABLE`.

After writes land on Hetzner, rolling back loses them. Two choices remain: fix forward, or set up a reverse subscription from Neon to Hetzner. The reverse subscription needs `wal_level = logical` on the box plus DDL on Neon, and both are owner calls. Decide the window length before the freeze. Fifteen minutes of smoke checks is the default.

## After

- Drop the three subscriptions once the window closes. `DROP SUBSCRIPTION` also drops the Neon slot. If you skip this, Neon removes the inactive slots after about 40 hours anyway, and each one pins WAL until then.
- Timescale retention is `loyal-db-retention.timer` on loyal-db-primary: daily at 04:30 UTC it runs `loyal-db-replica retain`, which drops hypertable chunks older than 62 days (`timescaledb.max_background_workers = 0`, so no policy job). It keeps running after cutover.
- Rescale to cpx51 (owner call D2). It needs a stop of the box. Do it outside a copy, and before the freeze rather than after.
