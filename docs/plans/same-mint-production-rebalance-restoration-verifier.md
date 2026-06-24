# Same-Mint Production Rebalance Restoration Verifier

Use this as the verifier-first goal for restoring production same-mint Kamino
reserve rebalancing after the amount-semantics incident and dry-run safety
period.

This verifier checks the end state, not the implementation steps. Do not mark it
PASS because a patch was merged, an image was built, or the worker looks healthy.
It passes only when a skeptical runner can prove from repo files, dry-run output,
database readbacks, Render service state, and production logs that the monitor
can safely execute and that production is actually running with `--execute`.

## Goal

Production `loyal-same-mint-yield-monitor` must safely resume continuous
same-mint rebalancing between eligible Kamino reserves.

Overall PASS requires all of the following:

- chain reconciliation records a routeable liquidity amount for current Kamino
  obligation positions, without pretending collateral/share units are liquidity;
- the executor carries separate source-collateral and redeemable-liquidity
  amounts through planning, validation, decision storage, transaction building,
  and confirmation;
- the monitor still refuses ambiguous amount semantics, but current routeable
  production positions no longer get stuck at `unsupported_amount_semantics`;
- new rebalance decisions include nonzero APY/edge evidence and complete
  route-amount metadata;
- the production Render service is pinned to a fixed immutable light-worker
  image and its command includes `--execute`;
- at least one post-fix production rebalance is confirmed from the Render
  monitor path, or the verifier remains FAIL until a real routeable positive
  edge is available and executed.

Staging dry-run evidence is useful, but staging alone can never make this
verifier pass. Production must be running with `--execute`.

## Required Checks

### 1. Routeable Chain Reconciliation

PASS only if chain reconciliation writes enough metadata for a planner to derive
the exact liquidity amount that can be deposited into the target reserve while
still preserving the source Kamino collateral amount used for withdraw.

Required local evidence:

```sh
rg -n "chain_preview_reconciled_state|redeemable_source_liquidity_amount_raw|source_collateral_amount_raw|amount_semantics|kamino_obligation_collateral_deposited_amount|redeemable_liquidity_amount" crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs crates/loyal-yield-orchestrator/src/domain.rs crates/loyal-yield-orchestrator/src/store.rs
```

Required result:

- A chain-reconciled source position with nonzero Kamino obligation collateral
  includes `amount_semantics = kamino_obligation_collateral_deposited_amount`.
- The same source position includes `source_collateral_amount_raw`.
- The same source position includes
  `redeemable_source_liquidity_amount_raw` or an equivalently named routeable
  liquidity field derived from the live reserve exchange rate.
- `route_amount_evidence(...)` can return
  `route_amount_semantics = redeemable_liquidity_amount` for that source only
  when the redeemable liquidity amount is present and positive.
- A source with collateral/share units but no redeemable liquidity proof still
  fails closed with `unsupported_amount_semantics`.

FAIL if chain reconciliation only writes
`kamino_obligation_collateral_deposited_amount` plus idle vault token balance and
expects the planner to infer a route amount later.

### 2. Dual-Amount Executor Contract

PASS only if the executor no longer passes one raw amount blindly into both the
Kamino withdraw and target deposit instructions.

Required local evidence:

```sh
rg -n "withdraw.*collateral|source_collateral_amount_raw|deposit.*liquidity|redeemable_source_liquidity_amount_raw|expected-route-amount|expected-source.*collateral|expected.*liquidity" crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs crates/loyal-yield-orchestrator/src/types.rs crates/loyal-yield-orchestrator/src/store.rs
```

Required result:

- The monitor handoff includes the source snapshot id, source reserve, target
  reserve, liquidity mint, route liquidity amount, route amount semantics,
  source amount semantics, source collateral amount when applicable, source APY,
  target APY, and estimated edge.
- `same-mint-reserve-swap --optimization-cycle` validates those monitor
  expectations before writing a decision.
- The source Kamino withdraw instruction uses the source collateral amount.
- The target Kamino deposit instruction uses the routeable liquidity amount.
- Decision `amount_raw` represents the routeable liquidity amount, not the
  source collateral/share amount.
- Decision execution metadata preserves both amounts so confirmation and later
  accounting can explain what happened.

FAIL if a code path can still build:

```text
withdraw(source, amount_raw)
deposit(target, amount_raw)
```

without proving that `amount_raw` is correct for both sides.

### 3. Dry-Run Fleet Proof Before Execute

PASS only if the fixed image first proves, in production dry-run, that active
routeable vaults can pass reconciliation and planning without unsafe semantics.

Required command shape:

```sh
op run --env-file=.env.1password -- sh -c \
  'same-mint-yield-monitor --once --all-active-vaults --poll-interval-seconds 300 --rebalance-cooldown-seconds 300'
```

If the fixed binary is not installed locally, use the exact image binary in a
safe one-shot environment. Do not add `--execute` to this dry-run command.

Required output:

- Top-level status is `fleet_poll`.
- `execute` is `false`.
- Candidate data is fresh enough under `maxCandidateAgeSeconds`.
- For every active vault with nonzero current value and policy-eligible same-mint
  candidates, the result is one of:
  - `planned_dry_run`;
  - `skipped` with `already_at_winner_or_no_positive_edge`;
  - `skipped_recent_rebalance`;
  - `skipped_active_decision`.
- No routeable active vault is skipped with `unsupported_amount_semantics`.
- No vault reports `reconcile_failed`, `vault_error`, or missing policy route
  mode unless the database readback proves the vault is not currently eligible.
- Any `plannedMove` includes source/target APY bps, positive edge bps, liquidity
  mint, route liquidity amount, route amount semantics, and source collateral
  amount when the source amount semantics are collateral/share based.

FAIL if dry-run hides the issue by selecting no active vaults.

### 4. Database Guardrails

PASS only if Yield Neon readbacks show no stuck active decision and no malformed
post-fix decision.

Set the cutoff to the commit/deploy time of the restoration fix:

```sh
export SAME_MINT_RESTORE_CUTOFF=<ISO-8601 UTC timestamp>
```

Run read-only queries through 1Password:

```sh
op run --env-file=.env.1password -- sh -c 'psql "$NEON_DATABASE_URL" -X -v ON_ERROR_STOP=1'
```

Required SQL checks:

```sql
SELECT id, vault_id, status, created_at, updated_at
FROM loyal_yield.rebalance_decisions
WHERE status IN ('prepared', 'simulating', 'simulation_ready', 'submitting', 'submitted', 'confirming')
  AND updated_at < now() - interval '15 minutes'
ORDER BY updated_at;
```

Required result: zero rows.

```sql
SELECT id, vault_id, status, decision_reason, amount_raw, execution_plan
FROM loyal_yield.rebalance_decisions
WHERE created_at >= :'SAME_MINT_RESTORE_CUTOFF'
  AND decision_reason = 'target_supply_apy_exceeds_source'
  AND (
    amount_raw IS NULL
    OR amount_raw <= 0
    OR execution_plan->>'route_amount_semantics' <> 'redeemable_liquidity_amount'
    OR NULLIF(execution_plan->>'redeemable_source_liquidity_amount_raw', '') IS NULL
  )
ORDER BY id;
```

Required result: zero rows.

```sql
SELECT id, vault_id, source_apy_bps, target_apy_bps, estimated_edge_bps
FROM loyal_yield.rebalance_decisions
WHERE created_at >= :'SAME_MINT_RESTORE_CUTOFF'
  AND decision_reason = 'target_supply_apy_exceeds_source'
  AND (
    source_apy_bps IS NULL
    OR target_apy_bps IS NULL
    OR estimated_edge_bps IS NULL
    OR estimated_edge_bps <= 0
    OR target_apy_bps <= source_apy_bps
  )
ORDER BY id;
```

Required result: zero rows.

```sql
SELECT c.vault_id, c.reserve, c.liquidity_mint, c.amount_raw, c.has_value,
       c.planning_metadata->>'amount_semantics' AS amount_semantics,
       c.planning_metadata->>'source_collateral_amount_raw' AS source_collateral_amount_raw,
       c.planning_metadata->>'redeemable_source_liquidity_amount_raw' AS redeemable_raw,
       c.observed_at
FROM loyal_yield.vault_reserve_positions_current c
JOIN loyal_yield.managed_vaults v ON v.id = c.vault_id
JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
WHERE v.active = true
  AND p.active = true
  AND 'same_mint_kamino' = ANY(p.route_modes)
  AND c.has_value = true
  AND c.amount_raw > 0
ORDER BY c.vault_id, c.reserve;
```

Required result:

- Every routeable valued source has enough metadata for
  `route_amount_evidence(...)`.
- Any valued source that still lacks redeemable liquidity evidence is treated as
  intentionally non-routeable, and the reason is documented in the verification
  run.

### 5. Local Static Checks

PASS only if the relevant local gates pass before image build and deploy.

```sh
git diff --check
```

```sh
cargo fmt --all -- --check
```

```sh
cargo check -p loyal-yield-orchestrator --bin same-mint-yield-monitor --bin same-mint-reserve-swap
```

If tests are added for the amount conversion or executor handoff, run the
smallest targeted test command that covers them and record the exact command and
result. Do not replace the compile gate with a code search.

### 6. Render Image And Command Readback

PASS only if live Render and repo config agree on the fixed image and production
execution command.

Required service:

- production `loyal-same-mint-yield-monitor`
- service id `srv-d8n7gqbbc2fs73emk610`

Required readback:

```sh
op run --env-file=.env.1password -- sh -c 'render services -o json'
```

```sh
op run --env-file=.env.1password -- sh -c 'render deploys list srv-d8n7gqbbc2fs73emk610 -o json'
```

Required result:

- Runtime is `image`.
- Registry credential is `loyal-ghcr`.
- Image ref is an immutable `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>`
  tag for the commit containing the restoration fix.
- `render.yaml` is not pinned to an older light-worker image for the same
  service.
- Latest deploy status is `live`.
- Production command is exactly the approved execution posture, including
  `--execute`:

```text
/usr/local/bin/same-mint-yield-monitor --all-active-vaults --execute --poll-interval-seconds 300 --rebalance-cooldown-seconds 300
```

If the operator chooses a non-default minimum edge, the command may also include
`--min-edge-bps <BPS>`, but that operator decision must be recorded in the
verification run.

FAIL if production is still dry-run, if the service command omits `--execute`,
if the service uses a mutable image tag, or if live Render is on a different
image than the verifier claims.

### 7. Production Execution Proof

PASS only if production logs and Yield Neon prove that the Render monitor path
actually executed a post-fix rebalance.

Required log readbacks after the `--execute` deploy:

```sh
op run --env-file=.env.1password -- sh -c \
  'render logs --resources srv-d8n7gqbbc2fs73emk610 --start <DEPLOYED_AT_UTC> --text execute --limit 100 --output text'
```

```sh
op run --env-file=.env.1password -- sh -c \
  'render logs --resources srv-d8n7gqbbc2fs73emk610 --start <DEPLOYED_AT_UTC> --text executed --limit 100 --output text'
```

```sh
op run --env-file=.env.1password -- sh -c \
  'render logs --resources srv-d8n7gqbbc2fs73emk610 --start <DEPLOYED_AT_UTC> --level error --limit 100 --output text'
```

Required result:

- Logs show `execute: true` for the production fleet poll.
- Logs show at least one `planned_execute` followed by `executed`, or the JSON
  route execution output for a successful same-mint rebalance.
- Error logs after deploy contain no new `reconcile_failed`,
  `unsupported_amount_semantics`, monitor plan drift, missing-account, or
  transaction failure entries for the executed vault.

Required Yield Neon proof:

```sql
SELECT id, vault_id, status, decision_reason, source_reserve, target_reserve,
       liquidity_mint, amount_raw, source_apy_bps, target_apy_bps,
       estimated_edge_bps, signature, created_at, updated_at,
       execution_plan->>'route_amount_semantics' AS route_amount_semantics,
       execution_plan->>'source_amount_semantics' AS source_amount_semantics,
       execution_plan->>'source_collateral_amount_raw' AS source_collateral_amount_raw,
       execution_plan->>'redeemable_source_liquidity_amount_raw' AS redeemable_raw
FROM loyal_yield.rebalance_decisions
WHERE created_at >= :'SAME_MINT_RESTORE_CUTOFF'
  AND status = 'confirmed'
  AND signature IS NOT NULL
ORDER BY id DESC
LIMIT 10;
```

Required result:

- At least one row exists.
- The row was created after the fixed image was deployed with `--execute`.
- The row has positive `estimated_edge_bps`.
- `target_apy_bps > source_apy_bps`.
- `route_amount_semantics = redeemable_liquidity_amount`.
- `redeemable_raw` is present and positive.
- The confirmed signature is not from a manual local run; it must correspond to
  the Render monitor execution window.

If no live positive edge exists, this section remains FAIL rather than being
waived. Wait for a routeable edge or use an explicitly approved production
canary with a tiny amount. Do not redefine PASS around "no opportunity today."

### 8. Post-Confirmation Chain Reconciliation

PASS only if the executed vault is reconciled from chain after confirmation and
the current rows reflect the actual destination reserve state.

Required evidence:

```sql
SELECT s.id, s.vault_id, s.observed_slot, s.chain_slot, s.observed_at, s.context
FROM loyal_yield.vault_position_snapshots s
WHERE s.vault_id = <EXECUTED_VAULT_ID>
ORDER BY s.id DESC
LIMIT 5;
```

```sql
SELECT c.vault_id, c.reserve, c.liquidity_mint, c.amount_raw, c.has_value,
       c.snapshot_id, c.observed_slot, c.observed_at, c.planning_metadata
FROM loyal_yield.vault_reserve_positions_current c
WHERE c.vault_id = <EXECUTED_VAULT_ID>
ORDER BY c.reserve;
```

Required result:

- The latest snapshot for the executed vault is chain-derived after the
  confirmed rebalance, not only a projection from decision amounts.
- Source reserve current value is zero or reduced as expected.
- Target reserve current value is positive.
- Current position metadata remains routeable or intentionally fail-closed with
  documented reason.

FAIL if UI/accounting state would have to trust only the projected decision row
after confirmation.

## Verdict Format

```text
Routeable Chain Reconciliation: PASS|FAIL - note
Dual-Amount Executor Contract: PASS|FAIL - note
Dry-Run Fleet Proof Before Execute: PASS|FAIL - note
Database Guardrails: PASS|FAIL - note
Local Static Checks: PASS|FAIL - note
Render Image And Command Readback: PASS|FAIL - note
Production Execution Proof: PASS|FAIL - note
Post-Confirmation Chain Reconciliation: PASS|FAIL - note
Overall Verdict: PASS|FAIL
```

Overall PASS requires every section to pass. No section is deferrable for final
completion. In particular, production dry-run is FAIL for this verifier because
the target end state is production running with `--execute`.
