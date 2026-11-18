# deploy/aws

## box.sh

`deploy/aws/box.sh <box> [--dry-run]` builds one AWS box from `deploy/aws/boxes/<box>.conf` and converges on rerun. A second run prints `<box>: 0 change(s)`. Boxes: `robinx-main`, `loyal-main`, `xstock-live`, `weather-live`.

Per box it ensures, each found again by its Name tag: a key pair imported from the conf's public key, a security group (tcp 22 from anywhere, nothing else), the instance (Ubuntu 24.04 for the conf's architecture, AMI read from Canonical's SSM parameter, IMDSv2 required, termination protection on), its gp3 volumes with size, IOPS and throughput from `VOLUMES`, attached and tagged, and one Elastic IP associated. It never deletes or terminates. A volume size or IOPS mismatch is converged online (gp3 grows, never shrinks; the filesystem is grown on the box by hand). An instance type mismatch is a `warning:` line until `BOX_RESIZE=1` is set, then the script stops the instance, modifies the type and starts it again (same architecture only; the Elastic IP and termination protection stay). The security group allows tcp 22 plus the conf's optional `INGRESS` entries (`tcp|udp:port:cidr`), each added when missing; any other inbound rule is a `warning:` line, not a change. Formatting and mounting the data volumes is not its job.

Conf fields, all required, none with a default: `PROFILE` (or `AWS_PROFILE`), `REGION` (or `AWS_REGION`), `INSTANCE_TYPE`, `ARCH` (`arm64` or `x86_64`), `PUBKEY` (or `BOX_PUBKEY`; a path to a `.pub` file, never a private key), `VOLUMES` (`name:GB[:iops[:MB/s]]`, with `root` first). `ACCOUNT` is optional and, when set, must match the profile's account. `PROFILE` is empty for `robinx-main` and `xstock-live` until the vCPU quota read picks `main` or the new account, and `REGION` is empty for `weather-live` until the Polymarket probe picks one.

`--dry-run` prints every aws command it would run and changes nothing. Its reads (`sts get-caller-identity` first) need a live session; without one it says so in one line and lists every step as missing.

## loyal-main-prepare.sh

