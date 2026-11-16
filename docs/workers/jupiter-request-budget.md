# Shared Jupiter request budget

Backyard, Loyal Earn MAX and retail use the same Jupiter organisation allowance.
Different API keys do not isolate that allowance. The Nov 13 paired probe showed
the vault and retail keys advancing the same gateway counter (93–96), with the
same reset timestamp. The exact paid gateway window remains unverified: it
reported a 100-request bucket, while the [published Developer plan](https://developers.jup.ag/docs/portal/rate-limits)
documents 10 requests/second over a 60-second sliding window. Organisation
firewall rules may impose a tighter limit. Do not infer a plan downgrade from
that counter alone.

On Nov 16, a read-only 15-minute journal sample found 14
`jupiter_quote_http_429` exit-quote messages in Earn MAX and six in Backyard.
The old client reports its local cooldown with that same code, so these are
**not** counts of requests that reached Jupiter. Current traffic volume and
production savings must be measured after request metrics are deployed.

## Coordination and scope

Yield migration `0145_jupiter_request_budget.sql` adds one organisation row.
Every worker uses its existing Yield database URL and a separate two-connection
pool. All three services must point at the **same database**. The separate pool
avoids borrowing Backyard's single connection that holds its writer lock.

Requests share an eight-per-second ceiling, with independent class limits:

| Class | Limit | Callers |
| --- | ---: | --- |
| Execution | 2/second | Fresh execution quotes, swap instructions, retail `/swap/v2/build`, Multiply |
| Valuation | 2/second | Held-position exit-cost observations used by NAV and withdrawals |
| Background | 4/second | Selector discovery and leverage candidate pricing |

Unused class capacity is not transferred to scans. Admission is paced, without
accumulated bursts, and waits at most 1.5 seconds or the caller's earlier
deadline. Waiters hold no SQL locks and reserve no future slots. Deferred scans
return to the existing sampling loop; they do not retry smaller trade sizes.
The budget stores cooldown and admission timestamps across process restarts.
Database failure refuses new HTTP requests instead of bypassing coordination.

Held-position exit-quote batches allow up to eight seconds to finish under the
shared valuation pacing, while honoring earlier caller deadlines. Urgent risk
protection still skips quote acquisition. A local budget deferral does not
start the five-second quote-failure backoff or block the next valuation attempt.

Successful exhaustion and 429 responses persist the later of `Retry-After` and
`X-Ratelimit-Reset`; missing or expired headers retain the existing five-second
fallback. Reset frees a slot, not the whole bucket. Existing admitted requests
may finish after another worker encounters a 429. No HTTP or transaction retry
is introduced. Service startup requires the migration and a paid Jupiter key;
Multiply now uses the retail key instead of its former keyless endpoint.

Only simultaneous identical background quotes within one client are coalesced.
The complete request URL binds amount, mints, slippage, instruction version,
account limit and DEX restrictions. Each caller receives its own decoded bytes.
No completed quote is cached; fresh execution and signer-specific instructions
never join discovery requests. Existing quantity, mark, time and slot validity
checks still apply. Canceling a follower stops its wait; canceling the leading
request fails that shared read for its followers, which can sample again later.

The coordinated production paths are the two vaults' v1 quote/instruction
clients, retail's v2 build client, and Multiply's keyed v1 client. Diagnostic
CLI invocations and unrelated services are outside this budget. The earlier
xstock probe showed a separate counter; include it only if new evidence proves
it shares this organisation. Manual probes must stay within the unused headroom.

## Measurement

The existing worker metrics endpoint exports:

- `loyal_jupiter_requests_total{class,endpoint,outcome}`: actual HTTP attempts,
  including redirects, with status codes or `transport_error`.
- `loyal_jupiter_deferred_total{class,reason}`: local budget, cooldown,
  unavailable-coordinator or cancellation outcomes; these are not 429 requests.
- `loyal_jupiter_request_seconds{class,endpoint}`: latency including admission.
- `loyal_jupiter_quota{field}`: gateway current/remaining/reset and the observation
  timestamp; stale or missing headers must not be read as current quota.
- `loyal_jupiter_quotes_coalesced_total`: redundant background callers served by
  one request, including shared failures.

Service identity comes from the existing scrape target labels. Metrics have no
keys, wallets, request URLs or mint labels. Quota exhaustion logs contain bounded
endpoint/class labels, status, gateway request ID and whether cooldown persisted.

Compare actual request and 429 rates across all three targets, grouped by class
and endpoint. Also compare local deferrals, quota age, completed family work,
exact lane success timestamps, NAV/risk observation age and the oldest pending
withdrawal. A lower request count alone does not establish healthy progress.

## Release and rollback

Implementation does not activate this policy in production. Before release:

1. Verify all three services' configured Yield database identities match without
   printing credentials. Verify their runtime identities have SELECT and UPDATE
   on the new budget table and each uses the intended paid organisation key.
2. Apply migration 0145 through `loyal-migrate` using the normal approved release
   process. Do not restart the new binaries before migration succeeds.
3. Record the current immutable release for each affected service and its health
   baseline. Release the three consumers together; older binaries do not obey
   the shared budget. Do not interpret a mixed-version rollout as coordinated.
4. Observe at least 30 minutes of normal demand. Stop and report on the first
   failed/held money operation, service exit/restart, stale NAV/risk evidence or
   stalled exact lane. Check finality before any recovery action; do not replay
   unrelated work or restart to reset a progress metric.
5. If the change regresses progress, restore the recorded previous immutable
   service releases. Leave the additive budget table in place; rolling back
   binaries restores the previous uncoordinated request behavior. Do not erase
   cooldown state or alter financial operation rows.

## Local verification

Use a fresh local PostgreSQL database named `jupiter_test*` on `127.0.0.1`, passed
as `JUPITER_TEST_DATABASE_URL`. Tests refuse remote or non-test databases, apply
the actual migration, and clean up their own schema. CI provisions this database
and requires the database tests to pass rather than accepting skipped tests.
Run the affected Go tests and `go vet` serially under the devbox resource limits.
Existing Backyard withdrawal/NAV regressions need their separate disposable
fixtures prepared with `scripts/prepare-vault-withdrawal-test-db.sh`.

The contention replay submits 28 simultaneous requests from independent pools
to a local HTTP server and checks protected work and bounded scan admission.
The coalescing replay submits 12 identical discovery reads plus fresh execution:
discovery should send one HTTP request, saving eleven, while execution sends its
own request. These are controlled scenarios, not a measured production saving.
