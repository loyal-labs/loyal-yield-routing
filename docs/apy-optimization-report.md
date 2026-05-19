# APY Optimization Report

This report summarizes the five-day Kamino reserve APY analysis using the dataset in `data/kamino-vaults-api.jsonl`.

## Inputs

| Input | Value |
| --- | --- |
| Sample window | `2026-05-07T15:15:16.609Z` to `2026-05-12T20:49:48.507Z` |
| Rows parsed | `42,317` |
| Distinct snapshots | `1,467` |
| Average snapshot spacing | `5.14` minutes |
| Starting position | `$1,000` in Prime Market USDC |
| Reserve filter | Stablecoin reserves only |
| TVL filter | Reserve TVL must be above `$100,000` |
| APY outlier filter | Exclude APY values at or above `50%` |
| Compounding | Continuous compounding over the observed elapsed time |

## Fee Model

The fee-aware run includes two costs:

1. Jupiter swap loss, computed from Jupiter `outAmount / inAmount` quotes.
2. One Solana base transaction fee per pool change.

Withdrawing from one reserve and depositing into another reserve can be done in one transaction, so the pool-change fee is modeled as `5,000` lamports per vault or reserve change. Using `SOL = $84.82`, this is `$0.0004241` per pool change.

Same-token reserve moves, such as USDC reserve to another USDC reserve, do not pay Jupiter swap loss. They do still pay the `5,000` lamport pool-change fee.

## Jupiter Quote Results

Jupiter quotes were sampled through the quote API using `$1,000` notional input size, assuming six-decimal stablecoin units. Each available directed pair was queried three times and the median fee was used.

The optimal path used only these cross-token swaps:

| Swap | Samples | Median fee |
| --- | ---: | ---: |
| USDC -> PYUSD | 3 | `0.125` bps |
| PYUSD -> USDC | 3 | `0` bps |

Other candidate stable pairs were also queried. Several returned no route or hit public API rate limits during the run, so those directed edges were treated as unavailable instead of estimated.

## Results

The best net strategy is still the `5m` cadence, which is effectively every observed dataset snapshot. After adding Jupiter quote loss and the `5,000` lamport pool-change fee, the strategy produces:

- Ending value: `$1,001.241876`.
- Profit over the sample: `$1.241876`.
- Annualized APY: `9.0437%`.
- Decisions: `1,465`.
- Pool changes: `19`.

Frequency scan:

| Cadence | Ending value | Profit | Annualized APY | Pool changes |
| --- | ---: | ---: | ---: | ---: |
| 5m | `$1,001.241876` | `$1.241876` | `9.0437%` | 19 |
| 10m | `$1,001.237207` | `$1.237207` | `9.0082%` | 17 |
| 15m | `$1,001.233667` | `$1.233667` | `8.9813%` | 15 |
| 20m | `$1,001.231036` | `$1.231036` | `8.9613%` | 13 |
| 30m | `$1,001.228861` | `$1.228861` | `8.9448%` | 9 |
| 45m | `$1,001.222608` | `$1.222608` | `8.8974%` | 6 |
| 60m | `$1,001.224572` | `$1.224572` | `8.9123%` | 7 |
| 90m | `$1,001.217152` | `$1.217152` | `8.8560%` | 5 |
| 120m | `$1,001.214242` | `$1.214242` | `8.8339%` | 5 |
| 180m | `$1,001.211242` | `$1.211242` | `8.8112%` | 6 |
| 240m | `$1,001.207931` | `$1.207931` | `8.7861%` | 5 |
| 360m | `$1,001.197534` | `$1.197534` | `8.7073%` | 5 |
| 480m | `$1,001.195940` | `$1.195940` | `8.6952%` | 5 |
| 720m | `$1,001.176322` | `$1.176322` | `8.5467%` | 5 |
| 1440m | `$1,001.140230` | `$1.140230` | `8.2741%` | 2 |

## Interpretation

The `5m` cadence wins on the historical sample, but the margin is narrow after costs. The difference between `5m` and `20m` is about `0.0824` annualized percentage points and about `$0.01084` on `$1,000` over the five-day window.

That makes `5m` the mathematical optimum for this data, while `20m` is close enough to be a practical default if execution reliability, priority fees, RPC load, or operational simplicity matter.

## Caveats

- This is a historical five-day backtest. It should not be read as a forward-looking guarantee.
- Jupiter quotes were sampled after the APY window, so quote costs are an execution-cost estimate rather than exact historical quotes.
- Priority fees, compute-unit pricing, account creation, rent effects, failed transactions, and retry costs are not included.
- The analysis assumes reserve withdrawals and deposits can be bundled into one transaction for a `5,000` lamport base fee per pool change.
- Public Jupiter rate limits prevented fresh quotes for every possible directed pair. Unquoted pairs were treated as unavailable.
