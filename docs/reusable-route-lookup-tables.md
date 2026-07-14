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

The separate `route-lookup-table-alert-monitor` is signerless and read-only
with respect to routes and ALTs. It reads Neon plus finalized RPC, maintains
durable incident/outbox state, and delivers semantic alerts. Its service must
not contain `POLICY_KEYPAIR`, any other signer environment, or
`TIMESCALEDB_URL`; this monitor needs only the Neon control plane and finalized
Solana RPC truth.

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

## Semantic Alert Monitor

Migration `0021_reusable_alt_production_controls` adds a durable, versioned
rule catalog plus incident and delivery-outbox tables. It seeds exactly nine
enabled version-1 rules. A rule cannot be deleted or renamed; changing its
enable state, severity, description, or configuration must advance its version
exactly once. Disabling a rule produces a healthy observation so an existing
incident resolves durably instead of remaining open. The monitor evaluates
exactly these routing keys:
`readiness_regression`, `missing_coverage`, `operation_backlog`,
`capacity_headroom`, `authority_prefix_drift`, `provisioning_budget`,
`orphaned_tables`, `fallback_use`, and `cleanup_anomalies`. Repeated identical
evidence updates one incident; changed evidence or the reminder interval emits
a reminder; a healthy observation resolves the same incident. Delivery leases,
fencing tokens, retry wait, and dead-letter state survive process restarts.

Migration `0022_shared_market_alt_bundles` separates one logical shared-market
catalog from its deterministic ordered bundle of physical ALT shards. It keeps
each physical shard within the family's measured high-water and Solana's
256-address limit, while immutable pre-cutover parent and per-shard child rows
record the complete finalized bundle. For the measured production bootstrap,
237 logical addresses at a 219-address high-water means two physical shards of
219 and 18 addresses.

Required values are `NEON_DATABASE_URL`, `SOLANA_RPC_URL`,
`YIELD_ALT_CLUSTER`, and the non-secret `YIELD_ALT_POLICY_PUBKEY`. Preferred
delivery is a generic JSON webhook in `YIELD_ALT_ALERT_WEBHOOK_URL`; an optional
bearer token comes from `YIELD_ALT_ALERT_WEBHOOK_BEARER_TOKEN`. Mainnet is
always production mode. Production startup fails unless the webhook exists or
the command explicitly includes `--render-failure-delivery`. The latter is an
intentional fallback: it records the outbox delivery, emits one allowlisted
sanitized JSON record, and exits nonzero so a configured Render worker-failure
notification becomes the delivery signal. Ordinary logs are not a delivery
channel.

Run one scan or a continuous worker without exposing injected values:

```sh
op run --env-file=.env.1password -- sh -c \
  'unset POLICY_KEYPAIR YIELD_ROUTER_KEYPAIR SOLANA_TESTING_PK DEPLOYMENT_PK TIMESCALEDB_URL;
   bun run same-mint:alt-alert-monitor -- --once'

op run --env-file=.env.1password -- sh -c \
  'unset POLICY_KEYPAIR YIELD_ROUTER_KEYPAIR SOLANA_TESTING_PK DEPLOYMENT_PK TIMESCALEDB_URL;
   bun run same-mint:alt-alert-monitor -- --watch --interval-seconds 60'
```

`--test-alerts` atomically inserts one condition-specific test delivery for each
of the exact nine durable rules; it creates no incident, provisioning request,
route decision, or ALT operation. The dispatcher leases only those nine IDs,
never an unrelated production backlog, and reports success only after all nine
reach `delivered`. Use it after configuring the destination. A Render-failure
delivery test emits nine sanitized records and is expected to exit nonzero:

```sh
op run --env-file=.env.1password -- sh -c \
  'unset POLICY_KEYPAIR YIELD_ROUTER_KEYPAIR SOLANA_TESTING_PK DEPLOYMENT_PK TIMESCALEDB_URL;
   bun run same-mint:alt-alert-monitor -- --once --test-alerts'
```

Thresholds have matching flags and `YIELD_ALT_ALERT_*` environment variables
for overdue coverage, queue age/depth, capacity headroom, rolling budget,
cleanup grace, reminder interval, and delivery retry limits. Set
`YIELD_ALT_ALERT_BUDGET_MAX_LAMPORTS` to the same ceiling as the provisioner (or
allow the monitor to read `YIELD_ALT_MAX_LAMPORTS`). Every physical identity,
authority, lifecycle, warmup, and ordered-prefix comparison uses finalized RPC.
Cleanup alerts keep every imported familyless legacy row observable after
retirement flips `durable` to false. They detect overdue active/retiring/
deactivated rows, missing finalized deactivate or close signatures, missing
deactivation slots, a missing or nonpositive rent refund, and a refund recipient
that differs from the policy identity.

