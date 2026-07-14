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
vault's active shard binding. Exact source/target route scopes remain
readiness/audit identities only; the normal runtime has no legacy fallback.

Extra addresses in an ALT are not authorization. Squads policies and the route
builder continue to constrain usable accounts and instructions.

## Process Boundaries

The continuous same-mint monitor and route executor are reuse-only. They may:

- derive typed address requirements;
- resolve active bindings;
- load physical ALTs from RPC;
- compile, measure, and simulate;
- record readiness blockers; shared-catalog drift emits a repair signal but no
  vault allocation request, while missing vault coverage seals one idempotent
  provisioning request in `reusable_only`;
- fail closed when coverage is missing.

They must never create, extend, freeze, deactivate, or close an ALT.

The dedicated `route-lookup-table-provisioner` command owns mutation. Cleanup
remains a separate dry-run-first command. Neither command should be part of the
continuous route execution command.

## Required Configuration

All commands require an explicit cluster. Do not infer a cluster from an RPC URL.

Non-secret configuration:

- `YIELD_ALT_CLUSTER`: `mainnet-beta`, `devnet`, `testnet`, or `localnet`;
- provisioner operation/concurrency limits plus `YIELD_ALT_MAX_LAMPORTS` and
  `YIELD_ALT_BUDGET_WINDOW_SECONDS` for the durable cluster-wide rolling budget;
- measured largest atomic expansion, shard safety margin, vault growth
  reservation, and maximum bound-vault settings;
- rollout mode defaults.

Secret configuration, injected through the repository's 1Password environment:

- `NEON_DATABASE_URL`;
- `SOLANA_RPC_URL` for every cleanup run, every provisioner reconciliation or
  execute run, and every same-mint run that uses a managed/non-default endpoint,
  because managed endpoints may embed access credentials;
- `POLICY_KEYPAIR` for explicit provisioner/legacy-cleanup execute mode and the
  existing authorized Earn movement path. The policy identity is intentionally
  reused as the ALT authority, payer, and old-table refund recipient.

The signerless shared-catalog seeder and legacy import command do not load
`POLICY_KEYPAIR`. The route monitor cannot mutate an ALT even though it uses the
same key for authorized Earn movement. Never print the keypair value, signed
transaction bytes, full database URL, or access tokens.
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

After simulation and before signing, every mutating attempt reserves its
worst-case fee plus rent in PostgreSQL under the operation's fencing token.
`--max-lamports` is a cluster-wide rolling-window ceiling, not a process-local
or per-batch counter; `--budget-window-seconds` defines that window. Durable
reservations survive provisioner restarts and serialize overlapping Render
instances. A denied reservation sends nothing, and replaying the same
operation/fence/accounting is idempotent. Conflicting accounting or a stale
lease fails closed.

Capacity planning and transaction chunking are separate controls. Each family
stores immutable catalog evidence for:

```text
allocation high-water = 256 - largest atomic expansion - safety margin
```

`--largest-atomic-expansion` supplies the measured catalog value when families
are bootstrapped. `--address-chunk` only bounds one extend transaction and must
not change the stored high-water mark. The ALT authority and payer must be the
same standard policy identity. The family catalog persists both values and
rejects authority/payer drift; reusing the policy identity is intentional and
is not an ownership conflict.

The current 13-fixture route catalog measures a largest single-class atomic
expansion of `21` addresses. With the documented default safety margin of `16`,
the bootstrap high-water is therefore `256 - 21 - 16 = 219`. Treat this as
generated catalog evidence, not a forever constant: rerun
`bun run verify:reusable-alts:routes` after any route/action-account change and
use its final `reusable_alt_catalog_summary` JSON line when bootstrapping a
new catalog version.

The shared-market catalog is deliberately one exact physical ALT per
generation. If the complete catalog exceeds that family's allocation
high-water mark, the signerless publisher and planner fail before a catalog
write, operation enqueue, signer load, or transaction. The current system does
not truncate or auto-shard shared data. A larger catalog requires a future
shared-sharding schema, resolver, compiler-fixture, and migration verifier.

