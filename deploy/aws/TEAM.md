# Access and deploy the Loyal services on AWS

This guide is for the Loyal team. It covers how to sign in to AWS, reach the Loyal box, deploy, and read service state.

## Before you touch a service

`loyal-main` is the AWS box that runs every Loyal service. It replaced the two Hetzner boxes `loyal-workers` and `loyal-db-primary`, which are retired. Leave those two boxes untouched until the owner deletes them: do not start, stop, or change anything on them.

Run this command to see which side is live:

	deploy/aws/team.sh status

The first line reads `loyal-main is LIVE`.

## What you have

| Thing | Value |
|---|---|
| AWS account | `loyal`, id `026852303320`, region `us-east-1` |
| Sign-in user | `main@askloyal.com`, shared by the team |
| Role | `loyal-team-admin`, AdministratorAccess in the `loyal` account only |
| Sign-in page | https://ssoins-7223c74d180d2e32.portal.us-east-1.app.aws |
| Box | `loyal-main`, r7g.2xlarge, arm64, Ubuntu 24.04, public address `100.57.84.125` |
| Backups | S3 bucket `loyal-pgbackrest-026852303320` |
| Audit log | CloudTrail trail `loyal-audit`, bucket `loyal-cloudtrail-026852303320` |

The role does not reach the `main` account, which holds robinX and mnemonic, or the management account. The owner holds the Hetzner project (the retired boxes), the Cloudflare DNS zone, and the 1Password Environments. Ask the owner for the `main@askloyal.com` password.

Every action you take with the role is recorded in `loyal-audit` under the shared user. Say in the team channel when you change something on the box.

## Sign in from your terminal

You need AWS CLI version 2 (`brew install awscli`) and `python3`.

1. Set the region for a new profile named `loyal-team`.

		aws configure set region us-east-1 --profile loyal-team

2. Sign in. A browser opens. Sign in as `main@askloyal.com` and pick the role `loyal-team-admin`.

		aws login --profile loyal-team

3. Check the session and the box.

		deploy/aws/team.sh check

	The check prints four `ok` lines and exits 0. When the session is missing or points at another account, the error names the command to run.

`team.sh` uses the profile `loyal-team`. Set `LOYAL_AWS_PROFILE` to use a profile with another name.

## Reach the box

Run one command as root. This route goes through AWS Session Manager, so it needs no ssh key and no open port.

	deploy/aws/team.sh run 'systemctl is-active postgresql@18-main'

Open a shell as `ubuntu`. The `ubuntu` user has `sudo` without a password.

	deploy/aws/team.sh ssh

Run one command over ssh.

	deploy/aws/team.sh ssh 'sudo journalctl -u loyal-web -n 50 --no-pager'

`team.sh ssh` uses your key at `~/.ssh/id_ed25519.pub`. Set `LOYAL_SSH_PUBKEY` to use another key. On each call the script does three things:

- It reads the box's host key through Session Manager and pins that key in `~/.ssh/known_hosts.loyal-main`.
- It sends your public key to the box with EC2 Instance Connect. The box accepts the key for 60 seconds.
- It connects.

No key of yours stays on the box, so there is no key list to maintain when someone joins or leaves.

Do not stop, terminate, or resize the instance, and do not edit its security group. `deploy/aws/box.sh loyal-main` owns those settings.

## Deploy

A merge to `main` is the deploy. The workflow `.github/workflows/production.yml` builds the changed services, connects to the live box over a WireGuard tunnel, and activates the release as root with a deploy key that only CI holds. You need write access to `loyal-labs/loyal-yield-routing`. You do not need AWS access to deploy.

The workflow finds the box through two settings of the GitHub environment `production`: the variable `LOYAL_WORKERS_HOST` and the secret `LOYAL_WORKERS_KNOWN_HOSTS`. Both name `loyal-main`.

The manual path, `bun run deploy <service> <sha>`, connects to the ssh alias in `LOYAL_WORKERS_SSH`, default `loyal-workers`, as root. That alias is the retired Hetzner box. Do not run the manual path against it. The manual path cannot install on `loyal-main` yet, because the login there is `ubuntu`.

## Read service state on the box

These commands read state and are safe at any time.

| Question | Command on the box |
|---|---|
| Is a service up? | `systemctl status loyal-web` |
| What did a service log? | `sudo journalctl -u loyal-web -n 100 --no-pager` |
| Is the web app healthy? | `curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:10080/readyz` |
| Is Postgres up? | `systemctl is-active postgresql@18-main` |
| Which release is installed? | `ls -l /opt/loyal/releases` |

Health ports on `127.0.0.1`:

| Service | Port and path |
|---|---|
| `loyal-web` | `10080/readyz` |
| `loyal-admin` | `10081/readyz` |
| `loyal-stats` | `10082/readyz` |
| `loyal-telegram` | `10083/healthz` |
| `loyal-realtime` | `10000/readyz` |
| `loyal-observer` | `9101/metrics` |
| `loyal-retail` | `9102/metrics` |
| `loyal-backyard` | `9103/metrics` |

Postgres 18 listens on port 5432 on the box only. PgBouncer listens on port 6432 on the box only, and serves the apps. Open a database shell with `sudo -u postgres psql -d loyal_app`. The databases are `loyal_app`, `loyal_yield_prod`, and `kamino_timescale`.

Service settings are in `/etc/loyal`. Secrets are sealed systemd credentials in `/etc/credstore.encrypted` and are readable only by the unit that loads them.

Caddy on `loyal-main` serves two names:

- `realtime.askloyal.com`, the realtime API.
- `observability.askloyal.com`, the ClickStack dashboards.

## Where the rest is written

- [`README.md`](README.md) in this directory describes `box.sh`, the box configs, and the backup scripts.
- [`../hetzner/README.md`](../hetzner/README.md) describes the units, the tunnel, and the monitoring stack. It still names the Hetzner boxes where it describes their history.
- `deploy/aws/db/cutover.sh` and `deploy/aws/switch/s4-loyal.sh` ran the switch. The owner ran them. Do not run them with a team session.
