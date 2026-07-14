# Earn Reusable ALT Migration Verifier

Use this document as the fixed verifier-first goal for replacing exact-route
Address Lookup Table allocation with reusable shared-market and packed-vault
tables in `loyal-yield-routing`.

This verifier checks observable end state, not whether an implementation plan
was followed. Do not mark it PASS because migration `0017` exists, a new worker
compiles, or one route can use a reusable table. A skeptical runner must be able
to prove the complete implementation and its safety boundaries from schema,
code, tests, dry runs, database readbacks, compiled v0 messages, and, after
separate operator approval, direct production-migration evidence.

Do not weaken this verifier to match a partial implementation. If a required
condition is discovered to encode the wrong safety goal, update this document
explicitly and record the reason before continuing.

Operator correction recorded 2026-07-14: this migration creates durable v2
ALTs; it does not create any new exact-scope legacy ALT. Bootstrap the complete
stable shared-market address catalog once, then let packed vault-data shards
grow only from genuine route attempts. There is no fleet-wide vault backfill
and no requirement that every valued or policy-enabled vault have reusable
coverage before cutover. A vault's first missing-coverage attempt must fail
before decision creation or fund movement, seal an idempotent typed request,
and succeed on a later monitor cycle after the provisioner has verified the
required packed binding.

Operator correction recorded 2026-07-14: production uses one direct switch to
`reusable_only`, not a canary or cohort expansion. Cutover readiness means the
fully populated shared v2 family, packed-shard allocator, request path, and
`POLICY_KEYPAIR` provisioner are operational. It does not mean pre-provisioning
the current fleet. The normal monitor should begin attempting live
optimizations immediately after the switch so demand is discovered and served
without manufacturing unused tables.

Operator correction recorded 2026-07-13: reusable ALT creation, extension,
deactivation, closure, fees, and rent use the existing standard
`POLICY_KEYPAIR`. The durable family authority and payer must both equal that
policy public key. A separate ALT-manager key is neither required nor allowed
for this migration. The provisioner remains a dedicated mutation process; the
word "dedicated" describes the worker boundary, not a separate signing
identity. Closed legacy ALT rent must return to the same policy account.

Operator approval recorded 2026-07-13: after implementation and pre-cutover
proof pass, perform the production migration directly, verify it, then retire,
deactivate, wait the mandatory SlotHashes cooldown, and close/refund the old
ALTs. This is the explicit approval required by checks 14-20; it does not waive
any proof, cooldown, or fresh zero-reference gate.

Verifier correction recorded 2026-07-14: the signerless pre-cutover probe is a
production-connected, finalized-RPC, rollback-only verification path. It is
eligible production evidence because it exercises the real database store
paths, commits no route demand or ALT mutation, proves zero residual request,
binding, operation, decision, or send, and retains only an immutable audit
summary. It is not permission to manufacture a production vault binding.

Verifier correction recorded 2026-07-14: the provisioner is the only process
allowed to mutate reusable v2 ALTs. The separately approved, exhaustive legacy
cleanup command is the sole post-cutover exception and may only deactivate or
close imported familyless legacy tables under check 19's fresh fleet, identity,
simulation, finality, cooldown, zero-reference, and refund fences. Normal route
workers may never emit an ALT-program mutation.

Verifier correction recorded 2026-07-14: the first reusable generation and a
vault's first packed binding have no honest reusable predecessor. Requiring a
duplicate standby fleet solely to manufacture an initial rollback target would
double rent and defeat the reuse goal. Once the no-legacy worker is deployed,
global force-legacy is a fail-closed emergency stop, not a path back to the old
tables. Every previous reusable generation or binding that actually exists
after a later rollover/relocation must be validated and kept through its
rollback window; absence on first activation is valid and must not be
represented by a fabricated pointer.

## Goal

Earn routing must reuse a logical shared-market ALT family plus packed
multi-vault ALT shards without creating or extending tables during normal money
movement.

The target resolution path is:

```text
route plan for vault V
  static accounts        -> v0 message static keys
  shared-market accounts -> contributing shards in active shared generation
  vault accounts         -> V's active packed-shard binding
                           -> compile -> coverage -> packet check -> simulate
```

The durable architectural rules are:

- route/action builders own account requirements and semantic classification;
- logical families, manifests, and bindings own reusable address sets;
- physical ALTs are append-only, replaceable transport artifacts;
- one logical shared-market family deterministically append-packs its complete
  ordered manifest into as many physical shards as required per generation;
  every physical shard stays at or below the configured high-water mark, shard
  ordinals and membership ranges are stable, and routes select only the
  contributing subset. The production bootstrap evidence is 237 logical
  addresses against a 219 per-table high-water, so the required exact shape is
  two shards containing 219 and 18 addresses—not lowered capacity evidence,
  truncation, or a legacy table;
- vault-dependent addresses are packed into bounded multi-vault shards by
  default, with dedicated tables only for measured outliers;
- source/target route keys are readiness/audit fingerprints, never owners of
  new physical tables;
- the normal executor is reuse-only and fails closed before decision creation
  and again before send when coverage is incomplete;
- reusable v2 ALT creation, extension, rollover, deactivation, and closure
  belong to a dedicated, budgeted provisioner/reconciler using
  `POLICY_KEYPAIR`; imported familyless legacy tables use only the separately
  audited cleanup exception above;
- existing mixed exact-route tables are imported only for exact audit and rent
  recovery; no new legacy table may be created;
- legacy resolution is removed from the deployed normal worker before the old
  tables are retired, deactivated, cooled down, closed, and refunded.

