# ALT Funds Leak Verifier

Use this as the verifier-first goal for fixing the Address Lookup Table funds
leak in `loyal-yield-routing`.

This verifier checks the end state, not the implementation steps. Do not mark it
PASS because a flag was removed, a cleanup script exists, or a Render deploy
completed. It passes only when a skeptical runner can prove from repo files,
tests, dry-run output, database readbacks, Render state, and on-chain evidence
that live routing no longer creates disposable ALTs and old reclaimable ALT rent
has been recovered safely.

## Goal

Production routing must stop draining SOL into one-off Address Lookup Tables.

Overall PASS requires all of the following:

- live route execution reuses durable known ALTs or fails closed without
  spending SOL;
- normal production execution cannot submit `create_lookup_table` or
  `extend_lookup_table` instructions accidentally;
- any ALT creation or extension happens only through an explicit provisioning
  mode that is durable, idempotent, scoped, authority-checked, and recorded;
- durable tables are discoverable by the executor from the registry and/or
  configured `YIELD_ROUTE_LOOKUP_TABLES`, not forgotten after one run;
- stale/orphan ALTs created, paid, or authorized by the affected routing keys,
  starting with `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5`, are
  discovered, protected if still durable, then deactivated and closed after the
  ALT cooldown so reclaimable rent returns to the chosen treasury/signer;
- Render production worker state reflects the fixed image and reuse-only live
  command;
- post-deploy signer history shows no new non-provisioning ALT create/extend
  transactions.

Staging, local tests, and dry-run evidence are necessary, but they cannot make
this verifier pass alone. Production readback and on-chain cleanup proof are
required.

## Scope And Safety

Run from repo root:

```sh
cd /Users/zotho/Dev/loyal/alt_fix/loyal-yield-routing
```

Do not run live `--execute`, Render mutation, ALT deactivate, or ALT close
commands unless the operator explicitly approves that phase. Read-only Render,
database, RPC, and dry-run checks are allowed.

Do not print private key material. It is acceptable to print derived public
keys, service ids, transaction signatures, ALT addresses, and lamport balances.

Affected keys to audit:

- always include `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5`;
- derive and include public keys for any configured `YIELD_ROUTER_KEYPAIR`,
  `POLICY_KEYPAIR`, `DEPLOYMENT_PK`, and `SOLANA_TESTING_PK` that are present in
  the environment used by the routing workers;
- do not assume a variable name proves ownership. The public key readback is the
  source of truth.

Durable ALT protection sources:

- the new durable ALT registry in Yield Neon;
- `YIELD_ROUTE_LOOKUP_TABLES`, if still supported;
- any repo-documented manual allowlist for durable tables;
- any not-yet-sent prepared transaction or route bundle that still references a
  table.

## Required Checks

### 1. Live Execution Cannot Create Ephemeral ALTs

PASS only if normal live same-mint route execution cannot create or extend an
ALT as a side effect of missing coverage.

Required static inspection:

```sh
rg -n "--provision-lookup-table|create_lookup_table|extend_lookup_table|AddressLookupTableProgram|createLookupTable|extendLookupTable|lookup table create|lookup table extend" \
  crates/loyal-yield-orchestrator/src/bin \
  scripts \
  docs \
  render.yaml
```

Required result:

- `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs` does not
  pass `--provision-lookup-table` into `same-mint-reserve-swap` for
  `--optimization-cycle --execute`.
- `same-mint-reserve-swap --optimization-cycle --execute` either loads durable
  table coverage and proceeds, or returns a clear non-spending ALT coverage
  failure before building/submitting any route transaction.
- No normal production command in `render.yaml`, `docs/render-worker-images.md`,
  or worker handoff code includes an option that can create/extend a fresh ALT
  during route execution.
- Any remaining `create_lookup_table` or `extend_lookup_table` callsite is
  reachable only from an explicit provisioning/setup command, not from the
  fleet monitor's live execution path.
- CLI help and operator docs no longer instruct operators to combine ordinary
  same-mint live execution with fresh ALT provisioning.

FAIL if a live worker can still handle missing coverage by deriving a table
from the current slot, creating it, extending it, warming it up, using it once,
and forgetting it.

### 2. ALT Coverage Is Durable And Idempotent

PASS only if durable ALT state exists outside process memory and outside a
single transaction log.

Required static evidence:

```sh
rg -n "address_lookup|lookup_table|alt_registry|advisory|FOR UPDATE|ON CONFLICT|unique" \
  crates/loyal-yield-orchestrator/src \
  crates/loyal-yield-orchestrator/migrations \
  scripts \
  docs
```

Required result:

- A Yield Neon migration creates a registry for route lookup tables, or extends
  an existing table with equivalent fields.
