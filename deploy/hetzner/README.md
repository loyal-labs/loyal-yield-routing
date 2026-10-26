# Go workers on the Hetzner host

Each Go worker is one static binary run directly by systemd. There is no
container, controller or env launcher. Secrets are `systemd-creds` files
encrypted with the host key. Each process exposes its health on loopback
`/metrics`. Prometheus evaluates the alert rules, and Alertmanager sends
alerts to Telegram. Logs are slog JSON on stdout, and the collector ships
them from journald to ClickStack.

| Unit | Binary | Families | `/metrics` |
|---|---|---|---|
| `loyal-observer.service` | `loyal-observer` | observer | `127.0.0.1:9101` |
| `loyal-retail.service` | `loyal-engine` (scope `retail`) | autodeposit, fleet, multiply, lookup | `127.0.0.1:9102` |
| `loyal-backyard.service` | `loyal-engine` (scope `backyard`) | backyard | `127.0.0.1:9103` |

## Build

```sh
cd go/workers
make release            # GOARCH=arm64 for an ARM host
sha256sum bin/*
```

`make release` produces `CGO_ENABLED=0` static Linux binaries. The linker
stamps the release identity `sha-<commit>` into
`internal/engine.Release`.

## Install (once per host)

```sh
install -m 0555 bin/loyal-observer bin/loyal-engine /opt/loyal/bin/
install -m 0644 deploy/hetzner/systemd/*.service /etc/systemd/system/
install -d -m 0755 /etc/loyal       # non-secret <unit>.env files, see unit comments
install -d -m 0700 /etc/credstore.encrypted/loyal-retail   # likewise per unit
```

Each `LoadCredentialEncrypted=NAME:/etc/credstore.encrypted/<unit>/NAME`
line in a unit names one secret. Encrypt every secret from stdin, so the
value never appears in argv, a file or shell history. `--name` must equal
`NAME`. Write the value without a trailing newline: the collector reads
its credentials verbatim.

```sh
systemd-creds encrypt --with-key=host --name=RETAIL_DATABASE_URL - \
  /etc/credstore.encrypted/loyal-retail/RETAIL_DATABASE_URL
```

The binaries read secrets only from `$CREDENTIALS_DIRECTORY` and never from
the environment. Every `*_DATABASE_URL` that a family's lock is taken on
(`RETAIL_DATABASE_URL`, `BACKYARD_DATABASE_URL`, `NEON_DATABASE_URL`) must be
the direct Neon endpoint, not the `-pooler` one: a session advisory lock
held through a transaction pooler belongs to whichever client the pooler
hands the session to next. Credentials by unit:

