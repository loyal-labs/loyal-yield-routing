# Looping monitoring

This separate dashboard and its two alerts cover `ServiceName = 'loyal-backyard'` and `LogAttributes['vault'] = 'looping'`. They use existing webhook `6a60b666dcc64beb0ded8d48`. The CLI creates missing objects and reuses exact matches; it never updates existing objects or sends test notifications.

| Scalar tile | Window | Alert |
| --- | --- | --- |
| Heartbeat count | 5 minutes ending at evaluation time | Below 2, every 5 minutes |
| NAV fresh (1/0) | Latest reconciled REPORT_NAV within 75 minutes; scan 2 hours | Below 1, every 5 minutes |

Both SQL queries aggregate without grouping and return one row. Empty input gives heartbeat `count() = 0`; NAV is zero because `count() > 0` is false. The NAV SQL is the reviewed RWA query with only the vault value changed. This measures report age, not exit-price validity.

## Review and apply

Run from the repository root. The default dry run reads only the local manifest and prints it; it does not construct a transport, read credentials, make network calls, or create files. `--output` optionally creates a fresh local plan file.

```sh
python3 scripts/clickstack-looping-alerts.py
python3 scripts/clickstack-looping-alerts_test.py
```

For the explicitly authorized monitoring apply, supply `LOYAL_CLICKSTACK_API_KEY` through the repository's existing secret-injection workflow, and execute:

```sh
python3 scripts/clickstack-looping-alerts.py --apply --journal /absolute/fresh/path/looping-apply-journal.json
```

The tool prints server-assigned dashboard/alert IDs after full readback. The API assigns tile IDs even when the create request supplies them. Manifest tile IDs are local references; the CLI maps them to actual IDs by unique tile name plus exact configuration/layout. It prints and journals the resulting `tileIds` map. The create response is deliberately ignored: inventories supply IDs, and full detail supplies configuration. The journal records and fsyncs intent before each mutation, then records readback IDs. Existing journals cannot be overwritten. On a lost response, retain the failed journal and rerun with a fresh journal: an exact existing object is recovered through inventory, without another create.

Before each create, complete unfiltered inventories are checked twice. Dashboard identity is its exact name or unique tag; alert identity is its exact name or mapped server tile ID, including renamed alerts. Full details must match canonically. Runtime alert history/state/timestamps and known empty API defaults are excluded; configuration and team identity are checked. Ambiguous duplicates, unknown fields, changed policy, nonempty dashboard filters, or changed destinations abort. No concurrent creator should run this script: the API offers no atomic create-if-absent, so a write race can create duplicates; readback detects this and stops.

## Required live verification

After apply, run `clickstack_query_tile` on **both** tile IDs from the resulting dashboard, using the actual server tile IDs from `tileIds` and the current time, and retain the scalar results. Also run both with `startTime = 2000-01-01T22:00:00Z` and `endTime = 2000-01-02T00:00:00Z`, before this source existed. Expect exactly one row containing zero in each historical query. Verify complete dashboard/alert inventories show exactly one dashboard and two alerts with the manifest identities, then rerun the CLI with a fresh journal and confirm all three objects are reused.

A successful CLI readback proves configuration only. The implementation tests use fake HTTP JSON-RPC/SSE envelopes and the captured tool catalog; they do not prove live SQL execution, evaluation, notification delivery, or production readiness. The captured dashboard response and the two create-tool schemas live in `scripts/fixtures/`, resolved relative to the test file. The raw dashboard capture retains its original pre-correction name as provenance; the product monitor is named Looping only.

## Snapshot isolation for the original RWA repair

The original RWA CLI loads every JSON file in its `--snapshots` directory. Later dashboard readbacks alongside original captures can produce duplicate-detail errors. For an RWA dry run or apply, use a separate directory containing only the original reviewed `*-before.json` files; keep the newer Looping captures and journals outside it. This Looping CLI does not consume snapshot directories.
