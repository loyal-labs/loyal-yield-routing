# Restore public Earn MAX access

The 2026-11-05 canary controls admitted two authorities and at most 100 USDC per
deposit/claim. The new client separately required a signed minimum payout and SOL
fee budget; the worker excluded unconsented demand before selling assets. Removing
the form alone would not restore funded withdrawals.

## Retain

| Change | Benefit and verification |
| --- | --- |
| Certified NAV, held-size exit quotes and 10 bps publication drift | Values holdings with exit costs and fees; reports meaningful price changes. Existing NAV, exit-quote and publication tests remain. Intermittent `exit_unpriced` pauses existed before separation and remain a fail-closed valuation condition. |
| Deposit pricing and LP bounds | Prevents minting shares against stale or mismatched pricing. Go quote tests, signed TS golden and real Voltr/Lighthouse replay remain. |
| Claim NAV publication check | Refuses a stale-high published price before claim preparation without requiring a user consent record. The ordinary on-chain claim still uses Voltr's published price; this server check is not an atomic certificate. |
| Receipt and policy-seed fence, atomic payout transfer | Prevents a prepared claim consuming a replaced receipt and prevents silently paying less than the prepared payout. Public and legacy signed wires run through the same real Squads/Voltr rejection scenarios. |
| Stage only when restore can follow | Avoids stranding cash between custody accounts. Existing custody/target tests remain. |
| Unavailable RPC handling and signed operation recovery | Network failures are not fabricated refusals; ambiguous signed operations remain tracked and reconciled. No tables or history are reset. |
| Earlier 20 bps withdrawal limit, reserves, diversification and emergency priority | Existing quote/wire/persist-sign tests verify the withdrawal limit. Deployment does not reset targets or rebalance positions. |

## Roll back for Loyal

- Remove the minimum-USDC/maximum-SOL-fee form and separate message signature.
- Admit ordinary public withdrawal demand without a consent record. Stop applying
  the stricter zero-incremental-holder-cost sale floor to Loyal; retain the 20 bps
  execution limit. A 20 bps limit does not guarantee a zero-loss exit.
- Open Loyal `official_deposit` and `claim_attempt` controls without owner lists or
  amount caps using version-checked changes. Retain operator pause controls,
  authentication, cooldown and liquidity checks. Backyard controls are independent.
- Keep the legacy consent API and settlement records for already released clients.
  Mobile source changes require a subsequent native release to reach installed apps.

## Deployment

Deploy the separated Loyal worker and Go/Vite web release from the reviewed merged
commit. Verify zero in-flight Loyal operations before changing the worker, existing
targets unchanged afterward, correct vault ownership, and the first held/failed
operation or restart. Open public controls only after the matching backend and
frontend are live. Re-read control versions and public release headers.

New investments (`optional_addition`) require a separate explicit decision: a
worker update itself must not turn an investment hold into a trade. This rollback
restores public deposits and withdrawal funding; it does not change strategy targets.