Finalized RPC truth can invalidate an otherwise exact database catalog. An
authority, lifecycle, ordered-membership, prefix, or account-presence mismatch
is persisted as immutable physical-drift evidence fenced to the catalog
revision, table, and mutation epoch. The active head immediately leaves
`active`, routes emit the shared-catalog repair blocker without creating vault
demand, and the planner builds a complete replacement generation. The report
is resolved only when that replacement is finalized, warm, exact, and active;
the provisioner must not "repair" or reactivate the drifted generation in
place.

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

## Rollout Controls

The deployed Earn runtime executes only when the effective mode is
`reusable_only` and force-legacy is false. Missing v2 coverage fails closed and
seals provisioning demand; it never discovers or resolves an exact-scope
legacy table.

Historical database values remain readable for migration compatibility, but
their runtime meaning is deliberately narrow:

- `legacy`, `shadow`, and `prefer_reusable` are fail-closed stop states;
- global or per-vault force-legacy is also a fail-closed stop;
- only `reusable_only` with force-legacy disabled may compile or send.

Operational rollback after cutover is therefore one of:

- pause routing or the provisioner;
- point a shared family to its verified previous v2 generation;
- point one vault to its verified previous v2 binding.

The first shared generation and first vault binding honestly have no reusable
predecessor. Do not create duplicate standby ALTs merely to manufacture one.

Do not roll schema backward during an operational rollback.

## Safe Migration Order

1. Apply and verify migration `0017`, existing realtime migration `0018`,
   legacy audit migration `0019`, and shared-catalog migration `0020` on an
   isolated database branch, then production.
2. Import the complete eligible legacy fleet only for immutable audit and
   refund accounting. Label it `legacy_mixed`; never promote it to a v2 family,
   create another exact-scope table, or copy its scope allocation strategy.
3. Bootstrap both v2 family records with the public key derived from the
   standard `POLICY_KEYPAIR`.
4. Run the signerless shared-catalog command against the complete active,
   safe, enabled-stable Kamino reserve set, review its finalized dry run, then
   publish the immutable catalog head. Do not derive this bootstrap set from
   one vault or one attempted route.
5. Deploy the continuously running, budgeted provisioner and let it create,
   fully populate, warm, and verify that exact durable shared-market v2 ALT.
   Keep routing in a fail-closed stop mode. Stop and drain the old monitor,
   prove there is no prepared decision or send still using it, then deploy the
   no-legacy monitor from the same immutable light-worker image as the
   provisioner.
6. Atomically prove the shared head, align every per-vault override, and switch
   directly to global `reusable_only`. There is no vault backfill, canary, or
   all-vault coverage gate. Let the normal monitor immediately attempt current
   optimizations. Missing
   vault coverage must stop before decision creation/send, seal one idempotent
   request, and return. The provisioner packs that genuine demand into the
   best-fit v2 vault shard; the next monitor cycle retries normally.
7. Verify at least one funded production vault completes a real defer ->
   provision -> retry -> confirmed movement sequence. Join its finalized
   signature to the route decision and reusable table bundle, prove the source
   chain position decreased and the selected higher-yield eligible target
   position increased, reconcile the same state into Neon, and prove a later
   monitor cycle neither repeats the move nor leaves its request stuck. Then
   prove the deployed worker has no legacy resolution plus zero remaining live
   references to every old table. A no-op poll or provisioner success is not
   sufficient production proof.
8. Mark each legacy row nonselectable. For every table, perform a fresh
   zero-reference preview, simulate immediately before deactivation, submit and
   prove finality, wait the mandatory Solana SlotHashes cooldown, then repeat
   the zero-reference preview and simulation immediately before close. Prove
   finalized account closure and the rent balance delta to the policy account.

The expected first-use latency is one monitor cycle for a vault whose packed
data is not present yet. That rebalance is deferred, not lost or recorded as a
failed movement. Later routes reuse the same shared table and existing packed
shard headroom.

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