- The registry records at least: cluster, scope, table address, authority, payer,
  status, durable/protected marker, address count or address hash, creation
  signature, extend signatures, last extended slot, warmup/usable slot or
  equivalent readiness state, created/updated timestamps, and close/deactivation
  metadata when cleanup begins.
- The registry has a uniqueness or locking story that prevents duplicate active
  durable ALTs for the same scope/authority/address set. Acceptable evidence is
  a DB unique constraint plus `ON CONFLICT`, a transaction-level advisory lock,
  or an equivalent serialized provisioning path.
- Provisioning reuses an existing durable table when the authority matches and
  capacity remains.
- Provisioning extends only missing addresses that are not already present.
- Provisioning creates a new table only when no matching durable table can be
  safely extended.
- Every create/extend transaction is recorded before the next normal route run
  can depend on it.

FAIL if the fix only reuses values from a process-local vector, command stdout,
or an operator note without a durable registry/allowlist that the next worker
run can load.

### 3. Transaction Guard Rejects ALT Create/Extend Outside Provisioning

PASS only if there is a hard guard before transaction submission that rejects
Address Lookup Table Program create/extend instructions outside the explicit
provisioning mode.

Required static evidence:

```sh
rg -n "AddressLookupTable|address_lookup_table|create_lookup_table|extend_lookup_table|reject|guard|provision" \
  crates/loyal-yield-orchestrator/src \
  scripts
```

Required result:

- The guard inspects the actual instructions about to be signed/submitted, not
  only CLI flags.
- The guard permits ALT create/extend only when the command is in the explicit
  provisioning/setup mode.
- The guard applies to live route execution, policy update execution, e2e helper
  execution, and any script wrapper that can submit routing transactions.
- A regression test proves an ALT create/extend instruction in normal live route
  execution is rejected before RPC send.

FAIL if the code relies only on "the monitor no longer passes the flag" while
another executable path can still submit ALT create/extend instructions in
execute mode.

### 4. Missing Coverage Fails Closed Without Spending

PASS only if an active route that lacks durable ALT coverage fails before
spending SOL.

Required dry-run command shape:

```sh
op run --env-file=.env.1password -- sh -c \
  'same-mint-yield-monitor --once --all-active-vaults --poll-interval-seconds 300 --rebalance-cooldown-seconds 300'
```

If the fixed binary is not installed locally, use the exact fixed image binary
in a safe one-shot environment. Do not add `--execute` to this dry-run command.

Required output:

- top-level status is `fleet_poll`;
- `execute` is `false`;
- for any routeable active vault, packet and lookup-table evidence is present;
- if durable lookup-table coverage is complete, the route reports reusable table
  addresses and `missingBeforeProvision` or its replacement is empty;
- if coverage is incomplete, the route reports a clear non-spending status such
  as `alt_coverage_missing`, `lookup_table_coverage_missing`, or equivalent;
- no dry-run output reports `wouldCreateLookupTable: true` for ordinary live
  execution;
- no output includes `createSignature` or non-empty `extendTransactions` outside
  an explicit provisioning run.

FAIL if dry-run hides the issue by selecting no active vaults while production
has active same-mint vaults, or if missing coverage is reported only after a
transaction has been submitted.

### 5. Focused Local Checks

PASS only if the changed surface has focused tests and the Rust binaries still
build.

Required commands:

```sh
NO_DNA=1 cargo fmt --check
```

```sh
NO_DNA=1 cargo check -p loyal-yield-orchestrator \
  --bin same-mint-yield-monitor \
  --bin same-mint-reserve-swap \
  --bin yield-migrations \
  --bin route-lookup-table-cleanup
```

Run the focused ALT tests. These filters may be adjusted only to the exact test
names introduced by the implementation, and the verification record must show
that at least one relevant test ran:

```sh
NO_DNA=1 cargo test -p loyal-yield-orchestrator lookup_table -- --nocapture
NO_DNA=1 cargo test -p loyal-yield-orchestrator alt -- --nocapture
```

Required focused test coverage:

- live route execution rejects ALT create/extend instructions outside
  provisioning mode;
- missing durable coverage fails closed before RPC send;
- provisioning reuses an existing table when authority and capacity allow it;
- provisioning extends only missing addresses and persists the update;
- provisioning is idempotent under repeated or concurrent attempts for the same
  scope;
- cleanup candidate filtering skips durable/configured tables and only targets
  orphan tables whose authority still matches an audited key;
- cleanup refuses to close a table referenced by registry, env config,
  allowlist, or prepared unsent route data.

FAIL if the test filters run zero tests or if coverage is only asserted by
manual review.

### 6. Cleanup Tool Is Dry-Run-First And Conservative

PASS only if a repo-local cleanup command can discover, classify, deactivate,
and close old orphan ALTs without touching durable tables.

