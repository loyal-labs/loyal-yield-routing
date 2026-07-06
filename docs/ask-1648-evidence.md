# ASK-1648 Loyal Hub Staging Evidence

Date: 2026-07-06

Scope: read-only evidence for Linear issue `ASK-1648 Deploy Loyal Hub to staging`.
No DB, chain, Render, GitHub, or git mutation was performed. Secret-backed
commands were run only through `op run --env-file=.env.1password -- sh -c '...'`;
no secret values were printed.

## Summary

- Same-mint routing is live in Render staging as a dry-run fleet monitor:
  `loyal-same-mint-yield-monitor-staging` runs
  `/usr/local/bin/same-mint-yield-monitor --all-active-vaults --poll-interval-seconds 300 --rebalance-cooldown-seconds 300`.
- Production same-mint routing is live with `--execute`; staging omits
  `--execute`.
- There is no live Render service named for Loyal Hub orchestration, and the
  staging worker command still targets `same-mint-yield-monitor`.
- Repo code can express Loyal Hub swap lanes and the mainnet E2E script can run
  Hub-backed policy flows, but `same-mint-reserve-swap` currently constructs
  `swap_lanes = Vec::new()` for its same-mint policy path.
- On-chain Mainnet Loyal Hub is initialized and unpaused, but inventory is
  effectively empty in the checked lanes. `admin`, `hub_authorizer`, and
  `inventory_rebalancer` are all `GTpqQf...`; this has not been bound to a
  staging orchestrator signer in the evidence gathered here.
- Neon/Timescale live readback could not be performed from this checkout because
  the local `.env.1password` mount did not expose `NEON_DATABASE_URL` or
  `TIMESCALEDB_URL`.

## Repo Evidence

Command:

```sh
git status --short --branch
```

Result:

```text
## main...origin/main
```

`package.json` exposes the relevant entrypoints:

- `same-mint:swap`: `cargo run -p loyal-yield-orchestrator --bin same-mint-reserve-swap --`
- `same-mint:monitor`: `cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor --`
- `hub:cli`: `cargo run -p loyal-hub-cli --`
- `hub:mainnet-test`: `bun scripts/mainnet-loyal-hub-tests.ts`

`render.yaml` declares production and staging same-mint services. Production
includes `--execute`; staging does not:

- production `loyal-same-mint-yield-monitor`: image
  `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-02081d6fe666bd76969a56a3a6f678ac7f95b37b`,
  command
  `/usr/local/bin/same-mint-yield-monitor --all-active-vaults --execute --poll-interval-seconds 300 --rebalance-cooldown-seconds 300`.
- staging `loyal-same-mint-yield-monitor-staging`: image
  `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-ce5fe2ead0ab55bf3cac4a597cf6aac52232ee3a`,
  command
  `/usr/local/bin/same-mint-yield-monitor --all-active-vaults --poll-interval-seconds 300 --rebalance-cooldown-seconds 300`.

Source pointers:

- `src/features/yield-routing/domain/swap-lanes.ts` supports `same-mint`,
  `loyal-hub`, and `jupiter` lanes. For cross-mint routes it tries a Hub fill
  first, then requires a Jupiter residual quote if Hub does not cover the whole
  amount.
- `crates/loyal-actions/src/actions.rs` has `SwapLane::LoyalHub`, route helpers
  for `same_mint_route`, `jupiter_route`, and `loyal_hub_route`, and builders
  that accept `swap_lanes`.
- `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs` is
  explicitly gated to `SAME_MINT_ROUTE_MODE = "same_mint_kamino"`.
- `crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs` builds
  the same-mint policy universe with only `USDC_MINT` and sets
  `swap_lanes = Vec::new()` in the policy update path. The JSON helper can
  serialize `SwapLane::LoyalHub`, but the same-mint path does not construct one.
- `scripts/mainnet-loyal-hub-tests.ts` defaults to program
  `LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH`, reads Hub state, defaults to
  simulate-only, and requires `CONFIRM_MAINNET=1` for live mainnet execution.

Prior repo evidence in `docs/loyal-hub-mainnet-spending-limit-report.md` says a
Mainnet user Loyal Hub route executed successfully through the Hub swap in
ASK-1556:

- Hub swap signature:
  `SayEEh2dKDGioqyZyyMAoXVeCs8CQPeDfXkF2myQPsjiL59Bmn7RvMPYA1mWa2SbaXJtwdcZXU8an7TXomHovwj`
