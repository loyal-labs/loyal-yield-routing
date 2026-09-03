# Loyal Kamino fleet planner

Phase 1 targets the standalone market-state-to-planner hop for one fixed,
same-mint USDC reserve pair and one vault cohort. Kamino has at most one reserve
for a mint in each lending market, so a real same-mint route normally moves
between two markets. This implementation does **not yet** replace the Rust
revalidator, executor, confirmer, or route reconciler and is not cutover-ready.

The worker:

1. reads both pinned KLend reserves in one confirmed `getMultipleAccounts` RPC;
2. obtains Kamino's current slot duration (unless explicitly configured),
   decodes the frozen 8,624-byte reserve layout, and computes APY in Go;
3. atomically advances a complete, slot-fenced in-memory snapshot;
4. hydrates only durable vault/policy/inflight state from Neon;
5. runs a pure, capacity- and cost-aware decision;
6. in `publish` mode, writes an immutable optimizer epoch and an existing
   `rebalance_opportunities` `revalidate` row before notifying downstream work.

Timescale is not on this decision path. PostgreSQL remains authoritative for
position recovery, per-vault publication serialization, economic idempotency,
capacity reservations, executable handoff, downstream leases, signed wires,
and audit. A lost wakeup is safe because downstream workers scan the durable
queue.

Any eventual cutover is operationally singleton for its migrated scope. The
production Rust planner currently owns thousands of other same-mint vaults, so
it cannot be stopped globally for this one-vault slice; an explicit
migrated-vault exclusion or a complete fleet migration is required first. Do
not use rolling or overlapping ownership for the same vault.

## Required configuration

- `NEON_DATABASE_URL`
- `SOLANA_RPC_URL`
- `KAMINO_FLEET_VAULT_ID`
- `KAMINO_FLEET_SOURCE_RESERVE`: JSON `{ "address", "market", "mint" }`
- `KAMINO_FLEET_TARGET_RESERVE`: JSON with the same shape

Optional:

- `KAMINO_FLEET_MODE=shadow|publish` (default `shadow`)
- `KAMINO_FLEET_CLUSTER` (default `mainnet-beta`)
- `KAMINO_FLEET_POLL_INTERVAL` (default `1s`)
- `KAMINO_API_BASE` (default `https://api.kamino.finance`)
- `KAMINO_FLEET_SLOT_DURATION` (explicit override; otherwise fetched from
  Kamino's `/slots/duration` endpoint)

Phase 1 rejects non-USDC, cross-mint, stale-position, incomplete
amount-evidence, active-work, cooldown, insufficient-capacity, and uneconomic
inputs. `publish` must be enabled explicitly after shadow parity review.

## Verification

From the repository root:

```sh
scripts/verify-kamino-fleet-planner-e2e.sh
```

The verifier uses a mock confirmed Solana RPC and disposable PostgreSQL. It
explicitly does not build or invoke the replaced Rust reserve monitor or
opportunity planner. It proves coherent Go ingestion/planning through the real
durable W3 queue contract, economic idempotency across slot-only churn,
active-work exclusion, and queue-based restart recovery without a new
migration. It does not yet prove Rust-compatible optimizer-epoch JSON,
route/requirements fingerprints, ALT/packet/simulation revalidation, or the
`ready`/execution lifecycle.
