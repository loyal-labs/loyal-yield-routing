# Loyal Kamino fleet planner and route revalidator

This Go module owns migrated Kamino same-mint and cross-mint Jupiter
opportunity planning and route revalidation. It keeps PostgreSQL as the authoritative handoff
to the retained Rust executor, confirmer, reconciler, health projector, and ALT
provisioner.

The planner:

1. loads the complete active supported-reserve catalog as one typed immutable
   market epoch, retaining blocked mint coverage without letting one incomplete
   mint stop otherwise healthy mint lanes;
2. reads every reserve in the epoch's complete per-mint routable subset in one
   coherent confirmed RPC observation and validates identities, slot coverage,
   and epoch lifetime. Publish mode also requires exact account hashes. Shadow
   mode logs hash differences as `kamino_fleet_planner_observation_difference`
   and continues planning from the unchanged verified database epoch, like the
   retained Rust planner; it never substitutes the newer RPC economics;
3. loads every active migrated vault in a read-only `REPEATABLE READ`
   transaction, including policy target restrictions, active-work fences, and
   non-released durable capacity reservations counted exactly once;
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
  and compute limits, and verifies simulation against those exact bytes;
- for `cross_mint_jupiter`, re-reads withdraw/swap/deposit policies and custody
  accounts at finalized commitment, validates the narrow ExactIn one/two-hop
  AlphaQ contract, independently verifies Jupiter and Loyal ALTs, and simulates
  the exact atomic withdraw-plus-swap preflight without signing or broadcasting;
  and
- commits `waiting_alt`, `ready`, or fused `leased/execute` atomically after
  rechecking lease, conflict, capacity, opportunity, and optimizer-epoch
  fences. Under the locked frontier, fused execution recomputes projected APY,
  edge, gain, and fee cap, persists distinct observed/projected APYs, reserves
  capacity, and saves exact transaction/simulation evidence before visibility.

In publish mode Go claims both mature `same_mint` and enabled
`cross_mint_jupiter` work. Before evidence loading it sweeps up to 10,000 expired
unstarted opportunities per cycle, preserving live leases, linked decisions,
and unresolved signed submissions. This prevents expired work from blocking
vaults indefinitely even during evidence outages. The retained Rust executor
remains unfiltered as the signer/submission owner.

Timescale monitor identity remains on the publication-evidence path; the worker
never invents a production `state_event_id`. Planner registration supports
source fan-out, while `last_seen_at` advances only after a complete successful
planning pass so persistent Timescale, RPC, or planning failures become stale
health signals in publish mode. The default mode is `shadow`; it does not
register/heartbeat, publish optimizer epochs, refresh capacity, sweep, or claim
work. Shadow rejects enabled revalidation. It is safe to evaluate alongside
Rust using a database role with read-only access. `publish` is an explicit
deployment choice and is valid on mainnet.

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
- `KAMINO_FLEET_REVALIDATOR_ENABLED` (default `false` in shadow, `true` in publish;
  enabling it in shadow is rejected)
- `KAMINO_FLEET_DELEGATED_SIGNER` (public identity; required for revalidation and cross-mint)
- `KAMINO_KLEND_PROXY_PATH` and `KAMINO_KLEND_PROXY_SHA256` (digest-pinned proxy;
  the packaged image supplies these)
- `KAMINO_FLEET_FUSED_EXECUTE` (default `false`)
- `EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER` (default `false`)
- `EARN_ROUTER_CROSS_MINT_MAX_VALUE_LOSS_BPS` (default `50`)
- `EARN_ROUTER_CROSS_MINT_MAX_SLIPPAGE_BPS` (default `50`)
- `JUPITER_SWAP_BUILD_URL` (default `https://api.jup.ag/swap/v2/build`)
- `JUPITER_API_KEY` (optional; sent only as `x-api-key`)
- `KAMINO_API_BASE` (default `https://api.kamino.finance`)
- `KAMINO_FLEET_SLOT_DURATION` (otherwise fetched from Kamino's
  `/slots/duration` endpoint)
- legacy `KAMINO_FLEET_VAULT_ID`, `KAMINO_FLEET_SOURCE_RESERVE`, and
  `KAMINO_FLEET_TARGET_RESERVE` values may remain during deployment, but no
  longer scope planning.

Production revalidation must pin the built proxy path and SHA-256 in its process
configuration. No production credentials are accepted by the parity harness.

## Deployment

1. Run the full local audit below and resolve existing stalled submissions before
   cutover. Local fixtures alone are not evidence of live routing availability.
2. Build with `worker-images`; use the immutable
   `kamino-fleet-planner:sha-<verified-commit>` tag and record its digest.
3. Deploy a **separate parallel shadow service** with `KAMINO_FLEET_MODE=shadow`
   and `KAMINO_FLEET_REVALIDATOR_ENABLED=false`. Keep both Rust services running.
   Use read-only DB credentials/options and no migration pre-deploy command on
   the shadow service. Never repoint the only production planner to shadow.
4. Verify repeated Go-instance `kamino_fleet_planner_cycle` logs with
   `mode=shadow`, fresh reserve evidence, expected vault coverage, and candidate
   economics. The shared cluster heartbeat is not Go-specific proof. Alert on
   missing successful Go cycles and repeated cycle/revalidation failures; verify
   alert delivery and queue-stage age monitoring before cutover.
5. Configure the publisher and revalidator explicitly, including signer identity,
   proxy digest, and the cross-mint flag needed to cover existing work. If
   cross-mint is disabled, retain a Rust owner for that lane; do not strand queued
   cross-mint opportunities. Keep fused execution off for initial rollout unless
   its ownership handoff has been independently verified.
6. Use a coordinated cutover with no observation period in which neither planner
   publishes: start the verified publisher, verify its own successful-cycle logs,
   and drain/stop the replaced Rust planner and revalidator. Any brief overlap
   relies on durable publication/lease fences, not private process state. Keep
   the unfiltered Rust executor, confirmer, reconciler, health projector, ALT
   provisioner, reserve monitor, and autodeposit services running.
7. Verify actual queue advancement, ALT readmission, expiry recovery, and completed
   reconciliation—not just process liveness. On regression, stop Go publication
   and restore Rust ownership, allowing existing leases to drain/expire. Do not
   delete queue rows, release signed ownership blindly, or rebroadcast work.

Never use a mutable image tag or call a shadow heartbeat a production cutover.

## Verification

From the repository root:

```sh
scripts/verify-kamino-fleet-planner-e2e.sh
scripts/verify-kamino-market-epoch-parity.sh
scripts/verify-kamino-route-parity.sh
scripts/verify-kamino-planner-revalidator-parity.sh --audit-current
```

The full local audit creates disposable PostgreSQL, runs all Go tests with the
race detector and no skipped fleet tests, and executes the retained Rust
isolated-database lifecycle verifier. Independent Rust/Go artifacts compare
computed planner opportunities and exact same-mint message/wire bytes only.
Negative runtime checks live in executable tests, not hard-coded artifact
outcomes. No transaction is broadcast and no production credentials are loaded.
See `docs/verifiers/kamino-fleet-parity/README.md` for exact coverage and limits;
live cross-mint/Jupiter execution and production readiness still need rollout
verification.