`ssh loyal-main 'sudo bash -s' < deploy/aws/loyal-main-prepare.sh` makes the blank arm64 box `loyal-main` match the two Hetzner boxes it replaces, and rerun prints `loyal-main: 0 change(s)`. It sets the hostname, formats the 300 GB volume ext4 at `/srv/postgresql` (the db box's mount options, by UUID) and the 150 GB volume at `/srv/loyal-services` with bind mounts to `/opt/loyal` and `/srv/loyal-clickstack`, creates the empty directory tree and modes the services expect, and installs the same package versions as loyal-workers-1 and loyal-db-primary for arm64: PostgreSQL 18.6, timescaledb 2.24.0, pgbouncer, pgbackrest 2.59.1 (PGDG archive, sha256 pinned), docker, prometheus, alertmanager, node exporter, caddy, cloudflared 2026.10.2 and otelcol-contrib 0.162.0 (release assets, sha256 pinned). It sets `create_main_cluster = false` before PostgreSQL installs, as the db box has, so no cluster exists. Every packaged unit is left disabled and stopped. It copies no unit, release, secret or data.

## loyal-main-config.sh

`ARTIFACTS=<gh run download dir> LAUNCHER=<arm64 env-launcher dir> deploy/aws/loyal-main-config.sh stage` runs from the Mac after `loyal-main-prepare.sh` and stages, without enabling or starting anything, what loyal-workers-1 and loyal-db-primary run for Loyal: the arm64 release of `RELEASE_SHA` laid out under `/opt/loyal/releases` as `scripts/deploy-workers.sh` would, with its `40-verified-release.conf` drop-ins; every `loyal-*` unit, drop-in and timer; the cloudflared, otelcol, Prometheus, Alertmanager, Caddy, PgBouncer, pgBackRest and receiver configs; and every sealed credential, decrypted on the old box and re-sealed on loyal-main under the same name and path through one pipe (`deploy/aws/box-copy.py`), never touching disk or a terminal on the way. Database URLs and listen addresses are rewritten to 127.0.0.1 inside that pipe. The Postgres cluster config is staged beside the cluster under `/opt/loyal-db-staged` for `deploy/aws/db/pgmove.sh`; the tunnel credential is sealed but referenced by no unit. Old boxes are only read. It refuses to run when the running commit and `RELEASE_SHA` differ in service source, ends `loyal-main: <n> change(s)`, and fails if any `loyal-*` unit is enabled. `proof <dir>` writes credential decrypt results and database host:port per credential (no values), `systemd-analyze verify` per unit, enabled and listening state, and each binary's `--help` exit code run without network.

## robinx-main-config.sh

`BUILD=<arm64 build tree> deploy/aws/robinx-main-config.sh stage` stages on robinx-main what robinx-prod and the mnemonic box run, without enabling or starting anything: the arm64 release of the commit robinx-prod's live slot runs, the wallet indexer, the pinned monitoring tools, every robinx unit with its drop-ins, the config trees, the sealed credentials re-sealed for this host and `/srv/robinx-data/recovery` (postgres 0700, as on robinx-prod). It goes through `box-copy.py` like `loyal-main-config.sh` and ends `robinx-main: <n> change(s)`; a rerun prints 0. The build steps are in the script header. `deploy/aws/robinx-main-restage.sh` does the whole restage in one command: it reads the release live on robinx-prod, builds it for arm64 from a clean archive of wen, stages it and reruns the stage, which must print 0 changes. Run it right before the robinX switch window, because production ships often.

`deploy/aws/robinx-main-config.sh check` is read-only on all three hosts. It runs the same senders as `stage`, applies the same rewrites on the Mac, and compares the result with what robinx-main holds (content hash, mode, owner, link target). Each path a rerun would change prints as a `differs:` line, then `release:`, `check: <n> difference(s)` and `secrets: <b> of <a> ...`. It exits 0 only when nothing differs, the staged release is the live one and every secret matches. Without `BUILD` the two binaries are checked by the release sha inside the binary and by presence. Credentials are compared by `sums`, never by content.

## switch/s5-robinx.sh

`deploy/aws/switch/s5-robinx.sh preflight` is the read-only gate before `subscribe robinx`. It runs every check even when one fails and prints one line per check as `ok`, `BLOCKER` or `PARKED`. It ends `<n> ok, <m> blocker, <k> parked` and exits 0 only when m is 0. PARKED marks an owner call or a known gap that does not stop the subscribe: the backup age, root and deploy login on robinx-main, mnemonic units after S1, and a source that is already subscribed. Check `units-cover` reads the CUT_* lists of `db/clusters/robinx.conf` and units.tsv (`S5_UNITS_TSV`): every listed unit must exist on robinx-main, disabled and inactive, and every robinx-main row with fate move must be in a list, a cluster pgmove moves, mnemonic (S1) or the script's allow-list (`robinx-parsers-captune`, `robinx-lockwatch`: owner call, not moved, stopped on robinx-prod at freeze). A row in none is a BLOCKER. The production backup units (`robinx-backup-full.timer`, `robinx-backup@.service`) are PARKED until `backup.sh robinx-main` installs them at runbook step 1.7.

The checks are:

- 0348 on production
- no publication or slot on robinx-prod
- `subscribe`, `init` and `partitions` dry runs through `db/remote.sh`
- the target cluster, its room, schema and settings against robinx-prod
- the 337 GB slot cap at the measured WAL rate
- `robinx-main-config.sh check` (rerun and secrets)
- no robinx or monitoring unit enabled or active on robinx-main
- the recovery directory
- wen's `deploy/aws/repoint.sh check`
- the `wen-prod` alias
- cloudflared versions
- both app slots in `CUT_STOP`

The switch itself runs from `deploy/aws/db`, in the order of the S5 runbook in the migration tracker.

## switch/s4-loyal.sh

Switch S4 moves loyal-workers and the database box loyal-db-primary to loyal-main with one tool: `db/cutover.sh loyal freeze|switch|verify|unfreeze|abort|rollback`. `clusters/loyal.conf` lists every loyal-main unit of the migration's units.tsv; the database moves by pgmove. `switch/s4-loyal.sh` holds what the conf calls and the S4 gate.

- `preflight` is read-only on all three boxes. It runs `s4-services-preflight.sh`, `remote.sh run preflight loyal`, `cutover.sh loyal preflight` and its own checks: the 28 rows against the conf and their states on loyal-main, the running and boot sets on both old boxes, the PgBouncer path (config, certificate, every app credential's port, user and SCRAM secret against the role, the password against the secret, computed without a login), the tunnel credential, the public realtime route and the deploy route. One line per check as ok, BLOCKER or PARKED, then `<n> ok, <m> blocker, <k> parked`; exit 0 only with 0 blockers. It needs `PGMOVE_SRC_ADMIN` and `PGMOVE_AGENT`.
- `handover` is `CUT_HANDOVER`. freeze runs it once the old units are proven stopped and before the fence: `loyal-clickstack-copy.sh final`, `loyal-receivers-repin.sh state` (it must print the final-copy line), then the sealed tunnel credential decrypted to `/etc/cloudflared/loyal-workers.json` on loyal-main.
- `handback` is `CUT_HANDBACK`. abort and rollback run it once loyal-main is stopped: the placed credential goes.
- `dbcheck` is `CUT_DBCHECK`. switch runs it after PgBouncer starts and before any app, unfreeze before the engines, a refused rollback before the restart: every app credential at 127.0.0.1 logs in, at least one through 6432.

The cutover.sh pieces it uses are general. `CUT_FIRST` units run only on `CUT_NEW` and start first; the source keeps its own (Loyal's PgBouncer and Squads firewall front the source through the reverse window). A `CUT_SRCSTOP` unit also listed in `CUT_SERVICES` or `CUT_TIMERS` moves: it stops on the source in freeze, starts on `CUT_NEW` with its list, and abort or rollback start it on the source again if it ran there before freeze. Hooks run from `deploy/aws/db` and are killed after `CUT_HOOK_SECONDS` (1200).

## host-diff.sh

`deploy/aws/host-diff.sh <old-alias> <new-alias>` is read-only on both hosts (one `bash -s` over ssh each, nothing written). It prints only differing lines and ends `<n> difference(s)`: package versions with the architecture suffix ignored, `/etc/systemd/system` unit files by name and sha256, and the names (never contents) of what sits under the `/etc` trees the services read, drop-ins included. The default scopes are in the script header and can be replaced with `HOST_DIFF_PKGS`, `HOST_DIFF_UNITS`, `HOST_DIFF_TREES`, `HOST_DIFF_EXCLUDE` and `HOST_DIFF_SSH_OPTS`.

## Loyal services on loyal-main (S4 service side)

None of these starts or enables a Loyal unit; every one reruns to a no-op.

- `loyal-images.sh build|prove` builds the arm64 ClickStack and relay images on loyal-main from the committed `web/observability` tree (tag `arm64-<tree hash>`, so a rerun builds nothing), records their ids in `/opt/loyal/ops/arm64-images`, and pins them in the two unit files. `loyal-main-config.sh stage` reads that file and rewrites the amd64 ids it copies from loyal-workers, so a restage keeps the pins. `prove` starts each image with no network and no published port, waits for its health check and removes the container. Docker is started for the run and stopped after.
- `loyal-clickstack-copy.sh pass|delta|final` moves `/srv/loyal-clickstack` from loyal-workers through this machine as a tar stream with a manifest comparison. `pass` runs with the old service up (ClickHouse merges parts away mid-copy; that is tolerated and the next pass lists them), `delta` only lists, `final` refuses while the container runs on the old box, fails on any moved file and then deletes target-only files. The MongoDB (WiredTiger) files under `.clickstack` and ClickHouse's status files are consistent only with the service stopped, so only `final` makes them trustworthy.
- `loyal-receivers-repin.sh apply|state` re-pins the observer (8443) and squads-watch (8444) receivers to 127.0.0.1: three `transport.py`, `firewall.nft`, `private-tls.json`, `policy.json`, `PINS.json`, and a new CA and server certificate for the same public key (the old certificate names only 10.92.1.1). The CA key exists in `/dev/shm` for the run and is shredded. `state` copies `/var/lib/loyal-observer-monitor` and `/var/lib/loyal-squads-watch` from db-primary. The marker `/opt/loyal/ops/receivers-repinned` makes `loyal-main-config.sh stage` leave those nine files alone.
- `loyal-web-assets.sh` fills `/opt/loyal/assets/web` from loyal-workers' retained assets, then folds in the staged release's assets with `retain-web-assets.sh`.
- `s4-services-preflight.sh` is the read-only gate over all of the above plus the unit states, the two unreferenced Neon-URL credentials and `host-diff.sh` against both old boxes with every line classified in `s4-services-hostdiff.tsv`.

## backup.sh

`deploy/aws/backup.sh <box> [--dry-run]` creates the pgBackRest backup storage for a database box from `deploy/aws/boxes/<box>.conf` and converges on rerun. A second run prints `<box>: 0 change(s)`. Boxes: `robinx-main`, `loyal-main`. The conf's `BACKUP_BUCKET` names the bucket and must carry the account id; `ACCOUNT` must be set and match the profile.

Per box it ensures, each found again by name: the bucket in the box's account and region (all four public-access blocks on, SSE-S3 default encryption, versioning on, bucket-owner-enforced ownership, a bucket policy statement `DenyInsecureTransport`, and a lifecycle rule `pgbackrest-housekeeping` that expires noncurrent versions after 30 days and aborts incomplete multipart uploads after 7); the IAM role `<box>-pgbackrest` trusted by `ec2.amazonaws.com` whose one inline policy `pgbackrest-s3` allows `s3:ListBucket` on the bucket and `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` on its objects; the instance profile `<box>-pgbackrest` holding that role; and the profile associated with the instance found by its Name tag. Policy statements and lifecycle rules with other ids are kept. Extra inline or managed policies on the role and a different profile on the instance are a `warning:` line, not a change. It never deletes a bucket, object, role or policy and sets no object lock or retention. Reads need a live session; a dry run without one says so and lists every step as missing.

Buckets: `robinx-pgbackrest-227949368511` (account `main`) and `loyal-pgbackrest-026852303320` (account `loyal`), both us-east-1. A box's role reaches only its own bucket.

### Stanzas on the box

`backup.sh <box> [--dry-run] [--full]` then goes on over ssh (`BOX_SSH`) for a conf that lists `STANZAS` (`name|repo1-path|cluster|port|data dir|socket dir`; robinX production on robinx-main is one more line, shown in the conf). Per box it ensures `/etc/pgbackrest/pgbackrest.conf` (S3 repo, `repo1-s3-key-type=auto` so the instance role signs and no key is on disk, one `[stanza]` block each with its own `repo1-path`), a cipher passphrase generated on the box once into `/etc/pgbackrest/conf.d/cipher.conf` (root:postgres 0640, never printed, never copied), `stanza-create`, `archive_command` of the cluster in its `conf.d/zz-pgmove-archive.conf` applied by `systemctl reload`, `pgbackrest check`, and the backup unit files installed and left disabled. A cluster whose `archive_mode` is off or pending a restart is reported and its `archive_command` left alone; Postgres is never restarted. `--full` starts the first full when no full is listed, as the transient unit `pgbackrest-first-full-<stanza>` (Nice 10, CPUQuota 200%, IOWeight 10, a read cap on the data volume, MemoryMax 4G) through `/usr/local/sbin/pgbackrest-lagged-full`, which saves `pg_stat_subscription` lag to `/var/log/pgbackrest/<stanza>.full.lag-{before,during,after}.txt`. Done is `pgbackrest --stanza=<stanza> info` listing the full with a stop timestamp.

Retention mirrors the old boxes: loyal-main keeps two fulls and seven diffs (loyal-db-primary), robinx-main keeps fulls 14 days and diffs 7 (Hetzner robinX and parsers). The roles hold no `s3:DeleteObjectVersion` and no `s3:AbortMultipartUpload`. pgBackRest expiry deletes with `DeleteObject`, which on this versioned bucket leaves a delete marker and keeps the old version; the lifecycle rule removes noncurrent versions after 30 days, so retention expiry frees no space for 30 days and no role can remove a version sooner. Incomplete multipart uploads are cleaned by the lifecycle rule after 7 days. The lifecycle rule has no expired-delete-marker clause, so lone delete markers may linger (unproven).

Backup units: loyal-main's `loyal-db-backup@.service`, `-full.timer` (Sun 03:00 UTC) and `-diff.timer` (Mon to Sat 03:00 UTC) come from `loyal-main-config.sh` and are only verified disabled. robinx-main's `robinx-parsers-backup@.service` and `robinx-parsers-backup-full.timer` (Sun 09:00 UTC) and `robinx-backup@.service` and `robinx-backup-full.timer` (stanza robinx, Sun 03:00 UTC) are in `boxes/robinx-main.units/`; `cutover.sh robinx` moves all four through CUT_TIMERS and CUT_ONESHOTS. Both timers stay disabled and switch with their box. `loyal-main-config.sh` no longer copies any pgBackRest config; `/etc/pgbackrest/pgbackrest.conf` on loyal-main has one writer, `backup.sh`.