- `loyal-observer`: `HELIUS_API_KEY`, `SOLANA_RPC_URL`, `NEON_DATABASE_URL`, `TIMESCALEDB_URL`
- `loyal-retail`: `RETAIL_DATABASE_URL`, `RETAIL_TIMESCALE_DATABASE_URL`, `RETAIL_SOLANA_RPC_URL`, `RETAIL_JUPITER_API_KEY`, `RETAIL_DELEGATE_KEYPAIR`, `RETAIL_FEE_PAYER_KEYPAIR`. Add `RETAIL_LOOKUP_MANAGER_KEYPAIR` only for active lookup mode. When the unit writes Autodeposit, add `RETAIL_SWEEP_NOTIFY_ENDPOINT` and `RETAIL_SWEEP_NOTIFY_SECRET` (the app's failed-sweep push; the TS `SOLANA_WEEK_NOTIFY_ENDPOINT` and `SOLANA_WEEK_NOTIFY_SECRET`) in a drop-in, both or neither, as the unit's comment shows. A missing `LoadCredentialEncrypted=` file fails the unit (243/CREDENTIALS), so these optional credentials never go in the unit itself.
- `loyal-backyard`: `BACKYARD_DATABASE_URL`, `BACKYARD_SOLANA_RPC_URL`, `BACKYARD_POLICY_KEYPAIR`, `JUPITER_API_KEY`, `BACKYARD_HELIUS_API_KEY`. Its selector mode, canary entry and `BACKYARD_LASERSTREAM_ENDPOINT` go in `/etc/loyal/loyal-backyard.env`.
- Alertmanager: `telegram_bot_token`
- Alertmanager: `heartbeat_url`, the external dead man's switch that the Watchdog alert pings. Not wired yet: until it exists the heartbeat receiver is empty, and nothing pages if the whole host or monitoring stack is down.
- Collector: `CLICKSTACK_OTLP_ENDPOINT`, `CLICKSTACK_INGESTION_KEY`

Then run `systemctl daemon-reload`.

## Upgrade

Backyard, retail and the observer deploy with `bun run deploy` (next
section).

On a restart, signed and sent rows stay on their operation rows, and the
next start lands them (`land()`). Restart one unit at a time. Only one writer
per family runs: with `HoldFamily`, a second process exits and restarts until
the first one stops.

## Release (Backyard, retail and observer)

Each family deploys with one command from a Mac clone, and only with the
owner's go-ahead (implementation lanes never deploy):

```sh
bun run deploy backyard [sha] [--migrations-applied] [--stopped]   # sha defaults to origin/main
bun run deploy retail [sha] [--migrations-applied] [--stopped]
bun run deploy observer [sha] [--migrations-applied] [--stopped]
```

| Family | Unit | Release binary |
|---|---|---|
| `backyard` | `loyal-backyard.service` | `/opt/loyal/releases/backyard/<sha>/loyal-engine` |
| `retail` | `loyal-retail.service` | `/opt/loyal/releases/retail/<sha>/loyal-engine` |
| `observer` | `loyal-observer.service` | `/opt/loyal/releases/observer/<sha>/loyal-observer` |

Each unit runs its release through
`/etc/systemd/system/<unit>.d/40-verified-release.conf`.

`scripts/deploy-workers.sh` reaches the host through the ssh alias in
`LOYAL_WORKERS_SSH` (default `loyal-workers`, logging in as root). It
refuses a commit that is not on origin/main, and it refuses when
`migrations/` differs between the running release and the target: apply
them first, then rerun with `--migrations-applied`. It has two steps, the
engine and then the rules; a target that already runs (and is active) gets
only the rules step, so rerunning a deploy retries a failed rules install.

Engine: it builds `make release` from a `git archive` snapshot of the commit
in a fresh `~/.loyal/deploy-build.*` dir, uploads the binary and
`activate.sh` from that snapshot to `/opt/loyal/releases/<family>/<sha>/`,
and runs `activate.sh` there as a transient unit (`systemd-run --wait
--pipe`), so a dropped ssh session cannot cut the swap short. The host log
is `/var/log/loyal-deploy/<family>-<sha>.log`.

`activate.sh <family> <sha> <sha256> running|stopped` on the host:

1. Takes `/run/lock/loyal-deploy.lock` (waits up to 300 s): one deploy at a
   time on the host, any family, whichever session starts it.
2. Checks the binary's sha256 and reads the running release from
   `/etc/systemd/system/loyal-<family>.service.d/40-verified-release.conf`.
   The same release already active is a no-op. A unit that is not active
   stops the deploy (someone stopped it on purpose) unless the deploy says
   `--stopped`, which in turn requires the unit to be inactive.
3. Backyard only: gates on `loyal-engine backyard in-flight` of the new
   release, run under the unit's own `BACKYARD_DATABASE_URL` credential: the
   count of operations a restart would resume. It waits up to 120 s for 0,
   stops the unit, and checks again; a nonzero count starts the previous
   release. Retail and the observer have no gate: a stop leaves retail's
   signed rows on their operation rows and the next start lands them.
   With `--stopped` the unit is already down, so nothing is stopped.
4. Backs the drop-in up to
   `/root/loyal-<family>-40-verified-release.conf.<prev>.bak`, points it at
   the new release and starts the unit.
5. Backyard counts as up when, within 60 s, the unit is active, `NRestarts`
   is 0 and the journal shows `backyard_worker_start` with lease owner
   `…sha-<sha>`. Retail and the observer count as up when they are active
   with `NRestarts` 0 for 20 s. Otherwise the backup comes back and the
   previous release restarts (Backyard: only with nothing in flight); with
   `--stopped` the unit stays stopped on the previous drop-in.

A failed engine step ends the deploy before the rules step.

Rules: `/etc/prometheus/loyal.rules.yml.sha` records the commit the
installed rules came from. Rules never move backwards: the deploy installs
only when that record is missing or names the target or one of its
ancestors in the local clone; otherwise it says so and leaves the rules. To
install, it uploads the snapshot's `monitoring/` and `install-rules.sh` to a
fresh `/tmp/loyal-monitoring.*` dir and runs `install-rules.sh <sha>
<recorded sha|none> <dir>` there as a transient unit (log
`/var/log/loyal-deploy/rules-<sha>.log`). On the host it:

1. Takes the same deploy lock, and stops if the record no longer says what
   the deploy read (another deploy installed rules meanwhile).
2. If the uploaded `loyal.rules.yml` is byte-identical to the host's, only
   records the sha.
3. Otherwise runs `promtool check rules loyal.rules.yml`, `promtool test
   rules loyal.rules.test.yml` and `python3 verify-alert-messages.py` in the
   upload; any failure leaves the host file as it was.
4. Replaces the file atomically, runs `systemctl reload prometheus` and
   reads Prometheus on `127.0.0.1:9090`: a successful config reload after
   the install, and the rule count `promtool` reported served from that
   file. A failed reload or another count puts the previous file back and
   reloads; success records the sha.

The deploy does not install `alertmanager.yml`: the host's copy carries the
real alerts `chat_id` in place of the repo's placeholder, which the repo does
not capture. Install an `alertmanager.yml` change by hand: replace the
placeholder, `amtool check-config`, then `systemctl reload
prometheus-alertmanager`.

A migration that the running retail and observer releases must not see
deploys in this order. `--stopped` is what lets the deploy start a unit the
operator stopped:

```sh
ssh loyal-workers systemctl stop loyal-retail loyal-observer
bun run yield:migrate        # and timescale:migrate when it changed
bun run deploy retail <sha> --migrations-applied --stopped
bun run deploy observer <sha> --migrations-applied --stopped
```

If a `--stopped` deploy fails, its unit stays stopped on the previous
drop-in; fix the cause and rerun it, or start the previous release by hand
only if it runs against the migrated schema.

Roll back with `bun run deploy <family> <prev-sha>`. It keeps the newer
rules. Only commits that contain `activate.sh` and `install-rules.sh` (and,
for Backyard, the `in-flight` command) can be deployed this way; an older
release goes back by hand through the drop-in.

## Realtime (Rust)

`loyal-realtime.service` runs `loyal-yield-realtime`, the SSE service behind
`realtime.askloyal.com`. Caddy on the host proxies `/events`, `/readyz` and
`/healthz` to `127.0.0.1:10000`. The binary is the one inside a pinned
`realtime-image` tag (`ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>`),
copied out of the image and run directly by systemd. The host does not run
docker or python for it, and it does not pull from GHCR.

The binary reads its configuration from the environment.
`/opt/loyal/bin/loyal-env-launcher`, a pinned static Go binary, sets that
environment from one credential, `realtime-envelope`. The credential is a
JSON envelope:

```json
{"service_id": "loyal-realtime", "deployment_identity": "loyal-realtime",
 "values": {"NEON_DATABASE_URL": "...", "REALTIME_AUTH_SECRET": "...", "...": "..."}}
```

`values` must hold exactly the `--keys` in the unit: `NEON_DATABASE_URL`,
`REALTIME_ALLOWED_ORIGINS`, `REALTIME_ALLOWED_VERCEL_PREVIEW_PROJECT`,
`REALTIME_ALLOWED_VERCEL_PREVIEW_TEAM`, `REALTIME_AUTH_SECRET`,
`REALTIME_CATCH_UP_LIMIT`, `REALTIME_CHANNEL`, `REALTIME_CLIENT_BUFFER`,
`REALTIME_HEARTBEAT_SECONDS`, `REALTIME_MAX_TOKEN_LIFETIME_SECONDS`,
`REALTIME_READY_MAX_LAG`, `REALTIME_RETENTION_BATCH_SIZE`,
`REALTIME_RETENTION_DAYS`, `REALTIME_RETENTION_INTERVAL_SECONDS` and
`RUST_LOG`. A missing or extra key stops the launcher before the binary
starts. The launcher accepts only `/run/loyal-credential.json` as the input
path, with mode 0400, owned by the process's own uid and gid, on a read-only
tmpfs. The unit's comment explains how it delivers the envelope there. The
empty, root-owned `/run/loyal-credential.json` on the host is the mount
point that systemd creates for that bind. It holds no data.

The unit sets `PORT=10000` and `REALTIME_RETENTION_CLEANUP_ENABLED=true`.
Exactly one realtime instance may run with cleanup enabled. Never start a
second copy against the production database to test it. The unit also sets
`REALTIME_BIND_ADDRESS=127.0.0.1`, so only Caddy can reach the binary.
Binaries built before #278 ignore that setting and bind `0.0.0.0:$PORT`. On
those binaries, the cloud firewall must keep port 10000 closed.

### Extract and install a binary

```sh
IMG=ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>
docker pull --platform linux/amd64 "$IMG"
docker image inspect --format '{{index .RepoDigests 0}}' "$IMG"   # record the digest
c=$(docker create --platform linux/amd64 "$IMG")
docker cp "$c":/usr/local/bin/loyal-yield-realtime ./loyal-yield-realtime
docker rm "$c"
sha256sum loyal-yield-realtime                                     # record the sha
```

The image is built on Debian bookworm. The binary links only glibc and uses
rustls with built-in roots. Before you install it, check that
`ldd loyal-yield-realtime` resolves every library on the host. Copy the
binary to the host, compare its sha256 with the recorded one, then:

```sh
install -m 0555 loyal-yield-realtime /opt/loyal/bin/loyal-yield-realtime
install -m 0644 deploy/hetzner/systemd/loyal-realtime.service /etc/systemd/system/
systemctl daemon-reload && systemctl restart loyal-realtime
curl -fsS http://127.0.0.1:10000/readyz
```

When you upgrade, set `LOYAL_IMAGE_VERSION` in the unit to the new tag. A
restart drops open SSE streams, and clients reconnect with their cursor.

The first host binary came from `680613e3` on branch
`codex/hetzner-realtime-20260929`, not from main. Its parent `d1866a93` adds
`REALTIME_RETENTION_CLEANUP_ENABLED`, a 10s statement timeout and a bounded
45s drain on SIGTERM. #278 ports that commit to main and adds
`REALTIME_BIND_ADDRESS`. After #278 merges, re-extract the host binary from
the `realtime-image` tag that main builds, using the steps above. Until then,
the host still binds every interface. Never install a tag from main that
predates #278: it always runs retention and does not bound shutdown.

### Seal the credential

```sh
install -d -m 0700 /etc/credstore.encrypted/loyal-realtime
<envelope JSON on stdout> | systemd-creds encrypt --with-key=host --name=realtime-envelope - \
  /etc/credstore.encrypted/loyal-realtime/realtime-envelope.cred.next
mv /etc/credstore.encrypted/loyal-realtime/realtime-envelope.cred.next \
   /etc/credstore.encrypted/loyal-realtime/realtime-envelope.cred
```

`--name` must equal the credential ID `realtime-envelope`. To change one
value, run `systemd-creds decrypt --name=realtime-envelope <cred> -`, edit
the JSON and run `systemd-creds encrypt` again, all in one pipe. The
plaintext must never reach a file, argv or shell history. Then restart the
unit.

### Cutover from the container runtime (once)

The old runtime is `loyal-realtime-reader.service`. It runs a python
controller that starts the same binary in a docker compose container, and
its credential is sealed under `/etc/credstore.encrypted/loyal/`. To move to
the new unit:

1. Stage the binary and launcher on the host and check their sha256s.
2. Re-seal the old credential as the envelope above, decrypting and
   encrypting in one pipe. Check that the key set has not changed.
3. Install both binaries into `/opt/loyal/bin` and install the unit. Run
   `systemd-analyze verify`.
4. Preflight while the old unit still serves. Start a copy of the unit whose
   `ExecStart` ends in `/usr/bin/true`. It must exit 0. This proves the
   credential delivery and the launcher's checks without opening a second
   database client.
5. Run `systemctl disable --now loyal-realtime-reader`, then
   `systemctl enable --now loyal-realtime`. Wait until
   `http://127.0.0.1:10000/readyz` returns 200, then check
   `https://realtime.askloyal.com/readyz`.

To roll back, run `systemctl disable --now loyal-realtime`, then
`systemctl enable --now loyal-realtime-reader`. The old unit re-validates its
own pins and credential at start. Keep its files until the new unit has run
cleanly for a while.

`LoyalUnitDown` and `LoyalUnitRestartLoop` cover the unit through the
`loyal-.+\.service` filter. Prometheus does not scrape the realtime
`/metrics`, and the collector does not ship its logs, which are plain text
in the journal under `loyal-realtime`.

## Web (loyal-web)

`loyal-web.service` runs `loyal-web`, the Go binary that serves both Loyal
sites: the Vite build of `web/apps/loyal` is embedded in it, and every `/api`
path it does not answer is forwarded to the Next deployment. The Cloudflare
tunnel `loyal-workers` carries `next.askloyal.com` to `127.0.0.1:10080`
("Cloudflare tunnel" below); Caddy is not in its path. The proxy needs no
credentials; the public Earn stats pilot optionally reads an App Neon credential
through a `LoadCredentialEncrypted` drop-in. Prometheus covers it only through
`LoyalUnitDown`/`LoyalUnitRestartLoop`; there is no `/metrics` scrape, and
its slog JSON logs stay in the journal under `loyal-web`.

The build writes `page-paths.json` from the Vite route table and the blog
archive. Unknown page URLs return the existing error screen with HTTP 404 and
`Cache-Control: no-store`; valid pages and public files keep their normal status.
The embedded page manifest is required for readiness, so a build without it
cannot be activated by the deployment script.

| Unit | Binary | Release |
|---|---|---|
| `loyal-web.service` | `loyal-web` | `/opt/loyal/releases/web/<sha>/loyal-web` |

The unit pins what the proxy needs: `LOYAL_WEB_ADDRESS=127.0.0.1:10080` and
`LOYAL_WEB_UPSTREAM=https://app.askloyal.com`. `LOYAL_WEB_APP_HOST` is
deliberately unset (the test host serves both sites from one hostname); a
drop-in sets it if the sites split onto separate hostnames.

### Public Earn stats pilot (opt-in)

Without `WEB_APP_DATABASE_URL`, `/api/earn/stats` still forwards to Next.
Installing that systemd credential switches GET/HEAD on that path to Go.
It must connect to **App Neon**, where `public.loyal_stats_snapshots` lives,
using a role with SELECT on that table (and, for the mobile content reads below,
on `summaries`, `communities`, `trusted_dapps`, `library_sections` and
`library_articles`); it is not the Yield Neon database.
The reader uses a read-only transaction and a transaction-local 10-second
statement timeout. Missing, stale (over five minutes), or unreadable snapshots
return the same 503 error body as Next, with `Cache-Control: no-store`.
All other API paths, including mobile Earn MAX and authentication, still proxy.

After provisioning the encrypted credential at
`/etc/credstore.encrypted/loyal-web-app-db`, install
`/etc/systemd/system/loyal-web.service.d/50-stats-reader.conf`:

```ini
[Service]
LoadCredentialEncrypted=WEB_APP_DATABASE_URL:/etc/credstore.encrypted/loyal-web-app-db
```

`loyal-web-app-db` is the encrypted credential file, not a database name;
encrypt it with the credential name `WEB_APP_DATABASE_URL`. Do not put the
connection string in the unit or command arguments. Deploy a binary containing
the pilot, then run `systemctl daemon-reload && systemctl restart loyal-web`.
Verify the served release as usual, then run:

```sh
curl -si https://next.askloyal.com/api/earn/stats
bun scripts/web/api-diff.ts https://next.askloyal.com https://app.askloyal.com
```

`X-Loyal-API-Source: go` proves the reader is active; absent means it is still
proxying (or an older cached response). The comparison uses unique query strings
and requires this header, matching snapshot timestamps, and equal JSON. A 503
with the header means the reader is active but cannot serve a fresh snapshot.
`/readyz` checks the embedded web build, not database health. To restore proxy
mode, remove this drop-in, daemon-reload, and restart `loyal-web`.
This is a test-host pilot: keep production DNS and `LOYAL_WEB_UPSTREAM` unchanged.

### Mobile content reads (opt-in)

With `WEB_APP_DATABASE_URL` installed, GET/HEAD/OPTIONS on `/api/summaries`,
`/api/mobile/dapps` and `/api/mobile/library` are served by Go; the role needs
SELECT on `summaries`, `communities`, `trusted_dapps`, `library_sections` and
`library_articles`. At startup `loyal-web` checks those grants once; if any is
missing it logs one line naming them and keeps proxying those paths to Next. `/api/mobile/tokens/{mint}` is served by Go
only when the optional systemd credential is installed, using the
credential name `COINGECKO_API_KEY`. Without them each path keeps forwarding to
Next. `X-Loyal-API-Source: go` marks a Go response.

### Session auth (opt-in, owner's go)

`loyal-web` verifies the Next sessions (the web cookie and the mobile
`Authorization: Bearer`) and never signs one. Without `AUTH_JWT_SECRET` and
`AUTH_JWT_RS256_PUBLIC_KEY` it starts as before, the session routes are not
registered, and their paths keep proxying to Next. With either one it also
needs `WEB_APP_DATABASE_URL` (the stats drop-in above), with SELECT on
`app_users` and `app_user_smart_accounts`, and `WEB_SOLANA_RPC_URL`. At startup
`loyal-web` checks them once; if any is missing it logs one line and keeps
proxying the session routes to Next.

| Credential | Required | Use |
|---|---|---|
| `AUTH_JWT_SECRET` | yes, until Next signs RS256 | the Next deployment's value; Go verifies HS256 under `sha256(secret)` |
| `AUTH_JWT_RS256_PUBLIC_KEY` | no | SPKI PEM; verifies RS256 once Next signs with the private key |
| `WEB_SOLANA_RPC_URL` | yes | reads the smart account's settings signers |
| `WEB_APP_DATABASE_URL` | yes | App Neon owner credential (the same value as `ADMIN_APP_DATABASE_URL`, by owner decision 2026-10-23: no role is created and no GRANT runs); SELECT on `app_users` and `app_user_smart_accounts` (sessions), SELECT and INSERT on `earn_max_invite_redemptions` (Earn MAX routes), SELECT, INSERT and UPDATE on `push_tokens` (`/api/push-tokens`) |
| `WEB_YIELD_DATABASE_URL` | no | Yield Neon owner credential (the same value as `NEON_DATABASE_URL`, by owner decision 2026-10-23: no role is created and no GRANT runs); the Earn families below check their grants against it; without it Earn MAX reads show no current APY and no withdrawal health |
| `EARN_MAX_INVITE_CODE_HASHES` | no | the Next deployment's value: comma-separated sha256 hex of the invite codes; unset or malformed, `POST /api/smart-accounts/earn-max/invite` keeps proxying to Next (a malformed entry logs one line) |
| `AUTH_JWT_RS256_PRIVATE_KEY` | no | PKCS#8 PEM, the Next deployment's value; with it `loyal-web` mints the mobile Earn session JWT RS256, without it HS256 under `AUTH_JWT_SECRET` (`POST /api/smart-accounts/earn/session`, `POST /api/auth/privy/complete`) |
| `EARN_YIELD_ROUTER_PUBLIC_KEY` | no | base58 public key of the deployment policy signer; Earn transaction prepares read only this public key, never `DEPLOYMENT_PK` |
| `SMART_ACCOUNT_SPONSOR_PK` | no | the Next deployment's value: base58 64-byte secret key that cosigns sponsored smart account setup (deposit prepare, Earn MAX context) |
| `OBSERVABILITY_OTLP_ENDPOINT` | no | the Next deployment's value; `/api/observability/*` forward to its `/v1/{logs,metrics,traces}` |
| `OBSERVABILITY_INGESTION_API_KEY` | no | the Next deployment's value, sent with each forwarded OTLP request |
| `PRIVY_APP_ID` | no | file `loyal-web-privy-app-id`; the Next deployment's `NEXT_PUBLIC_PRIVY_APP_ID` |
| `PRIVY_APP_SECRET` | no | file `loyal-web-privy-app-secret`; the Next deployment's value; with `PRIVY_APP_ID`, Go serves `POST /api/auth/privy/complete` |
| `REALTIME_AUTH_SECRET` | no | the realtime deployment's value (one `realtime-envelope` input); with it Go serves `POST /api/smart-accounts/yield-optimization/realtime/token`, without it the route answers 503 `realtime_unavailable` |
| `REALTIME_EVENTS_URL` | no | the events URL returned in the token response (`realtime.askloyal.com/events` by default) |
| `TIMESCALEDB_URL` | no | file `loyal-web-timescale-db`; the Next deployment's value (the reserve APY feed); without it earn earnings, earn state and earn deposit prepare log one line and keep proxying to Next |
| `MOBILE_PUSH_API_BASE_URL` | no | file `loyal-web-mobile-push-base-url`; the Next deployment's value, the app backend that sends wallet pushes |
| `MOBILE_PUSH_API_SECRET` | no | file `loyal-web-mobile-push-secret`; the Next deployment's value (it matches the app's `PUSH_DEBUG_SECRET`); without both push values Go still serves the autodeposit family, logs one line that wallet push is off and sends no push |

