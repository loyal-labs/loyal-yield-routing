# Earn withdrawal investigation, 2026-11-09

Wallet: `G3FUA7Qif6gLJuxF3Lt8kPT5Rrfo6itnzKtzfP2PZHge`  
Settings: `69kuvyP9iAqcfw2eG4ceGUDXx4kBPXbCDSUduLyx4gH7`  
Earn vault: `9SnPtcP6td1T6ZsQEY6uEvgqhWsCGgwYZBc7i7MVTaAy` (managed vault 514)

## Withdrawal

Production `loyal-web` release at investigation: `ba007f7737fa26e427b872caef059faaaf47a074`.
The two changed withdrawal preparation files matched that release before editing.

Seven preparation failures in the November 9 service logs, between 15:54 and 16:05 UTC,
reported Kamino `NetValueRemainingTooSmall` (6092). These occurred in unsigned
preparation simulation, before wallet approval. They did not submit withdrawals.

The browser sends `amountRaw: "max"` for a full source withdrawal. The Go resolver
discarded that intent after calculating a numeric liquidity estimate. The upstream
instruction's collateral amount then left a tiny deposit after reserve interest was
refreshed. Kamino rejects that nonzero residual.

The local fix preserves max intent, uses the existing validated reserve instruction
builder with Kamino's full-collateral amount (`u64::MAX`), and transfers the simulated
redeemed liquidity. Numeric partial withdrawals retain their requested transfer.
The existing API response mode remains `partial`: it describes a withdrawal from one
source, not disabling the entire Earn account or its policies.

### Verification

- Regression: `TestWprepMaxRedeemsAllCollateralWithoutChangingPartialWithdrawals`
  passed for max and numeric partial requests. It exercises request parsing, live
  holdings decoding from fixtures, resolution, upstream instruction decoding,
  compilation of the simulation wire, and transfer/persistence amounts. The max case
  includes interest accrued after the quote and existing idle funds.
- An unsigned read-only probe against the actual wallet reproduced the old failure:
  requested liquidity `1124217148` raw, error 6092, residual value about $0.0001.
- The corrected **complete** prepared transaction simulated successfully at slot
  `467384188`: 619 wire bytes, 158616 compute units, wallet USDC increase
  `1124217216` raw (**1124.217216 USDC**), and zero remaining OnRe collateral.
  Kamino redeemed all `1053378256` collateral units.
- No transaction was signed or broadcast, and no database rows were changed.
  Database probes used read-only transactions with a local 10-second statement timeout.
- The local build/test was capped at one CPU and 3 GiB RAM. The remote probe was
  capped at half a CPU and 512 MiB RAM. Both passed; temporary probe source and remote
  executables were removed. Production web and retail services remained active.

Private local diagnostic artifacts: `/tmp/loyal-g3f-check.log`,
`/tmp/loyal-g3f-live-probe.log`, `/tmp/loyal-g3f-fixed-withdraw-wire.json`.
The latter is unsigned preparation data, not a submitted transaction.

## Rebalancing

The last confirmed rebalance was October 30, 2026 at 20:30:44.595 UTC, Main Kamino
to OnRe, decision 30506, amount `1122326831` raw. The next opportunity was marked
stale with `capacity_replan` at 20:32:59 UTC; no subsequent decision was created.

The current position and route policy remain active. The worker is running. At the
November 9 16:20:38 UTC planner snapshot (epoch 2699514), USDC supply APYs were:

| Market | APY |
| --- | ---: |
| OnRe (current holding) | 6.14% |
| Prime | 4.30% |
| Main Kamino | 3.96% |
| Maple | 3.95% |

The vault has **no cross-mint opt-in or swap policies**. Its available automation is
therefore same-mint routing, despite the Earn policy listing multiple supported
stablecoins. Higher rates on another stablecoin do not grant permission to swap.

The current snapshot gives no profitable eligible USDC destination. Absence of a
recent rebalance does not stop interest accruing in the existing Kamino position.
This establishes current routing eligibility; it is not a reconstruction of every
market opportunity throughout the intervening ten days.

## Release status and limits

The investigation did not change production. Release was subsequently authorized
on November 9. Before release, the regression also passed the Go confirmation
parser, and focused Go vet checks passed. The actual simulated preparation wire
passed browser SDK hydration and persistence round-trip checks and compiled to
619 bytes requiring exactly the user's wallet signature; all signature bytes
remained zero. Deployment status is tracked in the release PR.

No production browser session was impersonated and no real withdrawal was
executed. A real withdrawal remains dependent on current liquidity and the
user's wallet approval.
