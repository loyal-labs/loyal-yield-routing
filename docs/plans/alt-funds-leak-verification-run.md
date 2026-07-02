# ALT Funds Leak Verification Run

Recorded: 2026-07-03.

Verifier: `docs/plans/alt-funds-leak-verifier.md`.

## Evidence Captured

- Branch `ask-1652-reclaim-orphan-alt-funds` is pushed to origin at
  `a941dec fix(yield-routing): reject seeded route ALT setup`.
- Implementation commits on this branch:
  - `c4ec592 fix(yield-routing): require durable ALTs for same-mint routes`
  - `a463d2d feat(yield-routing): add orphan ALT cleanup audit command`
  - `9f0c526 ci(worker-images): package ALT cleanup binary`
  - `3668df1 docs(yield-routing): document ALT funds leak verifier`
  - `1de5c6b fix(yield-routing): preflight route ALT coverage`
  - `d64dffd fix(yield-routing): guard live route ALT mutations`
  - `a941dec fix(yield-routing): reject seeded route ALT setup`
- `same-mint-yield-monitor` no longer passes `--provision-lookup-table` to the
  `same-mint-reserve-swap --optimization-cycle --execute` child path.
- Normal same-mint route execution is reuse-only. It loads durable lookup-table
  coverage from the route lookup-table registry plus configured lookup tables,
  then fails before route transaction build/simulation/send if coverage is
  missing.
- `same-mint-reserve-swap` now runs the ALT mutation guard against the actual
  live route `transaction_instructions` before blockhash fetch, transaction
  compile, simulation, or send.
- The route execution preflight also runs the same ALT mutation guard before
  writing a `rebalance_decisions` row, so accidental route create/extend
  instructions fail before DB command writes.
- `--provision-lookup-table` is policy-update/admin-only and is rejected with
  `--optimization-cycle`.
- Route lookup-table setup is isolated behind explicit
  `--provision-route-lookup-table --reconcile-from-chain --source-reserve
  <PUBKEY> --target-reserve <PUBKEY>`.
- `--provision-route-lookup-table` is rejected with `--optimization-cycle`,
  `--provision-lookup-table`, or `--seed-from-user-position`.
- `--provision-route-lookup-table --execute` no longer writes current-position
  state through chain reconciliation or user-position seed reconciliation while
  reporting `writesCurrentPositions: false`.
- A durable Yield Neon migration exists at
  `crates/loyal-yield-orchestrator/migrations/0008_route_lookup_tables.sql`.
  It creates `loyal_yield.route_lookup_tables`, records ALT lifecycle and
  readiness metadata, and adds active durable scope uniqueness.
- `NeonSqlClient` includes durable route lookup-table read/write methods,
  cleanup state updates, and a transaction-scoped Postgres advisory lock for
  `(cluster, scope, authority)` provisioning serialization.
- Explicit provisioning reloads durable table coverage after taking the
  advisory lock, reuses a matching authority-owned table when possible, extends
  only missing addresses, records create/extend signatures, persists
  `readySlot` into `warmup_slot`, and exits without sending the route.
- `route-lookup-table-cleanup` exists as a dry-run-first audit/cleanup binary.
  It supports registry/env/manual protection, audited authorities, Helius
  `getProgramAccountsV2` fallback, signer history scanning, RPC URL redaction,
  active-table deactivation, cooled-down table close, and registry updates.
- `Dockerfile.light-workers` packages `route-lookup-table-cleanup`.
- `package.json` exposes `same-mint:alt-cleanup`.
- Operator docs in `docs/same-mint-reserve-swap.md` and
  `docs/render-worker-images.md` document durable route ALT setup, reuse-only
  normal execution, cleanup, and the updated light-worker command timeout.
- Focused local checks passed during the implementation/review loop:

```sh
NO_DNA=1 cargo fmt --check
NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin same-mint-reserve-swap --bin same-mint-yield-monitor --bin route-lookup-table-cleanup --bin yield-migrations
NO_DNA=1 cargo test -p loyal-yield-orchestrator lookup_table -- --nocapture
NO_DNA=1 cargo test -p loyal-yield-orchestrator current_positions -- --nocapture
NO_DNA=1 cargo test -p loyal-yield-orchestrator alt -- --nocapture
git diff --check
```

