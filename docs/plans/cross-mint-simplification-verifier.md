# Cross-mint simplification verifier

This is the standing goal for removing accidental complexity from cross-mint
Earn routing without weakening custody, finality, recovery, or the already
proven on-chain policy. Run it cold against the repository. A green build is not
enough; the architecture and required behavioral evidence must both pass.

## Orientation facts

- The intended on-chain authority is exactly two immutable generalized Jupiter
  policies: Classic sources and Token-2022 sources. Each authorizes both current
  Jupiter V2 dialects and all canonical Earn destination ATAs.
- A single six-source create is 1,298 bytes and does not fit; each source shard
  is 1,148 bytes and does fit. Two policies are necessary complexity.
- Fresh mainnet evidence already proves all 30 ordered non-self pairs and ten
  history-derived withdraw -> swap -> deposit routes under those two policy
  shapes.
- A read-only live database check at `2026-08-16 16:11:11 UTC` found no deployed
  cross-mint capability table, 4,227 active Earn policies, and zero active Earn
  policies with swap lanes. There is no deployed exact-pair compatibility state
  to preserve.
- Finalized movement-attributed deltas, persisted signed bytes, fencing,
  capacity ownership, source recovery, target fallback, ambiguity quarantine,
  and manual intervention are safety boundaries. This task must not simplify
  them away.

## Target architecture

```text
strict on-chain policy detector
  -> one manifest event
  -> one database row per on-chain policy
  -> pure authorize(source, target) using the canonical registry
  -> one typed immutable route plan
  -> finalized policy readback + fresh Jupiter build
  -> transactional store admission
  -> existing finalized movement/recovery state machine
```

The pair is a query against a policy, not the stored identity of the policy.

## Required checks

### 1. One policy model

- `EARN_STABLECOINS` is the sole canonical six-mint registry. Classic and
  Token-2022 source membership is derived from it rather than repeated in
  detector, store, or planner lists.
- New cross-mint routing has one policy family and one semantic fingerprint.
  Exact-pair create/update builders, detectors, runtime fallbacks, default
  `exact_pair_v1` values, and exact-pair release tests are absent.
- The older all-in-one Earn swap-lane API is not an admission source for V1
  cross-mint planning. The separate swap policy remains mandatory.
- Independent builder and detector logic remains because it protects an
  external byte contract; tests compare their results through Squads behavior,
  not a second test-only wire model.

### 2. One policy row

- The store has one row per generalized on-chain policy account, not one row per
  source/target/dialect Cartesian product.
- A row stores policy identity, source shard, cap, slippage, semantic
  fingerprint, active/finality state, and observation evidence. Canonical
  destinations and dialect indexes are code-owned invariants and are not copied
  into 30 rows.
- One manifest event causes one transactional insert/update. Removal or an
  incompatible observation invalidates that one row.
- Planner admission requires exactly one finalized Classic row and one
  finalized Token-2022 row for the same settings, vault, and delegated signer.
  Half-install, half-removal, ambiguity, stale slots, or duplicate shards fail
  closed.
- Because the feature migrations were never deployed, their first committed
  schema is direct: no create-exact-then-alter-generalized migration sequence,
  compatibility projection, backfill, or importer.

### 3. One plan contract and one owner per check

- Planner, store, and worker share one typed swap-policy binding/route-plan
  contract. Generic JSON may be the persisted encoding, but it is serialized and
  deserialized through that type rather than independently field-parsed.
- The plan does not repeat fields derivable from the canonical registry or fixed
  policy shape: token programs, policy kind, target universe, or a dialect map.
- The fresh Jupiter build chooses the actual dialect. Strict finalized policy
  readback supplies its constraint index.
- Store owns the authoritative transactional database admission and fencing
  check. Worker owns finalized on-chain readback, fresh-build validation,
  balance observation, and execution. Worker does not repeat the store's full
  policy-catalog SQL gate.
- One route-kind field and one canonical typed plan own fingerprint material.
  Missing fields are errors; there are no silent legacy defaults.

### 4. Verifiers test the current design

- The Safe topology verifier creates/decodes the two generalized policies and
  maps every current different-mint topology through them. It creates zero
  exact-pair policies.
- The generalized Squads SBF test must fail when its required fixture is absent;
  it cannot return success without executing policy bytes.
- Environment-dependent DB/live tests are explicitly ignored in generic Rust
  runs and have required wrapper commands that fail or report `NOT_RUN` when
  prerequisites are absent. A skip never counts as release evidence.
