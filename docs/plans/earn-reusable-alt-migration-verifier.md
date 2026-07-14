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

Operator correction recorded 2026-07-13: production uses one direct cutover,
not a canary or cohort expansion. The pre-cutover fleet is every vault/scope
that currently references a live legacy ALT plus every route that is executable
at the cutover snapshot. Dormant active policy rows and route shapes that have
never materialized are provisioned on demand: their first attempted movement
must fail closed, seal a typed request, and wait for the dedicated provisioner.
Precreating one ALT allocation for every theoretical active-policy route would
reintroduce the waste this migration removes.

## Goal

Earn routing must reuse a logical shared-market ALT family plus packed
multi-vault ALT shards without creating or extending tables during normal money
movement.

The target resolution path is:

```text
route plan for vault V
  static accounts        -> v0 message static keys
  shared-market accounts -> active shared-market generation
  vault accounts         -> V's active packed-shard binding
                           -> compile -> coverage -> packet check -> simulate
```

The durable architectural rules are:

- route/action builders own account requirements and semantic classification;
- logical families, manifests, and bindings own reusable address sets;
- physical ALTs are append-only, replaceable transport artifacts;
- one logical shared-market family may use one physical table while it fits,
  but the schema and resolver support generations and measured sharding;
- vault-dependent addresses are packed into bounded multi-vault shards by
  default, with dedicated tables only for measured outliers;
- source/target route keys are readiness fingerprints and legacy fallback
  identities, never owners of new physical tables;
- the normal executor is reuse-only and fails closed before decision creation
  and again before send when coverage is incomplete;
- ALT creation, extension, rollover, deactivation, and closure belong to a
  dedicated, budgeted provisioner/reconciler;
- existing mixed exact-route tables remain available as an emergency fallback
  until the direct cutover has been verified and every legacy reference has
  drained.

## Verdict Levels

This document has two explicit verdicts.

### Implementation Verdict

`IMPLEMENTATION: PASS` requires every check in **Implementation Required
Checks** to pass. It means the repository is ready for an additive direct
migration, but it does not claim that production tables were created or traffic
was cut over.

### Production Migration Verdict

`PRODUCTION MIGRATION: PASS` additionally requires every check in **Production
Migration Required Checks**. Applying the production migration, funding an ALT
manager, sending ALT transactions, changing Render, cutting traffic over, or
closing legacy tables requires separate explicit operator approval. Those
actions must not be inferred from this verifier or from an implementation task.

Until that approval and evidence exist, report `PRODUCTION MIGRATION: NOT RUN`,
not PASS.

## Latest Verifier Run

Run on 2026-07-13 from a genuinely empty disposable Postgres database
(`loyal_reusable_alt_root_11`). The full checked-in verifier passed, including
all migrations from 1 through 17, a second idempotent migration run, the Rust
schema verifier twice, the SQL schema verifier twice, and the adversarial
database behavior verifier twice. Migration 13's recorded checksum was also
compared with its unmodified source bytes after the blank-database compatibility
path ran; both were
`e2a5a99a1440cf6aa6cd15a095eb531900d61bf2a37965283b4990be3693f0cd`.

