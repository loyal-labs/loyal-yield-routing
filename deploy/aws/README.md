# deploy/aws

## box.sh

`deploy/aws/box.sh <box> [--dry-run]` builds one AWS box from `deploy/aws/boxes/<box>.conf` and converges on rerun. A second run prints `<box>: 0 change(s)`. Boxes: `robinx-main`, `loyal-main`, `xstock-live`, `weather-live`.

Per box it ensures, each found again by its Name tag: a key pair imported from the conf's public key, a security group (tcp 22 from anywhere, nothing else), the instance (Ubuntu 24.04 for the conf's architecture, AMI read from Canonical's SSM parameter, IMDSv2 required, termination protection on), its gp3 volumes with size, IOPS and throughput from `VOLUMES`, attached and tagged, and one Elastic IP associated. It never deletes or terminates. A volume size or IOPS mismatch is converged online (gp3 grows, never shrinks; the filesystem is grown on the box by hand). An instance type mismatch is a `warning:` line until `BOX_RESIZE=1` is set, then the script stops the instance, modifies the type and starts it again (same architecture only; the Elastic IP and termination protection stay). The security group allows tcp 22 plus the conf's optional `INGRESS` entries (`tcp|udp:port:cidr`), each added when missing; any other inbound rule is a `warning:` line, not a change. Formatting and mounting the data volumes is not its job.

Conf fields, all required, none with a default: `PROFILE` (or `AWS_PROFILE`), `REGION` (or `AWS_REGION`), `INSTANCE_TYPE`, `ARCH` (`arm64` or `x86_64`), `PUBKEY` (or `BOX_PUBKEY`; a path to a `.pub` file, never a private key), `VOLUMES` (`name:GB[:iops[:MB/s]]`, with `root` first). `ACCOUNT` is optional and, when set, must match the profile's account. `PROFILE` is empty for `robinx-main` and `xstock-live` until the vCPU quota read picks `main` or the new account, and `REGION` is empty for `weather-live` until the Polymarket probe picks one.

`--dry-run` prints every aws command it would run and changes nothing. Its reads (`sts get-caller-identity` first) need a live session; without one it says so in one line and lists every step as missing.

## loyal-main-prepare.sh