## Verdict Levels

This document has two explicit verdicts.

### Implementation Verdict

`IMPLEMENTATION: PASS` requires every check in **Implementation Required
Checks** to pass. It means the repository is ready for an additive direct
migration, but it does not claim that production tables were created or traffic
was cut over.

### Production Migration Verdict

`PRODUCTION MIGRATION: PASS` additionally requires every check in **Production
Migration Required Checks**. Applying the production migration, spending from
`POLICY_KEYPAIR`, sending ALT transactions, changing Render, cutting traffic
over, or closing legacy tables requires separate explicit operator approval.
The correction above records that approval for this migration; it does not
authorize unrelated production changes.

Until the production evidence exists, report `PRODUCTION MIGRATION: NOT RUN`,
not PASS.

## Latest Verifier Run

The earlier 2026-07-14 PASS predates migrations `0021` and `0022`, the durable
production fences, the multi-shard shared-market bundle, the source/target
monitor split, and their database regressions. It is historical evidence only
and is not valid for the current implementation worktree. Replace this section
with the exact committed `HEAD`, complete command output, migration 1–22 replay,
database-check count, and immutable image evidence only after the current
implementation verifier has passed from a clean checkout.

```text
1-13. Current implementation verification: NOT RUN
IMPLEMENTATION: NOT RUN
14-20. Production migration checks: NOT RUN
PRODUCTION MIGRATION: NOT RUN
```

## Mandatory Implementation Order

The implementation must preserve these dependency gates. Later work may be
developed in parallel, but it cannot become authoritative before its
prerequisites pass.

1. **Freeze the baseline and account universe**
   - Preserve migration `0008` and its pinned checksum.
   - Enumerate every supported route shape: ordinary same-mint, destination
     obligation setup, farm setup, idle-vault deposit, full withdrawal, and
     policy/setup operations that require ALTs.
   - Measure the exact compiler-eligible address sets and packet sizes.
2. **Land the additive control-plane schema**
   - Add migration `0017`; do not rewrite existing migrations.
   - Preserve migration `0018_earn_activity_realtime`; it is the existing
     realtime migration and is not part of ALT allocation.
   - Add the immutable legacy audit import as migration `0019` and the
     demand-driven shared-market catalog as migration `0020`.
   - Add operational verification migration `0021` for rollback-only probe
     audits, durable provisioner pause state, and durable alert
     incident/delivery state; it must seed no vault manifest, binding, request,
     or ALT operation.
   - Add migration `0022` for a logical shared-market catalog that may span a
     deterministic ordered bundle of physical ALT shards and for immutable
     per-shard pre-cutover evidence; it must not weaken the measured physical
     high-water or Solana's 256-address hard limit.
   - Register all ordered migrations in the dedicated `yield-migrations` runner and schema
     validator.
   - Keep legacy tables and reads valid.
3. **Land typed manifests and allocation rules**
   - Classify accounts from route-builder provenance.
   - Prove static, shared-market, and vault-dependent sets are disjoint and
     complete against the actual v0 compiler result.
   - Allocate complete vault manifests into capacity-reserved packed shards.
4. **Land durable mutation operations**
   - Implement short database reservations, leases/fencing, an operation
     outbox, chain reconciliation, per-suffix warmup, and generation rollover.
   - Package mutation in a dedicated worker command; keep it disabled from
     normal execution.
5. **Land reusable resolution and demand evidence**
   - Resolve the active shared generation and vault binding independently of
     route fee payer.
   - Compile and record exact reusable coverage, packet, and simulation
     evidence; missing coverage seals demand rather than selecting legacy.
6. **Land cutover and rollback controls**
   - Require `reusable_only` with force-legacy disabled for money movement.
     Historical `legacy`, `shadow`, `prefer_reusable`, and force-legacy control
     values are fail-closed stop states once the legacy resolver is removed.
   - Make active-generation and active-binding changes atomic and reversible.
7. **Land cleanup and operational safeguards**
   - Protect every active/standby/bound/leased/pending/in-flight table.
   - Retire only after zero references and the required cooldown.
   - Route reusable-v2 deactivate/close through the provisioner operation
     queue; keep the exhaustive imported-legacy cleanup path separately fenced.
   - Land the signerless rollback probe and the signerless nine-rule alert
     evaluator before production provisioning.
8. **Run the complete implementation verifier**
   - Do not begin production provisioning until `IMPLEMENTATION: PASS`.
9. **Perform the approved demand-driven direct migration**
   - Apply migrations, import legacy state for refund accounting, populate and
     fully provision the durable shared catalog, and start the packed-shard
     provisioner while routing remains fail closed.
   - Drain the old monitor, deploy the no-legacy monitor, and switch directly
     to `reusable_only` without a vault backfill.
   - Only after that switch, prove a genuine missing-vault attempt defers
     safely, seals one request, is provisioned, and succeeds on retry with a
     confirmed optimized movement.
   - Deactivate/close only zero-reference legacy tables after Solana's required
     cooldown, with simulation, finality, and policy-account refund proof.

## Implementation Required Checks

### 1. Additive Schema And Migration Ownership

PASS only if migration `0017` adds a normalized reusable-ALT control plane
without changing migration `0008` or invalidating legacy readers.

Required schema concepts, whether implemented with these exact names or clearly
equivalent normalized names:

- lookup-table families with cluster, kind, desired state, planner/catalog
  version, active generation, and previous generation;
