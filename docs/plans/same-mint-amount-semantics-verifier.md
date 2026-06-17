# Same-Mint Amount Semantics Verifier

Use this document as the fixed verifier for the same-mint amount-semantics
incident fix. Do not treat it as an implementation checklist. The work passes
only when a skeptical runner can prove from repo state, Neon rows, Solana chain
state, and local or Render logs that Kamino collateral/share amounts cannot be
planned, submitted, confirmed, or projected as USDC liquidity amounts.

## Goal

Same-mint routing must fail closed unless the route amount is explicitly known
to be USDC liquidity raw units. A chain reconciliation row whose amount
semantics are `kamino_obligation_collateral_deposited_amount` must never feed
`rebalance_decisions.amount_raw`, a Kamino withdraw liquidity amount, a Kamino
deposit liquidity amount, or a confirmed target current-position projection as
if it were USDC.

The verifier is anchored to the June 16 incident:

- wallet `J4dwgp9ahWd3Mm4zjo34hx3EkLUNZiH3av9aKEqKbDJV`
- settings `2B6TiSnDMwD7UMroKupTasGpYSofh38hfoQiYxe1T1TG`
- vault `GRc6yE784gTEgGkpAVA8kYzTgBnFRMXnGffrrUYZck75`
- deposit `480000000` USDC raw
- bad same-mint decision `229` planned `404323479` from collateral semantics
- vault USDC ATA `CBeayrtDtS18CduF36jRm1uFwoTiw3i9onoh3oniJUJb` held idle
  USDC after the bad route

Overall PASS is impossible if a dry-run, live run, or DB readback can still
produce a same-mint `amount_raw = 404323479` from that collateral-semantics
source.

## Scope

The required fix belongs in the yield-routing control loop and route builder:
`crates/loyal-yield-orchestrator/src/domain.rs`,
`crates/loyal-yield-orchestrator/src/store.rs`, and the same-mint binaries that
create chain reconciliation previews and route instructions.

The frontend can improve UX separately. This verifier is for the yield-routing
side: chain reconciliation, planning, decision persistence, route construction,
confirmation projection, Render rollout posture, and live safety evidence.

Do not add or update Rust tests before the real behavior is working. Focused
tests are added only after the fix is proven end to end, or while waiting for a
deployment/Render rollout gate.

## Commands Under Verification

Run secrets-backed commands through:

```sh
op run --env-file=.env.1password -- sh -c '<command>'
```

Required command surfaces:

```sh
cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor -- --once --all-active-vaults
```

```sh
cargo run -p loyal-yield-orchestrator --bin same-mint-yield-monitor -- --once --all-active-vaults --execute
```

```sh
cargo run -p loyal-yield-orchestrator --bin same-mint-reserve-swap -- --settings <PUBKEY> --vault-index 1 --reconcile-from-chain
```

If final command names change, update this section only when the replacement
commands expose the same observable dry-run, execute, reconcile, and DB readback
behavior.

## Required Checks

### 1. Fail-Closed Collateral Semantics

PASS only if a reconciled source position with
`planning_metadata.amount_semantics =
"kamino_obligation_collateral_deposited_amount"` cannot become a planned
same-mint route.

The verifier must run or inspect a dry-run against the affected vault and show
one of:

- an explicit skip/error such as `unsupported_amount_semantics`; or
- a plan whose route amount is not sourced from the collateral amount and whose
  execution plan declares routeable USDC liquidity semantics.

It is FAIL if `draft_same_mint_decision`, `prepare_same_mint_rebalance`,
`same_mint_execution_plan`, or a monitor dry-run can treat collateral
`amount_raw` as same-mint USDC liquidity.

### 2. Typed Route Amount

PASS only if same-mint execution plans distinguish the routeable amount from
Kamino collateral/share diagnostics.

Every new same-mint `execution_plan` must include enough metadata to audit:

- route amount semantics, expected to be `redeemable_liquidity_amount`;
- source reserve and target reserve;
- liquidity mint;
- source collateral/share amount when available;
- redeemable source liquidity amount when available;
- idle vault liquidity amount when available.

It is FAIL if `amount_raw` remains the only persisted route amount evidence or
if its units can be inferred only from prose metadata on a snapshot row.

### 3. Incident Regression

PASS only if the exact incident shape cannot reproduce:

- input/user principal: `480000000`
- chain collateral amount: `404323479`
- idle vault USDC around `75676540`
- routeable total liquidity near `480000000`

A dry-run or verifier harness must prove no same-mint decision is created with
`rebalance_decisions.amount_raw = 404323479` for this case. The accepted
outcomes are either fail-closed before decision creation or a planned route whose
USDC liquidity amount accounts for redeemable Kamino liquidity plus idle vault
USDC according to the chosen implementation.

### 4. Confirmation Projection

PASS only if same-mint confirmation no longer blindly projects
`decision.amount_raw` into the target current position unless that value is
explicitly routeable liquidity.

Preferred proof: after a confirmed route, the worker reconciles from chain and
writes the post-confirmation snapshot from observed chain state.

Minimum acceptable proof: `confirm_same_mint_rebalance` rejects decisions whose
execution plan lacks `redeemable_liquidity_amount` semantics and never writes a
collateral/share count into `vault_reserve_positions_current`.

### 5. DB Guardrail

PASS only if a read-only Neon verifier query can identify any unsafe existing or
new same-mint decisions.

The query must fail the verifier when a same-mint decision is planned,
submitted, or confirmed with missing amount semantics, collateral semantics, or
a route amount equal to a source current position whose
`planning_metadata.amount_semantics` is
`kamino_obligation_collateral_deposited_amount`.

The final report must include the query and its result after the fix is
deployed or ready to deploy.

### 6. Render Rollout Safety

PASS only if execution remains disabled or fail-closed until the verifier passes
locally. A deployed worker may run in dry-run mode while the fix is being
validated, but continuous `--execute` rollout is FAIL before:

- incident regression PASS;
- local dry-run against the affected vault PASS;
- read-only Neon DB guardrail PASS;
- local checks PASS;
- explicit operator approval to redeploy or re-enable execution.

Render must keep the pinned worker-image workflow from `AGENTS.md`; do not
switch worker services back to source Docker builds.

### 7. Focused Tests After Proof

PASS only if focused tests are added after behavior is proven or while waiting
for deployment. Do not let broad test cleanup block the behavioral fix.

Focused coverage should pin:

- collateral semantics cannot plan as routeable liquidity;
- redeemable liquidity semantics can plan;
- the 480/404/idle-USDC incident shape fails closed or plans the correct
  liquidity amount;
- confirmation refuses unsafe amount semantics;
- DB/read-model projection cannot store collateral units as current liquidity.

### 8. Local Checks

PASS only if these checks pass before commit or deploy:

```sh
NO_DNA=1 cargo fmt --check
```

```sh
NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin same-mint-yield-monitor --bin same-mint-reserve-swap
```

After focused tests are added, run the narrow test commands that cover the
changed modules. Broad workspace test cleanup is not required for this verifier.

## Verdict Format

Report:

```text
Fail-Closed Collateral Semantics: PASS|FAIL - note
Typed Route Amount: PASS|FAIL - note
Incident Regression: PASS|FAIL - note
Confirmation Projection: PASS|FAIL - note
DB Guardrail: PASS|FAIL - note
Render Rollout Safety: PASS|FAIL - note
Focused Tests After Proof: PASS|FAIL|DEFERRED - note
Local Checks: PASS|FAIL - note
Overall Verdict: PASS|FAIL
```

Overall verdict is PASS only when every required non-deferred section passes.
`Focused Tests After Proof` may be `DEFERRED` only while the real behavior is
working and deployment is actively waiting. Once deployment is complete or no
longer waiting, it must become PASS before final handoff.