`AUTH_COOKIE_PARENT_DOMAIN` is plain config, not a credential:
`loyal-web.service` sets `Environment=AUTH_COOKIE_PARENT_DOMAIN=askloyal.com`,
the domain Next's prod session cookie carries. Unset, `loyal-web` logs one line
and `POST /api/auth/privy/complete` keeps proxying to Next.

### Earn family grants

At startup `loyal-web` checks each Earn family's grants once
(`CheckEarnGrants`, `go/workers/internal/webapp/earn_register.go`) and
serves the family only when every grant below is present on both databases;
a family missing any grant logs the grants it lacks and keeps proxying to Next.
This table is the contract that check proves. Yield grants are all in schema
`loyal_yield`.

**Yield Neon (`WEB_YIELD_DATABASE_URL`).** By owner decision 2026-10-23 this is
the Yield Neon owner credential (`neondb_owner`, the `NEON_DATABASE_URL` value),
which already holds every privilege on `loyal_yield.*`; no role is created and
no GRANT runs.

| Family | Yield grants |
|---|---|
| earn state | SELECT on `balance_sweep_executions`, `balance_sweep_lot_claims`, `balance_sweep_policies`, `balance_sweep_scheduled_slots`, `balance_sweep_surplus_lots`, `balance_sweep_targets`, `cross_mint_swap_policies`, `cross_mint_vault_opt_ins`, `earn_deposit_onboarding_attempts`, `rebalance_decisions`, `route_policies`; SELECT, UPDATE on `managed_vaults`, `user_yield_positions`; SELECT, INSERT on `user_yield_position_holding_events`, `vault_position_snapshot_positions`; INSERT, UPDATE on `vault_position_snapshots`; SELECT, INSERT, DELETE on `vault_idle_token_balances_current`, `vault_reserve_positions_current` |
| earn earnings | SELECT, INSERT, UPDATE on `earn_earnings_snapshots`; SELECT on `earn_history_day_revisions`, `managed_vaults`, `rebalance_decisions`, `user_yield_position_deposits`, `user_yield_position_holding_events`, `user_yield_position_withdrawals`, `user_yield_positions`, `vault_idle_token_balances_current`, `vault_position_snapshot_positions`, `vault_position_snapshots` |
| earn transactions | SELECT on `balance_sweep_executions`, `balance_sweep_lot_claims`, `balance_sweep_policies`, `balance_sweep_surplus_lots`, `balance_sweep_targets`, `managed_vaults`, `rebalance_decisions`, `route_policies`, `user_yield_position_deposits`, `user_yield_position_withdrawals`, `vault_position_snapshot_positions`, `vault_position_snapshots`; SELECT, INSERT on `user_yield_position_holding_events`; SELECT, UPDATE on `user_yield_positions` |
| earn public reads | SELECT on `earn_fleet_allocations_hourly`, `earn_reserve_share_prices`, `solana_week_quest_completions` |
| earn deposit | SELECT on `managed_vaults`, `route_policies`; SELECT, INSERT, UPDATE on `solana_week_quest_completions` (the quest table) |
| earn deposit prepare (dprep) | every earn state grant, plus SELECT on `earn_deposit_onboarding_attempts` |
| earn autodeposit | SELECT on `balance_sweep_executions`, `balance_sweep_lot_claim_items`, `managed_vaults`, `realtime_events`, `route_policies`; SELECT, UPDATE on `balance_sweep_lot_claims`; SELECT, INSERT, UPDATE on `balance_sweep_scheduled_slots`, `balance_sweep_surplus_lots`, `balance_sweep_targets`, `balance_sweep_wallet_balances_current`; SELECT, INSERT on `balance_sweep_wallet_balance_events`; USAGE on sequence `balance_sweep_floor_rebaseline_event_id_seq` |
| earn refunds and cross-mint | SELECT on `balance_sweep_lot_claims`, `balance_sweep_policies`, `balance_sweep_surplus_lots`, `balance_sweep_targets`, `cross_mint_swap_policies`, `managed_vaults`, `rebalance_decisions`, `route_policies`, `user_yield_positions`; SELECT, UPDATE on `cross_mint_vault_opt_ins` |
| earn withdraw prepare (wprep) | SELECT on `managed_vaults`, `route_policies` |
| earn withdraw (confirm, cleanup prepare and confirm) | SELECT on `user_yield_position_withdrawals`, `balance_sweep_executions`, `balance_sweep_wallet_balance_events`, `balance_sweep_lot_claim_items`, `managed_vaults`, `route_policies`; SELECT, UPDATE on `balance_sweep_policies`, `balance_sweep_targets`, `balance_sweep_surplus_lots`, `balance_sweep_lot_claims` |
| Earn MAX reads | SELECT on `multiply_route_current_apy_days`, `multiply_route_states` |

