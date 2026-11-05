# Voltr and Earn MAX: integrity before allocation growth

Status: local implementation plus an ordered follow-on plan, November 5, 2026. Source base: `68cc8b42a634468fc7105cc02e108b0712a1164b`. No deployment, live policy change, migration or transaction is part of this change. The current patches do not complete the NAV or withdrawal-price contracts below.

## Outcome and evidence

Deposits, withdrawals and leverage changes must be explainable in token units: what the user paid, what they received, the fees, the execution expense, and the value still owned. Optional allocation must earn its costs under conservative execution assumptions. Missing valuation evidence must block new exposure and NAV certification without disabling observation of debt, custody or urgent risk reduction.

The investigation distinguished transaction mark gaps from user losses. Five October 29 ONyc sales had $236,562.962339 of marked value and $235,414.464768 of receipts, a $1,148.497571 gap, or 48.549340 bps of gross assets sold. These figures include proceeds paid directly to the lender; they are not an attribution of loss to customers. The historical deleverage, churn, clipped repayment and nonliteral 1x defects were corrected before this branch. Their recurrence on the current release has not been demonstrated.

Read-only November 5 inspection identified deployed release `8ceb6a898556d94311bc5a159c2a26b3f7b95983`, with the relevant Backyard source unchanged at this branch's base. In the captured post-start window there were NAV reports but no capital operations. This is evidence of process health and reporting, not evidence of profitable large deposits or withdrawals. Current quote failures and the code defects below remain relevant. Counts of log messages are not counts of independent losses.

## Root causes and ownership

| Incorrect assumption | Required behavior | Owner and disposition |
| --- | --- | --- |
| Same mark and sufficient recorded size make a quote reusable forever | Refresh at the existing cadence even when mark and holdings do not move | `exit_cost.go`: implemented scheduling repair; consumption validity remains open |
| Temporary account/history disagreement is a fatal worker failure | Hold uncertain observation, retain durable progress, then converge without repeated credit | `foreign_inflow_observe.go`: implemented for the identified temporal cases; malformed accounting and fixed sweep faults remain hard |
| Profit at quoted output is enough to admit a loop | Minimum proceeds, amount-matched hypothetical unwind and fees must pass the existing economic gates | `selector_price.go`, `leverage_step_cost.go`: implemented optional-entry correction; extra quote latency is a release gate |
| A capped quote covers the whole holding; old realized costs certify current exit cost | Store actual coverage and scoped, bounded-age evidence; historical realizations remain diagnostics | Follow-on NAV unit; changing coverage alone would select older realized evidence under today's fallback |
| Unknown exit cost makes all account and risk observation unusable | Preserve known balances and debt while net valuation is explicitly unknown | Follow-on observation and admission contract |
| A 20/50-bps swap tolerance bounds the user's total withdrawal expense | Bound cumulative request economics, including the starting price basis and completed partial fills | Follow-on user consent contract; ordinary requests stay pending outside consent |
| Retired service names still identify current vault health | Monitor each current vault independently; absence must produce an explicit failing scalar | Prepared alert repair with read-only query validation; not applied |

No off-chain global execution spending cap is restored. The October 10 owner decision remains in force. The November 5 decision is narrower: **keep an ordinary withdrawal pending until its user-approved maximum total cost can be met, show the reason, and require consent to accept more cost. Emergency risk reduction retains priority.**

## Ordered implementation units

### A. Land the bounded reliability and entry-economics corrections

The history patch uses the existing typed unavailable signal for same-slot convergence, an observation behind a durable checkpoint, bounded replay catch-up, and a missing execution receipt whose identity was returned by history. It does not add a general retry wrapper. A signed operation retains its original identity. Negative balances, invalid receipts, database faults, lost anchors and impossible fixed sweep ordering do not become ordinary availability holds.

The quote patch removes indefinite refresh suppression for unchanged holdings. Failed requests retain existing attempt pacing; zero or future timestamps cannot suppress refresh indefinitely. The one-hour cadence is a scheduling bound, not a claim that a one-hour-old quote is valid for a deposit or withdrawal. Existing fallback selection, provenance and amount-coverage defects are explicitly unresolved.