Legacy refund is not a single best-effort cleanup command. Before the first
deactivation, the old monitor must be stopped and drained, the no-legacy image
must be the only deployed route runtime, and all bindings, leases, operations,
prepared decisions, and readiness references must be zero. Each mutation must
use `--simulate-before-submit`, be confirmed at finalized commitment, and be
followed by an RPC lifecycle readback. After deactivation, wait until the table
is absent from the relevant SlotHashes window; do not estimate or bypass the
cooldown. Immediately before close, repeat the database/RPC reference scan and
simulation. The close recipient must equal the public key derived from
`POLICY_KEYPAIR`; record the finalized signature, pre/post recipient lamports,
reclaimed delta, and final RPC account absence.

## Required Readbacks

Operators should be able to inspect, without secrets:

- active/previous family generations;
- each shard's confirmed addresses, usable prefix, reservations, headroom,
  bound-vault count, and fragmentation;
- desired manifests and missing addresses;
- operation queue age, attempts, signature, and reconciliation state;
- durable rolling-budget window, active reservations, charged/remaining
  lamports, and denied attempts;
- reusable-ready vault coverage and demand-deferral reason;
- selected table contribution and serialized packet size;
- authority/prefix drift;
- immutable shared physical-drift reports and their replacement generations;
- lamports spent and reclaimed.

## Operator Command Cookbook

Run these commands from the repository root. The examples deliberately keep
all environment expansion inside the `op run` subprocess. `YIELD_ALT_CLUSTER`,
`NEON_DATABASE_URL`, and `SOLANA_RPC_URL` must come from the mounted environment
for cleanup; reconciliation and provisioner execution also require the RPC,
while status and database-only administration do not. The public values
`YIELD_ALT_POLICY_PUBKEY`,
`YIELD_ALT_CATALOG_VERSION`, `YIELD_ALT_OPERATOR_ID`, and the database IDs used
for rollback may be supplied by the operator without exposing secret material.

Before provisioning reusable tables, inventory and import the complete durable
legacy fleet. The importer is dry-run by default, uses one finalized RPC
snapshot, validates the configured genesis, owner, authority, active lifecycle,
warmup, exact ordered membership, count, and hash for every eligible row, and
writes nothing if any row fails. It never loads a signer. The command prints a
`registryFleetHash`; copy that exact hash into the separately approved write.
The current pre-reusable exact-scope tables contain both stable market and vault
addresses, so their classification is `legacy_mixed`. Do not guess or combine
different classifications in one run; a future heterogeneous fleet requires an
explicit per-table import design first.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-import-legacy -- \
    --legacy-kind legacy_mixed \
    --expected-table-count "$YIELD_ALT_LEGACY_EXPECTED_COUNT"'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-import-legacy -- \
    --legacy-kind legacy_mixed \
    --expected-table-count "$YIELD_ALT_LEGACY_EXPECTED_COUNT" \
    --expected-fleet-hash "$YIELD_ALT_LEGACY_FLEET_HASH" \
    --admin-write \
    --reason "classify and reverify complete legacy ALT fleet" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
```

Review the dry-run count, every table result, the finalized verification slot,
and the fleet hash before approving the write. A write re-locks and rereads the
full eligible registry inside a serializable transaction, so registry drift
after RPC verification aborts the import without a partial classification. An
exact replay verifies the existing immutable evidence instead of rewriting it.

Inspect the control plane and queued work without loading a signer or writing:

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- --cluster "$YIELD_ALT_CLUSTER" --status'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- --cluster "$YIELD_ALT_CLUSTER"'
```

Create or verify the two logical families. This is an idempotent metadata
operation: it validates an existing family's immutable configuration and does
not reset live generation pointers. It accepts only the public key corresponding
to the standard `POLICY_KEYPAIR`; it never loads that keypair.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --bootstrap-families \
    --policy-pubkey "$YIELD_ALT_POLICY_PUBKEY" \
    --catalog-version "$YIELD_ALT_CATALOG_VERSION" \
    --largest-atomic-expansion "$YIELD_ALT_LARGEST_ATOMIC_EXPANSION" \
    --safety-margin "$YIELD_ALT_SAFETY_MARGIN" \
    --admin-write \
    --reason "bootstrap reusable ALT families" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
