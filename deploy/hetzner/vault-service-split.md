# Separate Loyal Earn MAX and Backyard services

This is the initial cutover from the service that manages both vaults. It
requires an authorized production deployment. Do not run a second copy of the
old binary: it selects both vaults. Normal `bun run deploy` deliberately refuses
the initial cutover when the previous binary lacks `in-flight --vault`.

| Product | Service / deployment family | Required `BACKYARD_VAULT` | Voltr vault | Metrics |
| --- | --- | --- | --- | --- |
| Loyal Earn MAX | `loyal-earn-max` / `earn-max` | `rwa` | `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA` | `127.0.0.1:9105` |
| Backyard test vault | `loyal-backyard` / `backyard` | `looping` | `GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1` | `127.0.0.1:9103` |

`rwa` and `looping` are the existing registry names. Route keys, positions,
targets, release controls and transaction history keep their existing IDs in
the same database. No migration, reset, rebalancing command or transfer is part
of this split. Each process loads its own stream and lifecycle, selector and
withdrawal settlement work. Only Loyal reconciles the official Earn MAX claim
attempts. Missing or unknown vault selection fails before loading credentials.

The writer uses one direct PostgreSQL session, with an exclusive lock per vault
and a shared lock on the legacy family key. Different vaults can run together;
duplicate writers for a vault cannot. An old combined worker's exclusive family
lock cannot coexist with either new writer. Losing the session still kills that
writer rather than silently reconnecting without its locks. Never use a
transaction pooler for these credentials.

## Prepare without changing the running services

1. Use a reviewed, merged commit with passing worker verification. Build and
   stage its stamped `loyal-engine` binary using the build instructions in
   [README.md](README.md). Verify the SHA256 after upload. Place the same verified
   binary at both `/opt/loyal/releases/backyard/<sha>/loyal-engine` and
   `/opt/loyal/releases/earn-max/<sha>/loyal-engine`.
2. Record the old service's release drop-in, unit, environment and credential
   file paths for rollback. Review both routes' current targets, controls and
   pending/signed transactions without altering them.
3. Create `/etc/credstore.encrypted/loyal-earn-max/` with mode 0700. Copy the five
   existing encrypted `BACKYARD_DATABASE_URL`, `BACKYARD_SOLANA_RPC_URL`,
   `BACKYARD_POLICY_KEYPAIR`, `JUPITER_API_KEY`, `BACKYARD_HELIUS_API_KEY` files
   into it, retaining mode 0600. This copies ciphertext on the same host; do not
   decrypt secrets into files, shell arguments or logs. The database value is
   intentionally the same. Separate files allow later RPC/API credential changes.
4. Create `/etc/loyal/loyal-earn-max.env` from the current non-secret worker
   configuration, preserving the current selector and policy flags. Review
   `/etc/loyal/loyal-backyard.env` independently. Both require a LaserStream
   endpoint. Neither environment file may contain `BACKYARD_VAULT`,
   `LOYAL_WORKER_SCOPE`, `LOYAL_WORKER_INSTANCE` or `LOYAL_METRICS_ADDRESS`:
   those identities are supplied by the checked-in units. An `EnvironmentFile`
   would otherwise override systemd's `Environment` values.
5. Prepare the new units from `systemd/loyal-backyard.service` and
   `systemd/loyal-earn-max.service`, each with a `40-verified-release.conf`
   drop-in that clears `ExecStart` then names its staged release path. Do not
   start either new process yet.
6. Prepare monitoring: add port 9105 from `monitoring/prometheus.yml` and the
   new unit from `collector/otelcol.yaml`. Validate configuration before reload.
   Using fresh snapshots and the existing `scripts/clickstack-vault-alerts.py`
   review/apply procedure, apply `monitoring/clickstack-vault-alerts.json` and
   read it back. Its Loyal queries accept both the old and new service names
   while still requiring `vault=rwa`, preserving history across the cutover.
   Backyard's existing `vault=looping` queries keep their service name.

## Stop the combined writer, then start the two selected writers

1. Under the existing service's database credential, run the new binary's
   `backyard in-flight` **without** `--vault`: it must report zero across both
   routes. The command is read-only. If either route has pending work, let the
   old worker finish or follow the recovery runbook; never clear rows to pass.
2. Stop `loyal-backyard.service`, confirm its process exited, then repeat the
   combined in-flight check. If it is nonzero, restore/start the old service to
   recover that work and stop the cutover. No new service should start yet.
3. Install both prepared unit files and release drop-ins, run
   `systemctl daemon-reload`, then enable and start `loyal-earn-max.service`.
   Within 60 seconds require `active`, zero `NRestarts`, and a JSON
   `backyard_worker_start` with `vault=rwa` and the expected `sha-<sha>` owner.
   Check port 9105 and its fresh heartbeat/observation. Stop the rollout at the
   first failed/held money operation, exit or restart; investigate before
   continuing. A worker-start line alone does not prove a trade succeeded.
4. Start and enable `loyal-backyard.service`. Require the same checks with
   `vault=looping` on port 9103. Check that each journal contains starts for its
   own vault and neither contains a start for the other. Independently verify
   existing target/control values and recent operation history for both routes.
5. Reload the validated Prometheus and collector configuration. Verify both
   scrape targets are up, both service names appear in ClickStack, both
   heartbeat tiles return current data, and each NAV monitor retains its history.
   Prove each service is the sole writer for its vault before funding tests.

After this cutover, `bun run deploy earn-max <sha>` and
`bun run deploy backyard <sha>` operate independently. They gate on their own
vault's in-flight rows and verify the corresponding vault in the startup event.
Deploy one service at a time; keep any signed work with the binary that owns it.

## Rollback and boundaries

An ordinary rollback must use a version that already supports explicit vault
selection. Returning to the old combined binary is a coordinated rollback:
drain both routes, stop **both** new services, recheck both routes, restore the
original combined service/drop-in/environment, disable `loyal-earn-max`, then
start only the old combined service. Do not leave a legacy writer waiting on
the family lock alongside the split services: it could acquire it later.

This separates process failures, releases, configuration and database writer
ownership. Both services still share the host, database and currently pinned
delegated signer/Squads controller; the existing custody wallets and policies
are distinct. It is not an on-chain signing-permission or database-role security
boundary. Changing those permissions is a separate migration. This change also
does not lift the current app access gates or add gradual execution/slippage
protection; those policies need their own work and verification.
