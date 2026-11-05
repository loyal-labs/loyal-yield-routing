# Earn MAX loss controls — November 4, 2026

Status: implemented and verified locally on main `38e8dd309`. No production
migration, deployment, signing, commit or push was performed. The changes
reduce avoidable trading expense; they cannot guarantee positive returns.

## What caused the losses

The October 29 rate response sold collateral repeatedly as the desired
leverage fell from 2.5x toward 1x. Five ONyc sales returned $235,414.46 against
$236,562.96 of nearby oracle marks: $1,148.50 of immediate expense. At an
unchanged 3.02 percentage-point adverse spread, retaining that much debt costs
about $19.46 per day. The sale expense equals roughly 59 days of that carry.
That comparison explains why a temporary rate dip need not justify selling;
it does not establish that holding indefinitely is safe or cheaper.

Three code behaviors compounded this:

- Atomic sales repaid debt in the same transaction, but cost reconciliation
  looked at the final cash increase. It missed proceeds sent straight to
  the lender and left the learned exit cost stale.
- Optional reductions followed spread thresholds without requiring their
  expected interest savings to repay execution costs. Cash left by a step
  could then be bought back into collateral before the reduction finished.
- An economic target of 1x still meant a 1.5x position. It did not remove
  borrowing exposure.

Large withdrawals separately forced sales. The November 4 funding episode
sold $145,491.38 of marked collateral for $642.55 less than its marks. The
October 25 withdrawal was about 73% of the pool. The fixed $5,000 reserve
was introduced on October 26, after that first episode; it cannot explain
the October 25 loss as an already-active protection that failed. The reserve
is retained from incoming cash and can be exhausted by withdrawals.

The retained [receipt projections](../go/workers/internal/backyard/testdata/earn-max-sales-2026-10-29.json)
contain five October 29 ONyc sales, three AUTO sales, and the November 4
atomic ONyc sale. Marks come from adjacent observations within 100 slots,
not invented historical swap quotes. AUTO proceeds are valued in PYUSD,
using the saved token prices. The [scenario inputs](evidence/earn-max-policy-2026-11-04.json)
retain transaction signatures and the aggregate withdrawal observations.

## Implemented behavior

| Control | Local behavior |
|---|---|
| Learn the full sale cost | Reconstruct proceeds from the cash change plus the actual debt-reserve credit. Validate custody identities and the maximum repayment. A favorable sale clears an old expensive observation without making all future quotes outliers. Legacy operations lacking sale metadata still reconcile. |
| Avoid costly optional reductions | Quote the entire remaining reduction and compare seven days of interest saved with yield forgone, sale expense, network fees, $0.25 minimum benefit and 10 bps uncertainty on debt retired. Require 30 minutes of qualifying samples, with no gap over 10 minutes. |
| Recheck before each sale | Store an approval with a 10-minute expiry and an expense ceiling per dollar of collateral. Check the prepared transaction's minimum swap proceeds and fee allowance. Fresh approval of the same unfinished reduction cannot increase its active ceiling. |
| Preserve mandatory exits | Hard LTV, term maturity, operator exits and withdrawal funding retain priority. Missing, expired or malformed economic approval blocks optional sales, not these exits. |
| Stop buyback churn | Leave residual USDC alone while the stored debt reduction remains unfinished. That cash does not prevent repricing and renewing an expired reduction approval. This does not yet use it directly to repay debt. |
| Hold more withdrawal cash | Earn MAX reserves 10% of NAV, with a $5,000 floor and $25,000 cap, limited to available assets. Quote sizing and selection use the same amount; withdrawals can spend it. No forced sales refill it. |
| Price a shorter holding period | Earn MAX optional entry, rotation and leverage decisions use seven days instead of 30. Looping retains its existing horizon. |
| Make 1x literal | Earn MAX's economic 1x target uses the existing full-debt repayment path after approval. Looping retains its existing 1.5x economic floor. |

The expense ceiling is a ratio applied to every sale, not an absolute
cumulative dollar budget. A remaining reduction that cannot be quoted in
full is held conservatively. Existing positive-cost quote outlier rules are
otherwise unchanged. Historical operation rows and NAV are not rewritten.

Deployment must preserve the existing book. A full borrowing reserve blocks
new borrowing; it does not itself require closing existing loans or selling
collateral. Migration 0137 translates legacy Earn MAX level 1 to level 1.5,
preserving its old meaning without authorizing a new borrow. A later move
to actual 1x needs a fresh economic decision. Selector observation windows
are now bound to the policy that produced them: legacy windows or a changed
horizon/reserve policy restart the persistence period instead of allowing
an immediate trade. A normal restart with the same policy can retain valid
observations and must still recover already-signed transactions.

