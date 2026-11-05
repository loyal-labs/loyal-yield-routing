# Earn MAX diversification follow-up — November 5, 2026

Diversification is justified as protection against a concentrated asset/provider
loss. The alternative Maple swap route now has a successful small, funded,
sequential simulation. This does not establish that diversification will reduce
every withdrawal cost, or authorize allocating money to Maple.

This was a read-only investigation, 10:23–10:48 UTC. No production signing key
was loaded, transaction broadcast, database row changed, service restarted, or
production policy changed. The [sanitized measurements](earn-max-diversification-2026-11-05.json)
include quote payloads, simulation balances, slots, validation results and source
hashes. The probe used the same worker sources as release
`69e4c38e32e55fd64301a8350aa412f43345f4ec`.

## Concentration and cash

The refreshed snapshot at 10:23:14 UTC, slot 465670241, values the vault in USDC.
Equity below means collateral value less debt, before the shared exit allowance.

| Position | Gross collateral | Debt | Equity | Equity / vault NAV | Gross / vault NAV |
|---|---:|---:|---:|---:|---:|
| ONyc / USDC | $206,581.41 | $68,847.94 | $137,733.48 | 77.66% | 116.47% |
| AUTO / PYUSD | $51,140.44 | $17,030.82 | $34,109.62 | 19.23% | 28.83% |
| AUTO / wYLDS | $10,676.63 | $7,036.62 | $3,640.02 | 2.05% | 6.02% |
| PRIME / PYUSD | $10.33 | $0.00 | $10.33 | 0.006% | 0.006% |

Position equity plus $2,369.18 cash, less the $497.61 exit allowance counted
**once**, reconciles exactly to $177,365.01 NAV. Current withdrawal demand was
$245.48. The 10% cash target was $17,736.50, leaving a $15,367.32 shortfall.
Funding that target entirely through incoming cash would require approximately
$17,074.80 of net inflows because the target rises with NAV, assuming no other
changes. Raising the reserve percentage does not itself fund the reserve.

