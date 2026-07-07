# Idle Vault Routing Verifier

This document is the fixed verifier for idle-vault routing in the Earn router.
It is not an implementation checklist. Treat every section below as evidence a
skeptical reviewer can run against the repo, local dry-runs, and production
state before declaring the lane ready.

## Goal

PASS means active Earn vaults with meaningful idle USDC are routed into the best
eligible Kamino reserve by `loyal-same-mint-yield-monitor`, without depending on
the wallet balance-sweep worker.

Required end state:

- Idle USDC already inside an Earn vault is a first-class router input.
- The router picks a fresh policy-eligible Kamino reserve for the same liquidity
  mint and only plans a deposit when the APY edge is positive.
- The existing `rebalance_decisions` lifecycle remains the money-movement lock.
- Autonomous routing uses `POLICY_KEYPAIR`, whose derived pubkey must be present
  in the active route policy's delegated signer allowlist.
- The production worker stays on the pinned `light-workers` GHCR image path.

Verdict: PASS only if every required proof below passes, or any remaining FAIL
is an explicit external rollout blocker with no hidden code gap.

## Source Semantics

Required proof:

- Idle vault USDC is token liquidity held by the vault ATA.
- Idle vault USDC is never modeled as a Kamino reserve position, collateral
  amount, or share amount.
- Reserve liquidity still uses `route_amount_semantics =
  redeemable_liquidity_amount` when planning same-mint reserve rebalances.
- Idle deposit decisions make the idle source explicit with
  `execution_plan.kind = 'idle_vault_deposit'`,
  `execution_plan.source_kind = 'idle_vault'`, `source_reserve IS NULL`, and
  `source_snapshot_id IS NULL`.

Verifier query:

```sql
SELECT id, vault_id, status, source_reserve, source_snapshot_id, execution_plan
FROM loyal_yield.rebalance_decisions
WHERE execution_plan->>'kind' = 'idle_vault_deposit'
  AND (
    source_reserve IS NOT NULL
    OR source_snapshot_id IS NOT NULL
    OR execution_plan->>'source_kind' <> 'idle_vault'
    OR execution_plan ? 'sourceReserve'
  );
```

Expected: zero rows.

## Planner Proof

Required proof:

- `same-mint-yield-monitor` loads current idle balances for active vaults in a
  batch keyed by `vault_id`.
- It exposes `--min-idle-deposit-raw` and defaults it to `1000000`.
- Idle balances below `1000000` raw USDC are ignored and must not create
  `rebalance_decisions`.
- Idle planning runs before normal same-mint rebalance planning.
- In fleet mode, if any vault has a plannable idle deposit, that poll reports or
  executes the idle deposits first and explicitly defers normal same-mint
  rebalances for other vaults to a later poll.
- V1 idle planning is USDC-only.
- The planner applies threshold, policy, mint, freshness, and positive-edge
  gates before emitting a plan.
- The selected target is the best fresh policy-eligible candidate where
  `liquidity_mint == idle.mint`.
- Planned idle source APY is `0` and edge is the target APY in bps.
- Below-threshold, no-fresh-candidate, non-USDC, and no-positive-edge states are
  reported as skipped states, not malformed decisions.
- Fleet discovery derives the optimizer signer from `POLICY_KEYPAIR` and only
  discovers vaults whose active policy delegates to that pubkey.

Verifier command:

```sh
op run --env-file=.env.1password -- sh -c 'cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor -- --once --all-active-vaults'
```

Expected: dry-run output shows idle balance state and either a valid
`idle_vault_deposit` plan, a clear idle skip reason, or
`skipped_normal_rebalance_deferred_for_idle_vault_deposit` for vaults deferred
because another vault has idle liquidity priority.

## Executor Proof

Required proof:

- `same-mint-reserve-swap` supports
  `--deposit-idle-vault-reserve <RESERVE> <AMOUNT_RAW>`.
- Idle deposit mode uses `POLICY_KEYPAIR` as the outer fee payer and delegated
  policy signer.
- Idle deposit mode derives the `POLICY_KEYPAIR` pubkey and fails closed unless
  the active route policy allows that pubkey as a delegated signer.
- Idle deposit mode does not read or require `SOLANA_TESTING_PK`.
- It validates expected idle token account, observed slot, observed time,
  liquidity mint, amount, target APY, and edge before writing or submitting.
