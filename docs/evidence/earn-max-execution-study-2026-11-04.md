# Earn MAX execution and diversification study — November 4, 2026

Read-only investigation, 19:54–20:18 UTC. No production signing key was loaded,
transaction broadcast, database row changed, worker restarted, or policy changed.
The [captured measurements](earn-max-execution-study-2026-11-04.json) contain the
quote inputs, outputs, timestamps, slots, pools, valuation references, simulations,
capacity observations, and source hashes.

## Findings

| Proposal | Evidence | Decision supported by this sample |
|---|---|---|
| Split large sales into smaller, paced sales | Ten ONyc pieces improved the independent quotes by about $7.26 on $145.5k of collateral. Five AUTO pieces improved them by about $6.49 on $50k. | Do not enable a fixed splitting schedule on the assumption that it will eliminate losses. Require a measurable benefit and fresh quotes between pieces. |
| Give large withdrawals more time | Sixteen rounds over 15 minutes showed almost unchanged quotes. The current contract wait is 600 seconds and is a vault-wide setting. | No evidence here supports extending everyone's wait. Amount-specific waiting and deadline behavior need separate design. |
| Diversify across independent exposures | ONyc collateral is 117.5% of vault NAV; ONyc equity before the exit allowance is 78.3%. The two AUTO loans share collateral, and AUTO can exit through PRIME liquidity. | Concentration is real, but count underlying assets, providers and exit pools rather than loan names. Prefer new inflows over forced rotation. |
| Use a second destination with a usable exit | Maple's current Manifest-only route quoted 64–70% mark gaps. Broader routing including Whirlpool quoted approximately 0.04% instead. | Validate the alternative Maple transaction route before using it for diversification. The bad approved quote is not evidence that the asset has no better exit liquidity. |
| Tighten permitted deterioration after quoting | All four small repayment simulations passed, covering ONyc and AUTO at both 50 and 10 basis points. | A 10-basis-point allowance is technically feasible for these samples. Its reliability under congestion and market stress remains unmeasured. |

These are observations and conditional estimates, not guaranteed savings or a
production rollout decision.

## Current vault and funded cash

The confirmed snapshot at 19:54:43 UTC, slot 465426483, used the worker's unsigned
reserve-refresh valuation. Values below are denominated in USDC.

| Item | Value |
|---|---:|
| Vault NAV, after the existing exit allowance | $175,732.44 |
| Available withdrawal cash | $869.18 |
| Current withdrawal demand | $245.48 |
| Configured cash target | $17,573.24 |
| Unfunded portion of that target | $16,704.07 |

The observed demand was covered. The configured 10% reserve was not funded.
The existing policy retains incoming cash; it does not sell assets simply to
fill the reserve. Raising the target again would not create more available cash.

| Position | Gross collateral | Debt | Equity before exit allowance |
|---|---:|---:|---:|
| ONyc / USDC | $206,511.11 | $68,839.41 | $137,671.70 |
| AUTO / PYUSD | $51,129.96 | $17,026.79 | $34,103.18 |
| AUTO / wYLDS | $10,674.45 | $7,035.94 | $3,638.51 |
| PRIME / PYUSD | $10.33 | $0.00 | $10.33 |

Position equity plus cash, less the $560.45 exit allowance, reconciles to vault
NAV. A 50% equity allocation cap is not the same as a 50% exposure cap when
borrowing is involved. For example, a 1% ONyc collateral markdown would reduce
this vault's NAV by approximately 1.18%, before other changes. A simultaneous
1% markdown of all collateral would reduce NAV by approximately 1.53%.

If the proposed cap meant ONyc equity divided by current NAV, reaching 50% using
only non-ONyc incoming funds would require about $99,611 of new funds, assuming
unchanged positions and prices and ignoring deposit fees. This illustrates why
an immediate forced rebalance and gradual diversification have different costs;
it does not establish 50% as the right limit.

## Size and waiting comparison

The study collected 96 successful quotes: six sizes per round, sixteen rounds
from 19:57:30 to 20:13:21 UTC. Requests were spaced at least ten seconds apart,
and their order rotated each round. No quote failed or hit a rate limit.

Quotes used the production Jupiter ExactIn endpoint, V2 instructions, the
production lane DEX filters, at most four route legs, and the 50-account budget
for the standalone swap skeleton. Each quote used a 50-basis-point slippage
allowance. Input token quantities stayed fixed throughout the comparison.

The initial nominal sizes were $145,491.38 of ONyc and $50,000 of AUTO using
cached USD collateral marks. For the main comparison, outputs and collateral
are both expressed in USDC using the fixed, refreshed pre-study vault valuation.
That gives reference values of $145,515.64 and $50,008.87. Treating USDC as exactly
$1 would understate these gaps by approximately 1.7–1.8 basis points. Cached
reserve prices and timestamps are retained separately; a repeated account read
does not imply a newly updated oracle price.

