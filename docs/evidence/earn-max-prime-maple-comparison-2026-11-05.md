# Earn MAX: PRIME and Maple diversification comparison

Read-only evaluation on November 5, 2026. Both candidates passed a $1,000
simulation of the lending lifecycle **without borrowing**, including returning
the sale proceeds to the vault's idle withdrawal cash. Six financial transactions
retained the existing policies and transaction compilers. One additional unsigned
reserve-refresh observation reproduced the worker's valuation step.

No production signing key was loaded, transaction submitted, database row
changed, worker restarted, or production allocation/routing rule changed.
The [sanitized evidence](earn-max-prime-maple-comparison-2026-11-05.json) includes
timestamps, quotes, capacities, source hashes, transaction validation results,
and verified balance changes. Full raw simulations and the temporary Go overlay
remain in `/tmp/earn-max-prime-maple-20261105`; uploaded probes were removed from
the workers host.

## Decision

**PRIME has the stronger observed income case for the next additional allocation
without borrowing. Maple adds more provider diversification.** Both reduce ONyc
concentration when funded with new capital. Evaluate them against a combined
AUTO + PRIME provider limit, rather than excluding PRIME because it shares
infrastructure with AUTO or because its borrowing pool is full.

Keep the withdrawal reserve funded first. Apply any new concentration rule to
additional exposure, preserving existing holdings and leverage targets across
deployment/restart. No forced sell-and-rebuy is needed to start diversification.
No numerical concentration cap or allocation has been selected here.

## Comparable observations

Values are denominated in USDC. Native APYs are worker estimates captured around
12:35 UTC; the underlying native feed observations were timestamped 11:00 UTC.
They are annualized observations, not return forecasts. Collateral-reserve supply
APY was zero for both in these samples.

| Item | PRIME | Maple syrupUSDC |
|---|---:|---:|
| Observed native APY | 6.22% | 5.15% |
| Borrowing APY on the compared USDC route | 5.44% | 4.28% |
| Additional borrowing allowed by reserve calculation | No | Yes |
| Deposit-cap headroom without borrowing | ~$286.8m | ~$189.5m |
| $1,000 funded lending-cycle simulation | Passed | Passed |
| $10,000 independent buy/sale quote-pair gap | $1.54 / 0.01535% | $0.88 / 0.00882% |
| $50,000 independent buy/sale quote-pair gap | $13.81 / 0.02763% | $9.20 / 0.01839% |

Deposit headroom is the worker's reserve admission calculation after its
conservative adjustment. It is **not** available exit liquidity or permission to
allocate that amount. PRIME accepted the debt-free deposit while additional
borrowing remained blocked. The empty `Prime/PRIME/USDC` destination was used;
the existing small `Prime/PRIME/PYUSD` holding was preserved.

At an illustrative $50,000 invested, the observed native yield difference is
about $537 per year. PRIME's additional quoted buy/sale cost was about $4.62.
With unchanged rates/prices and a simple annual-rate calculation, that cost
difference is offset in roughly three days. This excludes network fees, future
rate changes and asset/provider losses; it does not establish superior
risk-adjusted returns.

Borrowing adds little to these illustrative rates: maintaining 1.5x exposure
would add roughly 0.39 percentage points for PRIME or 0.43 for Maple before
financing/trading fees, assuming unchanged borrowing rates. PRIME's compared
route could not add debt at the observed utilization. There is no need to add
borrowing to test diversification, and these calculations do not change the
targets of existing positions.

## What actually passed

The final simulations ran at slots 465707523 and 465707694, from
12:35:16–12:36:25 UTC. Each complete replay carried state forward through:

1. Allocate $1,000 from Voltr idle cash to shared Squads custody.
2. Buy the candidate collateral through Jupiter under installed policies.
3. Deposit the guaranteed purchase amount into Kamino, without borrowing.
4. Withdraw that entire Kamino deposit and sell the redeemed amount, allowing
   one token base unit for rounding, in the worker's atomic exit transaction.
5. Stage the sale proceeds in the Voltr strategy account.
6. Refresh reserve valuations using the production observer's instructions.
7. Restore the staged USDC to the vault's idle withdrawal account.

All seven steps returned success for both assets. Each final replay had zero
remaining debt and zero collateral deposited in its tested Kamino obligation.
The last transaction moved exactly $994.801007 for Maple and $994.902474 for
PRIME out of strategy staging into idle cash. Strategy staging finished empty.
The same final replay's account balances verify the transfer; a successful test
process alone was not treated as proof of a successful transaction.

