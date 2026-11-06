# Investment entry protection — 6 November 2026

This is the final implementation record. It supersedes the observation-release
proposal in the earlier comparison notes. The release enables the rule for
both Loyal and Backyard; there is no observation-only deployment or waiting day.
Deployment status is recorded in the rollout plan.

## Rule and scope

Before admitting fresh capital, project the whole collateral holding after the
purchase and planned borrowing. Include existing positions sharing that mint,
count common custody once, and include outstanding loans, borrowing fees and
the existing execution-window interest allowance. Check both the total exit
and the last roughly $1,000 portion against the 99.8% reference-value floor.
The existing purchase/exit profitability budget and reserve policy still apply.

Repay USDC debt from USDC proceeds. For PYUSD debt, sell collateral to PYUSD,
repay the loan, and price only the surplus PYUSD into USDC. These two routes
must use independent pools. Repay wYLDS by exact Hastra redemption of enough
AUTO, with rounding up, then sell the remaining AUTO. This supports simultaneous
AUTO/PYUSD and AUTO/wYLDS positions without inventing a direct wYLDS market exit.

Preparation repeats the check with its observed account batch and the actual
prepared purchase output, including ordinary leverage increases. The existing
NAV certificate binds the final request and expires before signing. Stored
targets and old selector approvals cannot bypass preparation. This changes no
transaction shape and requires no state migration. Withdrawals, repayments,
NAV reporting, signed recovery and emergency risk reduction keep their paths.
An expensive holding causes a new investment to wait; it does not request a sale
or reset leverage.

## Availability and diagnosis

Sixteen live paired quote probes exposed equivalent split routes listed in
different orders and occasional different context slots. Route comparison now
normalizes connected acyclic paths and exact basis-point allocations. It accepts
valid split routes and rejects disconnected, cyclic, incomplete or overlapping
liquidity. Integer percent fields may be rounded; basis points take precedence.

One changed-slot result permits one fresh complete quote set. A price rejection
or service outage is never retried as though it were a cheap smaller quote.
The selector can try a smaller addition after an expensive result. It preserves
distinct `investment_exit_too_expensive`, `investment_exit_unavailable`,
`investment_exit_quote_slots_changed` and `investment_exit_quote_route_changed`
reasons. Availability failures remain availability failures for selector history.

## Verification

All expensive work runs alone with CPUQuota=50%, MemoryMax=768M,
MemorySwapMax=0, TasksMax=64 and a 180-second limit. Offline replays have no
external network. No Node process, validator, production signature or funded
transaction was used.

- 204 focused cases plus the rounded-percentage regression passed; Go vet passed. Tests cover exact price boundaries, stale/missing prices, changed
  banks, split routing, smaller additions, shared custody, both AUTO debts,
  expired and modified requests, ordinary borrowing and signed recovery.
- Eleven formerly skipped database tests passed in a fresh disposable Postgres
  cluster with the actual withdrawal migrations. They cover accounting,
  reservations, recovery and vault-scoped durable pacing.
- New purchase/exit cases use freshly captured mutable accounts in one RPC bank.
  Program versions are verified before/after capture. Each executes ten actual
  $1,000 purchase wires, then quotes and executes sales against the resulting
  pool state. Pools are not topped up. Only hypothetical depositor cash is seeded.

| $10,000 purchase followed by immediate sale | Conversion cost, before SOL fees | New exit check |
|---|---:|---|
| OnRe | $3.358990 | Reject |
| AUTO | $4.399042 | Accept |
| Maple | $1.999905 | Accept |
| PRIME | $2.399691 | Accept |

OnRe's final approximately $1,000 portion quotes $997.505139, below its $998
floor. Its market price is also below the reference valuation. A small loss
relative to purchase cost therefore does not establish that the existing
reference-price execution guard will permit the whole normal exit.

A fresh $100,000-equity / 1.5x Maple unwind completed 151 protected transactions.
It included $34.258307 of hypothetical interest charged upfront: six hours at
100% APR. Final USDC was $99,929.818490, with no remaining debt or collateral.
The replay had already produced $10,098.228252 while collateral remained.
Every executed worker step passed compilation and persist/sign validation.
This tests a conservative fixed-rate carry scenario, not future interest rates.

The earlier thirteen $10k/$50k/$100k leveraged withdrawal cases remain documented
in [the native replay report](leveraged-withdrawal-native-replay-2026-11-06.md).
Their unaffected full matrix was not repeated. The native driver and runners
are retained in `investment_replay_test.go` and `scripts/vault-replay/`.

## Current-vault check

At 19:26 UTC the read-only candidate calculation returned
`investment_exit_too_expensive` for an additional OnRe purchase in both vaults.
Each completed in about two seconds. The separately sampled OnRe exit wire was
also refused by the already-deployed reference-price guard
(`withdrawal_waiting_price`). These are expected price refusals, not an RPC
outage. No wire was signed or broadcast, and no live target was changed.

## Limits

This screen measures current liquidity, not a guaranteed future price. Quotes
with genuinely different paths/banks or unavailable services can defer new risk.
The 0.2% collateral floor is not a maximum loss on leveraged equity and excludes
SOL fees; the existing profitability model separately budgets those costs.
The supported shared-debt combination is AUTO/PYUSD plus AUTO/wYLDS; holdings
with two other distinct non-USDC debts cannot currently establish this exit
proof and hold additional purchases of that collateral.

The new tests use local signatures and hypothetical balances. They do not
exercise a live user claim, future liquidity replenishment, changing rates over
real elapsed hours, or a real-money production lifecycle. The existing public
withdrawal API and partial-withdrawal planning are unchanged. Existing manual
investment permissions are preserved by deployment.

[Machine-readable results and hashes](investment-exit-validation-2026-11-06.json).

The input/result archive is retained read-only on `loyal-workers` at
`/opt/loyal/evidence/investment-exit-20261106/fixtures-and-results.tar.gz`.
SHA256: `4a482bbaeb2ad3333b4a72f5e75f025acebb4cab5211a1d85a7ce1a0f4a357d6`.