`ssh loyal-main 'sudo bash -s' < deploy/aws/loyal-main-prepare.sh` makes the blank arm64 box `loyal-main` match the two Hetzner boxes it replaces, and rerun prints `loyal-main: 0 change(s)`. It sets the hostname, formats the 300 GB volume ext4 at `/srv/postgresql` (the db box's mount options, by UUID) and the 150 GB volume at `/srv/loyal-services` with bind mounts to `/opt/loyal` and `/srv/loyal-clickstack`, creates the empty directory tree and modes the services expect, and installs the same package versions as loyal-workers-1 and loyal-db-primary for arm64: PostgreSQL 18.6, timescaledb 2.24.0, pgbouncer, pgbackrest 2.59.1 (PGDG archive, sha256 pinned), docker, prometheus, alertmanager, node exporter, caddy, cloudflared 2026.10.2 and otelcol-contrib 0.162.0 (release assets, sha256 pinned). It sets `create_main_cluster = false` before PostgreSQL installs, as the db box has, so no cluster exists. Every packaged unit is left disabled and stopped. It copies no unit, release, secret or data.

## loyal-main-config.sh

`ARTIFACTS=<gh run download dir> LAUNCHER=<arm64 env-launcher dir> deploy/aws/loyal-main-config.sh stage` runs from the Mac after `loyal-main-prepare.sh` and stages, without enabling or starting anything, what loyal-workers-1 and loyal-db-primary run for Loyal: the arm64 release of `RELEASE_SHA` laid out under `/opt/loyal/releases` as `scripts/deploy-workers.sh` would, with its `40-verified-release.conf` drop-ins; every `loyal-*` unit, drop-in and timer; the cloudflared, otelcol, Prometheus, Alertmanager, Caddy, PgBouncer, pgBackRest and receiver configs; and every sealed credential, decrypted on the old box and re-sealed on loyal-main under the same name and path through one pipe (`deploy/aws/loyal-main-copy.py`), never touching disk or a terminal on the way. Database URLs and listen addresses are rewritten to 127.0.0.1 inside that pipe. The Postgres cluster config is staged beside the cluster under `/opt/loyal-db-staged` for `deploy/aws/db/pgmove.sh`; the tunnel credential is sealed but referenced by no unit. Old boxes are only read. It refuses to run when the running commit and `RELEASE_SHA` differ in service source, ends `loyal-main: <n> change(s)`, and fails if any `loyal-*` unit is enabled. `proof <dir>` writes credential decrypt results and database host:port per credential (no values), `systemd-analyze verify` per unit, enabled and listening state, and each binary's `--help` exit code run without network.

## host-diff.sh

`deploy/aws/host-diff.sh <old-alias> <new-alias>` is read-only on both hosts (one `bash -s` over ssh each, nothing written). It prints only differing lines and ends `<n> difference(s)`: package versions with the architecture suffix ignored, `/etc/systemd/system` unit files by name and sha256, and the names (never contents) of what sits under the `/etc` trees the services read, drop-ins included. The default scopes are in the script header and can be replaced with `HOST_DIFF_PKGS`, `HOST_DIFF_UNITS`, `HOST_DIFF_TREES`, `HOST_DIFF_EXCLUDE` and `HOST_DIFF_SSH_OPTS`.

## backup.sh

`deploy/aws/backup.sh <box> [--dry-run]` creates the pgBackRest backup storage for a database box from `deploy/aws/boxes/<box>.conf` and converges on rerun. A second run prints `<box>: 0 change(s)`. Boxes: `robinx-main`, `loyal-main`. The conf's `BACKUP_BUCKET` names the bucket and must carry the account id; `ACCOUNT` must be set and match the profile.

Per box it ensures, each found again by name: the bucket in the box's account and region (all four public-access blocks on, SSE-S3 default encryption, versioning on, bucket-owner-enforced ownership, a bucket policy statement `DenyInsecureTransport`, and a lifecycle rule `pgbackrest-housekeeping` that expires noncurrent versions after 30 days and aborts incomplete multipart uploads after 7); the IAM role `<box>-pgbackrest` trusted by `ec2.amazonaws.com` whose one inline policy `pgbackrest-s3` allows `s3:ListBucket` on the bucket and `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` on its objects; the instance profile `<box>-pgbackrest` holding that role; and the profile associated with the instance found by its Name tag. Policy statements and lifecycle rules with other ids are kept. Extra inline or managed policies on the role and a different profile on the instance are a `warning:` line, not a change. It never deletes a bucket, object, role or policy and sets no object lock or retention. Reads need a live session; a dry run without one says so and lists every step as missing.

Buckets: `robinx-pgbackrest-227949368511` (account `main`) and `loyal-pgbackrest-026852303320` (account `loyal`), both us-east-1. A box's role reaches only its own bucket.

Repo stanza for a cluster on the box, in the pgBackRest config. The options other than the repo lines are the Hetzner `wen/deploy/postgres/pgbackrest.conf.example` ones. Credentials come from the instance role (`repo1-s3-key-type=auto`), so no key is written anywhere. Give each cluster its own `repo1-path`, and keep the cipher pass in `PGBACKREST_REPO1_CIPHER_PASS` as the Hetzner setup does. No stanza exists yet; `stanza-create` runs once the cluster does.

```ini
[global]
repo1-type=s3
repo1-path=/robinx
repo1-s3-bucket=robinx-pgbackrest-227949368511
repo1-s3-endpoint=s3.us-east-1.amazonaws.com
repo1-s3-region=us-east-1
repo1-s3-key-type=auto
repo1-cipher-type=aes-256-cbc
repo1-retention-full-type=time
repo1-retention-full=14
process-max=2
compress-type=zst
compress-level=3
start-fast=y
archive-async=y
```