Observability ingest and earn session check no yield grants.

**App Neon (`WEB_APP_DATABASE_URL`).** By owner decision 2026-10-23 this is
the App Neon owner credential (`neondb_owner` on `neondb`, the same value as
`ADMIN_APP_DATABASE_URL`), which already holds every grant below; no role is
created and no GRANT runs.

| Group | App grants |
|---|---|
| `AuthGrants` (sessions; earn public reads, refunds, wprep) | SELECT on `app_users`, `app_user_smart_accounts` |
| `EarnMaxGrants` (Earn MAX routes) | SELECT, INSERT on `earn_max_invite_redemptions` |
| `PushGrants` (`/api/push-tokens`) | SELECT, INSERT, UPDATE on `push_tokens` |
| privy `appGrants` (privy complete, earn deposit, dprep) | SELECT, INSERT, UPDATE on `app_users`, `app_user_smart_accounts`, `app_smart_account_signers`; INSERT, UPDATE on `app_user_wallets` |

With sessions verified, Go serves `GET /api/smart-accounts/earn-max/summary`,
`GET /api/smart-accounts/earn-max/activity` and `GET`/`POST
/api/smart-accounts/earn-max/invite` when the Earn MAX grants are present, and
`POST /api/push-tokens` when the push grants are present; it checks each group
once at startup, logs the grants it lacks and keeps proxying that group to Next.
`LOYAL_WEB_SOLANA_CLUSTER` (plain environment, `mainnet` or `devnet`, default
`mainnet`) gates the Earn MAX reads: anything but `mainnet` answers 409
`unsupported_cluster` after the invite check; an invalid value logs one line and
keeps proxying every Earn MAX path. A malformed or unreadable
`WEB_YIELD_DATABASE_URL` does the same; an unset one serves without APY.
An unreadable `COINGECKO_API_KEY` or `WEB_APP_DATABASE_URL` likewise logs one line and keeps proxying; none of these stops `loyal-web`. Install `EARN_MAX_INVITE_CODE_HASHES` like the
credentials below, into `/etc/credstore.encrypted/loyal-web-earn-max-invites`,
with a matching `LoadCredentialEncrypted=` line in the drop-in, and
`WEB_YIELD_DATABASE_URL` into `/etc/credstore.encrypted/loyal-web-yield-db`.