- physical table metadata attached to the existing
  `loyal_yield.route_lookup_tables` registry, including allocation kind,
  family, generation, shard, desired lifecycle, allocation acceptance,
  high-water capacity, usable prefix, verification slot/time, and mutation
  epoch;
- immutable manifests and normalized manifest addresses with subject, semantic
  class/role, desired-set hash, source slot, and planner version;
- a monotonic shared-catalog head that points to one sealed shared manifest and
  records enabled-mint, reserve-set, address-set, source-slot, and revision
  evidence independently of any vault request;
- vault-to-table bindings with allocation mode, reserved capacity, predecessor,
  activation interval, and lifecycle;
- normalized on-chain table membership with table, address, ordinal, added
  operation/slot, usable-after slot, and verification timestamp;
- durable operations and operation-address rows for create, extend, verify,
  rollover, deactivate, and close;
- route-readiness rows keyed by route/requirements fingerprint, not used as
  physical ownership;
- per-vault rollout mode and a global force-legacy control;
- durable cluster-wide provisioner pause state;
- immutable signerless pre-cutover probe audit summaries that cannot become
  provisioning demand; and
- durable semantic alert incidents plus fenced retryable delivery outbox rows.

Required constraints:

- physical `table_address` remains globally unique;
- family identity is unique per cluster and logical name;
- generation/shard identity is unique within a family;
- on-chain ordinals and addresses are unique within a table;
- operation idempotency keys are unique;
- alert incident and delivery idempotency keys are unique;
- only one active vault binding exists for a family/binding ordinal;
- reservations and observed membership cannot exceed the configured hard
  capacity of 256;
- lifecycle/status values are constrained to documented states;
- legacy rows remain valid with nullable v2 columns.

Required migration proof:

```sh
git diff --check
NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin yield-migrations
```

Run the normal migration apply and check against an isolated Postgres/Neon
branch using the repository's 1Password pattern. Production is not acceptable
as the first migration target.

FAIL if route binaries remain independent migration owners, if migration 0008
changes, or if a fresh migration cannot be applied and checked idempotently.

### 2. Typed Account Manifest Is Exact

PASS only if the canonical route/action planning layer emits typed ALT
requirements before account provenance is flattened.

Required classes:

- `MustRemainStatic`;
- `SharedMarket`;
- `Vault`.

Required behavior:

- fee payer, every signer, nonce accounts, and top-level invoked program IDs
  remain static;
- shared-market membership is derived from account roles such as market,
  reserve, mint/supply, oracle/Scope, and reserve farm state;
- vault membership is derived from account roles such as vault/settings,
  obligation, policy/action account, vault ATA, metadata, and farm-user state;
- classification does not depend on database frequency, string prefixes, or a
  hand-maintained list disconnected from route construction;
- for every supported route fixture, the three sets are pairwise disjoint and
  their union matches the compiler-derived account universe;
- the shared and vault manifests contain exactly the ALT-eligible subset, not
  keys the compiler must keep static;
- writable/read-only intent is retained for compilation evidence.

FAIL if the implementation merely filters `Instruction::accounts` for
non-signers or guesses shared accounts from how often they appeared in legacy
tables.

### 3. Packed-Shard Allocator Is Capacity Safe

PASS only if a deterministic allocator places a vault's complete desired
manifest as one unit into a shared vault shard or a measured dedicated fallback.

Required behavior:

- selection considers confirmed membership, distinct pending reservations,
  per-vault growth reservation, configured high-water mark, and maximum vault
  cohort size;
- the high-water mark is derived from
  `256 - largest measured atomic expansion - safety margin` and is configurable;
- candidate selection is deterministic and concurrent callers cannot overbook
  the same physical table;
- each vault's desired manifest is a durable union across all sealed,
  non-cancelled route-requirement cohorts for that vault, so observing a new
  route shape cannot drop addresses needed by an earlier shape;
- shared desired state comes only from the authoritative stable-market catalog;
  a route's shared manifest must be its subset and cannot grow the logical
  shared catalog or any physical shard;
- catalog target eligibility and source/exit retention are separate: active,
  safe, enabled-stable reserves define new targets, while physical shared
  inventory includes every known reserve for the explicit enabled stable mints
  regardless of active/risk state plus every Neon nonzero-held or in-flight
  source. All required reserves are re-decoded in one current finalized RPC
  snapshot. Publication also unions every address from all prior durable
  shared-catalog revisions, preserves the current physical prefix, widens
  role/writability metadata, and appends missing addresses. The current system
  has no durable proof of zero live holdings, policy reachability, pending
  operations, or in-flight route references, so it has no shared-address
  removal path and must never shrink this union;
- optimizer source eligibility follows the same exit-safe split: monitor
  reconciliation and source selection retain every valued, enabled-mint,
  policy-allowed position even when its reserve is inactive, unsafe, or absent
  from the current Timescale supported-reserve inventory. Such a source remains
  eligible until a chain reconciliation observes it at zero. Destination
  selection remains limited to active, safe, enabled, fresh target candidates;
  the retained source universe must never expand target eligibility;
- a binding is reserved transactionally before remote provisioning starts;
- a vault that cannot fit receives a new/preparing shard or dedicated table,
  rather than a partial binding;
- outgrowing a shard creates a complete preparing binding elsewhere, then
  atomically flips the head after warmup and verification;
- a durable desired-head revision supersedes older preparing/warming bindings,
  and activation transactionally rejects a stale revision or an older contender;
- the previous binding remains available for rollback;
- the stable family preserves the logical manifest order and deterministically
  chunks it by physical high-water. Existing shards keep their exact prefix,
  the tail shard extends until full, and later addresses append into additional
  shards; a replacement generation recreates that same complete partition;