```

Derive the authoritative catalog independently of any vault. Dry-run is the
default and performs one finalized reserve-account snapshot, checks every
reserve owner/market/mint identity, validates the family high-water mark, and
loads no signer. The approved write publishes the immutable head and queues v2
operations only; it still sends no transaction and never allocates vault data.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-shared-catalog -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --catalog-version "$YIELD_ALT_CATALOG_VERSION"'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-shared-catalog -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --catalog-version "$YIELD_ALT_CATALOG_VERSION" \
    --admin-write \
    --reason "publish complete durable Earn shared-market catalog" \
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
This is the only provisioner mode that loads `POLICY_KEYPAIR`; the positive
lamport limit is mandatory and applies across all overlapping workers in the
durable rolling window. `--max-attempts` bounds unsigned retries; signed
ambiguity is reconciled regardless of that retry budget.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --execute \
    --max-operations 1 \
    --max-attempts 5 \
    --max-lamports "$YIELD_ALT_MAX_LAMPORTS" \
    --budget-window-seconds "$YIELD_ALT_BUDGET_WINDOW_SECONDS"'
```

After the shared v2 catalog and demand-driven provisioner path are verified,
perform the direct cutover with one finalized-RPC plus database-fenced action.
The provisioner first loads the exact active shared ALT at finalized
commitment and proves table address, authority, lifecycle, ordered membership,
hash, usable count, verification slot, and mutation epoch against a database
preflight. The database transaction then rejects any changed preflight,
requires the packed-vault family, sets global `reusable_only` with force-legacy
disabled, and aligns every per-vault override. There is deliberately no
all-vault coverage precondition:

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --activate-reusable-only \
    --admin-write \
    --reason "activate demand-driven reusable v2 routing" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
```

The global force-legacy control is retained only as a fail-closed stop. It does
not resolve old tables. Clearing it does not itself advance any vault's stored
mode.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --force-legacy \
    --admin-write \
    --reason "stop reusable ALT routing" \
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
    --reason "v2 routing verified and legacy references drained" \
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
    --authority "$YIELD_ALT_POLICY_PUBKEY" \
    --recipient "$YIELD_ALT_POLICY_PUBKEY" \
    --authority-key-env POLICY_KEYPAIR \
    --scan-program-accounts \
    --scan-history'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-cleanup -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --authority "$YIELD_ALT_POLICY_PUBKEY" \
    --recipient "$YIELD_ALT_POLICY_PUBKEY" \
    --authority-key-env POLICY_KEYPAIR \
    --scan-program-accounts \
    --scan-history \
    --expected-fleet-count "$YIELD_ALT_LEGACY_EXPECTED_COUNT" \
    --expected-fleet-hash "$YIELD_ALT_CLEANUP_FLEET_HASH" \
    --simulate-before-submit \
    --execute'
```

The dry run must exhaustively discover the standard-policy legacy fleet and
print `legacyFleetCount` plus `inventoryFleetHash`; copy those exact cleanup
values into the separately approved execute environment. Do not substitute the
importer's registry hash. Execute ignores candidate limits and refuses a
partial fleet, a changed count/hash, any non-policy authority, or a non-policy
recipient. Run it once to deactivate all eligible active legacy tables. Verify
each signature finalized and each RPC lifecycle changed to deactivated, then
wait for the actual SlotHashes cooldown. Run the full exhaustive zero-reference
dry run again and capture its fresh fleet hash; only then run the same simulated
execute form to close. Close is complete only after finalized RPC absence for
every approved table and the recorded policy-account lamport delta matches the
reconciled reclaimed-rent evidence.

Treat `--execute`, rollout changes, rollback, and cleanup as production actions
when pointed at production. They require the separate approvals described by
the verifier; these examples do not authorize running them.

Use the verifier document for the exact implementation and production verdict
format. A successful dry run or provisioner no-op is not by itself a production
migration PASS.
