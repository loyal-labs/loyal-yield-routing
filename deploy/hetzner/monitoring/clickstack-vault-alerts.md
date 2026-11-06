# ClickStack RWA alert repair

Run from the repository root with Python 3; no dependencies are required. The default command reads only local snapshots and writes a reviewable plan containing exact before/after configuration and reverse-order rollback calls.

```sh
python3 scripts/clickstack-vault-alerts.py \
  --snapshots /Users/user/loyal/reports/vault-protection-2026-11-05 \
  --output /private/tmp/vault-alert-plan-review.json
python3 scripts/clickstack-vault-alerts_test.py
```

Snapshots use the existing read helper envelope: `request.name`, `request.arguments`, and `response`. Supply one complete alert inventory, one complete saved-search inventory, full details of the four RWA alerts and four RWA saved searches, and full dashboard `6abf9e82df00e17b4af9fd8a`. Duplicate detail/inventory snapshots abort; use a clean snapshot directory when refreshing. Other report JSON files are ignored unless they have a recognized snapshot request.

The manifest pins source, connection, team, object IDs, predicates, existing threshold/cadence policy, and desired messages. It repairs heartbeat, warning, restart, and confirmed REPORT_NAV searches. The NAV tile uses a scalar aggregate without GROUP BY: no matching reports produce one row with `nav_fresh = 0`. Fresh report age does not certify exit-price validity. The warning policy remains warning-only with the original threshold; critical-error policy is a separate decision.

To apply, use the authorized secret-injection session, a fresh plan path, and a separate fresh journal path. Never put the key in arguments or a file. The CLI reads only `LOYAL_CLICKSTACK_API_KEY` from its environment when `--apply` is present.

```sh
python3 scripts/clickstack-vault-alerts.py \
  --snapshots /Users/user/loyal/reports/vault-protection-2026-11-05 \
  --output /private/tmp/vault-alert-plan-apply.json \
  --apply --journal /private/tmp/vault-alert-apply-journal.json
```

Apply preflights every changed object, checks configuration again immediately before each write, and reads configuration back after each write. Repeating the same plan skips objects already at the desired configuration. Existing webhook channels, alert thresholds, cadence, names, search selections/tags, and dashboard layout remain intact. Only the named tile is patched. Alert evaluation history/state and evaluation-driven `updatedAt` changes are excluded from configuration comparisons. Unsupported fields and configurable drift abort. There is no API compare-and-set, so a change between the final check and write remains possible.

A partial failure leaves completed and in-flight mutations in the journal, including rollback payloads. Inspect an in-flight object before retry: an HTTP timeout may have applied it. To roll back, reread each completed object's configuration and require it still equals that operation's `after`, then invoke its `rollback` call in reverse completion order. Do not execute the full planned rollback list blindly after a partial apply. No automatic rollback or notification send is implemented.

After application, run `clickstack_query_tile` on the NAV tile and direct predicates for current RWA events, including an intentionally empty window. Verify the scalar is zero with no rows, evaluate heartbeat absence separately per vault, observe at least two evaluation windows, inspect errors/history and destination routing, and compare unrelated fields to snapshots. These are live acceptance checks; mocked transport tests do not establish ClickHouse semantics or notification delivery.

Independent looping heartbeat/NAV creation remains deferred. The available tile patch API replaces existing tiles; safe looping creation needs a reviewed new-dashboard/tile payload, verified returned object IDs, and full alert/search/dashboard detail inventory to detect equivalent monitors. This CLI never creates monitors and must not be reported as looping protection.
