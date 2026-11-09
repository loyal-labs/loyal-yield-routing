# Repayment-first exit pricing and observation rollout

> Historical experiment. The final rule and rollout supersede the observation
> proposal below: [final implementation](investment-exit-admission-2026-11-06.md).

The initial full-sale screen overstated AUTO's exit cost. It priced converting
all collateral into USDC, including the PYUSD that should instead repay the
PYUSD loan. The saved $100k-equity, 2x AUTO replay completed 202 protected
transactions while that screen rejected it.

## Corrected calculation

The screen includes the existing holding, custody, the proposed purchase and
borrowed purchases. Existing payoff bounds, new borrowing fees and the
execution-window interest allowance remain included.

For USDC debt, quote selling the collateral and subtract the USDC repayment.
For PYUSD debt, quote collateral into PYUSD, subtract the PYUSD repayment, then
quote only the remaining PYUSD into USDC. The latter two quotes may compose
only when they use disjoint pools. Unknown, split, changing or overlapping
paths, or quotes from different slots, are not treated as proof of an affordable exit. Multiple distinct
non-USDC debt assets also require separate stateful evidence.

In addition to the whole-position average, compare cumulative quotes for the
whole holding and for the holding minus one execution-sized portion. Their
difference estimates the last portion's proceeds under the same route. The
0.2% reference-price rule applies there too. For a PYUSD loan, carry that
last portion through the remaining-PYUSD conversion. This catches a cheap
average that conceals an expensive final portion.

These are present-liquidity estimates, not a guaranteed future execution price.
They assume unchanged liquidity and a comparable unsplit path. Actual execution
continues to requote, simulate and enforce its own price limit on every portion.
No swap instruction, signer policy, borrowing limit or withdrawal rule changes.
The existing profitability calculation still includes purchase, exit, borrow
and network costs; the new screen is additional evidence, not a replacement.

## Observation mode

`ObserveInvestmentExits` is enabled in the local Backyard configuration;
`CheckInvestmentExits` remains false for both vaults, and Loyal's observation
flag remains false. This source change has not been deployed.

After the existing selector finishes its decisions, state writes and worker
notifications, the sampler can compare its actual selected new-cash proposal
against the new screen. The comparison uses the proposed amount, projected
borrowed purchases and the original confirmed account batch. It has a separate
two-second budget and emits `investment_exit_comparison` with:

- vault/lane, original observation ID, proposed USDC and existing entry cost;
- `would_allow`, `would_reject` or `unknown`;
- reference value, repayment value, cash after repayment and last-portion prices.

The result is never passed into selection or preparation and cannot cancel the
investment, change its size, or replace a stored target. A quote outage remains
`unknown`. If the selector has no new-cash proposal, there is no fabricated
$1,000 candidate. Rotation proposals are outside this comparison.

Actual enforcement remains a separate reviewed activation after observing the
real decisions. Existing cash reserves and all existing execution protections
remain in force during observation.

## Verification

The corrected-price Go regression run passed **205 cases**, including subtests,
with **13 environment-gated skips** and no failures. `go vet` also passed. The skips require a database
or live export; neither was connected. Tests cover the repayment order, a good
average hiding an expensive last portion, changed quote slots, unknown/shared
routes, unavailable pricing, unchanged input decisions, prior signed recovery,
and the surrounding selector and withdrawal behavior.

The native pricing calibration used captured public pool state and executed the
swaps in LiteSVM through the Python native binding; no Node process or validator
was run. It kept the same fixed CPU and memory limits as the completed leveraged
replays: half one CPU core, 768 MiB, no swap, 64 tasks, 180 seconds, private network.
All four cases completed without an OOM or memory-limit event.

| Hypothetical case | Corrected screen | Estimated cash after loan repayment | Earlier complete protected replay |
|---|---|---:|---:|
| AUTO, $100k equity, 2x | Accept | $99,788.599427 | $99,788.615637 |
| Maple, $100k equity, 1.5x | Accept | $99,964.243661 | $99,964.253660 |
| PRIME, $100k equity, 1.5x | Accept | $99,917.898745 | $99,917.905224 |
| OnRe, $100k equity, 1.5x | Reject expensive exit | No accepted estimate | Safely refused the second sale |

AUTO's estimated final $1,000 collateral portion returns $998.443025, above the
$998 reference floor. Its former full-USDC-sale screen rejected the same
position. The small difference between the corrected cash estimate and the
complete replay is consistent with conservative payoff allowance and rounding.
These figures do not deduct SOL network fees from USDC; the existing entry
profitability calculation budgets network costs separately.

The price-calibration transactions execute Jupiter directly to measure pool
output, with hypothetical token balances and signature verification disabled.
They do not prove delegation or constitute full new withdrawal replays. AUTO's
residual conversion uses the Manifest leg already present in the captured
collateral-to-USDC route, with its original market state. The earlier complete
worker replays separately exercised Squads policies, Kamino repayment, exact
wire compilation, signing/persistence validation, and price guards. No production
wire, permission, transaction or account was changed.

[Machine-readable results and artifact hashes](investment-exit-comparison-2026-11-06.json)
record the exact inputs, outputs, limits and prior replay results. Logs are
`/tmp/investment-exit-corrected-regression.jsonl`,
`/tmp/investment-exit-corrected-vet.log`, and the
`/tmp/vault-native-replay-20261106/pricing-calibration-*.log` files.

The new veto is intentionally not activated by this change. Fresh market routes
may change between quotes, or use overlapping/split liquidity; comparison mode
records these as unknown and cannot turn them into an investment pause.