## Provisioning Lifecycle

The provisioner is dry-run/reconcile-first:

1. Lease one durable operation with a fencing token.
2. Reload its physical ALT and known signature from RPC.
3. Reconcile chain truth into normalized membership.
4. If work remains, build and simulate the bounded mutation.
5. In explicit execute mode, persist the signature/message hash and blockhash
   expiry, then commit one exact broadcast permit under the current durable
   cluster control epoch before broadcast.
6. Confirm and finalize, then reload owner, authority, lifecycle, address prefix,
   and order.
7. Mark only the newly appended suffix warm after a later usable slot.
8. Complete the operation and make a fully verified binding eligible for
   activation.

Do not hold a Postgres transaction or advisory lock across RPC reads,
simulation, broadcast, confirmation, or warmup. Pause administration and
broadcast-permit grant each lock the same durable cluster control row in their
own short transaction. Whichever commits first is the deterministic winner: a
committed pause denies a new permit, while a committed exact permit remains
visible as in-flight work for pause/cutover drain. The worker releases every
database lock before RPC send. A timed-out or restarted worker must inspect the
known signature, permit state, and physical table before planning another
mutation.

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
the per-physical-table bootstrap high-water is therefore
`256 - 21 - 16 = 219`. Treat this as generated catalog evidence, not a forever
constant: rerun
`bun run verify:reusable-alts:routes` after any route/action-account change and
use its final `reusable_alt_catalog_summary` JSON line when bootstrapping a
new catalog version.

The shared-market catalog is one logical, append-ordered manifest per
generation. It is deterministically chunked into contiguous physical shards at
the family high-water, and routes load only the shards that contribute compiled
indexes. The production bootstrap measured 237 logical addresses, so the
correct `219`-high-water layout is two exact shards containing 219 and 18
addresses. Lowering the measured expansion or safety evidence to force one
table would misstate the verified route catalog.

The active/safe/enabled-stable reserve query controls eligibility
for new deposits; it does not authorize removing accounts needed to exit an
existing or deprecated source position. Physical inventory therefore includes
every known reserve for the explicit enabled stable mints regardless of
active/risk state, plus any additional reserve referenced by a Neon nonzero
current position, active/unreconciled decision, or route-readiness row. The
seeder resolves every such reserve through Timescale identity data and decodes
its current oracle/Scope/farm pointers in the same finalized RPC snapshot; a
missing or inconsistent source reserve blocks publication.

On every publication, the signerless seeder also reads all immutable
shared-catalog revisions from Neon, preserves the current head's distinct
physical prefix, unions role metadata and writability, and appends historical-
only or newly required addresses. Admin publication locks and rechecks the
Neon held/in-flight source set after the RPC read, so concurrent source drift
fails instead of publishing stale coverage. There is currently no durable
zero-live-reference proof covering holdings, policies, pending operations, and
in-flight routes, so there is intentionally no address-removal path. A future
removal workflow must add and verify that proof before it may shrink the
durable union.

This retained-prefix ordering lets a newly discovered address extend the tail
shard of the current generation even when its base58 text sorts before an
existing address; once that shard reaches the high-water, the deterministic
next ordinal begins another physical shard. The new-target reserve-set identity
remains distinct from retained physical ALT order. The signerless publisher and
planner must fail before a catalog write, operation enqueue, signer load, or
transaction if the deterministic shards do not cover the complete logical
catalog exactly or if any physical shard exceeds its high-water. They never
truncate, duplicate, or reorder shared data.

Finalized RPC truth can invalidate an otherwise exact database catalog. An
authority, lifecycle, shard ordinal, ordered-membership, prefix, or
account-presence mismatch in any physical table
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
   legacy audit migration `0019`, shared-catalog migration `0020`, and
   production-control/alert/probe-audit migration `0021`, followed by
   multi-shard shared-bundle migration `0022`, on an isolated database branch,
   then production. Verify the complete migration 1–22 replay and checksums.
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
   fully populate, warm, and verify that exact durable shared-market v2 bundle.
   Keep routing in a fail-closed stop mode. Stop and drain the old monitor,
   prove there is no prepared decision or send still using it, then deploy the
   no-legacy monitor from the same immutable light-worker image as the
   provisioner.