Both buys filled above their guaranteed minimum. The remaining 4.196232
syrupUSDC and 4.680410 PRIME, approximately $5 each, remained in token custody
outside Kamino. Those assets are **not losses**. This proves the funded lending
cycle and cash restoration for the guaranteed portion, not a sale of every last
token or a $5 trading expense. The subsequent
[large-withdrawal and liquidity-stress study](earn-max-withdrawal-stress-2026-11-05.md)
now exercises larger funded positions and preceding market sales; its scope and
unproven cases are recorded separately.

All six financial wires per candidate compiled and passed exact-envelope,
hash/signature validation using an ephemeral test key and the existing wire
test helper. No signed transaction was persisted to a database. The valuation
step was unsigned observation, as in the production observer, and was not
treated as a financial policy envelope. Simulation signature verification was
disabled; production delegate credentials were never accessed.

## Routing and quote reliability

Maple's production filter remains `Manifest`. Its larger comparison quotes used
the candidate `Manifest,Whirlpool` filter only inside the probe. The full small
Maple lending-cycle test explicitly used Whirlpool. Enabling that broader route
would still require a production routing change. Earlier evidence documented
the unacceptable large quotes from the Manifest-only filter.

PRIME used its current routing configuration. One initial $10,000 exit request
was rejected by the existing `Jupiter program or account set drifted` guard.
The original response was not retained, so this does not distinguish an
unexpected program from exceeding the 64-account limit. A fresh $10,000 buy/sale
pair passed with the same 50-account quote budget and no manual fallback. Its
instructions had 43 and 27 accounts. The rejected request is preserved as a
failure, not assigned a zero sale value or counted as a $10,000 loss.

The $50,000 pairs for both assets passed quote/instruction checks, compilation
and ephemeral-key wire validation. The quoted sale was independently requested
for the amount returned by the buy quote. These pairs do not include the first
trade's effect on the second, network fees, or future liquidity replenishment.
The $10,000 PRIME number is from its 12:32 recheck; the other pairs were captured
12:23–12:26. These are screening measurements, not executable cost guarantees.
Keep existing rejection guards and reprice any unusable route before funding.

## Which risks each allocation spreads

PRIME's underlying exposure is home-equity lending; AUTO's is consumer auto
lending. They are different collateral exposures but share Hastra/Figure
infrastructure. Count both under a shared provider limit while also tracking
their individual asset exposures. See
[Hastra's asset comparison](https://help.hastra.io/auto/what-is-auto) and
[PRIME documentation](https://help.hastra.io/prime/what-is-prime-%28staked-wylds%29).
Hastra's published rate explanation describes its platform fee within the
holder rate; do not subtract that fee again without reconciling the rate source.
See [PRIME rate calculation](https://help.hastra.io/prime/how-is-the-prime-rate-determined).

Maple adds exposure to a different provider and collateralized institutional
lending. It is not Circle-guaranteed USDC. Maple documents normally immediate
primary withdrawals but a maximum possible withdrawal time of 30 days; that
cannot serve as a guaranteed fallback for Earn MAX's 600-second withdrawal wait.
See [Maple's lender FAQ](https://docs.maple.finance/syrupusdc-usdt-usdg-for-lenders/faq.md).
PRIME's ability to request unstaking is likewise not proof of a large immediate
Earn MAX exit. Both worker routes depend on Kamino liquidity and tradable exits;
both share Solana, Squads, Jupiter and some DEX infrastructure.

Neither option guarantees avoidance of losses. This study does not quantify
credit-default probabilities, stress correlations, or recovery values.

## Cash-first allocation illustration

The refreshed 12:35 snapshot had $176,958.77 NAV, $2,422.10 idle cash and
$245.48 withdrawal demand. The 10% cash target was $17,695.88: a $15,273.78
shortfall. Approximately $16,970.87 of net cash inflows would fill the target
if nothing else changed, because the target rises with NAV. The NAV includes
a $943.99 estimated exit allowance, subtracted once from position equity plus
cash; this allowance is not a realized trade loss.

For an illustration with **$50,000 of net incoming capital**, retain about
$20,273.78 to fund the enlarged reserve, leaving $29,726.22 for investment.
Holding existing positions/prices and the exit allowance fixed, before new
trading costs:

| Allocation of the investable remainder | ONyc equity / NAV | AUTO + PRIME equity / NAV | Maple equity / NAV |
|---|---:|---:|---:|
| All to PRIME | 60.68% | 29.73% | 0% |
| All to Maple | 60.68% | 16.64% | 13.10% |

Both reduce the present 77.83% ONyc equity share by the same amount. Maple adds a
third provider group; PRIME earns more in the observed rate sample while keeping
the combined Hastra share below 30% in this illustration. These are equity
shares, not gross leveraged exposure limits, and neither scenario is an approved
allocation. Choosing the provider and gross-exposure limits remains a policy
decision before deployment.