- logical shared-catalog size may exceed one table's high-water, while every
  physical shard remains within both that high-water and Solana's 256-address
  hard limit. Publication/planning must fail before durable writes or
  transactions if any address is truncated, duplicated, reordered, or left
  uncovered; silently lowering the measured `21`-address expansion or the
  physical safety contract to force 237 addresses into one table is a FAIL.

Required adversarial evidence includes concurrent reservation, duplicate
address, exact-capacity, one-over-capacity, growth-reservation, relocation, and
an unsafe/inactive Timescale-missing valued source moving only to a safe, fresh
target before disappearing after chain-observed zero.

### 4. Durable And Recoverable ALT Operations

PASS only if ALT mutations are represented by a durable state machine and can
recover from a process crash or ambiguous RPC result without blind replay.

Required operation lifecycle:

```text
queued -> leased -> signed -> submitted -> confirmed -> finalized -> reconciled -> complete
```

Retry/manual states must include equivalents of `retry_wait`,
`needs_reconcile`, `permanent_failure`, and `cancelled`.

Required behavior:

- the planner commits reservations and operation intent in a short transaction;
- no Postgres transaction or advisory lock remains held across RPC, send,
  confirmation, or slot warmup;
- leases have expiry plus a fencing token or mutation epoch;
- the signed transaction signature, message hash, and blockhash expiry are
  persisted before broadcast;
- one exact signed-identity broadcast permit is granted under the current
  durable control epoch in a short committed transaction before send; pause
  administration locks the same control row, unresolved permits survive a
  process crash, and no database transaction or advisory lock crosses RPC;
- if pause commits first, no permit is granted and the signed identity remains
  reconciliation-only; if a permit commits first, pause observes durable
  in-flight work without waiting for the network and cutover remains blocked
  until permit handoff/reconciliation and the mutation state both drain;
- a retry first checks the known signature and reloads the physical ALT;
- the on-chain table is authoritative and reconciliation verifies owner,
  authority, lifecycle, exact address prefix/order, and address hash;
- extensions append only genuinely missing addresses in bounded chunks;
- existing warmed entries remain usable while a new suffix warms;
- readiness tracks a usable prefix or per-address `usable_after_slot`;
- production readiness requires finalization and a later usable slot;
- after simulation and before signing, every mutation attempt reserves its
  worst-case fee plus rent in a PostgreSQL-backed, cluster-wide rolling budget
  under the operation fencing token; the limit survives restarts, serializes
  overlapping workers, replays the same fence idempotently, and denies
  overspend without signing or sending;
- create/extend spend is additionally limited by explicit cluster, payer,
  rate, and concurrency configuration;
- the table mutation authority and payer are the standard policy public key,
  loaded from `POLICY_KEYPAIR` only inside explicit provisioner/cleanup execute
  paths.

FAIL if state exists only in process memory, if an operation is first persisted
after send, or if timeout recovery submits another mutation without chain
reconciliation.

### 5. Dedicated Provisioner Boundary

PASS only if all ALT mutation is owned by a dedicated provisioner/reconciler
command packaged in the existing light-worker image path.

Required behavior:

- normal same-mint, idle-vault, policy, monitor, and E2E execution cannot call
  create, extend, freeze, deactivate, or close;
- the shared-catalog sync command is signerless, sends no transaction, and may
  only publish/plan an explicit, capacity-checked immutable revision;
- the provisioner can be paused independently of routing;
- it supports dry-run/reconcile-only behavior without a signer;
- execute mode requires `POLICY_KEYPAIR`, and the derived public key must match
  both the durable family authority and payer;
- execute mode reports cluster, table, operation kind, address count, expected
  rent/fee budget, and simulation result without printing secret material;
- transaction signing/sending remains disabled unless an operator explicitly
  invokes provisioner execute mode;
- worker packaging does not switch Render services back to source/Dockerfile
  builds.

Required negative inspection:

```sh
rg -n "create_lookup_table|extend_lookup_table|freeze_lookup_table|deactivate_lookup_table|close_lookup_table" \
  crates/loyal-yield-orchestrator/src
```

Every resulting mutation callsite must be exclusive to the dedicated
provisioner/cleanup boundary and unreachable from normal movement.

### 6. Reusable-Only Resolver And Cutover Controls

PASS only if a reusable resolver returns the smallest verified table bundle for
a vault without requiring the route fee payer to equal table authority.

Required behavior:

- `reusable_only` fails closed when reusable readiness is false and never falls
  back silently;
- `legacy`, `shadow`, `prefer_reusable`, and global/per-vault force-legacy are
  stop states that perform no legacy RPC lookup and send no movement after the
  legacy resolver removal;
- the normal reusable bundle is the active shared generation plus the vault's
  active shard binding;
- standby/previous generations are not selected except by an explicit rollback
  pointer;
- finalized RPC authority, lifecycle, presence, prefix, or ordered-membership
  drift persists an immutable report, demotes the shared head, blocks routes
  without creating vault demand, and forces a complete replacement generation;
- tables that contribute no compiled lookup indexes are omitted;
- resolver output is deterministic and records why every selected table was
  chosen;
- source/target scope remains only in readiness/audit records;
- explicit cluster configuration replaces URL-substring cluster inference;
- the same-mint worker, provisioner reconciliation/execute mode, and cleanup
  verify the RPC genesis hash against that explicit cluster before their first
  route or chain work, including rejecting a default-mainnet endpoint labelled
  as devnet, testnet, or localnet;
