# Vault exit-admission rollout: remaining work and release gates

Status: revised 6 November 2026 after the user rejected a separate observation
release and all 24-hour waiting gates. This document does not activate a rule or
submit a production transaction. Prioritize protection of the existing Loyal
position; complete the newer investment-entry check as a follow-up enforcing
release. No scheduled waiting period or shadow-only production release is
required. Keep new strategies, allocation weights, UI changes and withdrawal
engine redesign out of this work.

## Immediate execution protection deployed

Loyal was upgraded at 19:11:42 UTC on 6 November 2026 from
`a34346be9e1acbf014aa5226fec5623b27e43fc9` to the already merged
`44d008f82ddc48772027dede9b04888884f0e72d`. Both vault services now run the
execution-protection release; Backyard was not restarted.

The release limits ordinary trade portions to about $1,000, spaces them by at
least one minute, and enforces the 0.2% reference-price loss limit. Loyal's older
0.2% quote-slippage rule remains in place.

Exact-release CI and the verification artifact passed. Migration 0143 was
already present with the matching checksum. A supplemental unsigned transaction
simulation succeeded against current Loyal accounts. The canonical activation
procedure passed its in-flight checks before and after stopping the old worker.
No local build, Node process, validator or funded test was used.

A 101-second post-deployment watch ended successfully at 19:13:38 UTC: two new
NAV reports, no failed/held operations or restarts, and no trades. Before/after
snapshots confirmed unchanged collateral amounts, leverage targets and release
permissions. LTV remained 33.33%; the selector chose KEEP. The previous release
and guarded rollback path remain available.

Deployment evidence: `/tmp/loyal-execution-rollout-20261106/result.md` and its
adjacent snapshots and logs. The newer investment-entry check below remains
uncommitted and undeployed; it did not gate this completed cutover.

## New entry-rule release

The implementation now enables `CheckInvestmentExits` for both vaults. It has
no observation mode. No database, stored-target, allocation-weight, release
permission or transaction-shape changes are included.

Validation completed before release:

- [x] Sixteen live paired-quote probes. Fixed valid split-route ordering and
  bounded changed-slot retry; price and outage failures remain distinct.
- [x] Shared AUTO/PYUSD and AUTO/wYLDS repayment handling, without a fictional
  direct wYLDS sale. Existing entry profitability costs remain unchanged.
- [x] Real preparation check, current compiler and local persist/sign validation;
  changed/expired approval, restart recovery and smaller-candidate tests.
- [x] Fresh purchase-then-sale native cases for all four providers, preserving
  purchase pool effects. Three accept; OnRe rejects its expensive last portion.
- [x] Eleven previously skipped database tests on fresh disposable Postgres.
- [x] A 151-transaction $100k / 1.5x Maple unwind with six hours of stressed
  interest charged upfront. Existing full large-withdrawal matrix retained.
- [x] Native replay source retained; historical Node runner excluded.

Release gates to finish:

- [x] Final focused tests (204 passed) and vet.
- [x] Read-only current-vault probe; both refuse additional OnRe exposure on
  current prices, consistent with the existing execution guard.
- [ ] Scoped commit/PR, required CI and matching verification artifact.
- [ ] Preflight positions, targets, vault selection, permission rows and in-flight
  state; verify previous binary and safe rollback path.
- [ ] Deploy Backyard, verify the first fresh observations and fail/hold/restart
  signals, then repeat the preflight and deployment for Loyal.

There is no mandatory 24-hour window or observation-only release. New entries
may wait when a genuinely affordable exit cannot be established. Existing
withdrawals, NAV reporting and emergency risk reduction retain their paths.

See [the final implementation evidence](../evidence/investment-exit-admission-2026-11-06.md)
for exact results and limitations. These simulations do not execute a live user
claim or predict future liquidity. Existing partial-withdrawal planning is not
redesigned by this release.

### Cutover and rollback mechanics at every stage

Use the existing per-family deploy/activation procedure in
[the Hetzner runbook](../../deploy/hetzner/README.md). `backyard` maps to
`loyal-backyard.service` / vault `looping`; `earn-max` maps to
`loyal-earn-max.service` / vault `rwa`. Pin an explicit merged SHA, require the
matching verification artifact, verify the uploaded binary hash, and inspect
migration differences. Build on CI/a suitable builder, or within the established
hard devbox caps; never invoke an uncapped local release build.

The activator waits for the scoped in-flight count to reach zero, stops that
service, checks again, then switches its release. It verifies the selected vault,
release owner and restart count. It does not replace the longer behavior checks
above. Do not use --stopped or --migrations-applied to bypass a failed prerequisite.

On an unexplained failure: stop advancing the rollout, preserve operation state
and logs, and return that vault to the previous verified binary when the scoped
in-flight guard permits. An already signed operation must finish/reconcile
through the durable recovery path before a conflicting transition. Never delete
pending work, reset targets, force-sell assets, or repeatedly restart to make the
warning disappear. An expected expensive-price wait does not warrant disabling
NAV reporting or emergency risk reduction.
