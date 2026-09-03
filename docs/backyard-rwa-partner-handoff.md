# Backyard RWA vault partner handoff

## Canonical identities

- Voltr vault: `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`
- Voltr program: `vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8`
- Loyal adaptor: `FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW`
- Strategy/config: `9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj`
- Squads Settings: `5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6`
- Squads vault: `ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh`
- Go worker: Render service `loyal-backyard-rwa-worker`
- Worker image: `sha-bdae0957e394727dcdaf449775659bd8e92d3727`
- Image digest: `sha256:b93a5e260fa31116d71e487d5f06e72989614a8accd03b018da6a57f34293a99`

## Phase 1 operating flow

Deposited USDC first becomes Voltr idle. The bound adaptor moves allocatable
capital to the exact Squads USDC account. The serialized Go worker owns the
fixed `PRIME/USDC` route, swaps USDC to PRIME, deposits PRIME collateral into
Kamino, and attempts leverage only while the confirmed reserve and risk limits
allow it. If debt-reserve utilization blocks borrowing, it records a durable
`HOLD` and sends no risk-increasing transaction.

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
physical policies at seeds 67–136. This is authority capability only. The live
worker deliberately executes only the fixed PRIME/USDC Phase 1 route.

There is no optimizer, automatic market switching, consumer Earn Max behavior,
or Phase 2 multi-route executor in this release.

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

## Retained proof

The real confirmed deposit-to-claim proof is
`docs/evidence/backyard-rwa-go/lifecycle-v1.json`. Adaptor and policy evidence is
indexed under `docs/evidence/backyard-rwa-go/`. The operational audit command is:

```sh
op run --env-file=.env.1password -- \
  bun run --cwd tools/backyard-voltr verify:rwa-multiply-custom-lifecycle
```