- accepted RPC endpoints are absolute HTTP(S) URLs, and both structured output
  and fatal error paths omit userinfo, path, query, fragment, and access-token
  material while retaining non-secret method/status context.
- direct cutover obtains exact database preflight evidence, verifies every
  physical table in the same shared bundle at finalized RPC, and atomically
  rejects any revision, generation, bundle hash/count, shard ordinal, table,
  authority, ordered hash, verification-slot, or mutation-epoch change before
  aligning rollout controls.

FAIL if the runtime contains a legacy table discovery/resolution branch, unions
legacy and reusable tables, trusts cached JSON membership without RPC
verification, or filters by route payer as ALT authority.

### 7. Compiler, Packet, And Simulation Proof

PASS only if every supported route shape is compiled with the same v0 compiler
used by execution and records exact table contribution and serialized size.

Required fixtures include:

- ordinary same-mint source withdrawal and target deposit;
- missing destination obligation setup plus later route execution;
- obligation farm-user initialization;
- idle-vault deposit with and without setup;
- full withdrawal/cleanup;
- the widest supported reserve-refresh account set;
- policy/setup operations that use ALTs.

For each fixture, required evidence is:

- static key count;
- each selected ALT and its writable/read-only loaded indexes;
- no selected ALT with zero contribution;
- total unique account-key count within v0 index limits;
- serialized transaction size below 1,232 bytes;
- complete required-address coverage;
- successful simulation or a documented fixture limitation that blocks
  `IMPLEMENTATION: PASS` until resolved.

FAIL if packet safety is inferred from raw address counts or from the fact that
each physical ALT remains below 256 entries.

### 8. Fail-Closed Execution And Mutation Guard

PASS only if the current two fail-closed boundaries remain effective for every
movement lane.

Required behavior:

- missing coverage blocks before decision creation or other durable movement
  intent that could be mistaken for an attempted rebalance;
- coverage is reloaded and rechecked immediately before compile/simulate/send;
- a missing or changed binding, generation, address prefix, or warmed suffix
  blocks send;
- every Address Lookup Table Program mutation instruction—create, extend,
  freeze, deactivate, and close—is rejected from normal route transaction
  submission;
- shared-catalog drift records a structured readiness/repair blocker and does
  not create a vault provisioning request or reserve vault-shard capacity;
- missing vault coverage upserts a structured readiness blocker and one
  idempotent provisioning request without spending SOL;
- normal execution never waits for provisioning or mutates a table inline;
- extra addresses present in a packed vault shard do not weaken Squads policy
  checks.

Required regression evidence must prove rejection of all mutation instruction
variants, not only create and extend.

The existing `docs/plans/alt-funds-leak-verifier.md` remains a binding
non-regression contract. This migration may strengthen it but must not restore
execution-time provisioning or weaken its reuse-only/fail-closed guarantees.

### 9. Idle-Vault And Other Earn Lanes Use The Same Resolver

PASS only if same-mint reserve moves, idle-vault deposits, and any supported
policy/setup transaction use the same manifest and reusable-resolution service.

Required behavior:

- idle-vault execution no longer relies only on CLI/environment ALTs;
- onboarding and policy/catalog changes update desired manifests through one
  control-plane path;
- no lane creates a parallel ALT registry or allocator;
- a route-specific exception is explicit, tested, and fail-closed.

### 10. Binding-Aware Cleanup And Rollover

PASS only if cleanup treats registered state as a lifecycle rather than
protecting every durable table forever or closing tables solely by age.

A table is not reclaimable while any of these are true:

- it is the active or standby family generation;
- it has an active, ready, warming, preparing, or retiring binding;
- an unexpired route-resolution lease references it;
- a queued/leased/submitted/unreconciled operation references it;
- an in-flight prepared transaction or rollback window references it;
- chain and database membership/lifecycle disagree.

Required retirement order:

```text
stop allocations -> remove/retire bindings -> observe zero references
-> deactivate -> wait SlotHashes cooldown -> close -> record reclaimed rent
```

Cleanup must remain dry-run-first and require explicit execute approval.
Eligible registered reusable-v2 tables must enqueue fenced deactivate/close
operations for the provisioner; cleanup may not silently classify and skip them
or sign their mutations directly. Imported familyless legacy tables retain the
separate exhaustive direct-cleanup path required by check 19.

A durable cluster pause control must be observed by every provisioner process,
including an already-running watch instance. A one-shot invocation that prints
`paused` and exits is not a production pause mechanism.

### 11. Observability And Operator Controls

PASS only if structured logs/readbacks expose:

- reusable-ready active vault count and percentage;
- requests deferred for shared-catalog drift or missing vault coverage, with
  explicit reason;
- missing-address and route-readiness blockers;
- operation queue depth, oldest age, attempts, and terminal failures;
- durable rolling-budget reservations, active reserved lamports, actual spend,
  remaining budget, and window end;
- table family/generation/shard, address count, usable prefix, reservations,
  headroom, bound-vault count, and fragmentation;
- database/chain prefix or authority drift;
- open/resolved shared physical-drift evidence and replacement generation;
- provisioning rent/fees spent and cleanup rent reclaimed;
- compiled bundle table count and packet size;
- rollout mode and global force-legacy state.

The repo must document provisioner pause, force-legacy as a fail-closed stop,
previous-generation rollback, vault-binding rollback, reconciliation, and safe
cleanup.

The implementation must also expose a signerless nine-rule alert evaluator,
durable incident/delivery state, deduplicated open/resolved transitions,
restart-safe delivery retries, and a safe all-rules delivery test. Delivery is
verified in production under check 20; implementation PASS still requires the
rule catalog, redaction, idempotency, retry, and no-signer/no-mutation boundaries
to be tested locally and against the isolated database.