- It verifies the live vault ATA balance is at least the planned amount.
- It fails closed when the DB idle row is stale above the live chain balance.
- It builds no source withdraw; idle liquidity is deposited from the live vault
  ATA.
- If the target Kamino obligation is missing, idle mode first uses the existing
  market-scoped `init_obligation` route/setup policy with `POLICY_KEYPAIR` as
  fee payer and delegated signer, confirms that setup transaction, reloads
  chain/policy state, and only then builds the deposit through
  `build_initial_reserve_deposit_policy_plan`.
- Because the existing setup policy constrains Kamino's inner `fee_payer` to the
  vault, idle setup may include a public `POLICY_KEYPAIR` SOL transfer that
  tops the vault up to the KLend obligation rent floor before the protected
  `init_obligation`. That top-up must be exact, visible in dry-run JSON as
  `missingObligationSetup.vaultRentTopUp`, and encoded in
  `execution_plan.setup_obligation_vault_rent_top_up_lamports`.
- If no authorized `init_obligation` policy path exists, or setup simulation,
  submission, confirmation, or reload fails, the idle decision is failed with an
  explicit blocker and no deposit is submitted.
- It simulates before submission, submits only when `--execute` is present,
  confirms, and reconciles all policy-eligible reserves for that mint.
- It does not create fresh ALTs as part of normal idle execution.

Verifier command:

```sh
cargo check -p loyal-yield-orchestrator --bin same-mint-yield-monitor --bin same-mint-reserve-swap
```

Expected: both binaries compile, source inspection shows idle mode has no
`SOLANA_TESTING_PK` dependency, and execution signer checks name
`POLICY_KEYPAIR`, not `YIELD_ROUTER_KEYPAIR`. Dry-run output for a missing target
obligation shows `missingObligationSetup` plus `policyDepositRequiresSetup:
true`, while execute mode records an `idle_vault_deposit` decision whose
`execution_plan.setup_obligation_before_deposit = true` before sending setup or
deposit transactions. If the vault lacks SOL for Kamino obligation rent, dry-run
also shows the exact `vaultRentTopUp` amount and the DB plan records the same
lamports in `setup_obligation_vault_rent_top_up_lamports`.

## DB Guardrails

Required proof:

- Migration `0008_route_lookup_tables` is registered with live checksum
  `d20151ef6d6076961195da6c6cf3b4e11bb3e2045f729bdf4b118f6c7d3ddc34`.
- Migration `0009_idle_vault_routing` owns the routing-side
  `vault_idle_token_balances_current` shape, adds
  `idle_vault_liquidity_available`, and allows idle decisions with no source
  reserve.
- There is no separate idle-claim table in v1.
- No malformed idle decisions exist.
- No stale active decisions are stuck.
- No idle decisions exist for `amount_raw < 1000000`.
- Active vaults with `amount_raw >= 1000000` have before and after idle balance
  evidence, or a recorded blocker after rollout.
- Setup-aware idle decisions encode the setup step when required, so historical
  missing-obligation blocker decisions cannot be mistaken for the new
  executable plan.

Migration verifier:

```sh
op run --env-file=.env.1password -- sh -c 'bun run yield:migrate:check'
```

Malformed idle decisions:

```sql
SELECT id, vault_id, status, decision_reason, source_reserve, source_snapshot_id,
       execution_plan
FROM loyal_yield.rebalance_decisions
WHERE execution_plan->>'kind' = 'idle_vault_deposit'
  AND (
    decision_reason::text <> 'idle_vault_liquidity_available'
    OR source_reserve IS NOT NULL
    OR source_snapshot_id IS NOT NULL
    OR target_reserve IS NULL
    OR target_liquidity_mint IS NULL
    OR execution_plan->>'source_kind' <> 'idle_vault'
    OR execution_plan->>'target_reserve' IS NULL
    OR execution_plan->>'liquidity_mint' IS NULL
    OR execution_plan->>'amount_raw' IS NULL
    OR execution_plan->>'idle_token_account' IS NULL
    OR execution_plan->>'observed_slot' IS NULL
    OR execution_plan->>'target_supply_apy_bps' IS NULL
    OR execution_plan->>'edge_bps' IS NULL
  );
```

Expected: zero rows.

Below-threshold idle decisions:

```sql
SELECT id, vault_id, status, amount_raw, execution_plan
FROM loyal_yield.rebalance_decisions
WHERE execution_plan->>'kind' = 'idle_vault_deposit'
  AND amount_raw < 1000000;
```