- Hub input mint: `EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v`
- Route input amount: `999999` raw
- Result: finalized, `err=null`
- Final Hub state restored `admin`, `hub_authorizer`, and
  `inventory_rebalancer` to `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`,
  with lane 0 and lane 1 USDC/PYUSD inventory at `0`/`0`.

## Render Evidence

Command:

```sh
op run --env-file=.env.1password -- sh -c 'render services --output json'
```

Relevant live results:

| Environment | Service | ID | Image | Command |
| --- | --- | --- | --- | --- |
| production | `loyal-same-mint-yield-monitor` | `srv-d8n7gqbbc2fs73emk610` | `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-13acd7e8fedb19e9f9ccf5af18f1c98694053b45` | `/usr/local/bin/same-mint-yield-monitor --all-active-vaults --execute --poll-interval-seconds 300 --rebalance-cooldown-seconds 300` |
| staging | `loyal-same-mint-yield-monitor-staging` | `srv-d8plrj8js32c738s2f80` | `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-3668df10c02c23e3aff5a7be70c34500db6bdd96` | `/usr/local/bin/same-mint-yield-monitor --all-active-vaults --poll-interval-seconds 300 --rebalance-cooldown-seconds 300` |

The service inventory returned these staging worker names:

- `loyal-balance-sweep-ata-monitor-staging`
- `loyal-balance-sweep-ata-projector-staging`
- `loyal-balance-sweep-autodeposit-trigger-staging`
- `loyal-same-mint-yield-monitor-staging`

No service name or command in the live Render inventory indicates a
`loyal-hub` or cross-mint orchestrator worker.

Deploy history command:

```sh
op run --env-file=.env.1password -- sh -c 'render deploys list srv-d8plrj8js32c738s2f80 --output json'
```

Latest staging same-mint deploy:

```json
{
  "id": "dep-d93aan4m0tmc73da78ng",
  "createdAt": "2026-07-02T17:52:28.854727Z",
  "finishedAt": "2026-07-02T17:53:16.462528Z",
  "status": "live",
  "trigger": "api",
  "image": {
    "ref": "ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-3668df10c02c23e3aff5a7be70c34500db6bdd96",
    "registryCredential": "loyal-ghcr",
    "sha": "sha256:fd9d0c4ee92e5a7642bf2860a67a7106a962e33124adbf5622801fcf731294a9"
  }
}
```

Production comparison command:

```sh
op run --env-file=.env.1password -- sh -c 'render deploys list srv-d8n7gqbbc2fs73emk610 --output json'
```

Latest production same-mint deploy:

```json
{
  "id": "dep-d93m441kh4rs73dippvg",
  "createdAt": "2026-07-03T07:17:36.25272Z",
  "finishedAt": "2026-07-03T07:18:29.289245Z",
  "status": "live",
  "trigger": "api",
  "image": {
    "ref": "ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-13acd7e8fedb19e9f9ccf5af18f1c98694053b45",
    "registryCredential": "loyal-ghcr",
    "sha": "sha256:efed1899b3f97b635a973b688a9a9058b15e2de92026ebd396ea9efa969c5744"
  }
}
```

Staging log command:

```sh
op run --env-file=.env.1password -- sh -c 'render logs --resources srv-d8plrj8js32c738s2f80 --limit 30 --output text'
```

Relevant result:

```json
{
  "status": "fleet_poll",
  "execute": false,
  "allActiveVaults": true,
  "candidateCount": 19,
  "discoveredVaultCount": 0,
  "pollIntervalSeconds": 300,
  "rebalanceCooldownSeconds": 300,
  "results": []
}
```

Filtered staging log command:

```sh
op run --env-file=.env.1password -- sh -c 'render logs --resources srv-d8plrj8js32c738s2f80 --limit 20 --text fleet_poll --output text'
```

Result: `fleet_poll` appeared repeatedly from `2026-07-06 14:17:36` through
`2026-07-06 16:25:51`.

## Neon And Timescale Evidence

The checked-in Render config expects staging workers to have `NEON_DATABASE_URL`
and, for same-mint, `TIMESCALEDB_URL`. It also sets staging ATA stream
separation through `BALANCE_SWEEP_ATA_STREAM=staging`.

Secret mount availability command:

```sh
op run --env-file=.env.1password -- sh -c 'env | cut -d= -f1 | sort | grep -E "^(NEON|TIMESCALE|SOLANA|RENDER|YIELD|POLICY|BALANCE|EARN|HELIUS|LASERSTREAM|KAMINO|DEPLOYMENT|SF)_"'
```

Result: empty output, exit code `1`. No matching variable names were exposed by
this local env mount.

Attempted Neon read-only query:

