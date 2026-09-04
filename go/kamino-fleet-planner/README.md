# Loyal Kamino fleet planner and route revalidator

This Go module owns the migrated Kamino same-mint opportunity-planning and
route-revalidation boundary. It keeps PostgreSQL as the authoritative handoff
to the retained Rust executor, confirmer, reconciler, health projector, and ALT
provisioner.

The planner:

1. loads the complete active supported-reserve catalog as one typed immutable
   market epoch;
2. reads every confirmed reserve account in one coherent RPC observation and
   requires its identities, slots, and hashes to converge with retained monitor
   evidence;
3. loads every active migrated vault in a read-only `REPEATABLE READ`
   transaction, including policy target restrictions and active-work fences;
4. evaluates every same-mint target and, when explicitly enabled, the same six
   stablecoins and immutable Earn/Jupiter policy bindings as the retained Rust
   cross-mint service; deterministically selects a multi-vault wave while
   carrying inflow/outflow so no reserve crosses its 2% capacity frontier;
5. serializes the exact Rust `same_mint` or `cross_mint_jupiter` execution-plan
   contract and Rust length-prefixed opportunity identity; and
6. atomically publishes the immutable epoch and durable `revalidate` work.

The revalidation package:

- claims recoverable work with `SKIP LOCKED` leases and fencing tokens;
- validates fresh vault, reserve, obligation, token-account, farm, policy,
  opportunity, and epoch evidence;
- parses deployed Squads ProgramInteraction policy bytes and checks exact
  protected-program/data constraints and instruction indexes;
- invokes only the digest-pinned `loyal-klend-proxy`, which uses official KLend
  PDA/instruction builders over versioned stdin/stdout JSON and has no RPC,
  database, signer, or broadcast capability;
- wraps protected KLend instructions with Squads, selects active reusable ALTs,
  compiles exact Solana v0 message/unsigned-wire bytes, enforces packet, fee,
  and compute limits, and verifies simulation against those exact bytes; and
- commits `waiting_alt`, `ready`, or fused `leased/execute` atomically after
  rechecking lease, conflict, capacity, opportunity, and optimizer-epoch
  fences. Under the locked frontier, fused execution recomputes projected APY,
  edge, gain, and fee cap, persists distinct observed/projected APYs, reserves
  capacity, and saves exact transaction/simulation evidence before visibility.

Go claims only mature `same_mint` Kamino work. At cutover, the retained Rust
revalidator must use `--route-kind cross_mint_jupiter`; the retained Rust
executor must remain unfiltered and unchanged as the signer/submission owner.

Timescale monitor identity remains on the publication-evidence path; the worker
never invents a production `state_event_id`. The default mode is `shadow`;
`publish` is an explicit deployment choice and is valid on mainnet.

## Required configuration

- `NEON_DATABASE_URL`
- `TIMESCALE_DATABASE_URL` or `TIMESCALEDB_URL`
- `SOLANA_RPC_URL`

Optional:

- `KAMINO_TIMESCALE_SCHEMA` (default `kamino`)
- `KAMINO_FLEET_MODE=shadow|publish` (default `shadow`)
- `KAMINO_FLEET_CLUSTER` (default `mainnet-beta`)
- `KAMINO_FLEET_POLL_INTERVAL` (default `1s`)
- `KAMINO_FLEET_REVALIDATION_CONCURRENCY` (default `16`)
- `KAMINO_FLEET_REVALIDATION_POLL_INTERVAL` (default `250ms`)
- `KAMINO_FLEET_FUSED_EXECUTE` (default `false`)
- `EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER` (default `false`)
- `EARN_ROUTER_CROSS_MINT_MAX_VALUE_LOSS_BPS` (default `50`)
- `KAMINO_API_BASE` (default `https://api.kamino.finance`)
- `KAMINO_FLEET_SLOT_DURATION` (otherwise fetched from Kamino's
  `/slots/duration` endpoint)
- legacy `KAMINO_FLEET_VAULT_ID`, `KAMINO_FLEET_SOURCE_RESERVE`, and
  `KAMINO_FLEET_TARGET_RESERVE` values may remain during deployment, but no
  longer scope planning.

Production revalidation must pin the built proxy path and SHA-256 in its process
configuration. No production credentials are accepted by the parity harness.

## Deployment

The `worker-images` workflow builds the Go planner, `loyal-klend-proxy`, and
`yield-migrations` into `kamino-fleet-planner:sha-<merge-commit>`. After the
main build publishes that immutable tag, update the Render planner service to
that exact digest tag and update the retained Rust revalidator—also using the
new immutable `light-workers` tag—to add `--route-kind cross_mint_jupiter` and
set fused-execute concurrency to zero. Never point Render at a mutable `main`
tag. Keep `loyal-fleet-route-executor` on its unfiltered execute command.

## Verification

From the repository root:

```sh
scripts/verify-kamino-fleet-planner-e2e.sh
scripts/verify-kamino-market-epoch-parity.sh
scripts/verify-kamino-route-parity.sh
scripts/verify-kamino-planner-revalidator-parity.sh --audit-current
```

The complete replacement gate creates disposable PostgreSQL, uses only a
loopback RPC endpoint and frozen clock/input, builds the Rust reference and Go
candidate independently, verifies the proxy digest, compares complete planner,
route, transaction, negative-fence, and retained-lifecycle artifacts, and
rejects thirteen evidence mutations. It performs no transaction broadcast and
loads no production credentials.
