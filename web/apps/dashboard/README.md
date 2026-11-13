# Loyal Public Dashboard

Public dashboard app for shareable Loyal performance and reliability metrics.

## Local Development

Run through the same 1Password environment mounts as the admin app:

```sh
bun run dashboard:dev:1pass
```

This expands to:

```sh
op run --env-file=../.env.mainnet.1password --env-file=../../loyal-yield-routing/.env.1password -- sh -c 'bun dev'
```

Keep real values in 1Password. Do not write plaintext secrets into local env files, source code, logs, command arguments, or chat.

## Public API

### GET /api/earn/vaults

Public list of active Loyal Earn vault pubkeys (Squads smart accounts), plus the treasury autonomous vault.

Production: https://stats.askloyal.com/api/earn/vaults

200 response: JSON object with `vaults` (base58 pubkeys), `count`, and `updatedAt`.

Source: active rows in `loyal_yield.user_yield_positions`, plus treasury vault `F7zuL14omw4JJfS1cvsWXVb3wh48dvsonMJgoc9tYu3e` if it is not already in that result.

### GET /api/backyard

Public, read-only data for Backyard's page at https://backyard.askloyal.com: the custodial Loyal Looping vault (`GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1`). Served by `loyal-stats` (`go/workers/internal/stats/backyard.go`) from the Looping route's worker tables, the Voltr API and Kamino reserve history. Each source is `{ ok, value }` on its own, with no error text; one load per minute is shared by every viewer. On the backyard host only `/`, this API and static files are served; the page itself is `src/features/backyard`, also at `/backyard` on the stats host.

Safety limits use the active lane's latest hourly Kamino reserve reading and the worker's shared hard-LTV rule. Missing readings or readings older than two hours show unavailable. Money moves pair the planned input and mint from the same persisted account effect; multi-leg moves and unknown amounts show a dash because net custody bounds do not identify a single input. A failed refresh or a page snapshot older than two minutes displays stale data without a healthy verdict, while the last known balances remain visible.

## Deployment

Deploy a merged commit with `bun run deploy stats <sha>`. This builds the Vite assets into `loyal-stats` and activates the service on `loyal-workers`. The host's Cloudflare tunnel must include the `backyard.askloyal.com` ingress from `deploy/hetzner/cloudflared/config.yml`.