Optional entry and leverage forecasts use wire minimum outputs and a separately priced unwind for that amount. Quoted economics remain diagnostics. Hypothetical expense is not recorded as realized loss, and quoted held-exit/NAV evidence is not rewritten as a worst-case sale. Keep existing policy horizons and benefit thresholds: the default selector horizon is 30 days; seven-day analyses and fixtures must be labeled as such. Holding existing positions does not pay entry costs again. Withdrawal, term and hard-risk priority remain unchanged.

Amount-matched floor pricing can add one conversion quote per priced entry or leverage tranche: up to 50% more conversion requests for that tranche. Preserve the existing three-second ladder-start budget and eight-second collection deadline. An incomplete optional forecast must hold, never fabricate a zero cost. Qualify actual quote latency and successful coverage before activation; local fake HTTP timing is not production throughput evidence.

### B. Separate net-NAV certification from risk observation

Implement one coherent change across `nav_observe.go`, `route_observe.go`, `state.go`, `target.go`, `execution_observe.go`, `step_prepare.go` and `exit_cost.go`:

1. Observe marked collateral, debt, actual custody and LTV independently of exit-cost certainty. Represent net NAV as known or unknown with a concrete reason. Unknown is neither zero, gross NAV nor the last report.
2. Require certified net NAV for optional entry, leverage increases, rotation and every report-bearing transaction builder. Preserve exact-wire recovery and mandatory risk/term/operator/withdrawal reduction paths. Audit zero-asset and insolvency checks so unknown cannot trigger liquidation by itself.
3. Bind exit evidence to vault/manifest, lane, actual sold quantity, collateral and USDC reference marks, slot, time and route. Preserve actual capped quantity. Accept a newer smaller quote without pretending it covers a larger holding. Legacy global `(lane, source)` rows must not certify the new contract.
4. Remove the rule selecting an older cheaper realized cost when a current quote is over 3x that observation. Keep the discrepancy as an anomaly. Use actual execution time for historical diagnostics, not replay wall time. Never sum independent small quotes as cumulative executable depth.
5. Trigger reporting when known net exit liability changes materially, even if gross collateral value is unchanged. Keep unexplained gross-drift protection separate. Do not deduct an existing exit allowance twice.

Prefer tick-local scoped evidence for the first implementation. Reusing persisted authoritative evidence requires a migration, root-owned reader/upsert changes and an explicit validity policy. The existing 30-second selector convention is a candidate for execution admission; it is not automatically an approved full-holding NAV TTL.

Mandatory acceptance: expired/future/other-vault/wrong-USDC-mark/undersized evidence; a current expensive quote versus old cheap realization; partial coverage of one lane; delayed replay; unknown NAV blocking reports and adds while preserving risk reductions and signed-wire recovery; and liability-only NAV changes. Existing dust and repayment fixtures remain valid.

**Protocol dependency:** `StageSquadsToVoltr` and `VoltrRestoreIdle` currently recompute/report NAV as part of withdrawal funding. An unknown valuation cannot safely pass that builder merely because funding is urgent. Settle that ABI behavior before claiming withdrawal completion during a price outage. Worker checks also cannot stop a user calling public Voltr deposit/claim instructions directly; fair cashflow pricing needs an atomic program or enforceable entry-point constraint.

### C. Make the approved withdrawal policy enforceable

The recommended user-facing contract is a minimum aggregate USDC claim for a specified share/request amount, derived from a displayed reference valuation, all included fees and the user's maximum total cost. It is a proposal for the exact representation, not an already accepted numeric tolerance. Store the reference, consent version, deadline and request identity in the existing intent owner; aggregate completed claims, realized sale costs and outstanding minimum proceeds exactly once.