The two AUTO loans share collateral. PRIME has different underlying loans but
shares Hastra/Figure infrastructure with AUTO; group them together for provider
concentration while retaining separate asset limits. See
[Hastra's asset documentation](https://help.hastra.io/auto/what-is-auto).
Maple adds a possible third provider group. All these lending routes still share
Solana and Kamino infrastructure.

ONyc's underlying exposure is reinsurance, with NAV affected by claims;
Maple's syrupUSDC represents institutional collateralized lending. Neither
diversification nor collateral makes losses impossible. See
[OnRe's ONyc documentation](https://docs.onre.finance/introduction/onre-tokenized-reinsurance-onyc)
and [Maple's lender FAQ](https://docs.maple.finance/syrupusdc-usdt-usdg-for-lenders/faq.md).
Maple's primary redemption can take longer than its usual processing time,
including up to 30 days under the documented withdrawal window; it is not proof
of an exit within Earn MAX's current 600-second wait.

## What the alternative Maple route proved

The production Maple DEX filter is `Manifest`. The candidate filter tested in
memory was explicitly `Manifest,Whirlpool`; it was not an unrestricted route.
Independent buy and sell quotes at 10:23–10:24 UTC produced:

| Allowed exchanges | USDC input | USDC from independently quoted sale | Quote-pair gap |
|---|---:|---:|---:|
| Manifest | $50,000 | $14,525.15 | $35,474.85 / 70.9497% |
| Manifest + Whirlpool | $50,000 | $49,990.81 | $9.19 / 0.01838% |

These are screening quotes, **not realized losses or guaranteed round-trip
costs**. They exclude the first trade's effect on the second, network fees and
future liquidity changes. The vault has no current Maple position.

All four exact swap requests compiled within the 4,096-byte V1 limit, matched
the installed entry/exit policies, and passed persistence/signature validation
using an ephemeral, unfunded test key. The mixed routes were 1,232 bytes per
transaction. Standalone simulations reached Jupiter but failed with insufficient
input funds because both custody balances were zero; they did not prove swaps
executable at $50,000.

We then used the RPC's
[sequential bundle simulation](https://docs.triton.one/trading-apis/bundle-simulation-with-jito)
to test three separate production transaction shapes: fund shared USDC custody
with $1,000, buy syrupUSDC, and sell the guaranteed portion of that purchase.
The RPC carried simulated account changes forward between steps. Signature
verification was disabled for simulation; no production transaction was signed
or sent.

| Sample | Slot | Funding / buy / sell | Cash after sale | Unsold syrupUSDC |
|---|---:|---|---:|---:|
| Candidate filter, router selected Manifest | 465675603 | All succeeded | $994.988087 | 4.196492 |
| Whirlpool explicitly selected | 465677286 | All succeeded | $994.801009 | 4.196231 |

The sale used the buy quote's minimum output rather than its expected output.
Both buys filled above that minimum, leaving approximately $5 of syrupUSDC.
That remaining asset is **not a trading loss**. These balances alone should not
be presented as a cash-only round-trip profit/loss calculation.

The Whirlpool sample also passed separate compiler and persistence/signature
validation for all three steps. Their wire sizes were 1,043 / 980 / 980 bytes;
compute use was 146,402 / 129,949 / 143,661 units. Simulation logs show the actual
Whirlpool program and both quotes identify pool
`6fteKNvMdv7tYmBoJHhj1jx6rHcEwC6RdSEmVpyS613J`.
Logs were truncated by the simulator; all three returned success and account
balances confirmed the transfers.

An earlier synthetic combined transaction failed and was not used as evidence
for the separate production transaction shapes. One Whirlpool preflight also
hit a transient RPC node-behind error; the retry above completed. All uploaded
diagnostic files were removed from the workers host.

## What policy the evidence supports

1. **Cash first.** Retain incoming funds to cover the withdrawal reserve before
   new investments. The hypothetical funding simulation did not authorize
   spending today's underfunded reserve.
2. **Limit growth by asset and provider group, including borrowed exposure.**
   Count the two AUTO loans together. Track equity and gross collateral
   separately so leverage cannot conceal concentration.
3. **Diversify gradually.** An existing position above a new limit should block
   additional exposure, not trigger a sale merely because the worker restarts
   or a deployment changes its rules. Withdrawals and necessary risk reductions
   must remain possible. Keep cash when no destination passes admission checks.
4. **Qualify the whole exit before enabling Maple.** The small swap route works
   under existing policies. Still verify collateral deposit, any borrowing,
   repayment, collateral redemption and return of cash to Voltr at intended
   sizes, including failure/recovery behavior. Then assess joint withdrawal
   demand and stressed liquidity across destinations.

A 50% limit has **not** been chosen. Its definition materially changes the
result: a 1% ONyc collateral markdown currently reduces NAV by approximately
1.16%, with other values unchanged. A 50% equity allocation at 1.5x leverage
would reduce that sensitivity to 0.75%; a 50% gross-exposure limit gives 0.50%.
Reaching the former equity limit entirely through new non-ONyc net inflows would
need about $98,102; reaching the gross limit would need about $235,798. These
estimates hold existing positions/prices constant and exclude fees and accrual.
Gradual diversification will therefore take time. A faster reduction requires
an explicit decision about the costs of selling existing holdings.

The captured native APY was approximately 11.02% for ONyc versus 5.09% for Maple.
Borrowing, fees and trading costs change net returns; those annualized snapshots
are not forecasts. Accepting a lower expected yield can buy less concentration,
but does not recover past losses or guarantee cheaper withdrawals.

The remaining gates are full lending-lifecycle simulation, intended-size and
stress testing, and choosing the concentration metric and limit. No allocation
cap, DEX filter change or automated diversification trade was deployed.