- The redundant exact-pair lifecycle test and redundant one-pair live harness
  are removed. Keep the generalized SBF adversarial matrix, current 30-pair
  Jupiter matrix, all-pair mainnet artifact verifier, and ten-route artifact
  verifier.
- Older verifier documentation is deleted or marked superseded so one current
  product boundary remains: six stablecoins, two source policies, separate
  cross-mint opt-in.

### 5. Complexity budget

- In production Rust, SQL migrations, and current release tests, all of these
  searches are empty: `exact_pair_v1`, `JupiterSwapPolicySpec`,
  `CrossMintSwapCapability`, and `cross_mint_swap_capabilities`.
- The measured simplification surface listed below was 33,555 lines before this
  task and is at most 32,555 lines afterward. Deletion must exceed addition by
  at least 1,000 lines; formatting and moving code do not count as deletion.
- No new crate, framework, generic policy DSL, venue abstraction, duplicate saga
  table, or second transaction lifecycle is introduced.
- No new cross-mint function exceeds 250 lines.

Measure the line budget with:

```sh
wc -l \
  crates/loyal-actions/src/{actions,detection,jupiter}.rs \
  crates/loyal-squads-policy-monitor/src/lib.rs \
  crates/loyal-yield-store/src/{store,types}.rs \
  crates/loyal-yield-store/src/fleet_orchestration/movement.rs \
  crates/loyal-yield-store/migrations/003{5,6,7,8}_*.sql \
  crates/loyal-yield-orchestrator/src/fleet_orchestration/observation.rs \
  crates/loyal-fleet-worker/src/cross_mint.rs \
  crates/squads-test-harness/tests/{autonomous_vaults_kamino,cross_mint_pair_policy_lifecycle,cross_mint_safe_topology,cross_mint_generalized_policy}.rs \
  crates/loyal-yield-store/tests/{cross_mint_movement_db,cross_mint_swap_policy_db}.rs
```

If a named file is deliberately deleted, count it as zero. Adjust only the shell
glob needed to measure the same logical surface; do not soften the threshold.

### 6. Behavior and regression gates

- Generalized policy create bytes still fit at 1,148 bytes per shard; the
  six-source variant still fails packet fit; strict decoded semantics and
  fingerprint remain unchanged.
- Both dialects, all six source mints, all 30 non-self pairs, aggregate
  per-source caps, canonical output custody, slippage, zero fee, removal, and
  adversarial mutations pass through Squads SBF/LiteSVM.
- Disposable PostgreSQL applies the final migration set and passes policy
  finality, both-shard admission, duplicate/ambiguity/removal, opt-out race,
  movement restart, reconciliation, recovery, and same-mint regression rows.
- Existing finalized mainnet artifacts still decode under the strict detector
  and terminal reruns perform no new route sends. Fresh value-moving sends are
  unnecessary unless on-chain policy bytes change.
- `cargo fmt --check`, strict targeted Clippy, focused Rust tests,
  `bun run test:squads`, `bun run test:squads:e2e`,
  `bun run verify:cross-mint:store`, current Jupiter/topology verifiers,
  `bun run lint`, `bun run build`, and `git diff --check` pass.

## Implementation order

1. Fix false-green verifiers and freeze generalized policy byte/fingerprint
   behavior.
2. Delete exact-pair runtime/API/test support and derive shard membership from
   the canonical registry.
3. Replace the Cartesian capability projection with one policy catalog row and
   a pure authorization function; squash the undeployed migration shape.
4. Replace duplicate plan parsing with one typed contract and remove the
   worker's duplicate database admission query.
5. Remove stale verifier/docs/harnesses and measure deletion.
6. Run the verifier literally, fix failures, and repeat until all required rows
   pass.

## Verdict

Return exactly one:

- `PASS_CROSS_MINT_SIMPLIFIED`
- `FAIL_DUPLICATE_POLICY_MODEL`
- `FAIL_DUPLICATE_POLICY_STATE`
- `FAIL_DUPLICATE_PLAN_OR_ADMISSION`
- `FAIL_VERIFIER_FALSE_GREEN`
- `FAIL_BEHAVIOR_OR_RECOVERY_REGRESSION`
- `FAIL_EXTERNAL_DEPENDENCY`

Only `PASS_CROSS_MINT_SIMPLIFIED` completes this goal.