### 12. Implementation Verification Commands

PASS only if all relevant commands succeed from the repository root:

```sh
git diff --check
NO_DNA=1 cargo fmt --all -- --check
NO_DNA=1 cargo check -p loyal-actions -p loyal-yield-orchestrator --all-targets
NO_DNA=1 cargo test -p loyal-actions
NO_DNA=1 cargo test -p loyal-yield-orchestrator
NO_DNA=1 bun run yield:migrate -- --help
```

The migration check must run through:

```sh
op run --env-file=.env.1password -- sh -c 'bun run yield:migrate:check'
```

against an isolated branch containing migration `0017`, the existing realtime
migration `0018`, ALT migrations `0019` and `0020`, production-control
migration `0021`, and multi-shard shared-bundle migration `0022`. If the
database branch or credentials are unavailable, report this check FAIL; do not
replace it with source inspection.

The final implementation verdict must be reproduced from a clean checkout of
the exact commit intended for the worker image, with
`REUSABLE_ALT_VERIFY_EXACT_COMMIT=1`. A dirty developer-tree run may be useful
while iterating, but it cannot supply the final PASS evidence.

The exact-commit invocation is:

```sh
op run --env-file=.env.1password -- sh -c \
  'REUSABLE_ALT_VERIFY_EXACT_COMMIT=1 \
   RUN_REUSABLE_ALT_DATABASE_CHECKS=1 \
   YIELD_ALT_VERIFICATION_DATABASE_KIND=isolated \
   bun run verify:reusable-alts'
```

`NEON_DATABASE_URL` must name a disposable database whose name contains
`reusable_alt`. The emitted commit SHA must be the SHA used to build the
immutable monitor/provisioner image.

Any additional checked-in verifier command introduced by the implementation
must also pass. Tests may use localnet/devnet or read-only mainnet fixtures; no
mainnet transaction may be sent for implementation verification.

### 13. Scope And Diff Integrity

PASS only if the implementation modifies the reusable ALT/control-plane slice
and necessary packaging/docs, preserves unrelated user changes, and does not
move transaction submission into a frontend or web route.

Required inspection:

```sh
git status --short
git diff --stat
git diff --check
```

No plaintext secrets, private keys, full database URLs, or signed transaction
bytes may appear in source, logs, tests, docs, or the diff.

The checked-in verifier must scan tracked and untracked text without echoing
matched secret content, reject unmerged paths, and in exact-commit mode reject
every staged, unstaged, or untracked path. Working-tree iteration output is not
eligible evidence for final `IMPLEMENTATION: PASS`.

## Production Migration Required Checks

These checks are mandatory for `PRODUCTION MIGRATION: PASS` but must not be run
without explicit operator approval.

### 14. Production Migration And Legacy Import

- The old legacy-capable monitor is suspended and drained before the approved
  legacy fleet snapshot is taken; there are no planned, submitted, confirming,
  prepared, or in-flight route transactions that can change the fleet.
- Migration `0017`, existing realtime migration `0018`, legacy audit migration
  `0019`, and demand-driven shared-catalog migration `0020` are applied through
  `yield-migrations`; operational verification migration `0021` and
  multi-shard shared-bundle migration `0022` are also applied, and
  checksum/readback succeeds for migrations 1–22.
- Every live legacy physical table is reloaded from RPC before import.
- Imported tables remain `legacy_route`/`legacy_mixed`; none is relabeled as a
  clean shared or vault table.
- Unknown, missing, authority-drifted, or prefix-mismatched tables are blocked
  and reported.
- A historical sorted/NUL-delimited hash is accepted only for an unclassified,
  unimported familyless row whose complete ordered membership independently
  matches finalized RPC. The audited fleet transaction normalizes registry and
  immutable evidence to the reusable-v2 ordered hash before linking the import;
  a failure rolls back every normalization and evidence write, and cleanup
  remains reusable-v2-hash-only.
- Import is audit/refund bookkeeping only and cannot create or extend a legacy
  table.
- The write is fenced by the exact dry-run `inventoryFleetHash`, expected count,
  and finalized RPC evidence; a registry-only hash or stale snapshot is not an
  approval token.

### 15. Shared Generation Provisioning

- The complete measured shared manifest plus configured headroom fits the
  selected physical topology.
- Create/extend operations are durably recorded before broadcast and reconcile
  to finalized chain state.
- Every production mutation has an approved durable cluster-budget reservation
  derived from its simulation; overlapping workers and process restarts cannot
  reset or oversubscribe the rolling lamport ceiling.
- The durable shared generation contains the full stable market-data catalog,
  not merely the accounts seen in one vault's first route.
- A reserve becoming ineligible for new deposits does not remove accounts
  required to withdraw an existing position. Even the first v2 publication
  contains all known reserves for the six explicit stable mints regardless of
  active/safe target eligibility and any additional Neon held/in-flight source;
  each is decoded from the same current finalized RPC snapshot. Later
  publications preserve the current physical prefix and append historical or
  newly required addresses, so a lexicographically earlier addition extends
  the compatible generation instead of reordering it. Any future removal must
  be gated by a durable Neon proof of zero holdings, policies, pending
  operations, and in-flight references; no such removal is permitted by this
  implementation.
- The complete canonical stable-mint set is supplied explicitly. The catalog
  admin write is fenced by the dry-run desired-set, enabled-mint, reserve-set,
  and ordered-address hashes plus exact reserve/address counts and finalized
  source slot; any intervening change fails before a database write.
