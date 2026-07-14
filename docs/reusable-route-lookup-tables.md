# Reusable Earn Address Lookup Tables

This document describes the operator boundary for the reusable Address Lookup
Table control plane. The binding implementation and rollout verifier is
`docs/plans/earn-reusable-alt-migration-verifier.md`.

## Ownership Model

Earn routes consume two logical address collections:

- `kamino_stable_market`: shared market, reserve, mint/supply, oracle, Scope,
  and reserve-farm addresses;
- `earn_vault_shards`: packed shards containing vault, obligation, policy,
  token-account, metadata, and farm-user-state addresses.

A collection is durable; a physical ALT is replaceable. Collections have active
and previous generations, and a generation may have multiple measured shards.
The normal route should compile with the active stable generation and the
vault's active shard binding. Exact source/target route scopes remain legacy
fallback and readiness identities only.

Extra addresses in an ALT are not authorization. Squads policies and the route
builder continue to constrain usable accounts and instructions.

## Process Boundaries

The continuous same-mint monitor and route executor are reuse-only. They may:

- derive typed address requirements;
- resolve active bindings;
- load physical ALTs from RPC;
- compile, measure, and simulate;
- record readiness blockers and, outside force-legacy mode, request
  provisioning;
- fail closed when coverage is missing.

They must never create, extend, freeze, deactivate, or close an ALT.

The dedicated `route-lookup-table-provisioner` command owns mutation. Cleanup
remains a separate dry-run-first command. Neither command should be part of the
continuous route execution command.

## Required Configuration

All commands require an explicit cluster. Do not infer a cluster from an RPC URL.

Non-secret configuration:

- `YIELD_ALT_CLUSTER`: `mainnet-beta`, `devnet`, `testnet`, or `localnet`;
- provisioner operation, concurrency, and lamport budget limits;
- measured largest atomic expansion, shard safety margin, vault growth
  reservation, and maximum bound-vault settings;
- rollout mode defaults.

Secret configuration, injected through the repository's 1Password environment:

- `NEON_DATABASE_URL`;
- `SOLANA_RPC_URL` for every cleanup run, every provisioner reconciliation or
  execute run, and every same-mint run that uses a managed/non-default endpoint,
  because managed endpoints may embed access credentials;
- `YIELD_ALT_MANAGER_KEYPAIR` only for explicit provisioner execute mode.

The route monitor does not need `YIELD_ALT_MANAGER_KEYPAIR`. Never print the
keypair value, signed transaction bytes, full database URL, or access tokens.
Structured output may identify an RPC only by scheme, host, and port; it strips
userinfo, path, query, and fragment so provider credentials cannot enter logs.
The same-mint worker, provisioner, and cleanup command read the RPC genesis hash
and reject a configured cluster/RPC mismatch before route work, reconciliation,
chain inspection, or mutation. Fatal command errors, readiness blockers, and
persisted operational failures use the same bounded URL and credential
redaction instead of retaining raw client error chains. An RPC URL's hostname
is not cluster evidence.

Use:

```sh
op run --env-file=.env.1password -- sh -c '<command>'
```

so secret expansion happens inside the injected subprocess.

## Provisioning Lifecycle

The provisioner is dry-run/reconcile-first:

1. Lease one durable operation with a fencing token.
2. Reload its physical ALT and known signature from RPC.
3. Reconcile chain truth into normalized membership.
4. If work remains, build and simulate the bounded mutation.
5. In explicit execute mode, persist the signature/message hash and blockhash
   expiry before broadcast.
6. Confirm and finalize, then reload owner, authority, lifecycle, address prefix,
   and order.
7. Mark only the newly appended suffix warm after a later usable slot.
8. Complete the operation and make a fully verified binding eligible for
   activation.

Do not hold a Postgres transaction or advisory lock across RPC, simulation,
send, confirmation, or warmup. A timed-out or restarted worker must inspect the
known signature and physical table before planning another mutation.

Unsigned failures release the lease into bounded retry wait until the
operation's configured maximum attempt count is exhausted. Signed or submitted
failures enter reconciliation instead of being blindly rebuilt. Estimated
fees/rent are stored before broadcast; actual spend or reclaimed rent is
recorded only after a finalized signature and exact chain-effect
reconciliation.

Capacity planning and transaction chunking are separate controls. Each family
stores immutable catalog evidence for:

```text
allocation high-water = 256 - largest atomic expansion - safety margin
```

`--largest-atomic-expansion` supplies the measured catalog value when families
are bootstrapped. `--address-chunk` only bounds one extend transaction and must
not change the stored high-water mark. The manager authority and payer are
durably checked against active route authorities, delegated signers, wallets,
and legacy durable route payers; optional process environment is an additional
check, not the ownership boundary.

The current 13-fixture route catalog measures a largest single-class atomic
expansion of `21` addresses. With the documented default safety margin of `16`,
the bootstrap high-water is therefore `256 - 21 - 16 = 219`. Treat this as
generated catalog evidence, not a forever constant: rerun
`bun run verify:reusable-alts:routes` after any route/action-account change and
use its final `reusable_alt_catalog_summary` JSON line when bootstrapping a
new catalog version.

