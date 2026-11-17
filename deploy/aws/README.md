# deploy/aws

## box.sh

`deploy/aws/box.sh <box> [--dry-run]` builds one AWS box from `deploy/aws/boxes/<box>.conf` and converges on rerun. A second run prints `<box>: 0 change(s)`. Boxes: `robinx-main`, `loyal-main`, `xstock-live`, `weather-live`.

Per box it ensures, each found again by its Name tag: a key pair imported from the conf's public key, a security group (tcp 22 from anywhere, nothing else), the instance (Ubuntu 24.04 for the conf's architecture, AMI read from Canonical's SSM parameter, IMDSv2 required, termination protection on), its gp3 volumes with size, IOPS and throughput from `VOLUMES`, attached and tagged, and one Elastic IP associated. It never deletes, stops or terminates. A type, size or extra-rule mismatch on an existing box is a `warning:` line, not a change. Formatting and mounting the data volumes is not its job.

Conf fields, all required, none with a default: `PROFILE` (or `AWS_PROFILE`), `REGION` (or `AWS_REGION`), `INSTANCE_TYPE`, `ARCH` (`arm64` or `x86_64`), `PUBKEY` (or `BOX_PUBKEY`; a path to a `.pub` file, never a private key), `VOLUMES` (`name:GB[:iops[:MB/s]]`, with `root` first). `ACCOUNT` is optional and, when set, must match the profile's account. `PROFILE` is empty for `robinx-main` and `xstock-live` until the vCPU quota read picks `main` or the new account, and `REGION` is empty for `weather-live` until the Polymarket probe picks one.

`--dry-run` prints every aws command it would run and changes nothing. Its reads (`sts get-caller-identity` first) need a live session; without one it says so in one line and lists every step as missing.

## loyal-main-prepare.sh

`ssh loyal-main 'sudo bash -s' < deploy/aws/loyal-main-prepare.sh` makes the blank arm64 box `loyal-main` match the two Hetzner boxes it replaces, and rerun prints `loyal-main: 0 change(s)`. It sets the hostname, formats the 300 GB volume ext4 at `/srv/postgresql` (the db box's mount options, by UUID) and the 150 GB volume at `/srv/loyal-services` with bind mounts to `/opt/loyal` and `/srv/loyal-clickstack`, creates the empty directory tree and modes the services expect, and installs the same package versions as loyal-workers-1 and loyal-db-primary for arm64: PostgreSQL 18.6, timescaledb 2.24.0, pgbouncer, pgbackrest 2.59.1 (PGDG archive, sha256 pinned), docker, prometheus, alertmanager, node exporter, caddy, cloudflared 2026.10.2 and otelcol-contrib 0.162.0 (release assets, sha256 pinned). It sets `create_main_cluster = false` before PostgreSQL installs, as the db box has, so no cluster exists. Every packaged unit is left disabled and stopped. It copies no unit, release, secret or data.

## host-diff.sh

`deploy/aws/host-diff.sh <old-alias> <new-alias>` is read-only on both hosts (one `bash -s` over ssh each, nothing written). It prints only differing lines and ends `<n> difference(s)`: package versions with the architecture suffix ignored, `/etc/systemd/system` unit files by name and sha256, and the names (never contents) of what sits under the `/etc` trees the services read, drop-ins included. The default scopes are in the script header and can be replaced with `HOST_DIFF_PKGS`, `HOST_DIFF_UNITS`, `HOST_DIFF_TREES`, `HOST_DIFF_EXCLUDE` and `HOST_DIFF_SSH_OPTS`.