- The latest focused test evidence included:
  - `lookup_table`: 10 same-mint lookup-table tests passed.
  - `current_positions`: 2 same-mint current-position write-boundary tests
    passed.
  - `alt`: 10 cleanup/history tests passed.
- Known warning during Rust checks:
  `SelectedVault.swap_lanes` is currently dead code in
  `same-mint-reserve-swap.rs`.
- Read-only production database check from 2026-07-02 showed
  `to_regclass('loyal_yield.route_lookup_tables')` returned `missing`.
- Read-only Render service readback from 2026-07-02 showed production
  `loyal-same-mint-yield-monitor` still running
  `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-02081d6fe666bd76969a56a3a6f678ac7f95b37b`
  with command
  `/usr/local/bin/same-mint-yield-monitor --all-active-vaults --execute --poll-interval-seconds 300 --rebalance-cooldown-seconds 300`.
- Secret-safe env-derived audited authority readback found public keys:
  `oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C`,
  `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5`, and
  `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`.
- Read-only cleanup dry-run with program-account scan and a limit found active
  orphan ALT candidates for `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5`.
  The bounded sample reported `84299520` lamports reclaimable and only
  `deactivate` actions; protected count was `0`.
- Read-only signer-history dry-run found recent `unexpected_create_extend`
  history for audited authorities, including:
  - `DgqyoPkVCrmUEfxSzWbaDidpk5ySHYsNnZ85uBzJBTbH` with create signature
    `2fFG6Q8V8FyY9Lw35kXehbjufBRUy8iRQximAuCLgrCnGjgy6B4334EYtfBXZFWntneifcbhqbbgeGqrTM1BdsyB`
    and extend signature
    `3zdxk8JC9zxCVLs9xD8gNCuMbwLHzjzDJHTzBkx2u8FY8ewHsWX1qLJp8DhiaZUJU4fFY5vTs1ccUSY4adgDUvSH`.
  - `6ceK6ZLbTdAqL7esdYp7RTmMXk8mcdgqp4HbGPxoXKKz` with create signature
    `A8azezC3VJ5MwxbcvhZAdwNe5V8JgqR1PnwxEEVz7doqYcE1DVFYGcWwbq7kj9LSashMLTBJCVeTWGvcNYBmSDj`
    and multiple extend signatures.
  - `989wTqtY98hcA3L7QB2ubASYx2TEJ6Pkn9fDTuuvNZRW` with an older extend
    event for `oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C`.
- No live Render mutation, production migration, ALT deactivate, or ALT close
  has been executed in this verification run.

## Actions Completed

- Added durable route ALT registry migration and store APIs.
- Added registry relation existence handling so pre-migration reads fail closed
  without crashing production readers.
- Added durable lookup-table metadata persistence, including create signature,
  extend signatures, address hash, last extended slot, warmup slot, and cleanup
  lifecycle fields.
- Added transaction-scoped advisory locking for explicit route ALT provisioning.
- Removed automatic route ALT provisioning from the fleet monitor live child
  command.
- Split route ALT setup into explicit `--provision-route-lookup-table` mode.
- Made normal route execution reuse-only and fail closed on missing durable ALT
  coverage.
- Added pre-decision missing-coverage checks so missing coverage does not create
  a failing `rebalance_decisions` row every poll.
- Added hard ALT create/extend mutation guards before live route compile/send.
- Prevented route ALT setup from mutating current-position rows through chain
  reconcile or user-position seed paths while reporting no current-position
  writes.
- Added dry-run-first orphan ALT audit and cleanup tooling.
- Packaged cleanup tooling into the light-worker image.
- Added focused tests for parser guardrails, missing coverage fail-closed
  behavior, ALT mutation rejection, route provisioning/current-position write
  boundaries, readiness slot persistence, cleanup candidate classification,
  signer-history instruction decoding, and RPC URL redaction.
- Documented the verifier, route provisioning workflow, cleanup command, and
  worker image packaging changes.

## Verdict

Live Execution Cannot Create Ephemeral ALTs: STATIC PASS - the production live
child command no longer passes provisioning flags, normal route execution is
reuse-only, and the live route submit path now guards the final instructions
before compile/send. Production deployment readback is still required before
this can be a full PASS.