Each vault manifest is the durable address union of every sealed, non-cancelled
route-requirement cohort observed for that vault, rather than the latest route's
subset. Aggregate revisions are serialized through the vault-shard family row.
Publishing a new desired-head revision permanently supersedes older preparing
or warming bindings, and activation rechecks that revision transactionally so a
late provisioner cannot replace the head with an obsolete partial manifest.

Vault growth first reuses the active packed shard when the complete aggregate
manifest and its reservation still fit. If it does not fit, the allocator
creates a complete preparing binding on another packed shard (or a measured
dedicated outlier table), warms and verifies it, then atomically flips the
binding head. Dedicated tables are sealed against later allocations. A
same-generation shard with no remaining live binding may retire after the
rollback/reference fences clear; generation rollover is not required merely to
reclaim an empty shard.

## Rollout Modes

Per-vault modes are:

- `legacy`: exact-route tables only;
- `shadow`: legacy execution plus reusable compilation/readiness evidence;
- `prefer_reusable`: reusable when fully ready, otherwise complete legacy
  fallback;
- `reusable_only`: reusable or fail closed, with no silent fallback.

Mode resolution is deliberately independent: `legacy` and global force-legacy
do not query reusable RPC state, while `reusable_only` does not query legacy
state. `shadow` requires a verified legacy bundle and records reusable errors
as evidence. `prefer_reusable` may use a fully verified reusable bundle even if
legacy resolution is malformed, but fails when neither path is complete. The
global force-legacy path also skips reusable leases and provisioning requests,
and treats its minimal readiness write as best-effort, so a kill switch cannot
be blocked by an unhealthy reusable control plane.

A global force-legacy control overrides per-vault modes. Before legacy
retirement, rollback is a database pointer change:

- enable global force-legacy;
- move one vault back to `legacy`;
- point a family to its previous generation;
- point a vault to its previous binding;
- pause the provisioner.

Do not roll schema backward during an operational rollback.

## Safe Migration Order

1. Apply and verify migration `0017` on an isolated database branch.
2. Import legacy physical tables only after RPC readback; label them mixed
   legacy tables rather than promoting them.
3. Provision and verify the stable generation plus every packed binding for a
   live legacy consumer or currently executable route while legacy routing
   remains authoritative. Do not preallocate every theoretical route belonging
   to a dormant active-policy row.
4. Immediately before cutover, resolve, compile, packet-check, and simulate
   every live route requirement against the reusable state. Record the exact
   manifests, selected tables, verification slots, and rollback targets.
5. Set the eligible fleet directly to `reusable_only` in one fenced control
   operation. A canary cohort and an artificial observation delay are not part
   of this migration.
6. Read the control plane back, run the normal worker path, and reconcile at
   least one real reusable-ALT movement when an executable movement is
   available.
7. Mark each legacy row nonselectable only after a fresh zero-reference check.
8. Deactivate each eligible legacy ALT, wait the mandatory Solana SlotHashes
   cooldown, recheck references, then close it and record the refund.

A route that was not materialized in the cutover snapshot remains fail-closed.
Its first attempted movement seals a typed provisioning request; a later retry
may execute only after the provisioner has expanded or allocated the reusable
tables and the resolver has independently verified them.

Production migration, provisioning transactions, Render changes, direct money
movement, deactivation, and closure require explicit operator approval.

## Cleanup Safety

An ALT is protected while it is active or standby, bound, warming, preparing,
leased, referenced by a pending operation, referenced by a prepared route, or
in the rollback window. Database/chain disagreement also protects it.

Retirement order is:

```text
stop allocations -> retire bindings -> observe zero references
-> deactivate -> wait ALT cooldown -> close -> record reclaimed rent
```

Cleanup must preview the table, authority, recipient, lifecycle, and expected
lamports before an operator can choose execute mode. For a registered reusable
table, cleanup execute mode queues a fenced `deactivate` or `close` operation;
it does not sign or broadcast the mutation itself. The dedicated provisioner
reconciles that operation and is the only process that may send it. Direct
cleanup signing remains limited to explicitly audited, unregistered legacy
orphans.

## Required Readbacks

Operators should be able to inspect, without secrets:

- active/previous family generations;
- each shard's confirmed addresses, usable prefix, reservations, headroom,
  bound-vault count, and fragmentation;
- desired manifests and missing addresses;
- operation queue age, attempts, signature, and reconciliation state;
- reusable-ready vault coverage and legacy fallback reason;
- selected table contribution and serialized packet size;
- authority/prefix drift;
- lamports spent and reclaimed.

## Operator Command Cookbook

Run these commands from the repository root. The examples deliberately keep
all environment expansion inside the `op run` subprocess. `YIELD_ALT_CLUSTER`,
`NEON_DATABASE_URL`, and `SOLANA_RPC_URL` must come from the mounted environment
for cleanup; reconciliation and provisioner execution also require the RPC,
while status and database-only administration do not. The public values
`YIELD_ALT_MANAGER_PUBKEY`,
`YIELD_ALT_CATALOG_VERSION`, `YIELD_ALT_OPERATOR_ID`, and the database IDs used
for rollback may be supplied by the operator without exposing secret material.

