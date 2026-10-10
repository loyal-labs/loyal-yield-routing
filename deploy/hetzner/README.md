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
- `loyal-backyard`: `BACKYARD_DATABASE_URL`, `BACKYARD_SOLANA_RPC_URL`, `BACKYARD_POLICY_KEYPAIR`, `BACKYARD_TIMESCALE_DATABASE_URL`, `JUPITER_API_KEY`, `BACKYARD_HELIUS_API_KEY`. Its selector mode, canary entry and `BACKYARD_LASERSTREAM_ENDPOINT` go in `/etc/loyal/loyal-backyard.env`.
- Alertmanager: `telegram_bot_token`
- Alertmanager: `heartbeat_url`, the external dead man's switch that the Watchdog alert pings. Not wired yet: until it exists the heartbeat receiver is empty, and nothing pages if the whole host or monitoring stack is down.
- Collector: `CLICKSTACK_OTLP_ENDPOINT`, `CLICKSTACK_INGESTION_KEY`

Then run `systemctl daemon-reload`.

## Upgrade

```sh
install -m 0555 bin/loyal-engine /opt/loyal/bin/loyal-engine
systemctl restart loyal-retail
```

On a restart, signed and sent rows stay on their operation rows, and the
next start lands them (`land()`). Restart one unit at a time. Only one writer
per family runs: with `HoldFamily`, a second process exits and restarts until
the first one stops.

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

## Monitoring

All files are in `monitoring/`. Everything listens on loopback.

- `prometheus.yml` → `/etc/prometheus/prometheus.yml`
- `loyal.rules.yml` → `/etc/prometheus/loyal.rules.yml`
- node_exporter flags: `--collector.systemd
  --collector.systemd.unit-include='loyal-.+\.service'
  --collector.systemd.enable-restarts-metrics
  --web.listen-address=127.0.0.1:9100`.
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
| LoyalAutodepositOverdue | A selected Autodeposit claim, or a slot blocked by vault idle above `AUTODEPOSIT_IDLE_TOLERANCE_RAW`, is over 1h old |
| LoyalFeePayerLow | A fee payer has been below 0.55 SOL for 5m (warning) |
| LoyalFeePayerExhausted | A fee payer is below 0.05 SOL: Autodeposit starts nothing |
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
three units from journald and keeps its cursor across restarts. It sets
`service.name` from each unit's syslog identifier (`loyal-observer`,
`loyal-retail`, `loyal-backyard`), lifts the slog fields into attributes,
and exports OTLP/HTTP to ClickStack.

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