Required static evidence:

```sh
rg -n "deactivate_lookup_table|close_lookup_table|AddressLookupTableProgram|lookup table cleanup|orphan.*lookup|reclaimable|SlotHashes|deactivation" \
  crates \
  scripts \
  docs
```

Required cleanup command behavior:

- dry-run is the default;
- candidate discovery includes ALT create transactions paid, authorized, or
  created by the audited keys, starting with
  `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5`;
- each candidate account is fetched and decoded;
- candidates are considered reclaimable only if the account is owned by the
  Address Lookup Table Program and the table authority is still one of the
  audited keys;
- tables in the durable registry, `YIELD_ROUTE_LOOKUP_TABLES`, a manual
  allowlist, or prepared unsent route data are skipped with an explicit reason;
- dry-run output includes table address, authority, status, address count,
  lamports reclaimable, created/extended signatures when known, and one of
  `close`, `deactivate`, `defer`, or `skip` with a reason;
- `--execute` deactivates active orphan tables but does not try to close them
  until their deactivation slot is no longer recent in `SlotHashes`;
- the close phase sends rent to the configured treasury/signer recipient and
  records the close signature and lamports reclaimed;
- fees are never reported as recoverable rent.

FAIL if cleanup can close a table merely because it was created by our payer,
without proving the authority and durable-use protections above.

### 7. On-Chain Reclaim Proof

PASS only after the cleanup has actually reclaimed closeable orphan ALT rent.

Required evidence from the cleanup run:

- the first execute phase reports deactivation signatures for active orphan
  tables, or proves there were no active orphan tables;
- after cooldown, the close phase reports close signatures for every closeable
  orphan table;
- wallet/treasury SOL balance increases by the reclaimed rent amount minus new
  transaction fees;
- every remaining candidate is either:
  - durable/protected and skipped;
  - already closed;
  - still in cooldown with an explicit next-close-after slot/time, in which case
    this section remains FAIL/DEFERRED until the close phase completes.

Required post-cleanup readback:

```sh
# Use the implemented cleanup/audit command in read-only mode.
# It must include the affected key below and any derived routing/sponsor keys.
op run --env-file=.env.1password -- sh -c \
  'route-lookup-table-cleanup --include-env-authorities --authority 62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5 --scan-program-accounts --scan-history --dry-run'
```

Required result:

- zero `close` candidates remain after cooldown;
- zero `deactivate` candidates remain unless they are newly deactivated and
  still waiting out the cooldown;
- skipped tables all cite durable registry/env/allowlist/prepared-transaction
  protection;
- the verification record includes total reclaimable lamports found, total
  lamports reclaimed, and total unreclaimed lamports with reasons.

Overall PASS is impossible while closeable orphan rent remains unreclaimed.

### 8. Database And Registry Readbacks

PASS only if database state proves there is a coherent durable ALT set.

Run read-only SQL through 1Password:

```sh
op run --env-file=.env.1password -- sh -c 'psql "$NEON_DATABASE_URL" -X -v ON_ERROR_STOP=1'
```

Required SQL checks must be adapted to the final registry table/column names,
but they must prove these facts:

```sql
-- No duplicate active durable table for the same cluster/scope/authority.
SELECT cluster, scope, authority, COUNT(*)
FROM loyal_yield.<alt_registry_table>
WHERE durable = TRUE
  AND status IN ('active', 'warming', 'usable')
GROUP BY cluster, scope, authority
HAVING COUNT(*) > 1;
```

Required result: zero rows, unless the implementation explicitly supports
multiple active tables per scope because a single table reached capacity. In
that case the query must group by the implemented capacity bucket and still show
no accidental duplicates.

```sql
-- No durable active table lacks address coverage metadata.
SELECT *
FROM loyal_yield.<alt_registry_table>
WHERE durable = TRUE
  AND status IN ('active', 'warming', 'usable')
  AND (
    table_address IS NULL
    OR authority IS NULL
    OR address_count IS NULL
    OR address_count <= 0
    OR address_count > 256
  );
```

Required result: zero rows.

```sql
-- No closed/deactivated table is still selected by active routing config.
SELECT *
FROM loyal_yield.<alt_registry_table>
WHERE status IN ('deactivated', 'closed')
  AND durable = TRUE
  AND <selected_by_active_route_condition>;
```

Required result: zero rows.

FAIL if registry correctness is asserted only from code review and not from
read-only production database evidence.

### 9. Render Production Readback

PASS only if production Render state is running the fixed image and no production
service is configured to create fresh ALTs during normal execution.

Required commands:

```sh
render services --output json
render deploys list srv-d8n7gqbbc2fs73emk610 --output json
render logs --resource srv-d8n7gqbbc2fs73emk610 --since 60m --text
```

Required result:

- `loyal-same-mint-yield-monitor` is on an immutable
  `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>` image
  that contains the fix;
- its command is the intended production execution command, but does not include
  `--provision-lookup-table` or any replacement flag that creates/extends fresh
  ALTs in live route execution;
- service env includes whatever durable ALT source the implementation requires,
  such as DB-backed registry access through `NEON_DATABASE_URL` and/or explicit
  `YIELD_ROUTE_LOOKUP_TABLES`;
- service env does not add `SOLANA_TESTING_PK` to the live same-mint monitor;
- logs after deploy show fleet polls from the new image;
- logs after deploy contain no ordinary live route output with
  `wouldCreateLookupTable: true`, `lookup table create`, `lookup table extend`,
  `createSignature`, or non-empty `extendTransactions`;
- any ALT provisioning logs are from an explicit operator-approved provisioning
  command, not from the continuous monitor.

FAIL if `render.yaml` is fixed locally but live Render readback still runs the
old image or old command.

### 10. Post-Deploy Signer History Audit

PASS only if audited signer history after the production deploy contains no
unexpected ALT create/extend transactions.

Required evidence:

- record the fix deploy timestamp and image commit;
- run the implemented ALT audit/cleanup command in read-only history mode for
  every audited public key. Use `--min-slot <DEPLOY_SLOT>` when the deploy slot
  is known, or otherwise record the slot/time window used;
- inspect all signatures after the deploy timestamp that touch the Address
  Lookup Table Program;
- classify each as `expected_provisioning`, `cleanup_deactivate`,
  `cleanup_close`, or `unexpected_create_extend`.

Required result:

- zero `unexpected_create_extend` transactions after the production deploy;
- every `expected_provisioning` transaction has an operator-approved
  provisioning record and a durable registry row;
- cleanup transactions are limited to deactivate/close instructions and send
  reclaimed lamports to the configured recipient.

FAIL if the signer history check is skipped because logs "look clean".

## Verdict Format

Use this exact format when running the verifier:

```text
Live Execution Cannot Create Ephemeral ALTs: PASS|FAIL - evidence
ALT Coverage Is Durable And Idempotent: PASS|FAIL - evidence
Transaction Guard Rejects ALT Create/Extend Outside Provisioning: PASS|FAIL - evidence
Missing Coverage Fails Closed Without Spending: PASS|FAIL - evidence
Focused Local Checks: PASS|FAIL - commands and results
Cleanup Tool Is Dry-Run-First And Conservative: PASS|FAIL - evidence
On-Chain Reclaim Proof: PASS|FAIL|DEFERRED - reclaimed lamports, remaining lamports, reasons
Database And Registry Readbacks: PASS|FAIL - query results
Render Production Readback: PASS|FAIL - service/image/command/log evidence
Post-Deploy Signer History Audit: PASS|FAIL - audited keys and signatures
Overall Verdict: PASS|FAIL
```

Overall PASS requires every section to PASS. `On-Chain Reclaim Proof` may be
DEFERRED only while newly deactivated orphan tables are still inside the ALT
cooldown; the overall verdict remains FAIL until closeable rent is closed and
reclaimed.

## Current Baseline Record

Created on 2026-07-02 from the attached ALT leak context and current checkout.
The original baseline was expected to FAIL. Current local implementation state
after the ALT leak fix pass is still not an overall PASS until production
readback, durable registry readback, and on-chain reclaim proof are captured:

- `same-mint-yield-monitor.rs` no longer passes `--provision-lookup-table` to
  `same-mint-reserve-swap` in the `--optimization-cycle --execute` child path.
- `same-mint-reserve-swap.rs` has a transaction guard that rejects ALT
  create/extend instructions outside explicit provisioning mode.
- Route execution now loads durable lookup-table rows for the route scope and
  fails closed before route simulation/send when coverage is missing.
- `--provision-lookup-table` is policy-update/admin-only; route table setup uses
  the separate `--provision-route-lookup-table` mode and records durable
  registry rows.
- Explicit provisioning takes a transaction-scoped Postgres advisory lock on
  `(cluster, scope, authority)`, then reloads durable tables before deciding
  whether to create or extend, so concurrent setup attempts serialize.
- `route-lookup-table-cleanup` exists as a dry-run-first cleanup command and is
  packaged into the light worker image. It supports current account scanning
  and read-only signer history scanning, but live cleanup/deactivate/close proof
  is not yet captured here.
- `render.yaml` still runs production `loyal-same-mint-yield-monitor` with
  `--execute`; production service/image/env/log readback must prove the fixed
  binary is deployed before this verifier can pass.

Do not soften this verifier to match a partial implementation. If later work
discovers that a required check mis-encodes the real production safety goal,
update this document explicitly and record why before judging the fix.
