# Gradual vault execution and Backyard policy parity

Prepared 6 November 2026. Deploy and activate Backyard first; Loyal follows only
after the funded test. This change does not open release controls, replace stored
targets, migrate positions, change wallet authority, or merge the two services.

## Execution limits

Ordinary withdrawals and new investments use portions worth at most 1,000 USDC.
Each portion gets a fresh quote. At least 60 seconds must pass after the previous
broadcast trade finishes, including failed broadcasts. The existing per-vault
operation journal supplies this timestamp, so a restart does not reset it.
Collected USDC can return to the vault during the wait. An unresolved operation
is recovered before another portion is considered.

The final conversion must return at least 99.8% of its input's fresh reference
value. The discount already present in the quote consumes that same allowance;
the execution tolerance cannot add another 0.2%. Withdrawal sales also retain
the potentially stricter floor used by the certified NAV. The final request and
its valuation are checked again before signing. Repayment, withdrawal and swap
legs that are already atomic remain one transaction.

Loan emergencies, term-maturity actions and explicit operator exits retain their
existing execution rules. The limits above must not prevent emergency repayment.

This is a conservative fixed-size, paced implementation. It does not estimate
the optimum trade size or guarantee liquidity replenishes after one minute.
Network fees, market movements between portions and multiple conversions can
still cause losses. An unacceptable price causes waiting, not forced execution.

## Backyard settings and access

Backyard uses Loyal's seven-day investment payback horizon, provider-exposure
accounting and new-cash allocation policy: 90% across OnRe/AUTO and 5% each for
Maple and PRIME, subject to economics and available capacity. Existing holdings
are not sold merely to meet these targets. The cash reserve is 10%, capped at
25,000 USDC, without Loyal's 5,000 USDC minimum while test funding is small.

Valid Backyard withdrawal receipts from non-admin wallets are admitted without
an additional Loyal consent signature. Receipt validation, fresh NAV, automatic
sale floors, cost attribution and per-vault operator release controls remain.
Migration 0143 permits the new `direct` participant class in the existing cost
journal; it neither deletes historical records nor opens any control.

## Activation and verification

1. Inspect in-flight operations and recovery latches. Record both vaults' current
   releases, positions, targets and release-control versions. Apply migration
   0143 through `loyal-migrate` after checking all pending migrations.
2. Deploy the reviewed release to the separate Backyard service under hold.
   Verify its vault is `GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1` and that
   Loyal's service, targets and permissions are unchanged. Verify fresh pricing,
   NAV reports and successful unsigned simulations using the installed policies.
3. Read controls with `loyal-engine backyard release-control show --vault
   GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1`. Using the operator credential,
   set `ordinary_funding`, then `optional_addition`, to `open` with each current
   `--expect-version`, an explicit reason and operator. Do not add an owner list
   or a total-withdrawal cap: the worker bounds each portion. A missing row uses
   version 0. Re-read both controls and their recorded versions after each change.
4. Test with declared test funds and a non-admin wallet: deposit, partial
   withdrawal, multiple portions, restart between portions and a refused quote.
   Measure all completed fills and network fees. Stop on the first failed/held
   money operation, worker exit or restart and inspect it before proceeding.
5. Compare a large quote with portion quotes at the same reference prices, then
   measure funded results. Small-fixture success does not establish performance
   under stressed liquidity. Review these results before deploying to Loyal.

To stop new work, hold the two Backyard controls with version-checked updates.
Do not blindly roll back while signed or unresolved `direct` cost records exist:
an older binary cannot recover that new participant class. Drain or recover with
compatible code; preserve the additive migration and all journals.

## Local evidence

Tests exercise both vault identities' real transaction encoders and persist/sign
validation with ephemeral test keys, using saved instruction topology and
controlled RPC/quote fixtures. A fresh disposable PostgreSQL cluster exercises
the real migration, direct-wallet cost records and durable pacing. Worker-tick
tests check that waits and expensive quotes never reach a new build, a later
tick requotes, collected cash progresses, and signed recovery takes precedence.
These checks do not broadcast transactions or prove live fills.
