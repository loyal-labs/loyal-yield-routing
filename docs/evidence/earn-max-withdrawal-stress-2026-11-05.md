# Earn MAX: large funded exits and liquidity stress

Read-only tests on November 5, 2026 extended the earlier PRIME/Maple comparison.
Six large-position simulations completed, and three market-pressure scenarios
executed. The more severe Maple scenario could not obtain its pressure quote and
is explicitly **unproven**, not passed.

The practical finding is that successful execution can still be expensive:
a PRIME exit returned **$421.114941 less than its pre-stress quote** after a
roughly $5 million preceding sale. That 0.42337% deterioration fits within the
existing 0.5% slippage tolerance.

No live funds moved. Production signing keys were never loaded; no worker
settings changed and no transactions were broadcast. The [sanitized evidence](earn-max-withdrawal-stress-2026-11-05.json)
records timestamps, bank slots, route pools, transaction errors, wire checks,
and balance changes. Private raw simulations, source overlays and the verification
log remain in `/tmp/earn-max-stress-20261105`. All uploaded probes were removed
from the workers host.

## Why the original test was small

The original lifecycle test used $1,000 of the vault's actual available cash.
Larger amounts were checked as independent quotes. That was an incomplete
verification scope, not a fundamental inability to simulate larger positions.

These tests use unsigned funding fixtures inside RPC `simulateBundle`, which
carries state through a sequence of transactions. Only $1,000 of each position
comes through the real vault allocation instruction; additional USDC is a
hypothetical transfer into strategy custody. Funding signatures are invalid and
signature verification is disabled for simulation. No production or donor key
is accessed. The diagnostic HTTP transport rejects transaction submission.

The funding source for each pressure test is excluded from all of its observed
vault/reserve accounts and buy/exit/pressure instruction accounts. Actual swap
instructions consume the tested market liquidity; the harness does not edit
DEX balances or reserve accounting to manufacture the shock. Hypothetical
funding is not evidence that a donor would or could authorize a real transfer.

See [Solana simulation documentation](https://solana.com/docs/rpc/http/simulatetransaction)
and [sequential bundle simulation](https://docs.triton.one/trading-apis/bundle-simulation-with-jito).

## Normal large-position tests

| USDC funding for the position | Maple syrupUSDC | PRIME |
|---|---|---|
| $10,000 | Passed | Passed |
| $50,000 | Passed | Passed |
| $100,000 | Passed | Passed |

Each replay allocates the live $1,000, adds hypothetical funding, buys tokens,
deposits the guaranteed purchase amount without borrowing, withdraws that entire
Kamino deposit and sells it atomically, stages the USDC, refreshes valuations,
and restores the proceeds to Voltr idle withdrawal cash.

Production account decoders independently verified that each final replay had
zero debt, zero remaining deposited collateral, and zero shared/staged USDC.
The final idle-account increase exactly matched the staged sale proceeds.
Six financial wires per case passed the existing exact-envelope/hash/signature
test helper using an ephemeral key. No signed operation was persisted to a DB.

The buy's surplus above the guaranteed minimum—approximately 0.5% of funding—
remains as collateral tokens outside Kamino. This is not a trading loss. These
are exits of the guaranteed deposited portion of a large funded position,
not liquidation of every surplus token or payment of a user withdrawal receipt.

At the observed snapshots, both collateral reserves had zero borrowing and a
zero borrowing limit for the deposited asset. A full USDC borrowing pool does
not itself prevent these unborrowed deposits or collateral withdrawals. That
observation is specific to the captured reserve configuration.

## Market-pressure tests

A hypothetical preceding seller receives collateral and sells it through real
Jupiter/DEX instructions before the worker's quoted exit. Both execute in one
simulated bank. The preceding sale touches at least one pool used by the exit.
The worker's normal price limits and installed policies remain in force.

Each tested position was funded with $100,000. Pressure sizes below are rounded:
the first pressure sale is ten times the worker's exit token amount; the stronger
one is fifty times. These are roughly $1 million and $5 million of sales.

| Preceding sale | Asset | Worker exit result | Below pre-stress quote |
|---|---|---|---:|
| ~$1m | Maple | Executed; lending position cleared | $31.47 / 0.03163% |
| ~$1m | PRIME | Executed; lending position cleared | $76.03 / 0.07643% |
| ~$5m | Maple | Pressure quote unavailable; scenario not executed | Unproven |
| ~$5m | PRIME | Executed; lending position cleared | $421.11 / 0.42337% |

Differences use USDC balances immediately before and after the final exit in
that same replay, compared with its pre-stress quote. They are not percentages
of total vault NAV or predictions of future trading losses. The first buy's
market effect is also present in each replay. Network fees are separate.

For the stronger Maple case, the attempted pressure quote and a targeted recheck
returned HTTP 400; the recheck's explicit error was `NO_ROUTES_FOUND`. This is
specific to the tested Whirlpool route filter and account budget. It does not
prove that Maple has no liquidity through every venue, nor that its $100,000
exit would fail after a shock we could not execute.

Maple uses the candidate **Whirlpool** filter in these simulations; production
still uses **Manifest**. PRIME uses its current route configuration. Therefore,
the Maple result requires the proposed routing change before it applies to
production. No routing change was deployed here.

The stress replays stop after the atomic lending withdrawal/sale. Their large
hypothetical seller proceeds are not restored to the vault or treated as user
assets. Normal-case tests separately verify the cash-restoration steps.

## Failure control and exclusions

An intermediate test tried to sell purchase surplus measured in an earlier
simulation. Replaying the prefix at a fresh bank changed the buy fill slightly,
leaving its PRIME sale short by 548 token base units. That rejected transaction
is excluded from market-liquidity conclusions. All 55 captured principal account
states were identical before and after its failure, providing an atomic
rollback control. Final pressure tests sell only the guaranteed deposited
amount, with one base unit reserved for redemption rounding.

An initial broad USDC funding lookup hit the RPC's account-count limit; targeted
account reads replaced it. A preliminary PRIME run was held by unavailable
valuation refresh. Both are retained in private diagnostic artifacts and are
not counted as completed market tests. A separate attempted all-surplus PRIME
exit hit the same stale-fill fixture issue and is likewise excluded.

## Implications for the rollout

1. Large unborrowed exits are now exercised at meaningful position sizes. Keep
   the cash reserve funded and preserve existing positions/targets on rollout.
2. Successful execution is insufficient as a loss-control criterion. For
   ordinary unborrowed withdrawals, evaluate a stricter executable price floor
   alongside the proposed smaller sales and withdrawal scheduling.
3. A 0.2% tolerance relative to the stronger PRIME test's original quote would
   reject its observed fill: the output was 0.42337% worse. This is a calculation
   from the executed result, not a test or deployment of a new 0.2% policy.
   Refusing that price means waiting, finding another executable route, or
   drawing on available cash; it does not create liquidity.
4. Keep both providers under evaluation. These snapshots do not establish a
   universal liquidity or credit-risk ranking, or select an allocation cap.

The tests do not cover underlying borrower defaults, primary-redemption queues,
a token/oracle depeg, simultaneous withdrawal of the entire vault, recovery over
time, liquidity replenishment between paced trades, or exits of existing
leveraged positions. They do not establish that every future withdrawal can
finish inside the current user-facing wait period.
