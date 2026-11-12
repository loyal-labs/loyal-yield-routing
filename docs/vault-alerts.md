# Vault alerts: holder impact and required intervention

Telegram is for an unsafe loan, falling holder value, blocked withdrawals or
vault management that requires intervention. Recoverable attempt failures stay
in ClickStack and Prometheus for diagnosis. A warning count alone does not
establish holder impact or whether a transaction landed.

## Audit and status — 12 November 2026

This change is prepared locally; it has not been deployed or applied to live
alert definitions. The checkout is based on fetched main
`7d9861b1db5342894fc09d21f87328878c69f076`.

The two Telegram formats come from two active delivery paths:

* ClickStack log/count alerts go through the Telegram relay. The RWA warning
  search matches every warning from the two vault services with `vault=rwa`.
  Its one-minute threshold is one event. The relay's mute/digest limits delivery
  frequency but does not make those events actionable.
* Prometheus rules go through Alertmanager's own Telegram template. Previously,
  three Backyard failures in fifteen minutes, sustained for five minutes, opened
  an incident with a thirty-minute recovery hold. Successful recovery could
  therefore coexist with an apparently firing failure incident.

The live inventory also had duplicate ClickStack heartbeat/NAV/restart alerts.
The ClickStack NAV threshold was 75 minutes while Prometheus uses three hours.
The shared ClickStack `Errors` search also includes production vault errors;
its alert needs a vault-service exclusion to avoid a remaining duplicate path.
Generic in-flight suppression matched worker family, letting one vault's
successes hide another vault's stuck work. Both are corrected by this policy.

Historical execution losses are documented in [Earn MAX loss controls](earn-max-loss-controls.md).
This change adds direct estimated holder-value and loan-risk signals; it does
not change trading, cost limits, repayment decisions or transaction construction.

## Paging contract

The service identifies the product: `loyal-earn-max.service` is Loyal Earn MAX
(`vault=rwa`); `loyal-backyard.service` is Backyard (`vault=looping`). Telegram
names the vault and provides the condition, impact and next investigation step.

| Condition | Page threshold | First response |
| --- | --- | --- |
| Estimated holder value falls | More than 0.2% below a sustained five-minute per-share value in the previous six hours, for three minutes | Reconcile sales, debt credits, fees and collateral price changes; inspect further planned costly work |
| Loan reaches liquidation LTV | First coherent observation, no additional hold | Inspect confirmed debt/collateral and any protective transaction immediately |
| Loan breaches the worker's safety LTV | Two minutes | Establish why automatic debt reduction has not restored the margin |
| A required safety repayment is held for insufficient room | Current hard-LTV or maturity hold for two minutes, with a recent failed attempt | Inspect the precise constraint and executable repayment path |
| Term loan remains unpaid inside its automatic repayment window | Under 24 hours to maturity for fifteen minutes; expired debt pages immediately | Check renewal/repayment progress and signatures; do not manually roll the term |
| Management is paused for manual recovery | One minute | Reconcile the persisted operation and chain effects before clearing the latch |
| Expected vault worker is unavailable | Two minutes, including a missing scrape target | Inspect the service and its first failure; reconcile signed operations before restarting |
| Pending transaction stops completing | Existing in-flight duration, scoped to service and instance | Check the persisted operation and finality; a selector error cannot clear this page |
| Withdrawal funding needs attention | Existing durable attention state for one minute | Read the scoped reason and verify demand, cash and confirmed funding progress |
| Holder valuation cannot be refreshed | Ten minutes without a certified valuation plus five minutes persistence; five minutes if never published | Restore coherent quote and chain evidence |
| Outstanding loan risk cannot be refreshed | Five minutes without an observation plus five minutes persistence | Restore obligation/reserve observations and verify the position |

Existing critical fee-payer, infrastructure, withdrawal-health and NAV-publication
pages remain. Opportunity warnings and the generic Backyard failure-counter page
go to the empty diagnostic receiver. Other worker families retain their existing
routing. Worker-down and manual-recovery pages suppress their matching secondary
freshness pages; liquidation suppresses its less urgent safety-margin page.
Financial risk pages remain visible during an outage.

## Evidence semantics and limits

`net_value_per_share` uses certified total vault value, including idle cash,
divided by the LP supply including accrued fee shares from the same observation.
It is a ratio per raw share unit; compare it within a vault, not across vaults.
Proportional deposits and withdrawals do not imply a gain/loss. Unknown valuation
does not emit zero or advance the valued timestamp. Zero supply explicitly clears
the loss condition after a vault is emptied.

Loan telemetry visits every lane in a complete observation, including positions
with negative equity. Unknown/incomplete observations do not clear an unsafe
position. A fresh zero-debt observation is explicit, including after a restart.
Rules retain the last observation across scrape gaps for 24 hours; separate
freshness and worker-availability alerts cover missing evidence. These are
observation timestamps, not metrics-scrape timestamps.

The loss threshold is an initial operating threshold, tested with synthetic
cashflows, price spikes, losses and outages. It is not calibrated against a live
history of this new certified per-share metric. Market prices, exit liquidity
and fees can legitimately affect it; it signals a value decline to investigate,
not proof of realized loss. Slow declines below the threshold or outside the
six-hour comparison window can be missed. A resolved alert does not restore a
past loss. These alerts do not themselves stop or sign transactions.