- Every physical shard is warm and prefix/hash verified, and the aggregate
  bundle identity is exact, before the direct switch.
- A first generation may honestly have no reusable predecessor; later
  generations must retain and validate their real predecessor.

### 16. Demand-Driven Packed Vault Provisioning

- No fleet-wide vault manifest or packed binding backfill is run. Before the
  monitor creates genuine demand, the expected vault binding count may be zero.
- This proof runs after the direct switch in check 18; check numbering groups
  evidence by concern and is not permission to manufacture pre-cutover demand.
- A representative route with missing vault coverage fails before durable
  movement intent and before send, records a readiness blocker, and seals one
  idempotent provisioning request for its exact typed manifest.
- The continuously deployed provisioner leases that request, allocates or
  extends the best-fit packed v2 shard with `POLICY_KEYPAIR`, respects capacity,
  reservation, cohort, budget, and concurrency limits, and verifies warm chain
  state before marking it satisfied.
- The next normal monitor cycle independently resolves the binding and can
  compile, simulate, and execute the route. The rebalance is deferred, not
  lost or silently failed.
- Additional vaults repeat the same path and reuse existing shard headroom;
  only measured outliers receive a dedicated v2 table.

### 17. Demand-Driven Cutover Readiness

- The shared catalog generation is active, warm, and verified; packed-vault
  family metadata is active; and the deployed provisioner is healthy in
  bounded execute mode with the standard policy identity.
- A signerless production-connected rollback probe first verifies the exact
  active shared ALT bundle at finalized RPC. In one short database transaction, it
  injects a deterministic in-memory physical mismatch through the same
  drift-report path and observes a repair signal with zero vault demand; it
  then inserts the same typed missing-vault fixture twice and observes exactly
  one sealed request with zero decision, binding, operation, or send. The
  exercised mutations are rolled back to a savepoint, same-transaction
  readback proves zero routing or provisioning residue, and the transaction
  commits only an immutable parent audit row plus immutable per-shard children
  recording the bundle hash/count, finalized slot, shard ordinals, table
  identities, hashes, counts, mutation epochs, paused control epoch, and
  rollback result. The singular parent table fields remain only the selected
  synthetic drift target. The probe may not load `POLICY_KEYPAIR` or
  pre-provision a production vault.
- Activation remains fenced by desired-head revision, mutation epoch, leases,
  warmup, and chain-prefix verification so a stale provisioner cannot publish
  an obsolete generation or binding.
- The cutover command re-verifies every table in the exact shared bundle at
  finalized RPC and passes that complete evidence into the atomic database
  fence; a database-only warm/active flag or one-shard sample is insufficient.
- The rollback-only probe locks and rechecks the same durable paused control
  epoch after finalized RPC, requires zero active broadcast permits and zero
  in-flight mutations, and persists that epoch in its immutable PASS row.
- Readiness explicitly does not require coverage for every managed vault or
  every route visible in a database snapshot.

### 18. Direct Reusable-Only Cutover

- New onboarding and every new route attempt create v2 manifests/requests;
  nothing can create a new exact-scope legacy ALT.
- Routing is switched directly to global `reusable_only` after check 17; no
  canary cohort, staged rollout, or all-vault coverage gate is required.
- One cluster-fenced cutover transaction proves the exact active shared head,
  sets global `reusable_only` with force-legacy disabled, and aligns every
  per-vault override; no hidden legacy stop/resolver state remains.
- That transaction requires the durable pause, zero active broadcast permits,
  zero in-flight mutations, and the latest immutable PASS probe matching the
  current pause epoch, catalog revision, shared manifest, aggregate bundle
  hash/count, and every physical shard's ordinal, table identity, address,
  authority, mutation epoch, ordered hash, and count. It rejects any later
  operation/permit mutation and atomically rechecks the complete fresh
  finalized bundle observation before changing rollout controls.
- Post-cutover readback shows the normal monitor discovering genuine demand,
  the provisioner draining it, and at least one real funded production vault
  completing a confirmed, reconciled optimization through reusable v2 ALTs.
  Production PASS must wait for an executable positive-edge movement; a no-op
  poll, submitted signature, or provisioner success alone is not substitute
  evidence. That same live sequence supplies check 16 evidence: predecision
  defer, one request, packed-shard provision, next-cycle compile/simulate, and
  confirmed movement.
- The movement proof joins one finalized transaction signature to the route
  decision and exact reusable table bundle, then reloads chain state to prove
  the source position decreased, the selected higher-yield eligible target
  position increased, and Neon reconciliation agrees. A later monitor cycle
  recognizes the optimized position and neither repeats the move nor leaves
  its provisioning request stuck.
- From reusable-only cutover through the start of approved legacy cleanup,
  signer history shows zero ALT mutation outside the reusable-v2 provisioner.
  Across the complete post-cutover history, every ALT-program mutation belongs
  to exactly one of: a v2 provisioner operation durably recorded in
  `lookup_table_operations`, or an explicitly approved per-table legacy
  deactivate/close transaction satisfying check 19. The normal
  monitor/executor produces zero ALT mutation.

### 19. Legacy Resolver Removal And Retirement

- The deployed normal worker has no legacy ALT resolution/fallback path before
  any old table is deactivated. Legacy rows remain only as verified retirement
  inventory.
- Retirement candidates have zero bindings, operations, leases, prepared or
  in-flight references, and fallback observations.