Before implementation, verify the installed Voltr request/claim instructions and Squads policy capabilities. Decide whether enforcement belongs in the program or in an entry point that cannot be bypassed. An application-only consent row is insufficient if the public claim path ignores it. Specify shared-sale attribution across simultaneous requests, LP escrow, cancellation and partial claims before choosing storage fields. A deadline must not widen consent. Emergency reductions need separate authority and explicit attribution, rather than silently spending an ordinary user's budget.

Use states for waiting on price, executable, partly funded, claimable, completed and cancelled, with a visible reason and remaining approved cost. Persist reservations before signing; reconcile receipts and fees; recover ambiguous transactions before preparing replacements. Acceptance includes two overlapping users, a partial fill followed by a restart, cash-only withdrawal, worsening quotes, expiry, rejected consent widening and an emergency reduction. The UI must show the cost already incurred and the amount still pending.

### D. Prove net wealth, then optimize allocation

Extend receipt evidence at existing operation boundaries rather than introducing a second balance writer. Record asset quantities, slot, custody and lender-direct receipts, debt, actual transaction fees, fee LP and external user transfers. Unknown receipt components remain unknown.

The oracle is each user's claims received plus remaining post-fee redeemable LP value, less deposits and user-paid network fees. Terminal comparisons end in claimed USDC. Count escrow and pending claims exactly once. Debt includes accrued interest; classify interest without subtracting it twice. Reconcile all users and fee recipients to assets minus liabilities and external transfers. A two-user sequence must cover deposit/withdraw around stale NAV, fee accrual/harvest, partial sales and restart. This prevents optimizing a displayed APY while losing terminal wealth.

Prioritize improvements in this order:

1. Retain new inflows to satisfy the existing cash reserve before optional exposure. At the retained 22:09Z RWA observation, integer rounding gives total NAV $177,896.940577, reserve target $17,789.694057, idle $2,913.167017 and pending $245.484104: an uncommitted reserve gap of $15,122.011144. Do not infer an instruction to sell existing holdings or increase the target. Compare avoided execution expense with foregone net carry under identical cashflows.
2. Complete financed deposit-to-claim qualification at realistic sizes: LP mint, entry, borrow, conversion, deposit, interest, fees, partial withdrawal, repayment and claim. Compare cash, unlevered and financed paths with equal initial capital and equal terminal time. No capital top-ups to rescue a candidate.
3. Separate supply/borrow headroom from exit capacity. A valid small step does not certify a whole holding, and two lanes may consume the same liquidity. Gate additions on amount-specific capacity while preserving mandatory exits.
4. Qualify native redemption only after proving terminal mint, permissions, settlement windows, capacity and debt-maturity compatibility. ONyc windows, PRIME-to-wYLDS conversion and Maple withdrawal availability do not establish instant USDC liquidity.

Do not force the 5% Maple target from allocation labels. The retained 4.9601% annualized yield gives about 9.282 bps over seven days, below 10 bps of uncertainty before costs. Do not assume blanket splitting or longer waiting improves execution: the prior independent split quotes suggested only about $7.26 on $145.5k before cumulative impact and extra fees. These are qualification questions, not promised savings.

## Release and stop conditions

Each unit needs its discriminating regressions, integrated offline verifier and independent review. Passing tests proves the tested local behavior, not current on-chain suitability. No live activation until quote deadline behavior, relevant installed ABI, fee treatment and complete financed receipts support that unit's claim. Do not wait for every optimization to review a coherent correctness patch.

For release, capture the exact binary/source, saved policy and alert revisions, then observe quote successes/age/coverage, held requests, completed claims, debt/LTV, receipt reconciliation and user wealth. Stop optional adds on unexplained reconciliation, stale certified evidence or quote deadline regression; retain recovery and urgent risk authority. Rollback must preserve durable operation IDs and exact signed wires. Never discard an ambiguous transaction or reset accounting to make a restart appear clean.

The local evidence bundle is `/Users/user/loyal/reports/vault-improvements-2026-11-05/`; the preceding production investigation is `/Users/user/loyal/reports/vault-investigation-2026-11-05/`. These local paths describe this run and are not portable repository dependencies.