Encrypt each value from stdin, never from an argument, and never through
`echo "$VALUE" |`: a value on a command line lands in shell history and `ps`.
Either read it straight from 1Password (fill the `<placeholders>`):

```sh
op read 'op://<vault>/<item>/<AUTH_JWT_SECRET field>' | systemd-creds encrypt --with-key=host --name=AUTH_JWT_SECRET - \
  /etc/credstore.encrypted/loyal-web-auth-jwt-secret
op read 'op://<vault>/<item>/<WEB_SOLANA_RPC_URL field>' | systemd-creds encrypt --with-key=host --name=WEB_SOLANA_RPC_URL - \
  /etc/credstore.encrypted/loyal-web-solana-rpc
```

or paste at the prompt (the terminal echoes it, so do it unwatched; it never
reaches history), then press Enter and Ctrl-D:

```sh
systemd-creds encrypt --with-key=host --name=AUTH_JWT_SECRET - \
  /etc/credstore.encrypted/loyal-web-auth-jwt-secret
systemd-creds encrypt --with-key=host --name=WEB_SOLANA_RPC_URL - \
  /etc/credstore.encrypted/loyal-web-solana-rpc
```

Then install `/etc/systemd/system/loyal-web.service.d/60-session-auth.conf`
(a `LoadCredentialEncrypted=` line for a file that is not on the host stops
the unit, so these lines live in a drop-in, not in `loyal-web.service`):

```ini
[Service]
LoadCredentialEncrypted=AUTH_JWT_SECRET:/etc/credstore.encrypted/loyal-web-auth-jwt-secret
LoadCredentialEncrypted=WEB_SOLANA_RPC_URL:/etc/credstore.encrypted/loyal-web-solana-rpc
```

For RS256, encrypt the PEM the same way with
`--name=AUTH_JWT_RS256_PUBLIC_KEY` into
`/etc/credstore.encrypted/loyal-web-auth-rs256-public-key` and add
`LoadCredentialEncrypted=AUTH_JWT_RS256_PUBLIC_KEY:/etc/credstore.encrypted/loyal-web-auth-rs256-public-key`.
Run `systemctl daemon-reload && systemctl restart loyal-web`. To go back to
proxying, remove the drop-in, daemon-reload and restart.

### First install (once)

`activate.sh` reads the running release from the
`40-verified-release.conf` drop-in, and the deploy refuses to run without
one, so the first release is installed by hand. Build the linux binary from
a checkout at `<sha>`: the bundle inlines `NEXT_PUBLIC_*` from the build
environment (as Next does), so run it under
`op run --env-file=.env.1password --`, which carries the `NEXT_PUBLIC_*` set
Vercel production inlines (`NEXT_PUBLIC_PRIVY_APP_ID` is required;
`NEXT_PUBLIC_APP_ENVIRONMENT` stays unset, as on Vercel, and means prod).
Values are never echoed. The deploy always sets `NEXT_PUBLIC_GIT_COMMIT_HASH=<sha>` and
`NEXT_PUBLIC_GIT_BRANCH=main` itself.

```sh
git worktree add --detach /tmp/loyal-web-install <sha>
cd /tmp/loyal-web-install/web && bun install --frozen-lockfile && cd ..
op run --env-file=.env.1password -- env \
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOFLAGS=-ldflags=-X=main.stamp=<sha> \
  sh scripts/web/build.sh /tmp/loyal-web-bin
grep -qa <sha> /tmp/loyal-web-bin   # the binary must carry the sha
cd - >/dev/null   # leave the worktree before removing it
git worktree remove --force /tmp/loyal-web-install
```

Upload it, install the unit, write the first drop-in by hand, enable:

```sh
ssh loyal-workers 'install -d -m 0755 /opt/loyal/releases/web/<sha>'
scp -q /tmp/loyal-web-bin \
  loyal-workers:/opt/loyal/releases/web/<sha>/loyal-web.tmp
scp -q deploy/hetzner/systemd/loyal-web.service loyal-workers:/etc/systemd/system/
ssh loyal-workers 'chmod 555 /opt/loyal/releases/web/<sha>/loyal-web.tmp &&
  mv /opt/loyal/releases/web/<sha>/loyal-web.tmp /opt/loyal/releases/web/<sha>/loyal-web &&
  install -d -m 0755 /etc/systemd/system/loyal-web.service.d &&
  printf "[Service]\nExecStart=\nExecStart=/opt/loyal/releases/web/<sha>/loyal-web\n" \
    > /etc/systemd/system/loyal-web.service.d/40-verified-release.conf &&
  systemctl daemon-reload && systemctl enable --now loyal-web'
ssh loyal-workers 'curl -fsS -D- http://127.0.0.1:10080/readyz'   # 200, X-Loyal-Release: <sha>
```