| Asset and pieces | Mean gap versus reference valuation | Conditional total sale expense | Conditional improvement versus one quote |
|---|---:|---:|---:|
| ONyc, one | 0.24760% | $360.30 | — |
| ONyc, two | 0.24495% | $356.44 | $3.86 |
| ONyc, five | 0.24329% | $354.02 | $6.28 |
| ONyc, ten | 0.24261% | $353.04 | $7.26 |
| AUTO, one | 0.06275% | $31.38 | — |
| AUTO, five | 0.04977% | $24.89 | $6.49 |

“Conditional” means multiplying an independently quoted smaller sale by the
number of pieces. It does **not** mean those pieces were executed, nor that each
would obtain that price after earlier pieces consumed liquidity. Quotes within
a round were obtained over about 50 seconds, not simultaneously. Extra network
fees, cumulative market impact, replenishment and waiting exposure are excluded.
Token division leaves less than $0.000005 of unquoted dust in every case.

The full-size ONyc quote varied by only $0.053 over the observation window;
the full-size AUTO quote varied by $0.079. There was no meaningful improvement
from waiting beyond the existing ten-minute window. A roughly 0.005% adverse
price move on the ONyc amount would erase the quoted ten-piece benefit.

Even the smaller ONyc quotes remained above the proposed 0.1–0.2% total-cost
target. Some returned a Jupiter price-impact field of zero while still trading
below the vault valuation. That field alone is not a measure of the vault's loss.
Jupiter's quote and transaction-building interfaces are distinct; a quote is not
an executed fill. See the [official swap guide](https://developers.jup.ag/docs/guides/how-to-build-a-custom-swap-with-metis).

## Real repayment simulations and slippage allowance

The probe assembled existing worker debt-repayment steps from current positions,
installed policies and real Jupiter instructions. It ran unsigned chain
simulation and the production compiler/persistence/signature checks with a newly
generated, unfunded test key. The production delegate's private key was never
loaded. RPC methods were restricted to reads and simulation.

| Position / approximate collateral sold | Allowance | Result | LTV before → after | Compute units |
|---|---:|---|---:|---:|
| ONyc / $14,748 | 0.5% | Pass | 33.34% → 28.27% | 514,218 |
| AUTO / $10,224 | 0.5% | Pass | 33.30% → 16.78% | 593,116 |
| ONyc / $14,748 | 0.1% | Pass | 33.34% → 28.24% | 514,963 |
| AUTO / $10,224 | 0.1% | Pass | 33.30% → 16.68% | 593,334 |

These were four independent simulations, not sequential state changes. ONyc
repaid USDC; AUTO repaid PYUSD, so its repayment route differs from the standalone
USDC exit comparison. Each compiled sample specified 5,000 lamports of network
fees, with no additional priority fee. This is not a guarantee of future fee or
landing requirements, and a complete withdrawal has additional operations.

The allowance applies to deterioration after the quoted price. A quote already
0.24% below valuation plus a 0.1% allowance can still permit about a 0.34% gap.
Reducing the allowance therefore cannot itself achieve a 0.1% total sale cost.
The 50-basis-point ONyc quotes in the size study permitted roughly a 0.74% gap at
their minimum-output floor. Actual successful execution can be better than that
floor; lowering it is not an immediate realized saving.

## Diversification and executable liquidity

Both AUTO loans use the same collateral mint and collateral reserve. PRIME has
different underlying loans but shares Hastra/Figure infrastructure with AUTO,
as described in [Hastra's asset documentation](https://help.hastra.io/auto/what-is-auto).
The $50k AUTO quote also routed part of its output through PRIME/PYUSD pools.
All candidate loans still share Solana and Kamino infrastructure.

The following independent quotes were captured at 20:15–20:17 UTC. Sale gaps use
collateral and USDC prices from the same confirmed reserve read. These are gross
collateral sale amounts, not the cash released to a withdrawing user after debt
repayment. Quotes have not proved complete withdrawals executable.

| Collateral and routing | $50k sale: gap to mark | $100k sale: gap to mark |
|---|---:|---:|
| ONyc, current filter | 0.2455% | 0.2473% |
| AUTO, current filter | 0.0631% | 0.0711% |
| PRIME, current filter | 0.0314% | 0.0337% |
| Maple syrupUSDC, current Manifest-only filter | 64.1337% | 69.8858% |
| Maple syrupUSDC, broader routing including Whirlpool | 0.0392% | 0.0402% |

The poor Maple quotes used Manifest pool
`4im6emNBNT3LJXJSeTe4VWqC3pa1DqJtvtUHqQV96LNn`. Broader quotes included Whirlpool
pool `6fteKNvMdv7tYmBoJHhj1jx6rHcEwC6RdSEmVpyS613J` and were dramatically better.
Both quote sets retained the same account budget and route-leg validation.
The broader route's installed-policy compatibility and full entry/exit
transactions have **not** been validated. Removing a filter is not sufficient
authorization or evidence to trade through it.

A $50k USDC buy quote followed by an independent quote selling the returned
quantity produced a $34,863.29 gap on Maple's current route, $10.55 with broader
Maple routing, and $13.88 for PRIME. These pairs do not simulate the first trade's
effect on the second trade or include network fees. They are screening evidence,
not promised round-trip costs.

The 20:01 capacity observation allowed approximately $58.3m of additional Maple
borrowing under the reserve calculation and approximately $189.5m of collateral
deposit room. ONyc, AUTO/PYUSD and both observed PRIME destinations were blocked
from additional borrowing by utilization rules. Existing loans were unaffected.
AUTO/wYLDS had room, but is an overflow lane and shares AUTO exposure.
Capacity alone did not make Maple's current exit route acceptable, and is not
a complete new-entry admission check.

Captured native yields were approximately 11.02% for ONyc, 10.34% for AUTO,
6.11% for PRIME and 5.04% for Maple. At the observed borrowing rates, an
illustrative unchanged 1.5x position gives about 12.63% for ONyc versus 5.64%
for Maple before trading costs and other fees. These are rate snapshots, not
return forecasts. Diversification can sacrifice yield to reduce concentration.
Maple's institutional lending differs from ONyc's reinsurance exposure, but its
primary redemption process can take longer during liquidity stress; secondary
exit liquidity must be checked separately. See [Maple's lender FAQ](https://docs.maple.finance/syrupusdc-usdt-usdg-for-lenders/faq.md)
and [OnRe's collateral description](https://docs.onre.finance/introduction/onre-tokenized-reinsurance-onyc).

## Historical cases and withdrawal timing

The [retained historical analysis](../earn-max-loss-controls.md) establishes that
October 29 already had five ONyc sales in approximately 2 minutes 19 seconds.
Their gaps to adjacent marks worsened from about 0.37% to 0.65%; the total gap
was $1,148.50. Splitting alone did not prevent that episode. The later cheaper
sales were AUTO, so they do not prove that waiting improved ONyc liquidity.

November 4's earlier funding episode sold $145,491.38 of marked collateral for
$642.55 below its marks. Today's later quotes cannot be substituted for the
missing historical quote stream. The roughly $7 split-size improvement observed
now is not evidence that pacing would have saved hundreds of dollars then.

The live vault configuration at slot 465428661 specifies a 600-second wait.
The official Voltr interface exposes a vault-wide waiting period and request
arguments for amount, denomination and withdraw-all, not an amount-specific
waiting duration. The withdrawal receipt records when a claim becomes eligible.
See the [official Voltr interface](https://github.com/voltrxyz/vault-cpi).
An application-side delay must not be mistaken for an enforced on-chain delay.
Any future policy must preserve existing claim eligibility and have a bounded
fallback when a favorable quote never arrives. Emergency risk reduction must
not wait for a favorable price.

## Recommended next work

1. Validate the broader Maple route through the real transaction compiler,
   installed policies and an appropriate execution simulation before changing
   routing or allocating funds to it.
2. Retain incoming cash toward the existing reserve target. Define independent
   provider/collateral exposure limits, including gross leveraged exposure, then
   apply diversification gradually to suitable new inflows. Do not force a sale
   just to reach a candidate percentage.
3. Evaluate a tighter slippage allowance and quote-driven sale sizing. Continue
   only when a new executable quote shows worthwhile improvement after fees,
   and preserve the withdrawal deadline and risk overrides. The current quiet
   sample does not justify a fixed pacing schedule or a longer universal wait.

An additional ONyc comparison could examine its native redemption liquidity
path. OnRe describes size- and liquidity-dependent pricing through Titan in
its [liquidity-engine announcement](https://www.onre.finance/blog/introducing-onres-liquidity-engine-powered-by-titan).
Availability, eligibility, executable prices and compatibility with this vault
were not verified here; it is not assumed cheaper.

## Provenance and limits

Diagnostics compiled from `ceff2abbdf686c995bfd87c2db449053cd8f15d3`. The Backyard,
chain and program integration sources were unchanged from deployed worker
release `a8e77f9492286e0959085d14851a024ab2c180e0`. At 20:18:29 UTC the worker
remained active at PID 1931646 with zero restarts and the same release binary.
All temporary remote diagnostic processes had exited and their temporary
directories were removed. Arithmetic assertions check quote counts, money-unit
conversion, valuation reconciliation, successful simulation results and cleanup.

This is one quiet observation window. It does not test cumulative pool impact,
liquidity replenishment after our own sales, cancellation or restart recovery
for a future pacing engine, overlapping withdrawals, prolonged stress, or the
alternative Maple route's complete transaction path. No pacing engine,
concentration rule, longer wait or new live route was implemented or deployed.