Inspect the control plane and queued work without loading a signer or writing:

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- --cluster "$YIELD_ALT_CLUSTER" --status'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- --cluster "$YIELD_ALT_CLUSTER"'
```

Create or verify the two logical families. This is an idempotent metadata
operation: it validates an existing family's immutable configuration and does
not reset live generation pointers. It never loads the manager keypair.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --bootstrap-families \
    --manager-pubkey "$YIELD_ALT_MANAGER_PUBKEY" \
    --catalog-version "$YIELD_ALT_CATALOG_VERSION" \
    --largest-atomic-expansion "$YIELD_ALT_LARGEST_ATOMIC_EXPANSION" \
    --safety-margin "$YIELD_ALT_SAFETY_MARGIN" \
    --admin-write \
    --reason "bootstrap reusable ALT families" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
```

Reconcile persisted signatures and chain state without signing or sending:

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --reconcile-only \
    --max-operations 10'
```

Pause a worker instance without changing routing, then resume by removing
`YIELD_ALT_PROVISIONING_PAUSED` or omitting `--pause`:

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --pause'
```

Execute bounded mutation work only after reviewing status and dry-run output.
This is the only mode that loads `YIELD_ALT_MANAGER_KEYPAIR`; the positive
lamport limit is mandatory and applies to the selected batch. `--max-attempts`
bounds unsigned retries; signed ambiguity is reconciled regardless of that
retry budget.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --execute \
    --max-operations 1 \
    --max-attempts 5 \
    --max-lamports "$YIELD_ALT_MAX_LAMPORTS"'
```

Set one vault's rollout mode using its database `managed_vaults.id`. This is an
operator control and rollback tool, not a required canary step:

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --set-rollout-mode prefer_reusable \
    --vault-id "$YIELD_ALT_VAULT_ID" \
    --admin-write \
    --reason "explicit per-vault reusable ALT control" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
```

After the all-vault pre-cutover proof passes, the direct fleet switch writes the
global mode in one fenced operation (per-vault overrides, if any, must already
agree or be updated explicitly):

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --set-rollout-mode reusable_only \
    --admin-write \
    --reason "direct reusable ALT fleet cutover after full preflight" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
```

The global force-legacy kill switch overrides every per-vault mode. Clearing
it does not itself advance any vault's stored mode.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --force-legacy \
    --admin-write \
    --reason "reusable ALT rollback" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --clear-force-legacy \
    --admin-write \
    --reason "resume configured rollout modes" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
```

Roll a family back to its recorded previous generation, or restore an active
vault binding's recorded predecessor. Both are atomic database pointer changes
and neither loads a signer. The binding rollback requires a fresh observed
slot supplied by the operator.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --rollback-family "$YIELD_ALT_FAMILY_ID" \
    --admin-write \
    --reason "restore previous shared generation" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --rollback-binding "$YIELD_ALT_ACTIVE_BINDING_ID" \
    --observed-slot "$YIELD_ALT_OBSERVED_SLOT" \
    --admin-write \
    --reason "restore previous vault binding" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
```

After every family and binding rollback window has expired, explicitly retire
the standby references before cleanup. This releases their reserved capacity,
clears the previous-generation pointer, and marks the old physical generation
retiring. The command fails while any rollback deadline, lease, or operation is
still live.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --finalize-rollbacks "$YIELD_ALT_FAMILY_ID" \
    --admin-write \
    --reason "rollback window expired" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
```

Legacy rows are deliberately durable until an operator retires them with an
exact metadata fence. First remove every readiness/lease reference and copy
the expected public authority, ordered-address hash, and address count from
the status/readback. This database-only step makes the row nonselectable; it
does not deactivate or close the on-chain table.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --retire-legacy "$YIELD_ALT_LEGACY_TABLE" \
    --expected-authority "$YIELD_ALT_LEGACY_AUTHORITY" \
    --expected-address-hash "$YIELD_ALT_LEGACY_ADDRESS_HASH" \
    --expected-address-count "$YIELD_ALT_LEGACY_ADDRESS_COUNT" \
    --admin-write \
    --reason "direct cutover verified and legacy references drained" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
```

Preview cleanup first. A registered reusable table can only enqueue a fenced
deactivate/close operation; the provisioner remains the only process allowed
to sign it. Direct signing is reserved for explicitly selected, unregistered
legacy orphans controlled by the audited legacy authority. Cleanup requires an
explicit RPC in every mode and verifies its genesis hash before its first chain
read; the examples inject `SOLANA_RPC_URL` through 1Password.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-cleanup -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --table "$YIELD_ALT_CLEANUP_TABLE" \
    --limit 1'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-cleanup -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --table "$YIELD_ALT_CLEANUP_TABLE" \
    --limit 1 \
    --execute'
```

Treat `--execute`, rollout changes, rollback, and cleanup as production actions
when pointed at production. They require the separate approvals described by
the verifier; these examples do not authorize running them.

Use the verifier document for the exact implementation and production verdict
format. A successful dry run or provisioner no-op is not by itself a production
migration PASS.
