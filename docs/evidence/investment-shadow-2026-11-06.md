# Investment economics observer — 6 November 2026

> Historical experiment. The final rule and rollout supersede the observation
> proposal below: [final implementation](investment-exit-admission-2026-11-06.md).

Implemented a standalone read-only capture command and offline replay under
`scripts/investment-shadow`. Neither worker service calls this tool. No live
transaction was signed or submitted, no database was connected, and no service,
target, permission or investment setting was changed.

The capture command reuses the worker's entry-cost and seven-day forecast code.
The replay buys tokens with hypothetical 1,000 USDC, sells precisely those tokens
against the resulting pool state, and checks larger exits separately. It executes
the actual unsigned V1 Squads/Jupiter messages against captured programs, with
signature verification disabled. Token funding exists only in local LiteSVM.

## Latest completed comparisons

Backyard vault: `GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1`.
Captures completed between 17:38:02 and 17:38:47 UTC. Each final account/program
snapshot was fetched in one RPC batch. The simulator retained its exact captured
clock and pool state across transactions.

| Route | Capture slot | 1,000 USDC buy/sell cost, excluding SOL | Cost including simulated network fees | Existing full entry/exit cost budget |
| --- | ---: | ---: | ---: | ---: |
| OnRe / ONyc / USDC | 466196299 | 0.107134 | 0.108050 | 10.086180 |
| Maple / syrupUSDC / USDC | 466196094 | 0.199992 | 0.200909 | 7.190326 |
| PRIME / PRIME / USDC | 466196128 | 0.239973 | 0.240890 | 8.848865 |
| AUTO / AUTO / PYUSD | 466196163 | 0.439918 | 0.440834 | 12.037329 |

The last column is a conservative minimum-output budget, including the worker's
other applicable entry/exit costs. It is **not a measurement of fees paid**.
The corresponding quote-based total costs were 0.112450, 0.201726, 1.860563 and
2.066848 USDC. Quote-based totals can include setup and other lifecycle costs
absent from a conversion-only replay.

All four hypothetical 1,000 USDC entries were rejected by the existing economic
model over its seven-day horizon. The observer preserves that result. It does not
replace conservative costs with a favorable simulated fill or bypass live reserve,
capacity, allocation, persistence or in-flight-operation checks. These hypothetical
model results do not describe actual pending investment decisions.

## Larger exits

Each portion sells approximately 1,000 USDC of reference-valued collateral, with
the worker's quote/reference minimum-output protection enforced on the wire.
The table records USDC obtained, before SOL fees. These are conversions, not
complete user withdrawal/claim tests.

| Route | 10,000 reference value | 50,000 reference value | 100,000 reference value |
| --- | ---: | ---: | ---: |
| OnRe | Stopped at portion 2 | Stopped at portion 2 | Stopped at portion 2 |
| Maple | 9,997.636423 | 49,987.824715 | 99,974.755945 |
| PRIME | 9,996.878914 | 49,981.362745 | 99,956.308262 |
| AUTO | 9,994.506743 | 49,963.351564 | 99,911.990981 |

OnRe's first portion returned 998.037242 USDC. Its second hit Jupiter's 6001
minimum-output error. The failed transaction left the vault's token balances
unchanged. Error attribution checks the first failing program, so a different
program using error 6001 is not mistaken for a Jupiter price refusal.
[Jupiter's error reference](https://developers.jup.ag/docs/swap/v1/common-errors)
identifies 6001 as `SlippageToleranceExceeded`.

An earlier atomic OnRe capture at slot 466196060 selected a route through
`ZERor4xhbUycZ6gb9ntrhqscUcZmAbQDjEAtCf4hbZY`. That route returned error 5;
the observer correctly returned **unknown**, not a price refusal or a cost
estimate. One fresh capture produced the completed OnRe case above. Both
results are preserved; the precise cause of the route-specific error is unverified.

## Implications and limits

- Small buy/sell costs and large-exit liquidity are different constraints. A
  cheap OnRe round trip did not establish a cheap immediate large exit.
- A discount from reference NAV is not automatically the loss on a newly bought
  position. The observer compares actual cash spent and recovered.
- Conservative cost budgets and expected execution costs should remain visible
  separately. This evidence does not justify blindly lowering the safety budget
  or widening the price limit.
- The existing model already nets purchase and sale prices. The added evidence
  is stateful execution and larger-exit stress, rather than a new accounting formula.
- Frozen time, fixed routes and no outside liquidity replenishment make these
  bounded scenarios. They do not prove future prices, paced re-quoting, full
  leveraged debt repayment, lending deposits or user claim completion.
- The tool rejects mixed-slot account batches, missing/stale pricing evidence,
  unsafe amounts and unsupported post-buy quote changes. Captures currently must
  fit one 100-account RPC batch.

## Verification and artifacts

The Go command and existing pricing/selector regression tests passed, including
HTTP submission blocking and atomic snapshot tests. Four offline decision/input
tests passed. Actual replays verified exact V1 codec round trips, cash spent,
tokens fully sold, every successful exit's minimum output, and failed-sale token
balance rollback.

Reproduction instructions: `scripts/investment-shadow/README.md`.
Full public snapshots and replay logs are in
`/tmp/vault-shadow-evidence-20261106/{onre,maple,prime,auto}.json` and
`*-result.json`. The additional OnRe route failure is retained in
`onre-route-error.json` and `onre-route-error-result.json`.
Go test logs: `/tmp/vault-shadow-final-tests.log`.