```sh
op run --env-file=.env.1password -- sh -c 'psql "$NEON_DATABASE_URL" -X -v ON_ERROR_STOP=1 -P pager=off -c "SELECT now() AS checked_at, current_database() AS database_name, current_user AS role_name, inet_server_addr() AS server_addr; SELECT to_regclass('\''loyal_yield.route_policies'\'') AS route_policies, to_regclass('\''loyal_yield.managed_vaults'\'') AS managed_vaults, to_regclass('\''loyal_yield.rebalance_decisions'\'') AS rebalance_decisions, to_regclass('\''loyal_yield.route_lookup_tables'\'') AS route_lookup_tables, to_regclass('\''loyal_yield.staging_isolation_probe'\'') AS staging_isolation_probe; SELECT count(*) AS managed_vaults, count(*) FILTER (WHERE active) AS active_managed_vaults FROM loyal_yield.managed_vaults; SELECT count(*) AS route_policies, count(*) FILTER (WHERE active) AS active_route_policies, count(*) FILTER (WHERE route_modes ? '\''same_mint_kamino'\'') AS same_mint_route_policies, count(*) FILTER (WHERE swap_lanes::text ILIKE '\''%loyal_hub%'\'') AS policies_with_loyal_hub_swap_lanes FROM loyal_yield.route_policies; SELECT status, count(*) AS decisions, max(updated_at) AS latest_updated_at FROM loyal_yield.rebalance_decisions GROUP BY status ORDER BY latest_updated_at DESC NULLS LAST;"'
```

Result:

```text
psql: error: connection to server on socket "/tmp/.s.PGSQL.5432" failed: FATAL:  database "zotho" does not exist
```

Interpretation: because `NEON_DATABASE_URL` was unavailable, `psql` fell back to
the local default socket. No Neon or Timescale rows were read in this evidence
run.

## On-Chain Evidence

Command:

```sh
bun run hub:cli -- -u mainnet-beta --program-id LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH --json state
```

Result:

```json
{
  "initialized": true,
  "rpc_url": "https://api.mainnet-beta.solana.com",
  "program_id": "LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH",
  "config_account": "8BkE1rD3Xgb8DHrWffgVB3zfJYgEerxDxbxHSdRin8wb",
  "config_lamports": 5213040,
  "admin": "GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N",
  "hub_authorizer": "GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N",
  "inventory_rebalancer": "GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N",
  "max_fee_bps": 50,
  "paused": false,
  "lane_count": 4,
  "allowed_mints": [
    "CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH",
    "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH",
    "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
    "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
    "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
    "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"
  ]
}
```

Inventory summary from the same command:

- lane `0`: PYUSD ATA exists with amount `0`; USDC ATA exists with amount `0`;
  other allowed mints were missing.
- lane `1`: PYUSD ATA exists with amount `0`; USDC ATA exists with amount `0`;
  other allowed mints were missing.
- lanes `2` and `3`: all allowed-mint inventory accounts were missing.

This supports the issue text claim that Loyal Hub is deployed to Mainnet but not
configured with live inventory/orchestrator binding for staging use.

## Open Blockers

1. Add a cross-mint orchestration strategy to `loyal-yield-orchestrator`.
   Existing same-mint monitor code is hard-gated to `same_mint_kamino` and the
   same-mint policy update path constructs no swap lanes.
2. Decide the staging authority model. Current Hub on-chain
   `hub_authorizer`/`inventory_rebalancer` is `GTpqQf...`; staging needs an
   approved admin instruction sequence if the staging orchestrator key should
   authorize Hub swaps or inventory rebalances.
3. Provision/fund Hub inventory for the intended staging lanes/mints. Current
   readback showed zero USDC/PYUSD in lanes `0` and `1`, and missing inventory
   accounts in lanes `2` and `3`.
4. Verify staging Neon and Timescale readback once a usable secret mount is
   available. Minimum useful read-only checks: active route policies, `swap_lanes`
   containing `loyal_hub`, recent `rebalance_decisions`, route ALT registry
   coverage, and Timescale supported-reserve freshness.
5. Build and deploy a staging worker image that contains the cross-mint Hub
   orchestration path. Live staging is currently on
   `light-workers:sha-3668df10c02c23e3aff5a7be70c34500db6bdd96`; production is
   on `sha-13acd7e8fedb19e9f9ccf5af18f1c98694053b45`.
6. Keep staging dry-run first. Current Render staging proof shows
   `execute: false`; a Hub staging rollout should preserve dry-run/shadow mode
   until DB/on-chain/readback evidence proves the route is isolated and safe.