## Why these candidate settings

The reproducible [comparison](evidence/earn-max-policy-2026-11-04.md) evaluates
fixed and percentage reserves, lower leverage, and paced sales. Run
`python3 scripts/earn-max-policy-comparison.py` to regenerate it; its assertions
check the model's key invariants.

If a $25,000 reserve had been available before both recorded withdrawals and
replenished by inflows between them, it could have avoided $160–520 of sale
expense, against $64–99 of foregone yield over 12 days. Those are conditional
estimates. An unfunded reserve saves nothing, and an extra sale to fill it
has its own cost. Neither $25,000 nor $50,000 covers the large recorded
withdrawals completely. Ten percent scales the reserve down as the vault
shrinks; the cap limits cash drag as it grows. These values are candidates,
not statistically established optima.

Seven days makes optional trades earn back their expense sooner. The leverage
comparison shows why borrowing more can improve long-held returns while
worsening short-held returns after entry and exit costs. It does not justify
a universal 1x setting: every optional reduction still needs its own priced
benefit. Existing risk brakes remain authoritative.

## Rodion's proposals: concrete next experiments

The subsequent [read-only execution study](evidence/earn-max-execution-study-2026-11-04.md)
captured 96 size/time quotes, simulated four small repayments, and compared
diversification exits. Splitting and waiting had little benefit in that window;
broader Maple routing produced much better quotes than its current filter.
The study records the remaining execution checks before using that route.

The [November 5 diversification follow-up](evidence/earn-max-diversification-2026-11-05.md)
refreshes concentration and cash figures and validates a $1,000 Maple purchase
and sale through Whirlpool using sequential unsigned simulation under existing
policies. Full lending entry/exit and intended-size withdrawal stress remain
unverified. It also distinguishes equity allocation from gross leveraged
exposure; no limit or production routing change has been enabled.

The subsequent [PRIME/Maple comparison](evidence/earn-max-prime-maple-comparison-2026-11-05.md)
validates both small lending cycles without borrowing, including cash restoration
to the vault, and compares larger quote pairs. PRIME has the higher observed
native yield; Maple adds another provider group. PRIME remains a candidate even
when its borrowing pool is full. The subsequent
[large-withdrawal and liquidity-stress study](evidence/earn-max-withdrawal-stress-2026-11-05.md)
passes six funded-position cases at $10,000/$50,000/$100,000, plus executed
market-pressure tests. The stronger PRIME test still accepts a 0.423% worse
fill within today's 0.5% tolerance; the stronger Maple pressure scenario could
not obtain its quote and remains unproven. Evaluate a stricter executable price
floor for ordinary unborrowed withdrawals before treating diversification as
ready. The cash reserve takes priority over either allocation.

**Slower large withdrawals and smaller sales.** First compare one sale with
several freshly quoted smaller sales inside the existing 600-second waiting
period. Use the required cash shortfall after available cash, each route's
executable minimum output, extra network fees, and the remaining deadline.
Keep progress in the existing operation journal; re-read holdings and demand
before every step, and stop once the shortfall is covered. Hard-risk debt
repayment must not wait for a better price.

The comparison must measure liquidity replenishment between pieces. In the
included constant-product example, ten consecutive pieces with no refill
cost exactly as much as one sale before fees. With replenishment they cost
less. An actual 0.1–0.2% execution loss on the November 4 volume would save
$351.57–497.06 before waiting costs, but there is no retained quote sequence
proving that execution was available. Slippage tolerance alone cannot promise
that result: an already-discounted quote plus 0.1% tolerance can still lose
more than 0.1% against the collateral mark.

Before implementing pacing, capture size/depth quotes over a full withdrawal
window, test cancellation and partial completion through the real transaction
compiler and persistence/signing validation, and select a deadline fallback
from actual quotes. Extending the user's wait beyond 600 seconds additionally
needs withdrawal-contract compatibility and an explicit product policy.
Neither pacing nor a longer wait is enabled by this patch.

**Diversification.** Evaluate a 50% maximum exposure to one independent
collateral/provider group, beginning with new inflows. Assess shared borrower
reserves, collateral risks and exit pools: two borrowing loops using the same
collateral are not independent protection. Require executable exit capacity
for the expected withdrawal size in each proposed destination. If there is no
second suitable destination, keep excess cash rather than manufacture a
costly rotation to hit a percentage. Compare net return and joint withdrawal
stress before choosing the limit. No concentration cap or automatic
diversifying trades are enabled here.

