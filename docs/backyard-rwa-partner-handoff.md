# Backyard RWA vault partner handoff

## Canonical identities

- Voltr vault: `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`
- Voltr program: `vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8`
- Loyal adaptor: `FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW`
- Strategy/config: `9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj`
- Squads Settings: `5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6`
- Squads vault: `ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh`
- Go worker: Render service `loyal-backyard-rwa-worker`
- Worker source commit: `4b86f605964fac400c1b75ba01020aa62c1a6ccc`
- Render deploy: `dep-dad53am7bikc739n6jgg`
- Image digest: `sha256:b08f207f73411c17a0364617658d19e447cbd23a6e7da8b08084cd593615d351`

## Two-route operating flow

Deposited USDC first becomes Voltr idle. The bound adaptor moves allocatable
capital to the exact Squads USDC account. The serialized Go worker owns the
two enabled routes: fixed `PRIME/USDC` and the Phase 2 representative
`Maple/syrupUSDC/USDC`. It swaps USDC to the selected collateral, deposits it
into Kamino, and attempts leverage only while confirmed reserve and risk limits
allow it. Every other catalogued lane fails closed. If capacity or risk blocks
a safe action, the worker records a typed durable `HOLD` and sends no
risk-increasing transaction.

A withdrawal request immediately stops risk increases. The worker unwinds the
required budget, swaps back when necessary, stages exact USDC to the Voltr
strategy account, reports NAV through the atomic one-use adaptor ticket, and
restores Voltr idle. The user can claim after the onchain 600-second wait.

NAV is currently computed by the Go worker from its reconciled component
snapshot and authenticated through the Squads-signed adaptor ticket. It is not
yet independently calculated onchain.

## Capability boundary

The Squads account has a finalized Phase 2 policy catalog covering 11 RWA
Multiply lanes, 44 Kamino operations, and 52 directed Jupiter swap edges in 70
original physical policies at seeds 67–136. Current forward rollovers are
finalized through Settings seed 139. This is authority capability only. The
live serialized Go worker deliberately enables exactly `PRIME/USDC` and
`Maple/syrupUSDC/USDC`.

There is no optimizer, automatic market switching, caller-selected route,
registry, pre-hook, post-hook, consumer Earn Max behavior, or second
money-moving executor in this release.

## Monitoring and recovery

Use the read-only Backyard vault integration page for AUM/NAV, current custody,
position, route state, deposits, withdrawals, and operation history. Operational
health requires one active route lease, zero competing writers, and no operation
remaining nonterminal beyond the worker's reconciliation window.

For a nonterminal operation, inspect its persisted wire/signature first. Never
blindly resend: reconcile the signature and protocol/account poststate, then let
the worker continue or place the route into manual-recovery HOLD. For reserve
capacity or utilization blockers, retain the typed HOLD and retry only after the
observed live condition changes. Policy repair uses forward seeds and verifies
the replacement before retiring the superseded policy.

One finalized Phase 2 Voltr restore is retained as a retrospective incident:
operation `fe45a0369bf950da3ea311a4c493377cf9720a92c359c0bfbe739a3d9f699cbe`
requested `1,000,000` raw USDC, while Voltr swept the complete `3,793,417` raw
USDC strategy balance to its idle custody. Exact finalized deltas conserved the
funds and did not change destination. The durable operation remains
`manual_recovery`; do not replay it, relabel it as reconciled, or perform a
compensating transfer. The deployed guards now interleave staging/restoration,
fail closed above the ordinary per-transaction cap, and reconcile the complete
within-cap staged balance. This incident authorizes no cap increase or second
exception.

## Retained proof

The retained Phase 1 deposit-to-claim proof is
`docs/evidence/backyard-rwa-go/lifecycle-v1.json`. Phase 2 selection, current
policy rollovers, and the exact restore incident are under
`docs/evidence/backyard-rwa-go/phase2-runtime/`. Adaptor and policy evidence is
indexed under `docs/evidence/backyard-rwa-go/`. The Phase 2 close-out command is:

```sh
op run --env-file=.env.1password -- \
  bun run --cwd tools/backyard-voltr verify:rwa-phase2-runtime
```