6. Durably pause and drain the provisioner, run the signerless rollback-only
   production-connected probe, atomically prove the shared head, align every
   per-vault override, and switch directly to global `reusable_only`. Clear the
   durable pause once the new monitor and provisioner are pinned. There is no
   vault backfill, canary, or all-vault coverage gate. Let the normal monitor
   immediately attempt current optimizations. Missing vault coverage must stop
   before decision creation/send, seal one idempotent request, and return. The
   provisioner packs that genuine demand into the best-fit v2 vault shard; the
   next monitor cycle retries normally.
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
failed movement. Later routes reuse the same shared bundle and existing packed
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
table, a second database-native retirement inventory queues a fenced
`deactivate` or `close` operation; cleanup does not sign or broadcast that v2
mutation. The dedicated provisioner reconciles that operation and is the only
process that may send it. Direct cleanup signing remains limited to the
immutable imported, familyless legacy fleet.

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

The imported fleet is never rediscovered with a whole ALT-program scan and may
not be narrowed with ad-hoc table flags. Cleanup loads every immutable imported
row from Neon, including already closed tables, then verifies the full set with
finalized batched account reads. It paginates each imported authority's
finalized signature history with `before` until the approved minimum slot or
history exhaustion; the page size is not a total-history cap. The report keeps
the immutable fleet hash, history mutation-set hash, page/boundary evidence,
and stored deactivate/close signatures across partial retries.

Every familyless deactivate/close first simulates an unsigned transaction at
finalized commitment, estimates worst-case fee and rent, prepares a durable
attempt, and reserves that exact amount from the same PostgreSQL rolling
cluster budget used by v2 provisioner operations. `--max-lamports` and
`--budget-window-seconds` are mandatory positive execute fences. A denied or
missing reservation stops before `POLICY_KEYPAIR` signs; the database also
rejects any signed attempt that bypasses the reservation order.

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
writes nothing if any row fails. It never loads a signer. The command prints an
`inventoryFleetHash`; copy that exact hash into the separately approved write.
Rows created by the historical exact-scope writer may carry its sorted,
NUL-delimited v1 digest. The importer recognizes that digest only while the row
is unclassified and unimported. After exact ordered membership is independently
verified from finalized RPC, the same serializable fleet transaction normalizes
the registry and immutable evidence to the reusable-v2 ordered digest. Cleanup
and all post-import reads accept only the reusable-v2 digest.
The current pre-reusable exact-scope tables contain both stable market and vault
addresses, so their classification is `legacy_mixed`. Do not guess or combine
different classifications in one run; a future heterogeneous fleet requires an
explicit per-table import design first.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-import-legacy -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --legacy-kind legacy_mixed \
    --expected-table-count "$YIELD_ALT_LEGACY_EXPECTED_COUNT"'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-import-legacy -- \
    --cluster "$YIELD_ALT_CLUSTER" \
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
For production, explicitly review the canonical six-mint set—CASH, USDG, PYUSD,
USDC, USDT, and USDS—and pass their comma-separated addresses through
`--enabled-stable-mints`; do not rely silently on the command default.
The dry run emits seven `approvalFence` values. Copy those reviewed values into
the corresponding `YIELD_ALT_SHARED_EXPECTED_*` operator inputs. The write
requires all seven and re-derives the catalog from a fresh finalized snapshot;
any hash/count drift or minimum-source-slot regression aborts before a database
write. The reviewed source slot is a minimum freshness fence because a later
finalized RPC read normally advances; content identity remains exact through
the desired-set, enabled-mint, reserve-set, and ordered-address hashes plus the
reserve and address counts. The reserve-set hash/count describe current
new-target eligibility; the desired-set, ordered-address hash, and address
count cover the broad source-safe and retention-safe physical union. Review the
emitted `knownStableReserveCount`, `requiredSourceReserveCount`,
`sourceOnlyAddressCount`, `retainedOnlyAddressCount`, and
`appendedAddressCount` as part of approval. If this broad inventory exceeds one
physical high-water, review the deterministic shard count and per-shard counts;
every shard must be at most the high-water and their ordered union must equal
the logical catalog. Target filtering must not be used to make it fit.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-shared-catalog -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --catalog-version "$YIELD_ALT_CATALOG_VERSION" \
    --enabled-stable-mints "$EARN_ROUTER_ENABLED_STABLE_MINTS"'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-shared-catalog -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --catalog-version "$YIELD_ALT_CATALOG_VERSION" \
    --enabled-stable-mints "$EARN_ROUTER_ENABLED_STABLE_MINTS" \
    --expected-desired-set-hash "$YIELD_ALT_SHARED_EXPECTED_DESIRED_SET_HASH" \
    --expected-enabled-mints-hash "$YIELD_ALT_SHARED_EXPECTED_ENABLED_MINTS_HASH" \
    --expected-ordered-address-hash "$YIELD_ALT_SHARED_EXPECTED_ORDERED_ADDRESS_HASH" \
    --expected-reserve-set-hash "$YIELD_ALT_SHARED_EXPECTED_RESERVE_SET_HASH" \
    --expected-reserve-count "$YIELD_ALT_SHARED_EXPECTED_RESERVE_COUNT" \
    --expected-address-count "$YIELD_ALT_SHARED_EXPECTED_ADDRESS_COUNT" \
    --expected-minimum-source-slot "$YIELD_ALT_SHARED_EXPECTED_MINIMUM_SOURCE_SLOT" \
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