Expected: zero rows.

Stale active decisions:

```sql
SELECT id, vault_id, status, decision_reason, updated_at, execution_plan
FROM loyal_yield.rebalance_decisions
WHERE status IN ('planned', 'simulating', 'ready', 'submitted', 'confirming')
  AND updated_at < now() - interval '15 minutes';
```

Expected: zero rows, unless each row has an operator-owned blocker.

Before and after idle balance table:

```sql
SELECT mv.id AS managed_vault_id,
       mv.settings,
       mv.vault_index,
       mv.vault_pubkey,
       idle.mint,
       idle.amount_raw,
       idle.token_account,
       idle.observed_slot,
       idle.observed_at,
       last_idle_decision.id AS last_idle_decision_id,
       last_idle_decision.status AS last_idle_decision_status,
       last_idle_decision.updated_at AS last_idle_decision_updated_at
FROM loyal_yield.managed_vaults mv
JOIN loyal_yield.vault_idle_token_balances_current idle
  ON idle.vault_id = mv.id
LEFT JOIN LATERAL (
  SELECT rd.id, rd.status, rd.updated_at
  FROM loyal_yield.rebalance_decisions rd
  WHERE rd.vault_id = mv.id
    AND rd.execution_plan->>'kind' = 'idle_vault_deposit'
  ORDER BY rd.updated_at DESC
  LIMIT 1
) last_idle_decision ON true
WHERE mv.active = true
  AND idle.amount_raw >= 1000000
ORDER BY idle.amount_raw DESC;
```

Expected: after rollout, routed vaults do not remain above threshold unless the
latest decision records a blocker or a newer idle balance arrived after routing.

## Rollout Proof

Required local commands:

```sh
git diff --check
cargo fmt --all -- --check
cargo check -p loyal-yield-orchestrator --bin same-mint-yield-monitor --bin same-mint-reserve-swap
op run --env-file=.env.1password -- sh -c 'bun run yield:migrate:check'
op run --env-file=.env.1password -- sh -c 'cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor -- --once --all-active-vaults'
op run --env-file=.env.1password -- sh -c 'RUN_LOCAL_CHECKS=1 scripts/verify-same-mint-worker-fixes.sh'
```

Required deployment proof after operator approval:

- Apply migration `0009_idle_vault_routing` to Yield Neon and rerun
  `bun run yield:migrate:check` through 1Password until it passes.
- Run the local one-shot fleet dry-run and targeted executor dry-runs before
  deployment. If eligible targets are missing obligations, dry-run must show the
  policy-authorized `missingObligationSetup` transaction and must not require
  `SOLANA_TESTING_PK`.
- Build and push the worker image with the `worker-images` GitHub Actions
  workflow.
- Render services keep using immutable `ghcr.io/loyal-labs/loyal-yield-routing:sha-<commit>`
  images for `light-workers`.
- Render readback shows `loyal-same-mint-yield-monitor` still runs the current
  command shape with the pinned image, not `runtime: docker` or a worker
  `dockerfilePath`.
- Render readback shows `loyal-same-mint-yield-monitor` has `POLICY_KEYPAIR`
  configured, does not have `SOLANA_TESTING_PK`, and no normal idle-routing path
  requires `YIELD_ROUTER_KEYPAIR`.
- The deployed command polls every 300 seconds, includes `--all-active-vaults`
  and `--execute`, and keeps `--rebalance-cooldown-seconds 300` unless an
  operator intentionally changes those values.
- Production logs show at least one real poll cycle with no `reconcile_failed`,
  `unsupported_amount_semantics`, stale active decision, or missing signer error.
- Production logs show the policy signer pubkey derived from `POLICY_KEYPAIR`
  matches the delegated signer on at least one discovered active policy with
  idle USDC above threshold.
- Production logs show each eligible idle vault either reaches
  `planned_idle_vault_deposit_dry_run`, `idle_vault_deposit_executed`, or an
  explicit blocker.
- Production logs may show
  `skipped_normal_rebalance_deferred_for_idle_vault_deposit` for non-idle vaults
  while idle deposits are being prioritized.
- Post-deploy DB and RPC evidence show idle balances above `1000000` raw USDC
  were routed or blocked with explicit evidence, and balances below that floor
  were not touched.

Overall verdict: PASS only when source semantics, planner proof, executor proof,
DB guardrails, and Render rollout proof all pass.
