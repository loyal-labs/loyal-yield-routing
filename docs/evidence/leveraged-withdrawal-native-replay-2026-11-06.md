# Leveraged withdrawal replays — 6 November 2026

Completed **13 offline native replays**, without running Node. No production
signer, database, service configuration or real funds were used. The new
`CheckInvestmentExits` switch remains false in both vault configurations.

## Results

Amounts below name hypothetical starting equity. At 1.5×, a $100,000 case
starts with approximately $150,000 of collateral and a $50,000 loan, plus any
small existing balance in the captured Backyard accounts.

| Route | $10k equity, 1.5× | $50k equity, 1.5× | $100k equity, 1.5× | New entry check |
|---|---|---|---|---|
| OnRe / ONyc / USDC | Price refusal on step 2 | Price refusal on step 2 | Price refusal on step 2 | Rejects all three |
| Maple / syrupUSDC / USDC | Complete, 16 transactions | Complete, 76 transactions | Complete, 151 transactions | Accepts all three |
| PRIME / PRIME / USDC | Complete, 16 transactions | Complete, 76 transactions | Complete, 151 transactions | Accepts all three |
| AUTO / AUTO / PYUSD | Complete, 17 transactions | Complete, 77 transactions | Complete, 152 transactions | Accepts all three |

“Complete” means all simulated loan debt was repaid and all collateral was
converted to USDC. AUTO's remaining PYUSD was also converted to USDC. These
are strategy unwinds, not final Voltr claims into a user's wallet.

OnRe's first approximately $1,000 collateral sale and repayment succeeded.
Its second transaction failed the price floor. The test checked that the failed
transaction left the vault's token balances and obligation unchanged. The
remaining position was retained; a completed withdrawal is not claimed.
The native results matched the earlier completed Maple $10k and OnRe outcomes,
including final balances, remaining debt and transaction counts.

An additional **$100k AUTO case at 2×** ($200k collateral, $100k PYUSD loan)
completed in **202 transactions**, also ending with zero debt and collateral.
However, the new entry check rejected it: the simulated full-size sale returned
$199,351.304661 against a reference value of $199,999.999948, a roughly 0.324%
discount, above the 0.2% threshold. Gradual protected transactions could still
finish. This demonstrates a conservative rejection, not a broken withdrawal.

The per-transaction price floor is measured against the collateral converted.
It is not a guarantee of zero withdrawal cost or a 0.2% maximum loss on
leveraged equity. Native transaction fees are recorded separately in the raw
reports. No investment return or profitability claim follows from these exits.

## What was executed

The tests load captured public program and account data from one atomic RPC
snapshot per route. The existing Go worker builders produce the actual V1
withdraw/swap/repay messages and validate a separately compiled wire with an
ephemeral test signer. The native simulator executes the unchanged unsigned
worker messages against the captured Squads, Kamino and swap programs.
Each successful transaction's account changes remain in the bank for the next
transaction. There is no 20-transaction bundle limit and no reset of pool
liquidity between withdrawal portions.

Hypothetical collateral is seeded locally and deposited through the worker's
Kamino builder. The loan is then created by an actual simulated borrow. The
borrowed cash is removed locally to represent a position that has already
invested it. This setup does not replay the original purchase.

OnRe's captured pool refuses new borrowing above its utilization limit. To
construct an existing loan, the setup temporarily supplies additional local
reserve liquidity, borrows, and removes exactly that extra liquidity before
any withdrawal. This is a hypothetical prior position, not evidence that a new
OnRe loan could be opened against the captured live pool.

The new entry checker receives full-size proceeds from a local quote simulation,
using the pre-funding holdings and actual added collateral and debt. The quote
probe does not change the withdrawal bank; its state digest is checked before
and after. AUTO also exercises its separate collateral-to-PYUSD repayment quote.
The preceding unit tests remain the coverage for purchase pricing, the existing
profitability model, shared-lane accounting and selector size reduction.

## Resource controls and native runtime

Every build/replay ran alone in a transient local systemd service with:

- `CPUQuota=50%`: half of one CPU core.
- `MemoryMax=768M`, `MemorySwapMax=0`, `TasksMax=64`.
- `Nice=15`, `RuntimeMaxSec=180`, and `PrivateNetwork=yes` for execution.
- Go subprocesses limited to one runtime processor and a 64 MiB GC target.

The replay verifies the actual cgroup limits and absence of an external network
route before loading accounts. Peak memory across the 13 final simulations was
**252.11 MiB**. Every final report records zero memory-limit hits and zero OOM
kills. All temporary replay services exited and were collected.

The Python/native runtime is `solders==0.29.0`. This release needed its runtime
compute budget explicitly set from the V1 message's own compute and heap fields:
the default 32 KiB heap failed a Squads instruction despite a 64 KiB wire request.
The harness applies exactly the encoded limits, without increasing them or
changing the transaction bytes. The corrected Maple control reproduced the
previous result exactly. Native V1 and runtime APIs are documented in the
[release notes](https://github.com/kevinheavey/solders/releases/tag/v0.29.0) and
[LiteSVM API](https://kevinheavey.github.io/solders/api_reference/litesvm.html).

## Decision and limits

The large leveraged replay gap is now covered for these captured scenarios.
The tests support the price protection and entry-check arithmetic. They also
show that a full-size sale quote can reject an AUTO holding that can be unwound
gradually. An automatic rollout must consciously accept that extra restriction;
it should not be presented as a neutral change to investment availability.
Observing this new check's decisions before enforcing them is the conservative
rollout choice. Existing transaction price protection should remain in place.

These are frozen-time, fixed-route simulations. They do not prove future
liquidity, the arrival of replacement quotes or outside market makers, interest
accruing across a real paced withdrawal, production scheduling/recovery, or the
final user claim. Existing positions were not sold, targets were not migrated,
and no deployment or activation was performed.

## Artifacts

The companion [result summary](leveraged-withdrawal-native-replay-2026-11-06.json)
contains all 13 outcomes, balances, entry-check results, resource measurements,
and SHA-256 hashes of the raw reports.

- Native replay, result JSONs and logs: `/tmp/vault-native-replay-20261106/`.
- Captured public states and Go driver: `/tmp/vault-leveraged-replay-20261106/`.
- Native entrypoint: `replay.py`; sequential launchers: `run_matrix.py` and
  `run_final.py`. The entrypoint refuses to run without the stated cgroup caps.
- Go driver: `offline_test.go`, compiled through the recorded `overlay.json`.
- Superseded Node resource-timeout attempts are not counted as successful tests.