```text
1. Additive Schema And Migration Ownership: PASS - migration 0017, fresh apply/check twice, schema verification twice, and migration 0008 byte guard passed
2. Typed Account Manifest Is Exact: PASS - typed static/shared/vault provenance and exact compiler-universe tests passed
3. Packed-Shard Allocator Is Capacity Safe: PASS - deterministic capacity, concurrency, cohort-union, relocation, and stale-head adversarial tests passed
4. Durable And Recoverable ALT Operations: PASS - fenced outbox, signed-before-send, retry, crash recovery, warmup, and reconciliation tests passed
5. Dedicated Provisioner Boundary: PASS - mutation-callsite scan found only the provisioner and cleanup binaries; execute/signer/budget gates passed
6. Reusable Resolver And Rollout Modes: PASS - bounded exact-minimal resolver, all rollout modes, RPC genesis, and endpoint-redaction tests passed
7. Compiler, Packet, And Simulation Proof: PASS - all 13 v0 fixtures had exact coverage and execution, max packet 1,199 bytes, largest expansion 21
8. Fail-Closed Execution And Mutation Guard: PASS - both execution fences and all ALT mutation variants, including nested instructions, passed rejection tests
9. All Earn Lanes Use The Same Resolver: PASS - movement, idle-deposit, setup, and cleanup fixtures use the shared typed planning/resolution path
10. Binding-Aware Cleanup And Rollover: PASS - reference, lease, operation, rollback-window, cooldown, authority, and prefix fences passed
11. Observability And Operator Controls: PASS - readiness, topology, queue, accounting, drift, mode, pause, rollback, and cleanup readbacks are implemented and verified
12. Implementation Verification Commands: PASS - checked-in full verifier and isolated database gates passed, including repeat apply/check/schema/behavior runs
13. Scope And Diff Integrity: PASS - diff, formatting, migration-0008 byte, mutation-boundary, and secret scans passed; unrelated worktree changes remain excluded
IMPLEMENTATION: PASS

14. Production Migration And Legacy Import: NOT RUN - implementation verification did not mutate production
15. Shared Generation Provisioning: NOT RUN - implementation verification did not send transactions
16. Packed Vault Backfill: NOT RUN - implementation verification used disposable database fixtures
17. Direct Cutover Proof: NOT RUN - awaiting production execution
18. Fleet Cutover: NOT RUN - awaiting production execution
19. Legacy Retirement: NOT RUN - awaiting production execution and mandatory SlotHashes cooldown
20. Production Monitoring: NOT RUN - awaiting production deployment and readback
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
   - Register it in the dedicated `yield-migrations` runner and schema
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
5. **Land reusable resolution and comparison modes**
   - Resolve the active shared generation and vault binding independently of
     route fee payer.
   - Compile and record legacy versus reusable evidence while legacy remains
     authoritative.
6. **Land cutover and rollback modes**
   - Support `legacy`, `shadow`, `prefer_reusable`, and `reusable_only` per
     vault, plus a global force-legacy kill switch.
   - Make active-generation and active-binding changes atomic and reversible.
7. **Land cleanup and operational safeguards**
   - Protect every active/standby/bound/leased/pending/in-flight table.
   - Retire only after zero references and the required cooldown.
8. **Run the complete implementation verifier**
   - Do not begin production provisioning until `IMPLEMENTATION: PASS`.
9. **Perform the approved direct migration**
   - Import legacy state, provision shared and vault tables, prove every live
     reusable bundle before changing routing, cut the eligible fleet over in
     one controlled operation, verify the post-cutover path, then deactivate
     and close only zero-reference legacy tables after Solana's required
     cooldown.

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
- vault-to-table bindings with allocation mode, reserved capacity, predecessor,
  activation interval, and lifecycle;
- normalized on-chain table membership with table, address, ordinal, added
  operation/slot, usable-after slot, and verification timestamp;
- durable operations and operation-address rows for create, extend, verify,
  rollover, deactivate, and close;
- route-readiness rows keyed by route/requirements fingerprint, not used as
  physical ownership;
- per-vault rollout mode and a global force-legacy control.

Required constraints:

- physical `table_address` remains globally unique;
- family identity is unique per cluster and logical name;
- generation/shard identity is unique within a family;
- on-chain ordinals and addresses are unique within a table;
- operation idempotency keys are unique;
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
- a binding is reserved transactionally before remote provisioning starts;
- a vault that cannot fit receives a new/preparing shard or dedicated table,
  rather than a partial binding;
- outgrowing a shard creates a complete preparing binding elsewhere, then
  atomically flips the head after warmup and verification;
- a durable desired-head revision supersedes older preparing/warming bindings,
  and activation transactionally rejects a stale revision or an older contender;
- the previous binding remains available for rollback;
- the stable family extends only while the full desired manifest plus headroom
  fits; otherwise it builds a compact next generation;
- if a measured shared-market universe exceeds 256, the planner supports
  co-occurrence-based shared shards rather than silently truncating it.

Required adversarial evidence includes concurrent reservation, duplicate
address, exact-capacity, one-over-capacity, growth-reservation, and relocation
cases.

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
- a retry first checks the known signature and reloads the physical ALT;
- the on-chain table is authoritative and reconciliation verifies owner,
  authority, lifecycle, exact address prefix/order, and address hash;
- extensions append only genuinely missing addresses in bounded chunks;
- existing warmed entries remain usable while a new suffix warms;
- readiness tracks a usable prefix or per-address `usable_after_slot`;
- production readiness requires finalization and a later usable slot;
- create/extend spend is limited by explicit cluster, payer, SOL budget, rate,
  and concurrency configuration;
- the table mutation authority is independent of the route fee payer.

FAIL if state exists only in process memory, if an operation is first persisted
after send, or if timeout recovery submits another mutation without chain
reconciliation.

### 5. Dedicated Provisioner Boundary

PASS only if all ALT mutation is owned by a dedicated provisioner/reconciler
command packaged in the existing light-worker image path.

Required behavior:

- normal same-mint, idle-vault, policy, monitor, and E2E execution cannot call
  create, extend, freeze, deactivate, or close;
- the provisioner can be paused independently of routing;
- it supports dry-run/reconcile-only behavior without a signer;
- execute mode requires the dedicated configured ALT-manager authority/payer;
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

### 6. Reusable Resolver And Rollout Modes

PASS only if a reusable resolver returns the smallest verified table bundle for
a vault without requiring the route fee payer to equal table authority.

Required behavior:

- `legacy` resolves only the existing exact-route path;
- `shadow` executes through legacy and records reusable coverage, contribution,
  packet bytes, and simulation comparison without sending through reusable
  tables;
- `prefer_reusable` uses reusable tables only after exact coverage, warmup,
  generation, binding, packet, and simulation gates pass, otherwise falling
  back to a complete legacy bundle;
- `reusable_only` fails closed when reusable readiness is false and never falls
  back silently;
- a global force-legacy switch overrides per-vault mode without schema rollback;
- the normal reusable bundle is the active shared generation plus the vault's
  active shard binding;
- standby/previous generations are not selected except by an explicit rollback
  pointer;
- tables that contribute no compiled lookup indexes are omitted;
- resolver output is deterministic and records why every selected table was
  chosen;
- source/target scope remains only in readiness and legacy fallback records;
- explicit cluster configuration replaces URL-substring cluster inference;
- the same-mint worker, provisioner reconciliation/execute mode, and cleanup
  verify the RPC genesis hash against that explicit cluster before their first
  route or chain work, including rejecting a default-mainnet endpoint labelled
  as devnet, testnet, or localnet;
- accepted RPC endpoints are absolute HTTP(S) URLs, and both structured output
  and fatal error paths omit userinfo, path, query, fragment, and access-token
  material while retaining non-secret method/status context.

FAIL if the new resolver unions every legacy and reusable table, trusts cached
JSON membership without RPC verification, or continues filtering by route payer
as ALT authority.

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
- missing coverage upserts a structured readiness blocker and idempotent
  provisioning request without spending SOL;
- normal execution never waits for provisioning or mutates a table inline;
- extra addresses present in a shared shard do not weaken Squads policy checks.

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

### 11. Observability And Operator Controls

PASS only if structured logs/readbacks expose:

- reusable-ready active vault count and percentage;
- legacy fallback count and reason;
- missing-address and route-readiness blockers;
- operation queue depth, oldest age, attempts, and terminal failures;
- table family/generation/shard, address count, usable prefix, reservations,
  headroom, bound-vault count, and fragmentation;
- database/chain prefix or authority drift;
- provisioning rent/fees spent and cleanup rent reclaimed;
- compiled bundle table count and packet size;
- rollout mode and global force-legacy state.

The repo must document provisioner pause, force-legacy rollback, previous
generation rollback, vault-binding rollback, reconciliation, and safe cleanup.

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

against an isolated branch containing migration `0017`. If the database branch
or credentials are unavailable, report this check FAIL; do not replace it with
source inspection.

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

## Production Migration Required Checks

These checks are mandatory for `PRODUCTION MIGRATION: PASS` but must not be run
without explicit operator approval.

### 14. Production Migration And Legacy Import

- Migration `0017` is applied through `yield-migrations` and checksum/readback
  succeeds.
- Every live legacy physical table is reloaded from RPC before import.
- Imported tables remain `legacy_route`/`legacy_mixed`; none is relabeled as a
  clean shared or vault table.
- Unknown, missing, authority-drifted, or prefix-mismatched tables are blocked
  and reported.

### 15. Shared Generation Provisioning

- The complete measured shared manifest plus configured headroom fits the
  selected physical topology.
- Create/extend operations are durably recorded before broadcast and reconcile
  to finalized chain state.
- The generation is warm and prefix/hash verified before the direct cutover.
- Its previous generation pointer and rollback command are recorded.

### 16. Packed Vault Backfill

- Every vault/scope with a live legacy ALT reference and every route executable
  at the cutover snapshot is included; current money-moving consumers are
  never deferred behind dormant policy rows.
- Shards respect capacity, reservation, cohort, budget, and concurrency limits.
- Every active binding points to a verified warmed table containing its complete
  manifest.
- No vault is split across an incomplete preparing/active binding.
- A dormant or previously unseen route is not preallocated speculatively. Its
  first attempt must fail before movement intent/send, seal a provisioning
  request, and become eligible only after the normal provisioner verifies it.

### 17. Direct Cutover Proof

- Immediately before cutover, every live route requirement is resolved against
  the intended reusable state, has exact coverage, compiles within the packet
  limit, and simulates successfully where the route supports simulation.
- Packet size, selected tables, table contribution, manifest fingerprints, and
  verification slots are durably recorded.
- The force-legacy switch plus previous-generation and previous-binding
  rollback targets are present and validated without introducing a canary
  cohort or an artificial observation delay.
- The direct cutover is fenced so a concurrent lease, stale desired head, or
  stale provisioner cannot partially activate an obsolete generation or
  binding.

### 18. Fleet Cutover

- Every migration-eligible live legacy consumer and every currently executable
  route is reusable-ready before the fleet switch.
- New onboarding creates manifests/bindings rather than exact-route tables.
- The eligible fleet is switched directly to `reusable_only` only after the
  pre-cutover proof passes; no canary cohort or staged expansion is required.
- Post-cutover readback proves all intended vaults use the selected active
  generation/bindings, and the normal worker completes at least one real
  confirmed, reconciled reusable-ALT movement when an executable movement is
  available.
- Signer history shows zero ALT mutation outside the provisioner after cutover.

### 19. Legacy Retirement

- Retirement candidates have zero bindings, operations, leases, in-flight
  references, and fallback observations.
- Deactivation, cooldown, and close are individually verified from chain.
- A table is never closed merely because the direct fleet switch succeeded;
  closure still requires a fresh zero-reference check immediately before both
  deactivation and close.
- Reclaimed rent and close recipients are recorded.

### 20. Production Monitoring

- Alerts exist for readiness regression, missing coverage, operation backlog,
  capacity/headroom, authority/prefix drift, provisioning budget, orphaned
  tables, fallback use, and cleanup anomalies.
- Render uses the expected immutable light-worker image and separate provisioner
  command.
- Production logs and database readbacks agree with current chain state.

## Required Verdict Format

```text
1. Additive Schema And Migration Ownership: PASS|FAIL - evidence
2. Typed Account Manifest Is Exact: PASS|FAIL - evidence
3. Packed-Shard Allocator Is Capacity Safe: PASS|FAIL - evidence
4. Durable And Recoverable ALT Operations: PASS|FAIL - evidence
5. Dedicated Provisioner Boundary: PASS|FAIL - evidence
6. Reusable Resolver And Rollout Modes: PASS|FAIL - evidence
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
16. Packed Vault Backfill: PASS|FAIL|NOT RUN - evidence
17. Direct Cutover Proof: PASS|FAIL|NOT RUN - evidence
18. Fleet Cutover: PASS|FAIL|NOT RUN - evidence
19. Legacy Retirement: PASS|FAIL|NOT RUN - evidence
20. Production Monitoring: PASS|FAIL|NOT RUN - evidence
PRODUCTION MIGRATION: PASS|FAIL|NOT RUN
```

`IMPLEMENTATION: PASS` requires checks 1–13 to pass with direct evidence.
`PRODUCTION MIGRATION: PASS` requires checks 1–20 to pass. No production check
may be marked PASS from a plan, local mock, dry run, or static inspection alone.
