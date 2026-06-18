# Same-Mint Worker Fix Verifier

Use this as the fixed verifier for the same-mint worker incident follow-up.
It checks the end state, not the implementation steps.

## Goal

The production same-mint worker must be unable to repeat the June 16 failure
mode:

- it must not plan from Kamino collateral/share units unless a routeable USDC
  liquidity amount is explicitly present;
- it must not drop the monitor's APY edge when handing work to
  `same-mint-reserve-swap`;
- it must not write new same-mint decisions whose execution plan lacks
  `redeemable_liquidity_amount` route semantics;
- it must remain deployed through the pinned Render light-worker image path.

Overall PASS requires repo checks, DB guardrail evidence, Render readback, and
logs proving the deployed worker is running the fixed image in dry-run mode
until explicit operator approval re-enables continuous execution.

## Required Checks

### 1. Monitor Fail-Closed Planning

PASS only if `same-mint-yield-monitor` filters candidate source positions
through route amount evidence before creating `plannedMove`.

Observable proof:

```sh
rg -n "route_amount_evidence" crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs
```

A dry-run against active vaults must report `unsupported_amount_semantics` for
vaults whose only non-zero source position is
`kamino_obligation_collateral_deposited_amount` without redeemable liquidity
evidence. It is FAIL if such a vault emits `planned_dry_run` or
`planned_execute`.

### 2. Executor Handoff Integrity

PASS only if the monitor shells into `same-mint-reserve-swap` with explicit
expectations for:

- source snapshot id;
- route amount raw;
- route amount semantics;
- source APY bps;
- target APY bps;
- estimated edge bps.

`same-mint-reserve-swap --optimization-cycle` must refuse to run without those
expectations and must persist those APY/edge values into the prepared decision.
It is FAIL if a monitor-driven route can record `0/0/0` APY/edge when the
monitor saw a positive edge.

### 3. DB Guardrail

PASS only if the scoped guardrail returns zero rows:

```sh
op run --env-file=.env.1password -- sh -c \
  'SAME_MINT_FIX_CUTOFF=2026-06-18T00:00:00Z scripts/verify-same-mint-worker-fixes.sh'
```

Historical confirmed rows before the cutoff are incident evidence and are
tracked by `docs/same-mint-amount-semantics-guardrail.sql`; they do not fail
this rollout verifier.

### 4. Local Checks

PASS only if these pass before commit, image build, or deploy:

```sh
NO_DNA=1 cargo fmt --check
```

```sh
NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin same-mint-yield-monitor --bin same-mint-reserve-swap
```

Equivalent one-shot:

```sh
op run --env-file=.env.1password -- sh -c \
  'RUN_LOCAL_CHECKS=1 scripts/verify-same-mint-worker-fixes.sh'
```

### 5. Render Deployment

PASS only if `loyal-same-mint-yield-monitor` stays on the light-worker image
runtime and the deployed service uses an immutable image tag for the commit that
contains this fix:

```sh
render services --output json
```

The service must remain:

```sh
/usr/local/bin/same-mint-yield-monitor --all-active-vaults --poll-interval-seconds 300 --rebalance-cooldown-seconds 300
```

until explicit operator approval re-enables `--execute`.

After the image deploy:

```sh
render deploys list srv-d8n7gqbbc2fs73emk610 --output json
render logs --resource srv-d8n7gqbbc2fs73emk610 --since 30m --text execute
```

PASS requires logs showing `execute: false` and poll output from the new deploy.

### 6. Re-Enable Gate

Continuous `--execute` is FAIL until a separate approval records:

- local dry-run PASS;
- scoped DB guardrail PASS;
- Render dry-run PASS on the fixed image;
- an operator decision naming the minimum edge and cooldown to use;
- a plan for post-confirmation chain reconciliation before projecting user
  balances from decision amounts.

## Verdict Format

```text
Monitor Fail-Closed Planning: PASS|FAIL - note
Executor Handoff Integrity: PASS|FAIL - note
DB Guardrail: PASS|FAIL - note
Local Checks: PASS|FAIL - note
Render Deployment: PASS|FAIL - note
Re-Enable Gate: PASS|FAIL|DEFERRED - note
Overall Verdict: PASS|FAIL
```

Overall PASS requires every non-deferred section to pass. `Re-Enable Gate` may
be `DEFERRED` only when Render remains dry-run.