- Cleanup loads the entire immutable imported legacy fleet from Neon, including
  already closed rows, and matches a freshly approved stable count/hash. It
  uses finalized batched account reads and paginates each imported authority's
  finalized signatures with `before` until the approved minimum slot or
  exhaustion. Whole-program ALT scans, one-page/1,000-signature truncation,
  partial table limits, a stale inventory hash, incomplete mutation history,
  any other authority, or any other recipient fail closed.
- Registered reusable-v2 retirement remains a separate database-native
  inventory. Cleanup may enqueue its metadata-fenced deactivate/close
  operations, but only the provisioner may sign or broadcast them.
- Deactivation, cooldown, and close are individually verified from chain.
- The old monitor is drained and the no-legacy immutable image is confirmed
  active before deactivation. Every deactivate/close transaction is simulated
  immediately before submit and verified at finalized commitment.
- A table is never closed merely because the direct switch succeeded;
  closure still requires a fresh zero-reference check immediately before both
  deactivation and close.
- Reclaimed rent, finalized signatures, and recipient balance deltas are
  recorded, and every refund recipient is the standard policy account derived
  from `POLICY_KEYPAIR`.
- Every familyless legacy send has explicit positive `--max-lamports` and
  `--budget-window-seconds` fences. Its exact simulated worst-case fee plus
  rent is reserved before signing in the same PostgreSQL cluster rolling
  window and under the same concurrency lock as reusable-v2 operations.
  Reservation denial, replay-accounting drift, or a caller that attempts to
  persist signed metadata without the exact reservation fails before send.
- The audited legacy cleanup command is the sole allowed post-cutover exception
  to the provisioner-only v2 mutation boundary. Its signatures are disjoint
  from v2 operation signatures and exhaustively account for every remaining
  post-cutover ALT mutation. The set difference between policy ALT mutation
  signatures and the union of provisioner, legacy-deactivate, and legacy-close
  signatures is empty.
- Execute-mode cleanup ignores `YIELD_ROUTE_LOOKUP_TABLES` by design, and the
  operator explicitly unsets it for both preview and execute so the approved
  preview matches the mutation inventory. Before refund accounting, every
  other process able to spend from `POLICY_KEYPAIR` is suspended; partial
  retries retain the original imported fleet hash, paginated history boundary,
  mutation-set hash, stored deactivate/close signatures, and cumulative refund
  proof.

### 20. Production Monitoring

- A separately deployed signerless alert evaluator and durable delivery outbox
  have enabled, versioned rules for readiness regression, missing coverage,
  operation backlog, capacity/headroom, authority/prefix drift, provisioning
  budget, orphaned tables, fallback use, and cleanup anomalies. Open and
  resolved transitions are deduplicated and retried durably. Production
  readback shows all nine rules enabled, and test deliveries from the exact
  deployed image reached the configured operator destination. Expected
  first-use missing coverage uses a bounded grace period and does not trigger
  vault pre-provisioning.
- Render uses the immutable light-worker image built from the exact commit that
  passed checks 1-13, with the monitor, separate provisioner, and signerless
  alert evaluator pinned to that same image tag/digest and their expected
  distinct commands.
- Production logs and database readbacks agree with current chain state.
- A finalized post-cutover optimization satisfying check 18 is retained as the
  end-to-end health proof; production cannot PASS on service health, ALT
  creation, or dry-run evidence without an actual optimized fund movement.

## Required Verdict Format

```text
1. Additive Schema And Migration Ownership: PASS|FAIL - evidence
2. Typed Account Manifest Is Exact: PASS|FAIL - evidence
3. Packed-Shard Allocator Is Capacity Safe: PASS|FAIL - evidence
4. Durable And Recoverable ALT Operations: PASS|FAIL - evidence
5. Dedicated Provisioner Boundary: PASS|FAIL - evidence
6. Reusable-Only Resolver And Cutover Controls: PASS|FAIL - evidence
7. Compiler, Packet, And Simulation Proof: PASS|FAIL - evidence
8. Fail-Closed Execution And Mutation Guard: PASS|FAIL - evidence
9. All Earn Lanes Use The Same Resolver: PASS|FAIL - evidence
10. Binding-Aware Cleanup And Rollover: PASS|FAIL - evidence
11. Observability And Operator Controls: PASS|FAIL - evidence
12. Implementation Verification Commands: PASS|FAIL - evidence
13. Scope And Diff Integrity: PASS|FAIL - evidence
IMPLEMENTATION: PASS|FAIL

14. Production Migration And Legacy Import: PASS|FAIL|NOT RUN - evidence
15. Shared Generation Provisioning: PASS|FAIL|NOT RUN - evidence
16. Demand-Driven Packed Vault Provisioning: PASS|FAIL|NOT RUN - evidence
17. Demand-Driven Cutover Readiness: PASS|FAIL|NOT RUN - evidence
18. Direct Reusable-Only Cutover: PASS|FAIL|NOT RUN - evidence
19. Legacy Resolver Removal And Retirement: PASS|FAIL|NOT RUN - evidence
20. Production Monitoring: PASS|FAIL|NOT RUN - evidence
PRODUCTION MIGRATION: PASS|FAIL|NOT RUN
```

`IMPLEMENTATION: PASS` requires checks 1–13 to pass with direct evidence.
`PRODUCTION MIGRATION: PASS` requires checks 1–20 to pass. No production check
may be marked PASS from a plan, local mock, ordinary dry run, or static
inspection alone. The explicitly required production-connected,
finalized-RPC, rollback-only probe in check 17 is eligible only under its
zero-residue and immutable-audit requirements.