### Deploy and roll back

```sh
op run --env-file=.env.1password -- bun run deploy web [<sha>]   # sha defaults to origin/main
```

Same shape as the engine families: it refuses a sha that is not on
origin/main, reads the running release from the drop-in, builds, uploads the
binary and `activate.sh` to `/opt/loyal/releases/web/<sha>/`, and swaps under
the host's deploy lock (`/run/lock/loyal-deploy.lock`) as a transient unit.
The differences:

- It builds in a throwaway `git worktree` of the commit (the Vite build
  needs the whole checkout and a fresh `bun install`, not a `git archive` of
  two dirs), for the host's architecture, and refuses a binary that does not
  carry the sha. `LOYAL_DEPLOY_DRY_RUN=1` does everything up to and
  including the linux build, prints the binary's sha256 and the release
  string in it, and stops before any ssh.
- `--migrations-applied` and `--stopped` are rejected, there is no migrations
  check, and there is no Prometheus rules step: redeploying the sha that
  already runs (and is active) is a no-op.
- `activate.sh` counts web as up when the unit has stayed active with
  `NRestarts` 0 for 20 s **and** `GET http://127.0.0.1:10080/readyz` returns
  200 naming `X-Loyal-Release: <sha>` (30 s grace) — an old release also
  stays up, so the release header is the real gate. Otherwise the previous
  drop-in comes back and the previous release restarts.

Roll back with `bun run deploy web <prev-sha>`. The host log is
`/var/log/loyal-deploy/web-<sha>.log`.

### Cloudflare tunnel

`next.askloyal.com` is served the way robinx.trade is: a proxied Cloudflare
record and a locally managed tunnel, `loyal-workers`, in the Cloudflare
account that holds askloyal.com. Cloudflare terminates TLS, compresses and
caches the immutable `/assets/*`; `cloudflared.service` connects out from
the host and hands requests to loyal-web on loopback. Some networks reach
Hetzner Ashburn at well under 100 KB/s directly; through Cloudflare the same
page loads in about a second.
The Go web router trusts `X-Forwarded-Proto` only from loopback peers. When
Cloudflare reports an HTTP client request, it returns a 308 HTTPS redirect
before serving the app or API, preserving the host, path, query and method.
Direct loopback readiness probes without that header remain HTTP.

| File in the repo | On the host |
|---|---|
| `deploy/hetzner/cloudflared/config.yml` | `/etc/cloudflared/config.yml` |
| `deploy/hetzner/systemd/cloudflared.service` | `/etc/systemd/system/cloudflared.service` |
| (from `cloudflared tunnel create`, never in git) | `/etc/cloudflared/loyal-workers.json`, root 0600 |

`cloudflared` comes from Cloudflare's apt repository
(`deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared noble main`),
as on robinx-prod. After a config change:

```sh
scp -q deploy/hetzner/cloudflared/config.yml loyal-workers:/etc/cloudflared/config.yml.tmp
ssh loyal-workers 'cloudflared tunnel --config /etc/cloudflared/config.yml.tmp ingress validate &&
  mv /etc/cloudflared/config.yml.tmp /etc/cloudflared/config.yml && systemctl restart cloudflared'
curl -fsS -D- https://next.askloyal.com/readyz    # 200, X-Loyal-Release: <sha>
```

Managing the tunnel itself (create, route DNS, delete) needs an origin
certificate for the askloyal.com zone, from `cloudflared tunnel login` with
askloyal.com picked; keep it apart from robinx's `~/.cloudflared/cert.pem`
and pass it with `--origincert`. To recreate: `tunnel delete loyal-workers`,
`tunnel create loyal-workers`, install the new credentials file, update the
id in `config.yml`, then `tunnel route dns --overwrite-dns loyal-workers
next.askloyal.com`.

The owner adds `https://next.askloyal.com` to the Privy app's allowed
origins; until then sign-in on the test host fails.

## Monitoring

All files are in `monitoring/`. Everything listens on loopback.

- `prometheus.yml` → `/etc/prometheus/prometheus.yml`
- `loyal.rules.yml` → `/etc/prometheus/loyal.rules.yml`
- node_exporter flags: `--collector.systemd
  --collector.systemd.unit-include='(loyal|xstock)-.+\.service'
  --collector.systemd.enable-restarts-metrics
  --web.listen-address=127.0.0.1:9100`.
- xstock-bot runs on this host too. Prometheus scrapes `xstock-bot` (9111)
  and `xstock-pools` (9112) as job `xstock`; its rules,
  `/etc/prometheus/xstock.rules.yml`, live in the xstock-bot repo.
  `prometheus.yml` loads them by glob, so it works before they are installed.
- `alertmanager.yml` → `/etc/prometheus/alertmanager.yml`. Replace the
  placeholder `chat_id` with the alerts chat id.
  `alertmanager.service.d/telegram.conf` →
  `/etc/systemd/system/prometheus-alertmanager.service.d/`. It loads the bot
  token as a credential.
- Ubuntu's packages listen on all interfaces and start on install. Write
  `/etc/default/prometheus`, `/etc/default/prometheus-alertmanager` and
  `/etc/default/prometheus-node-exporter` with loopback `ARGS` (Alertmanager
  also `--cluster.listen-address=`) before `apt-get install`, and install
  with `--force-confold` so dpkg keeps them.

The alerts are:

| Alert | Fires when |
|---|---|
| LoyalFamilyProgressStale | No completed work for about 3m (observer), 10m (autodeposit, fleet) or 30m (multiply, lookup, backyard). Restarts do not reset this clock |
| LoyalFamilyFailing | At least 3 failed-attempt samples with one code in 15m, sustained for 5m; then held until the condition stays clear for 30m. Selector evaluations are not distinct transactions |
| LoyalBackyardWithdrawalAttention | Persisted withdrawal attention for a stable family/route, sustained for 1m |
| LoyalBackyardWithdrawalHealthUnavailable | Observation older than 5m or missing health metrics, sustained for 1m |
| LoyalBackyardNAVReportStale | No NAV report landed for 3h on a route whose vault holds value (an empty vault exports no series), or no report time exported at all, sustained for 5m |
| LoyalAutodepositOverdue | A selected Autodeposit claim, or a slot blocked by custody another family holds open on the vault USDC account, is over 1h old |
| LoyalFeePayerLow | A fee payer has been below 0.55 SOL for 5m (warning) |
| LoyalFeePayerExhausted | A fee payer is below 0.05 SOL: Autodeposit starts nothing. Alertmanager inhibits Autodeposit's LoyalFamilyProgressStale on the same unit while it fires, so the one cause pages once |
| LoyalLaneStalled | A lane (lookup planner or writer; fleet planner, position sweep or executor) has had no tick finish without error for 15m. Its clock starts when the lane starts; transient errors that clear do not page. One opportunity or submission failing is recorded on its row and does not fail the executor tick |
| LoyalInflightStuck | Work in flight with no landed or failed outcome for 3m, twice the blockhash expiry |
| LoyalWorkerDown | A Go unit that systemd is running does not serve `/metrics` for 2m |
| LoyalUnitDown | Any `loyal-*` unit is `failed`, or stuck `activating`, for 2m |
| LoyalUnitRestartLoop | Any `loyal-*` unit restarts more than 3 times in 15m |
| LoyalMonitoringDown | node_exporter, Prometheus or Alertmanager is not scrapeable for 2m |
| Watchdog | Always firing. It goes to the heartbeat receiver, and the external switch pages when it stops |