## Next implementation and release sequence — November 5

Status: the 20 bps ordinary-withdrawal guard was merged in [PR #729](https://github.com/loyal-labs/loyal-yield-routing/pull/729) and deployed to Backyard
on November 5 at 15:01 UTC, release `d27fbb4d1dfd450fdf584f231ff1e510d6ba917d`.
[Pre-release verification](evidence/earn-max-withdrawal-guard-2026-11-05.md) includes
funded position exits and atomic rejection of a stressed PRIME sale. Post-deploy
checks preserved all four collateral positions and leverage targets, with no
capital trades or restarts observed. Two temporary price-refresh read holds
and one expired NAV report recovered automatically; six NAV reports reconciled.
No live sale under the new 20 bps limit occurred during this observation.
Paced withdrawals and diversification controls remain planned.

### First release: stricter ordinary-withdrawal execution

- Set a 20 bps maximum deterioration from an approved quote for new ordinary
  withdrawal sales. Carry the withdrawal purpose and limit through quoting,
  preparation, executable minimum-output validation and the operation record.
  The implementation keeps the shared 50 bps default and records a stricter
  per-operation limit for ordinary withdrawals, including their residual sales.
- Preserve the existing bounded rules for liquidation prevention and other
  urgent debt reduction. A withdrawal deadline must not silently reclassify an
  ordinary sale as an emergency or widen its allowed slippage.
- Use available withdrawal cash first. If a sale cannot satisfy its limit,
  reobserve and obtain a usable route, or hold and alert. Record the reason and
  outstanding shortfall. Preserve the current 600-second withdrawal contract;
  the deadline itself is not permission to accept a worse price. Verify the
  pending-withdrawal behavior before release so a hold is visible and recoverable.
- Distinguish the quote-to-fill tolerance from total economic expense. A poor
  initial quote can still lose money with 20 bps slippage. Withdrawal pacing and
  repeated repricing need a separately preserved economic expense ceiling;
  lowering slippage alone must not be described as a 0.2% maximum total loss.
- Recover already-signed operations using their recorded wire and terms. Apply
  the new rule to newly prepared operations; do not discard or re-sign an
  ambiguous transaction to force it into the new policy.

The release gate is a real-wire simulation of the new 20 bps policy: normal
funded exits succeed, adverse fills below its minimum fail atomically, and a
later acceptable quote can complete the remaining withdrawal. Validate the
compiled wire/signing boundary, partial completion, deadline holds, restart
recovery and the separate urgent-repayment path. Repeat the current-vault
continuity check before activation: existing holdings and leverage targets
remain stable when borrowing reserves are full. Release this protection before
activating diversification; check the first held/failed operation as well as
successful execution after cutover.

### Second release: gradual diversification with funded cash reserves

The initial candidate was verified locally on November 5; activation is pending.
The [initial verification](evidence/earn-max-diversification-policy-2026-11-05.md)
records the earlier 50% gross-cap policy, live HOLD replay and funded Maple/PRIME
route simulations. The [revised candidate](evidence/earn-max-allocation-targets-2026-11-05.md)
replaces that cap and adds small allocation targets. Its full worker checks
passed, and a current-vault replay preserved the HOLD decision and all four
stored leverage targets. Automatic rotations and overflow moves remain paused
while new cash funds gradual diversification.

- Reserve incoming cash using the existing policy: 10% of NAV, a $5,000 floor
  and a $25,000 cap, subject to available assets. Confirm it is actually funded
  before investing the remainder. Do not force sales to fill it on deployment.
- Target 5% Maple, 5% PRIME and 90% OnRe/AUTO combined, measured in own capital
  after subtracting the withdrawal reserve and pending withdrawals from NAV.
  Count collateral minus debt in each bucket, plus loose collateral once per
  mint. All PRIME lanes share one target; all AUTO lanes join OnRe in the 90%
  bucket. These are admission targets: existing holdings are not sold to meet
  them, and appreciation or grandfathered positions can leave a bucket above target.
- Make both PRIME and Maple eligible for new allocations without borrowing.
  PRIME retains its current routing; Maple needs the tested Whirlpool route.
  Give qualifying below-target Maple/PRIME entries priority, then choose between
  OnRe/AUTO by net benefit. Size each quote to remaining target room and recheck
  before allocation and purchase. Keep the seven-day cost/benefit gate and its
  30-minute persistence; apply its 10 bps uncertainty allowance to the new
  investment amount. A small target does not authorize an unprofitable trade.
  Unfilled target room may remain in cash when no entry qualifies.
- Limit additional **gross** exposure to 100% of NAV per provider group,
  counting AUTO and PRIME together. Gross exposure includes borrowed money;
  this ceiling is separate from the 90/5/5 own-capital targets. Existing
  oversized positions are grandfathered. The higher ceiling lets the main
  allocations use borrowing while retaining a provider limit; it is a policy
  choice, not a statistically proven optimum.
- Verify the new-asset routes under the implemented withdrawal limit, then
  stage activation using new inflows. A release must not trigger a sell-and-rebuy
  cycle, reset existing positions to 1x, or treat unavailable new borrowing as
  a reason to close an existing position.

### Follow-on: price-aware smaller sales

Implement pacing after its existing withdrawal-window experiment and recovery
checks are complete. Select smaller sales only when freshly executable prices
justify their extra fees; retain progress and the economic expense ceiling
across pieces and repricing. Test actual liquidity replenishment before claiming
that splitting improves cost. Extending the 600-second wait remains a separate
product/contract change. No withdrawal-fee change is included in these releases.

## Withdrawal-cost fairness proposal

Do not add a flat withdrawal charge based on these examples. NAV already
includes an exit-cost allowance, so charging the full realized sale expense
again can charge users twice. First trace how burning shares releases that
allowance and reconcile the withdrawing user's entitlement with the
remaining holders' NAV.

The candidate rule is to quote a maximum incremental withdrawal expense
after crediting the allowance already borne by those shares, then settle
against actual receipts and return unused allowance. A withdrawal funded
entirely from cash incurs no incremental market-sale charge. Preserve a
separate treatment of the existing 40 bps issuance fee. Contract support,
attribution across overlapping withdrawals, partial fills and user-visible
consent need verification before changing fees. No withdrawal fee is changed.

## Validation and activation

Local validation passed:

- Existing Backyard tests before changes, then the updated package suite.
- Nine retained sale receipts, clipped final debt repayment, custody
  substitution rejection, and legacy operation compatibility.
- Cost/payback and persistence cases, expired/malformed approvals, quote
  deterioration, cash reserve boundaries, true 1x, and mandatory exits.
- Upgrade continuity: legacy 1x preserves its previous 1.5x meaning; full
  borrowing pools do not liquidate existing positions; old-policy evidence
  cannot immediately approve a new-policy trade; recorded live restart and
  signed-operation recovery cases continue to pass.
- The real transaction compiler and persist/sign validation with a generated
  test delegate. This is an offline wire test, not live chain simulation.
- Migration 0137 and approval write/read/revocation against a fresh temporary
  Postgres instance, stopped after the test; the existing foreign-inflow
  atomicity test also passed there.
- `scripts/verify-workers-v2.sh`: formatting, Go/TypeScript boundary check,
  `go vet ./...`, `go test -count=1 ./...`, and worker binary compilation.
- The scenario generator's assertions and `git diff --check`.

The subsequent [live read-only preflight](evidence/earn-max-live-preflight-2026-11-04.md)
confirmed HOLD/KEEP with the main borrowing pools full, preserved existing
targets, and simulated three representative debt repayments successfully.
The initial repayment rechecks were blocked by Jupiter rate limits. A paced
follow-up then cleared both main loans completely in simulation and passed
the real compiler and persisted-wire validation with a generated test key.
The smaller AUTO/wYLDS loan's full multi-step unwind remains untested.
Observed cash was about $872 against a proposed
$17,535 reserve, so the larger reserve still needs incoming funds.

Production activation is a separate step, following `deploy/hetzner/README.md`.
Before cutover, confirm no operation, allocation or rotation is unfinished
and that the selector has returned to KEEP. Stop the old worker and recheck
the journal before applying migration 0137 with `loyal-migrate`, then install
the new binary. The old worker must not write another legacy 1x target between
translation and cutover. Do not discard a signed transaction or clear an
unfinished rotation to make this check pass.

Compare fresh holdings, withdrawal demand, translated targets and the next
decision before enabling execution. The release must not initiate a new
sell/rebuy sequence because the worker restarted or new borrowing is full.
An existing withdrawal or risk emergency remains a legitimate reason to act.
Existing rows start without downward approval, and true 1x needs a new
cost-approved decision. A larger configured reserve is not proof of funded
cash: compare observed idle cash with the computed target.

At rollout, verify the deployed binary, record the first selector result,
then inspect each first executed step and its reconciled sale cost. Stop and
report on the first failed/held operation or worker restart; later steps are
unverified until observed. Do not bypass a hold to force the canary through.
Before any rollback, review the 1x meaning and pending operations: older code
does not enforce the new reduction approval. No rollout or live transaction
was performed as part of this local change.
