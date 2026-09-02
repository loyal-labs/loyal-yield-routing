# Loyal Kamino fleet planner

Phase 1 replaces the standalone market-state-to-planner hop for one fixed,
same-market USDC reserve pair and one vault cohort. It does **not** replace the
Rust revalidator, executor, confirmer, or route reconciler.

The worker:

1. reads both pinned KLend reserves in one confirmed `getMultipleAccounts` RPC;
2. decodes the frozen 8,624-byte reserve layout and computes APY in Go;
3. atomically advances a complete, slot-fenced in-memory snapshot;
4. hydrates only durable vault/policy/inflight state from Neon;
5. runs a pure, capacity- and cost-aware decision;
6. in `publish` mode, writes an immutable optimizer epoch and an existing
   `rebalance_opportunities` `revalidate` row before notifying downstream work.

Timescale is not on this decision path. PostgreSQL remains authoritative for
single-writer fencing, position recovery, capacity reservations, executable
handoff, downstream leases, signed wires, and audit. A lost wakeup is safe
because downstream workers scan the durable queue.

## Required configuration

- `NEON_DATABASE_URL`
- `SOLANA_RPC_URL`
- `KAMINO_FLEET_OWNER`
- `KAMINO_FLEET_VAULT_ID`
- `KAMINO_FLEET_SOURCE_RESERVE`: JSON `{ "address", "market", "mint" }`
- `KAMINO_FLEET_TARGET_RESERVE`: JSON with the same shape

Optional:

- `KAMINO_FLEET_MODE=shadow|publish` (default `shadow`)
- `KAMINO_FLEET_CLUSTER` (default `mainnet-beta`)
- `KAMINO_FLEET_COHORT` (default `usdc-fixed-route-v1`)
- `KAMINO_FLEET_POLL_INTERVAL` (default `1s`)
- `KAMINO_FLEET_LEASE_TTL` (default `30s`)
- `KAMINO_FLEET_SLOT_DURATION` (default `400ms`)

Phase 1 rejects non-USDC, cross-market, cross-mint, stale-position, incomplete
amount-evidence, active-work, cooldown, insufficient-capacity, and uneconomic
inputs. `publish` must be enabled explicitly after shadow parity review.

## Verification

From the repository root:

```sh
scripts/verify-kamino-fleet-planner-e2e.sh
```

The verifier uses a mock confirmed Solana RPC and disposable PostgreSQL. It
proves coherent ingestion through the real durable W3 queue contract, economic
idempotency across slot-only churn, restart watermark recovery, and stale-owner
lease fencing.