Known Autodeposit gaps:

- The fee payer is read once per pass. A pass that sets up many new vaults
  can spend it below 0.05 SOL partway through; the next pass stops.
- A target blocked at route preflight is released, not deferred with a
  marker, so `LoyalAutodepositOverdue` does not see it. Only the
  `autodeposit_preflight_blocked` code reports it.

A family whose Go process does not report facts yet stays at progress 0 and
pages after its grace. Enable a Go unit only after its families emit facts.

Check the monitoring config with these commands:

```sh
promtool check config prometheus.yml   # with the rule/target paths pointed here
promtool check rules monitoring/loyal.rules.yml
cd monitoring && promtool test rules loyal.rules.test.yml
amtool check-config monitoring/alertmanager.yml
python3 monitoring/verify-alert-messages.py  # requires python3-yaml and amtool
systemd-analyze verify deploy/hetzner/systemd/*.service   # Linux only
```

### Withdrawal attention

`LoyalBackyardWithdrawalAttention` reports persisted, display-only withdrawal
health. It grants no transaction authority. Alerts group by stable family and
route, not changing causes or retry IDs. The 1m debounce absorbs brief state
transitions; an ongoing incident repeats after 4h. Ordinary cooldown waits and
the typed `selector_finish_current_work_first` deferral are not failures.
Unknown selector failures remain covered by `LoyalFamilyFailing`; its existing
threshold math is unchanged.

1. Check the alert's route, the worker release and its logs. Read the persisted
   withdrawal health and its observation time for the canonical vault, program
   and cluster. Do not treat NAV-only activity as funding progress.
2. Confirm the pending withdrawal demand, available funding, in-flight operation
   and confirmed transaction state. Distinguish a covered cooldown from a
   blocker or persistent lack of funding progress.
3. Check the blocker against the worker's safety controls and obtain explicit
   operator approval for any corrective financial action. Never clear latches,
   raise limits or authorize full-debt repayment automatically to clear a page.
4. After remediation, verify a fresh coherent assessment and confirmed funding
   progress. Check on-chain claimability and payment separately. A resolved
   alert does **not** mean the user has been paid.

`loyal_backyard_withdrawal_attention{family="backyard",route="..."}` is 0 or 1.
`loyal_backyard_withdrawal_observed_timestamp_seconds` has the same labels and
advances only after a coherent assessment is durably written. Read errors must
retain the last attention state and must not refresh the observation timestamp.
A restart restores persisted health rather than assuming recovery.

The attention rule retains the last sample for up to 24h across scrape gaps.
The independent health-unavailable page detects stale observations and missing
metric pairs; process/down/stale alerts remain active. Route-level missing-data
coverage remembers routes for 24h. Total metric absence still pages after that
window, but a route that has never emitted metrics cannot be identified without
an expected-route inventory. Missing telemetry is unknown, not healthy. During
an outage an attention alert can expire after 24h; check the health-unavailable
alert before interpreting its resolved notification.

Telegram uses an inline HTML-escaped template with firing/resolved status,
severity, summary, impact, next action and this repository runbook. Recipients
need repository access. It omits internal Prometheus links and arbitrary labels.
Install the updated rules and Alertmanager config together, validate both with
the commands above, then reload the monitoring services through the approved
host deployment process. Confirm both metrics are emitted for the expected
route and inspect the alert states. No host deployment is performed by editing
these files.

## Logs

`collector/otelcol.yaml` → `/etc/otelcol-contrib/loyal.yaml`.
`collector/otelcol-contrib.service.d/loyal.conf` →
`/etc/systemd/system/otelcol-contrib.service.d/`. The collector reads the
three units, plus xstock-bot's `xstock-bot`, `xstock-pools` and
`xstock-rank` (plain text, no severity), from journald and keeps its cursor
across restarts. It sets `service.name` from each unit's syslog identifier
(`loyal-observer`, `loyal-retail`, `loyal-backyard`, ...), lifts the slog
fields into attributes, and exports OTLP/HTTP to ClickStack.

### Backyard alert response

Every checked-in rule has a signal description, impact, action and recovery
check. Telegram shows the rule and available stable code, service, lane or
route. Distinct causes/resources use separate groups to avoid combining
unrelated failures in one long message. Failure detection thresholds and the five-minute firing delay are unchanged.
`LoyalFamilyFailing` now uses a 30-minute recovery hold: bursts within that
window remain one incident instead of resetting the four-hour reminder timer.
A firing incident can therefore include a recovering worker. The hold delays
resolution, not the first page. Missing samples also count as a clear condition,
so operators must check fresh telemetry and the independent down/stale alerts
before declaring recovery. Receivers, four-hour reminders and inhibition rules
are unchanged. Withdrawal-attention, in-flight and service-down rules do not
use this additional delay.

| Alert | First checks | Recovery evidence |
|---|---|---|
| `LoyalFamilyFailing` | Exact code, first event, current release and dependency | Fresh attempts complete; not an expired counter window |
| `LoyalFamilyProgressStale` | Process/metrics, oldest operation, recovery latch, RPC/DB | Actual completions with advancing timestamps |
| `LoyalInflightStuck` | Persisted status, signature, last-valid height and finality | Reconciled terminal outcome; never blind resend |
| `LoyalLaneStalled` | Named lane's oldest work and dependency errors | That lane completes work, not a healthy sibling |
| `LoyalBackyardWithdrawalAttention` | Scoped health, cash/demand, active operation and guard | Blocker removed or funding progresses; payment checked separately |
| `LoyalBackyardWithdrawalHealthUnavailable` | Metric pair, observation age, DB reads/writes | Fresh durable assessment from coherent chain evidence |
| `LoyalBackyardNAVReportStale` | Latest REPORT_NAV operation, recovery latch, held decision | A new REPORT_NAV reconciled and the report time advanced |
| `LoyalWorkerDown` | Process, local metrics listener and first runtime error | Expected release, live metrics and work progress |
| `LoyalUnitDown` | Intended state and startup error; never revive retired units | Intended active service and dependent progress |
| `LoyalUnitRestartLoop` | First exit/OOM/configuration/dependency failure | Restarts stop and useful work resumes |
| `LoyalMonitoringDown` | Named service/listener, reload and notification failures | Scrapes, rule evaluation and delivery work |
| `Watchdog` | External receiver and host/monitoring reachability | Fresh external heartbeats and normal notifications |

Failure guidance distinguishes initializer native funding, SQL/locks, route
ownership, timeouts, recovery latches, debt-clear guards, quote/valuation
inputs, policy and leg-cap guards, transaction uncertainty and selector admission.
The exact diagnostic code remains visible. Unknown causes, including
`selector_evaluate_unavailable` and `worker_fault`, are explicitly unclassified;
operators check the first matching event, release, dependencies and persisted
execution stage instead of assuming funds were not sent.

`selector_finish_current_work_first` is an expected deferral, no longer a
failure metric in the current worker. Its legacy message directs operators to
check the deployed version/window and existing work, not cancel or restart.
Funding warnings require checking the intended payer and fee/rent requirement;
any funding still requires operator approval. Alert text grants no permission
to clear latches, loosen limits or repay the full debt.

