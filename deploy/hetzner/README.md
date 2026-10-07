# Go workers on the Hetzner host

Each Go worker is one static binary run directly by systemd. There is no
container, controller or env launcher. Secrets are `systemd-creds` files
encrypted with the host key. Each process exposes its health on loopback
`/metrics`. Prometheus evaluates the alert rules, and Alertmanager sends
alerts to Telegram. Logs are slog JSON on stdout, and the collector ships
them from journald to ClickStack.

| Unit | Binary | Families | `/metrics` | Rust services it replaces |
|---|---|---|---|---|
| `loyal-observer.service` | `loyal-observer` | observer | `127.0.0.1:9101` | kamino-reserve-monitor, balance-sweep-ata-monitor, balance-sweep-ata-projector, squads-policy-monitor |
| `loyal-retail.service` | `loyal-engine` (scope `retail`) | autodeposit, fleet, multiply, lookup | `127.0.0.1:9102` | balance-sweep-autodeposit-trigger, fleet-opportunity-planner, kamino-fleet-planner, fleet-route-revalidator/executor/confirmer/reconciler, route-lookup-table-provisioner, multiply-route-worker |
| `loyal-backyard.service` | `loyal-engine` (scope `backyard`) | backyard | `127.0.0.1:9103` | backyard-rwa-worker |

The Rust fleet-health-projector stays until phase 2.

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

- `loyal-observer`: `HELIUS_API_KEY`, `SOLANA_RPC_URL`, `NEON_DATABASE_URL`, `OBSERVER_APPS_DATABASE_URL`, `TIMESCALEDB_URL`
- `loyal-retail`: `RETAIL_DATABASE_URL`, `RETAIL_TIMESCALE_DATABASE_URL`, `RETAIL_SOLANA_RPC_URL`, `RETAIL_JUPITER_API_KEY`, `RETAIL_DELEGATE_KEYPAIR`, `RETAIL_FEE_PAYER_KEYPAIR`. Add `RETAIL_LOOKUP_MANAGER_KEYPAIR` only for active lookup mode.
- `loyal-backyard`: `BACKYARD_DATABASE_URL`, `BACKYARD_SOLANA_RPC_URL`, `BACKYARD_POLICY_KEYPAIR`, `BACKYARD_TIMESCALE_DATABASE_URL`, `JUPITER_API_KEY`. Its selector mode and canary entry go in `/etc/loyal/loyal-backyard.env`.
- Alertmanager: `telegram_bot_token`
- Alertmanager: `heartbeat_url`, the external dead man's switch that the Watchdog alert pings. Not wired yet: until it exists the heartbeat receiver is empty, and nothing pages if the whole host or monitoring stack is down.
- Collector: `CLICKSTACK_OTLP_ENDPOINT`, `CLICKSTACK_INGESTION_KEY`

Then run `systemctl daemon-reload`. Do not enable a unit until its family
is swapped.

## Upgrade

```sh
install -m 0555 bin/loyal-engine /opt/loyal/bin/loyal-engine
systemctl restart loyal-retail
```

On a restart, signed and sent rows stay on their operation rows, and the
next start lands them (`land()`). Restart one unit at a time.

## Swap a family from Rust to Go

Only one writer per family may run. Go enforces this with `HoldFamily`,
and a second Go process exits and restarts until the first one stops.
Rust is stopped by hand:

1. Close admission in the Rust worker for the family, using its existing
   pause or disable control. No new operation may start.
2. Wait until every in-flight operation is terminal: no Autodeposit
   attempts in `prepared|submitted|unknown|ambiguous`, no fleet decisions
   in `planned|simulating|ready|submitted|confirming`, and so on for the
   family's operation rows.
3. Run `systemctl stop <rust-unit>`. Never use `runtime.py stop`.
4. Run `systemctl enable --now <go-unit>`.
5. Watch `loyal_family_last_progress_timestamp_seconds{family=...}` advance.

To fall back, wait for `loyal_family_inflight{family=...} == 0`, then run
`systemctl stop <go-unit>`, then `systemctl start <rust-unit>`. Go keeps the
row states and legacy lease columns that Rust reads. A stopped Go unit is
inactive, so it does not alert.

During the swap window, a family whose Go and Rust units are both stopped
emits nothing. Rust units are named per host, so add one host-local rule per
family until phase 2 retires Rust:

```yaml
- alert: LoyalFamilyNoWriter
  expr: absent(node_systemd_unit_state{name=~"loyal-retail\\.service|<rust-units>", state="active"} == 1)
  for: 5m
  labels: {severity: page, family: autodeposit}
```

## Monitoring

All files are in `monitoring/`. Everything listens on loopback.

- `prometheus.yml` → `/etc/prometheus/prometheus.yml`
- `loyal.rules.yml` → `/etc/prometheus/loyal.rules.yml`
- node_exporter flags: `--collector.systemd
  --collector.systemd.unit-include='loyal-.+\.service'
  --collector.systemd.enable-restarts-metrics
  --web.listen-address=127.0.0.1:9100`. These cover the Go units and the
  Rust units during the fallback window.
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
| LoyalFamilyFailing | At least 3 terminal failures with one code in 15m, sustained for 5m |
| LoyalInflightStuck | Work in flight with no landed or failed outcome for 3m, twice the blockhash expiry |
| LoyalWorkerDown | A Go unit that systemd is running does not serve `/metrics` for 2m |
| LoyalUnitDown | Any `loyal-*` unit is `failed`, or stuck `activating`, for 2m (in practice Rust units, since Go units restart forever) |
| LoyalUnitRestartLoop | Any `loyal-*` unit restarts more than 3 times in 15m |
| LoyalMonitoringDown | node_exporter, Prometheus or Alertmanager is not scrapeable for 2m |
| Watchdog | Always firing. It goes to the heartbeat receiver, and the external switch pages when it stops |

A family whose Go process does not report facts yet stays at progress 0 and
pages after its grace. Enable a Go unit only after its families emit facts.

Check the monitoring config with these commands:

```sh
promtool check config prometheus.yml   # with the rule/target paths pointed here
promtool check rules monitoring/loyal.rules.yml
cd monitoring && promtool test rules loyal.rules.test.yml
amtool check-config monitoring/alertmanager.yml
systemd-analyze verify deploy/hetzner/systemd/*.service   # Linux only
```

## Logs

`collector/otelcol.yaml` → `/etc/otelcol-contrib/loyal.yaml`.
`collector/otelcol-contrib.service.d/loyal.conf` →
`/etc/systemd/system/otelcol-contrib.service.d/`. The collector reads the
three units from journald and keeps its cursor across restarts. It sets
`service.name` from each unit's syslog identifier (`loyal-observer`,
`loyal-retail`, `loyal-backyard`), lifts the slog fields into attributes,
and exports OTLP/HTTP to ClickStack.