Pause every provisioner instance for the explicit cluster with one durable
database control, read it back, then clear it. These admin commands never load
`POLICY_KEYPAIR`; watch workers poll the control and resume without a deploy.
An operation with a committed broadcast permit may drain, but paused workers do
not receive a new permit. Granting a permit and changing the pause both lock the
same cluster control row in separate short database transactions. If the pause
wins, the persisted signed operation moves to `needs_reconcile` without a send.
If the grant wins, the transaction commits an exact signature/message/control-
epoch permit before RPC; the later pause can commit immediately and the
unresolved permit remains visible as in-flight work until send outcome or
reconciliation retires it. No database transaction or advisory lock is held
across RPC, send, confirmation, or warmup. `YIELD_ALT_PROVISIONING_PAUSED`
remains only a process-local emergency startup stop.

The durable pause blocks execute-mode planning, signing, and broadcast, but it
deliberately permits `--reconcile-only` to drain already persisted signed or
submitted identities. Reconcile-only never loads `POLICY_KEYPAIR` and cannot
create a fresh transaction. Run it while the pause remains set until no
in-flight mutation remains; do not clear the pause merely to make the
pre-cutover probe pass.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --set-provisioner-pause \
    --admin-write \
    --reason "operator pause" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --provisioner-pause-status'

op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --clear-provisioner-pause \
    --admin-write \
    --reason "operator resume" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
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

Before direct cutover, keep routing stopped and the provisioner durably paused,
then run the production-connected rollback-only probe against one existing
active vault row. This special mode branches before status/admin handling and
before signer loading. It checks the mutation queue is drained both before and
after the finalized RPC read, then proves every shared table's shard ordinal,
address, authority, mutation epoch, last-extension slot, ordered addresses,
hash, and count at finalized RPC plus the aggregate bundle hash/count,
passes a deliberate one-address mismatch through the real shared-drift store
path, passes the same typed missing-vault request twice through the real request
upsert, observes one drift signal and one sealed request with zero decisions,
bindings, operations, or sends, and rolls the whole exercise back. It then
proves zero residue and an unchanged active catalog head before persisting only
an immutable PASS parent audit row plus per-shard child rows containing that
exact finalized bundle identity and durable paused control epoch. The singular
parent table fields identify only the selected synthetic drift target. The
rollback-only database transaction locks and
rechecks that epoch plus zero active broadcast permits and zero in-flight
mutations after the finalized RPC read, so a concurrent resume or grant makes
the probe fail instead of producing stale evidence.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --precutover-probe \
    --probe-vault-id "$YIELD_ALT_PROBE_VAULT_ID"'
```

Do not combine the probe with `--execute`, reconciliation, watch/status, pause,
or any admin action. The probe neither reads `POLICY_KEYPAIR` nor creates
durable vault demand; a missing or inactive probe vault, non-exact finalized
shared bundle, stale catalog fence, rollback residue, or non-zero side-effect
count is a hard failure. Any leased, signed, submitted, reconciling, or otherwise
in-flight ALT mutation is also a hard failure; wait for it to drain and rerun.

After the shared v2 catalog and demand-driven provisioner path are verified,
perform the direct cutover with one finalized-RPC plus database-fenced action.
The provisioner first loads every table in the exact active shared ALT bundle
at finalized commitment and proves shard ordinal, table address, authority,
lifecycle, ordered membership, hash, usable count, verification slot, and
mutation epoch against a database
preflight. The database transaction locks and rechecks the durable pause,
requires zero active broadcast permits and zero in-flight mutations, consumes
the latest immutable PASS probe for the same pause epoch and exact
catalog/manifest/bundle identity, and rejects later operation or permit
mutations. It also rechecks the complete finalized RPC observations (bundle
hash/count plus every shard's table id/address, authority, mutation epoch, slot,
last-extension slot, ordered addresses/hash/count), requires the packed-vault
family, sets global
`reusable_only` with force-legacy disabled, and aligns every per-vault override.
There is deliberately no all-vault coverage precondition:

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --activate-reusable-only \
    --admin-write \
    --reason "activate demand-driven reusable v2 routing" \
    --updated-by "$YIELD_ALT_OPERATOR_ID"'
```