ALT Coverage Is Durable And Idempotent: STATIC PASS - registry migration,
store read/write APIs, uniqueness, upsert behavior, and advisory lock
serialization are implemented. Production DB migration and registry readbacks
are still required before this can be a full PASS.

Transaction Guard Rejects ALT Create/Extend Outside Provisioning: STATIC PASS -
guards now inspect actual instructions in policy transaction building,
pre-decision route preflight, and live route submit. Focused tests cover
rejection outside explicit provisioning mode.

Missing Coverage Fails Closed Without Spending: PARTIAL PASS - code now checks
coverage before decision write and before route send. The required safe
production dry-run/fleet proof with fixed binary has not been captured.

Focused Local Checks: PASS - focused format, build, lookup-table,
current-position, ALT cleanup/history, and diff checks passed during the review
loop.

Cleanup Tool Is Dry-Run-First And Conservative: STATIC PASS - cleanup is
dry-run by default, protects registry/env/manual tables, audits authority
matches, redacts RPC URLs, classifies active/deactivated tables, and decodes
history. Live cleanup behavior is not yet executed.

On-Chain Reclaim Proof: FAIL - read-only scans found reclaimable orphan ALT
rent, but no approved deactivate/close execution has run and no lamports have
been reclaimed.

Database And Registry Readbacks: FAIL - the latest production readback showed
`loyal_yield.route_lookup_tables` is missing. Migration apply and registry
correctness queries are still required.

Render Production Readback: FAIL - production still read back as the old
`light-workers:sha-02081d6fe666bd76969a56a3a6f678ac7f95b37b` image on
2026-07-02. The fixed image has not been deployed or verified in production.

Post-Deploy Signer History Audit: FAIL - no fixed production deploy timestamp
exists yet, and read-only history still shows unexpected create/extend events
before the fix is deployed.

Overall Verdict: FAIL

## Next Required Moves

1. Build and publish immutable `laserstream-workers` and `light-workers` images
   from commit `a941dec` or a later commit that contains this fix stack.
2. Obtain explicit operator approval before any production mutation.
3. Deploy production `loyal-same-mint-yield-monitor` to the fixed
   `light-workers:sha-<commit>` image and keep the command reuse-only for live
   route execution.
4. Let `/usr/local/bin/yield-migrations --apply` run on the fixed image, or
   otherwise apply migration 8 with explicit approval, then verify
   `loyal_yield.route_lookup_tables` exists in production.
5. Run the verifier's production registry SQL checks:
   - no duplicate active durable table for the same supported scope;
   - no active durable table missing address metadata;
   - no closed/deactivated table selected by active routing config.
6. Run a safe production dry-run/fleet check from the fixed binary and capture
   structured `lookupTableProvisioning` evidence. Missing coverage must fail
   closed without `wouldCreateLookupTable`, `createSignature`, or
   `extendTransactions` in normal route execution.
7. If route coverage is missing for an intended active route, run explicit
   operator-approved `--provision-route-lookup-table` for that exact
   settings/vault/source/target scope. Record the create/extend signatures and
   registry row.
8. Run read-only cleanup/audit for every audited authority, including
   `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5` and env-derived routing
   authorities. Confirm protected durable tables are skipped.
9. With explicit approval, run cleanup execute phase to deactivate active
   orphan ALTs. Record deactivate signatures, table addresses, authority,
   lamports, and slots.
10. After ALT cooldown, run the approved close phase for closeable orphan ALTs.
    Record close signatures, recipient, reclaimed lamports, and wallet/treasury
    SOL balance delta net of fees.
11. Re-run cleanup dry-run after close. Required end state is zero closeable
    orphan candidates and zero deactivation candidates except newly deactivated
    tables still inside cooldown.
12. Record the fixed deploy timestamp and slot/time window. Run post-deploy
    signer-history audit for all audited authorities and verify zero
    `unexpected_create_extend` transactions after deploy.
13. Capture Render readback for service `srv-d8n7gqbbc2fs73emk610`: image,
    command, env-var names, deploy status, and logs. Logs after deploy must show
    fleet polls from the fixed image and no ordinary live route create/extend
    output.
14. Update this verification run with exact command outputs, signatures,
    registry query results, Render readbacks, reclaimed lamports, and final
    verdict.