Resolved messages have condition-specific checks. They do not assert that the
cause was fixed or a user was paid. Missing series and expired windows can clear
alerts; check freshness/down signals before closing an incident. Shared
Autodeposit and fee-payer alerts also have their own guidance rather than
withdrawal-specific fallback text.

## Admin credstore (loyal-admin)

`loyal-admin.service` reads these encrypted credentials; encrypt each file
under its credential name. The unit loads the three required ones; add the
optional ones in `/etc/systemd/system/loyal-admin.service.d/50-credentials.conf`.
A missing optional credential disables only the pages that need it.

| Credential name | File in `/etc/credstore.encrypted/` | Needed | Grants |
|---|---|---|---|
| `ADMIN_USER` | `loyal-admin-user` | required | the sign-in name |
| `ADMIN_PASSWORD` | `loyal-admin-password` | required | the sign-in password and the session HMAC key |
| `ADMIN_APP_DATABASE_URL` | `loyal-admin-app-db` | required | App Neon, DML on the admin tables |
| `ADMIN_YIELD_DATABASE_URL` | `loyal-admin-yield-db` | optional | Yield Neon, SELECT only |
| `ADMIN_TIMESCALE_DATABASE_URL` | `loyal-admin-timescale-db` | optional | Timescale, SELECT only |
| `R2_ACCESS_KEY_ID` | `loyal-admin-r2-access-key-id` | optional | library cover uploads |
| `R2_SECRET_ACCESS_KEY` | `loyal-admin-r2-secret-access-key` | optional | library cover uploads |
| `CLICKSTACK_READ_KEY` | `loyal-admin-clickstack` | optional | the metrics page |

### Admin first install and host (loyal-admin)

The first release goes in by hand exactly as `loyal-web`'s (First install,
above), with `admin` for `web`, `scripts/web/build.sh admin` and port 10081;
after that `bun run deploy admin [<sha>]` deploys and rolls back. The tunnel
serves it at `admin.askloyal.com` and `admin-next.askloyal.com`
(`cloudflared/config.yml`), with the DNS records from `tunnel route dns
loyal-workers <host>` (`--overwrite-dns` replaced the old `admin` CNAME).
Rollback is pointing the `admin` record back at its old target.
`LOYAL_ADMIN_CLICKSTACK_URL` is the ClickStack external API base, which on
the self-hosted ClickStack sits under `/api/api/v2` (the UI proxies `/api`).

### Admin environment (loyal-admin)

The unit sets only `LOYAL_ADMIN_ADDRESS`. Everything below goes in the host
drop-in `/etc/systemd/system/loyal-admin.service.d/60-env.conf` as
`Environment=` lines, never in the repo, because the hosts and keyed URLs are
private. An unset name disables only the page or panel that needs it.

| Name | Needed | What it is |
|---|---|---|
| `LOYAL_ADMIN_R2_ENDPOINT` | optional | https origin of the R2 S3 API, `https://<account>.r2.cloudflarestorage.com`; library cover uploads need it with both R2 credentials, the bucket and the CDN base |
| `LOYAL_ADMIN_R2_BUCKET` | optional | R2 bucket the covers go to |
| `LOYAL_ADMIN_R2_PREFIX` | optional | key prefix inside the bucket, joined ahead of `library/` |
| `LOYAL_ADMIN_CDN_BASE_URL` | optional | https base URL the stored covers are served from; may have a path |
| `LOYAL_ADMIN_SOLANA_RPC_URL` | optional | Solana RPC for the Earn max, autonomous vault and funding pages; may embed a key; unset, Earn max and the autonomous vault use the public mainnet endpoint and the funding page skips its RPC reads |
| `LOYAL_ADMIN_CLICKSTACK_URL` | optional | ClickStack base URL for the metrics dashboard and the journeys events |
| `LOYAL_ADMIN_CLICKSTACK_METRICS_SOURCE_ID` | optional | ClickStack source id for the metrics dashboard; unset uses the built-in id |
| `LOYAL_ADMIN_JOURNEYS_URL` | optional | UI base the journeys page links to; the journeys page is unavailable without it |
| `EARN_SETTINGS_AUTHORITY_PUBLIC_KEY` | optional | public key, comma separated; the Earn settings authority wallet to check |
| `SMART_ACCOUNT_SPONSOR_PUBLIC_KEY` | optional | public key; the smart-account sponsorship wallet |
| `EARN_POLICY_SIGNER_PUBLIC_KEY` | optional | public key; the Earn policy signer (`EARN_YIELD_ROUTER_PUBLIC_KEY` is read when it is unset) |
| `DEPLOYMENT_PUBLIC_KEY` | optional | public key; the gasless deployment payer |

The public-key names have no built-in default: unset, the Earn funding page
shows that role as unconfigured.

## Stats (loyal-stats)

`loyal-stats.service` runs `loyal-stats`, the public performance dashboard:
the Vite build of `web/apps/dashboard` embedded in a Go binary that answers
`/api/earn/stats` (the snapshot reader loyal-web uses) and the partner-facing
`/api/earn/vaults` itself, read-only. It listens on `127.0.0.1:10082`; the
tunnel serves it at `stats-next.askloyal.com` and, once DNS moves,
`stats.askloyal.com`. The snapshot row is written by the telegram app's Vercel
cron, which this move does not touch.

| Credential name | File in `/etc/credstore.encrypted/` | Grants |
|---|---|---|
| `STATS_APP_DATABASE_URL` | `loyal-stats-app-db` | App Neon, reads `public.loyal_stats_snapshots` |
| `STATS_YIELD_DATABASE_URL` | `loyal-stats-yield-db` | Yield Neon, reads `loyal_yield.user_yield_positions` |

Both are required. They hold the same values as admin's App and Yield
credentials, so seal them on the host from those without the value leaving it:

```sh
ssh loyal-workers 'cd /etc/credstore.encrypted &&
  systemd-creds decrypt --name=ADMIN_APP_DATABASE_URL loyal-admin-app-db - |
    systemd-creds encrypt --name=STATS_APP_DATABASE_URL - loyal-stats-app-db &&
  systemd-creds decrypt --name=ADMIN_YIELD_DATABASE_URL loyal-admin-yield-db - |
    systemd-creds encrypt --name=STATS_YIELD_DATABASE_URL - loyal-stats-yield-db'
```

The first release goes in by hand exactly as `loyal-web`'s (First install,
above), with `stats` for `web`, `scripts/web/build.sh stats` and port 10082;
after that `bun run deploy stats [<sha>]` deploys and rolls back. The deploy
needs no `op run`: the dashboard inlines no `NEXT_PUBLIC_*`.

`/api/earn/vaults` keeps the Next route's contract (documented in
`web/apps/dashboard/README.md`): a value is fresh for five minutes; after a
failed refresh the previous value is served until it is six minutes old, then
503 `{"error":"vault list unavailable"}` with `Cache-Control: no-store`.
`/readyz` checks the embedded build, not the databases.

### Stats DNS cutover and rollback

Add the test host, then, after parity, move the public name:

```sh
cloudflared tunnel --origincert ~/.cloudflared/askloyal-cert.pem route dns loyal-workers stats-next.askloyal.com
cloudflared tunnel --origincert ~/.cloudflared/askloyal-cert.pem route dns --overwrite-dns loyal-workers stats.askloyal.com
```

Rollback is pointing the `stats` record back at the DNS-only CNAME
`9bcddf8368ddf2b0.vercel-dns-016.com` (TTL 300); the Vercel project keeps
serving until it is retired.