Cutover intentionally does not clear the durable provisioner pause. Once the
new monitor and provisioner are both pinned to the approved image, clear the
pause so genuine missing-vault demand can be packed immediately; this is the
direct production start, not a canary phase.

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run same-mint:alt-provisioner -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --clear-provisioner-pause \
    --admin-write \
    --reason "start demand-driven reusable v2 provisioning" \
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
to sign it. The cleanup report exposes the returned operation identity/state
and a real `queuedProvisionerOperationCount`; reruns use the same metadata-
fenced idempotency key. A registered-only run does not load a signer or send a
transaction. Direct signing is reserved for verified, imported, familyless
legacy tables controlled by the audited legacy authority. Both inventories are
database-native: registered v2 retirement rows are separate from the complete
immutable imported-legacy fleet. Cleanup requires an explicit RPC in every
mode and verifies its genesis hash before its first chain read; the examples
inject `SOLANA_RPC_URL` through 1Password.

```sh
op run --env-file=.env.1password -- sh -c \
  'unset YIELD_ROUTE_LOOKUP_TABLES
   bun run same-mint:alt-cleanup -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --scan-history \
    --history-limit "$YIELD_ALT_HISTORY_PAGE_SIZE" \
    --min-slot "$YIELD_ALT_CLEANUP_MIN_SLOT"'

op run --env-file=.env.1password -- sh -c \
  'unset YIELD_ROUTE_LOOKUP_TABLES
   bun run same-mint:alt-cleanup -- \
    --cluster "$YIELD_ALT_CLUSTER" \
    --authority "$YIELD_ALT_POLICY_PUBKEY" \
    --recipient "$YIELD_ALT_POLICY_PUBKEY" \
    --authority-key-env POLICY_KEYPAIR \
    --scan-history \
    --history-limit "$YIELD_ALT_HISTORY_PAGE_SIZE" \
    --min-slot "$YIELD_ALT_CLEANUP_MIN_SLOT" \
    --expected-fleet-count "$YIELD_ALT_LEGACY_EXPECTED_COUNT" \
    --expected-fleet-hash "$YIELD_ALT_CLEANUP_FLEET_HASH" \
    --max-lamports "$YIELD_ALT_MAX_LAMPORTS" \
    --budget-window-seconds "$YIELD_ALT_BUDGET_WINDOW_SECONDS" \
    --simulate-before-submit \
    --execute'
```

Execute mode ignores `YIELD_ROUTE_LOOKUP_TABLES` by design, while dry-run mode
still treats it as a protection input. Unset it for both commands, as shown,
so the approved preview and the mutation inventory are identical.

The dry run must load the complete immutable imported fleet (including closed
rows), perform finalized batched account reads, and paginate signer history to
the approved boundary or exhaustion. It prints `legacyFleetCount`, stable
`inventoryFleetHash`, `historyMutationSetHash`, and per-authority page evidence;
copy the count/hash into the separately approved execute environment. Do not
substitute the importer's registry hash. Execute refuses a partial fleet, a
changed immutable count/hash, incomplete history, any non-policy legacy
authority, a non-policy recipient, or an exhausted budget. Run it once to
deactivate all eligible active legacy tables. Verify each signature finalized
and each RPC lifecycle changed to deactivated, then wait for the actual
SlotHashes cooldown. Run the full zero-reference dry run again; the immutable
fleet hash stays stable while lifecycle/history evidence expands. Close is
complete only after finalized RPC absence for every approved table and the
recorded policy-account lamport delta matches the reconciled reclaimed-rent
evidence.

Treat `--execute`, rollout changes, rollback, and cleanup as production actions
when pointed at production. They require the separate approvals described by
the verifier; these examples do not authorize running them.

Use the verifier document for the exact implementation and production verdict
format. A successful dry run or provisioner no-op is not by itself a production
migration PASS.
