# ASK-2200 balance-sweep monitor observability verifier

Run this verifier cold against the repository checkout. Do not accept a code
review, source summary, Render log, or dashboard screenshot as a substitute for
the commands and observable states below.

## Required end state

1. `scripts/verify-ask-2200-monitor-observability.sh` exits zero in an isolated
   local PostgreSQL environment.
2. The monitor exports one authoritative database snapshot containing:
   - the durable Earn LaserStream cursor slot;
   - pending reconciliation job count;
   - pending jobs with a recorded error;
   - oldest pending job age in seconds.
3. The OTLP metric contract contains these low-cardinality gauges:
   - `loyal.laserstream.cursor.slot`;
   - `loyal.earn.reconciliation.pending`;
   - `loyal.earn.reconciliation.failed_pending`;
   - `loyal.earn.reconciliation.oldest_pending_age`.
4. Cursor speed has a single source of truth: ClickStack derives it from
   successive `loyal.laserstream.cursor.slot` samples. The implementation must
   not add a second `advanced_slots` counter or locally calculated speed.
5. A retained proof failure emits the stable operational error
   `earn_reconciliation_job_failed`, and a consumer-loop failure emits
   `earn_reconciliation_consumer_failed`. The detailed runtime error remains in
   the local Render log and is not attached to OTLP metrics.
6. Metric attributes are bounded to stable consumer/cluster dimensions. No job
   ID, wallet, vault, settings, policy, signature, transaction, or raw error
   text may become a metric attribute.
7. Gauges are sourced from the committed database state, survive process
   restarts, and report an unchanged cursor when the account-only stream is
   legitimately idle.
8. The focused Rust tests, isolated database regression, compilation,
   formatting, and diff checks all pass.

## Deployment-only follow-up

The local verifier must report these as not executed rather than pretending to
prove them: immutable image publication, staging/production redeploy, ClickStack
dashboard changes, alert-rule changes, and exact-service-version canary queries.

## Verdict

Return `PASS` only when every required end-state item is demonstrated by the
executable verifier. Otherwise return `FAIL` and name each false condition.