**Independent outage coverage remains missing.** Prometheus and Alertmanager
share the workers host, and the `Watchdog` heartbeat receiver is empty. An
external dead-man's-switch destination must be configured and tested separately
to report whole-host/network failure. A local `up` rule cannot provide that.

## Controlled activation

1. Obtain authorization for the worker rollout; `go/workers/AGENTS.md` prohibits
   deployment without an overriding user instruction. Snapshot the running
   binaries, rule/routing configuration and six ClickStack alert objects privately.
   Preserve the live Telegram token-file path, chat ID, unrelated receivers and
   routing. The repository chat ID is a placeholder and must not replace it.
2. Re-read live release paths. At audit time Backyard ran main `7d9861b1`, while
   Earn MAX ran `8a8f0a61`. Backport only the telemetry changes to each intended
   production base, or explicitly review the full behavioral delta before using
   main for Earn MAX. Do not silently upgrade trading behavior with monitoring.
3. Roll out one vault at a time using the existing activation procedure. Stop on
   the first failed/held operation, exit or restart. Verify a fresh coherent
   valuation, the recovery gauge, and every debt lane's risk/has-debt metrics at
   the actual service metrics endpoint. Compare the amounts and LP supply with
   the existing observation evidence. Verify debt-free lanes emit zero. Do not
   activate missing-metric pages while the old binaries are still running.
4. Validate the merged production rule/routing files with the host's `promtool`
   and `amtool`, then reload through the existing service configuration procedure.
   Check reload success, loaded rule count, both scrape targets, alert labels and
   current conditions. Expect 39 rule variants. Keep old ClickStack coverage
   until the replacement metrics and rules are verified.
5. Retire only the six ClickStack alert objects below. Keep their saved searches,
   dashboard tiles, raw logs, relay and unrelated alert definitions. Also update
   shared saved search `6a5fb723dcc64beb0ded6cca` (used by `Errors` alert
   `6a60c6b9dcc64beb0ded91c3`): wrap its existing `where` predicate in parentheses
   and append `AND ServiceName NOT IN ('loyal-backyard', 'loyal-earn-max')`.
   Preserve all existing predicates, columns, filters and other configuration;
   re-read it before applying and stop if it has drifted. This affects vault
   paging through that search; the underlying error logs remain available.
   The documented
   REST operation is `DELETE /api/v2/alerts/{id}`; verify the installed API accepts
   the read contract first and retain each full object for rollback. Re-list
   alerts afterwards and verify only the approved IDs disappeared. The existing
   two `clickstack-*-alerts.py` CLIs now refuse `--apply` so they cannot recreate
   retired vault paging; offline historical plans remain available.
6. Verify delivery routing and message rendering offline. Do not send a synthetic
   page into the real alerts chat. Check subsequent real alert state/delivery
   records and fresh vault work before declaring the cutover complete.

| Retired ClickStack alert | ID |
| --- | --- |
| Earn MAX every-warning count | `6ab6eed7df00e17b4af91988` |
| Earn MAX heartbeat | `6ab6eed8df00e17b4af91990` |
| Earn MAX NAV freshness | `6ab6eedadf00e17b4af91998` |
| Earn MAX restart count | `6ab6eedbdf00e17b4af919a0` |
| Backyard heartbeat | `6aed1004a57960f7f83c6cda` |
| Backyard NAV freshness | `6aed1010a57960f7f83c6d64` |

The API contract is documented in the [ClickStack API reference](https://clickhouse.com/docs/clickstack/api-reference)
and [official OpenAPI specification](https://raw.githubusercontent.com/hyperdxio/hyperdx/refs/heads/main/packages/api/openapi.json).
This document does not claim those deletes have happened.

For rollback, restore the prior rule/routing files and verified binary releases,
validate/reload, and restore removed ClickStack alerts from the saved objects
through the supported create API. Created IDs may change. Record that mapping;
restore the shared Errors search's original predicate too. Do not run the
retired repair scripts to approximate the old state.

## Offline checks

From `deploy/hetzner/monitoring`, using the production validator versions:

```sh
promtool check rules loyal.rules.yml
promtool test rules loyal*.rules.test.yml
amtool check-config alertmanager.yml
python3 verify-alert-messages.py
```

The fixtures cover cashflows, a transient quote spike, sustained loss, unknown
data, restarts, repayment, maturity, unavailable targets, sibling-vault isolation,
diagnostic routing and Telegram rendering. Go tests also verify the emitted
financial observations and recovery latch semantics. Run Go checks under the
devbox CPU/memory limits; no chain transaction or live notification is required.

Local validation passed on 12 November: 65 rule scenarios with 154 assertions,
39 rule variants and 92 firing/resolved/hostile message renders, 24 legacy CLI
contract tests, the engine test suite, three new vault metric tests and Go vet
for both touched packages. The telemetry-only patch also applies cleanly to the
exact deployed Earn MAX source (`8a8f0a617618c6bfab73058baf5ebe85a47b238f`) and its
three new metric tests pass there. No production rollout, live delivery test or
host-outage drill was performed.
