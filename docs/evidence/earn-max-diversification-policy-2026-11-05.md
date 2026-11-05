# Earn MAX gradual diversification — implementation verification

Implemented locally on November 5, 2026. Not committed or deployed by this
verification. The baseline is `a96ab545e` (including the already released
20 bps ordinary-withdrawal guard). [Sanitized measurements and source
hashes](earn-max-diversification-policy-2026-11-05.json) accompany this report.

This records the earlier candidate. Its 50% gross provider cap is superseded
by the [90/5/5 allocation-target candidate](earn-max-allocation-targets-2026-11-05.md).
The measurements below remain historical evidence for the earlier source.

## Behavior

- New investments leave the existing cash reserve funded: 10% of NAV, with a
  $5,000 floor and $25,000 cap, bounded by NAV. Pending withdrawals are reserved
  separately. Cash already moved to Squads is returned through the existing
  stage/restore path if the reserve becomes underfunded before investment.
- Additional gross collateral exposure is capped at 50% of current vault NAV
  per provider group. AUTO and every PRIME lane share a group. Borrowed
  exposure counts; shared collateral custody counts once. Incomplete holdings
  prevent additions. Existing positions above the cap are retained.
- Maple and PRIME entries are priced without borrowing, including when their
  debt pools have capacity. Further borrowing on these lanes is blocked while
  this policy is active. Existing leverage targets are preserved; necessary
  repayments and risk reductions still run.
- Maple's allowed swap exchanges are Manifest and Whirlpool. PRIME's routing
  is unchanged. Both new entry forecasts price an exit using the 20 bps limit.
  Executable costs, the seven-day payback horizon, uncertainty allowance and
  30-minute persistence window still determine whether an entry qualifies.
- The worker rechecks cash and provider capacity at execution, including the
  newly fetched swap quote. Saved borrowing approvals cannot bypass the cap.
- Automatic rotations and overflow moves are paused for Earn MAX under this
  new-cash policy. Deployment does not sell existing collateral to diversify.
  Existing signed-operation recovery, withdrawals, term-debt protection and
  emergency liquidation protection retain priority. Looping's allocation
  policy is unchanged.

No database migration, new transaction shape or new dependency is needed.
The `DiversifyNewCash` setting is enabled in the Earn MAX vault definition;
activating this code requires a normal Backyard worker release.

## Live policy replay

At **15:35:37 UTC**, slot **465758096**, the final candidate read current
confirmed holdings and stored planning inputs without writing production
state. It returned **HOLD / at_target**, matching the prior policy's lifecycle
decision. The selector returned KEEP because there was no cash above the
reserve. No sell, allocation or borrow was proposed.

Available Voltr cash was **$2,425.10** against a **$17,695.65** reserve target,
plus **$245.48** of pending withdrawals. No position is sold to fill that gap;
incoming funds must cover it before additions. The target changes with NAV.

All four leverage levels remained unchanged in the preview: ONyc/USDC 1.75x,
AUTO/PYUSD 1.75x, AUTO/wYLDS 3x and PRIME/PYUSD 1.5x. There was no pending
operation or manual-recovery latch. Provider room at that observation was zero
for OnRe, approximately $26,653 for AUTO/PRIME together, and $88,478 for Maple.
These are admission limits, not orders to invest those amounts.

## Funded route simulations and local checks

At 15:29–15:30 UTC, each destination completed a separate $10,000-funded
sequential RPC simulation: buy, Kamino deposit without debt, redeem/sell with
20 bps slippage, stage cash and restore Voltr idle. Maple explicitly exercised
Whirlpool; PRIME used its existing unrestricted routing. Each ended with zero
deposited receipts and zero debt for the tested position.

Each route's six money-moving messages compiled and passed the persistence /
signature validation boundary using an ephemeral, unfunded test key. The
largest wires were 1,530 bytes for Maple and 1,462 bytes for PRIME. Production
signing keys were never loaded, and no transaction was broadcast. Hypothetical
funding and reserve refresh messages were unsigned simulation fixtures.

The funded tests deposited the purchase's guaranteed minimum, leaving the
better-filled purchase surplus outside Kamino. Cash restored was $9,948.01 for
Maple and $9,948.47 for PRIME, with 41.962247 syrupUSDC and 46.800147 PRIME
respectively still outside Kamino. These cash figures are **not a calculation
of total round-trip loss**. The simulations qualify execution; they do not
authorize using today's underfunded cash reserve.

The final `scripts/verify-workers-v2.sh` gate passed with a fresh disposable
Postgres cluster. Regression cases cover gross/shared-provider accounting,
missing observations, unborrowed entries, cash changes after allocation,
restart serialization, fresh-quote admission, saved up approvals, grandfathered
positions, policy persistence, signed recovery and withdrawal/emergency
priority. Historical lifecycle fixtures explicitly retain their earlier policy;
the new regression suite and live preview enable the new policy.

The disposable database was stopped and remote diagnostic files removed.

## Limits

The cap is checked using current valuation and executable quotes. Price moves,
better-than-quoted fills and NAV changes can subsequently move a group above
50%; that does not trigger a forced rebalance. Grandfathered concentration
falls gradually as other assets receive inflows. An uneconomic quote keeps
funds in cash, so entry timing and yield are not guaranteed.

Earlier $100,000 exits and adverse-liquidity experiments are documented in
[the withdrawal-guard evidence](earn-max-withdrawal-guard-2026-11-05.md).
This run did not repeat those stress scenarios or implement paced sales,
an overall withdrawal-cost ceiling, longer withdrawal windows or fee changes.
Provider defaults, token markdowns, common infrastructure failures and future
liquidity remain risks.
