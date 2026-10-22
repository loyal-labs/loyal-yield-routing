# Admin load time and trim

Status: proposal for owner sign-off (Oct 22 2026). Follows the read-only audit
of `loyal-admin` at b095ab37 (frontend, backend, live timing, usage census).

Goal: every admin page shows its shell at once and its data lands fast; the
pages the owner picks are removed.

## Done predicate

Measured on the host after deploy, with the harness from lane H, through
`/benchmark-checklist`. Baselines are the audit's observations (3 samples,
not a benchmark); lane H retakes them as medians before any deploy.

1. Every route paints the sidebar and a skeleton or content before its data
   request completes, on cold load and on in-app navigation: 19 of 19 routes.
   Baseline: `/overview` is blank for 2.8 s; Earn to Overview keeps Earn on
   screen for 1.9 s.
2. A save resolves before the page refetch finishes, and each save triggers one
   refetch, not two. Checked on the scratch rows used in S3.
3. Warm (median of 5): no page loader and no client section over 1.0 s.
   Cold (after 120 s idle, median of 3): nothing over 5 s.
   Baseline: overview 3.0 s, funding 3.4 s, operations 1.8 s, metrics 23.5 s
   cold, backyard 30 s cold.
4. No chunk loaded by earn, metrics or rebalance contains `@solana/web3.js`
   (checked on the build output). Baseline: a 625 kB (161 kB gzip) chunk.
5. `GET /api/earn/rebalance?section=executions` is under 100 kB on the wire.
   Baseline: 649 kB br.
6. Each page the owner removes is gone from routes, nav, the page table and
   the RPC table; `gen-routes --check` and the signed-in sweep pass on the
   remaining routes.
7. `loyal-engine`, `loyal-observer`, `loyal-realtime` NRestarts unchanged
   across the deploy.

## Lies table

| Lie underneath | The one fix | Deleted |
|---|---|---|
| A route cannot render until its data arrives | A default pending component in the router; gen-routes wires each `loading.tsx` as `pendingComponent` | The frozen-page behaviour; the three unused `loading.tsx` become used |
| A save is done only when the page has reloaded | `callAction` fires `router.invalidate()` without awaiting it | 18 redundant `router.refresh()` calls |
| The earn monitor needs Solana `PublicKey` objects | Mints as base58 strings, typed `Record<Stablecoin, string>` | The web3 import in `stablecoin-monitor.shared.ts` and its 625 kB chunk |
| A dashboard needs all history on every visit | Time windows and indexes on the heavy queries | All-time scans per request |
| Rebalance and earn sections depend on each other | Independent sections fetch at once | The overview gate and the visibility gates |
| A read needs its own transaction to stay read-only | The yield and Timescale roles are SELECT-only (verified first); `statement_timeout` set once per connection | BEGIN, SET LOCAL and COMMIT around every read |
| Funding must fetch a transaction to know its time | Each signature row already carries `blockTime` | `getTransaction` calls for signatures outside the window |
| Every admin page is in use | The owner's picks from the census | Picked pages, their loaders, RPCs and nav entries |

A cache is not in the table on purpose. A cache only goes on a query that still
misses predicate 3 after its source fix, and lane B1 has to show why that query
can't be fixed at source.

## Lanes

One `foreman` runs the lanes (more than two). Each lane works in its own worktree
off main, pushes its branch, and opens one narrow PR. One `review` at the end over
the whole diff.

Decomposition:
- **Blocking first steps.** None for code. Lane H's baseline must exist before
  any deploy. Owner calls O1 and O2 block only the windowed queries in B1 and
  the whole of lane R.
- **Independent workstreams.** H, F1, F2, B1 and B2 own disjoint files and run in
  parallel.
- **Shared mutable state.**
  - `routeTree.gen.ts` and the route files are written by F1 (pending
    components) and by R (deletions). R starts after F1 merges and regenerates
    them.
  - `pages.go` and `rpc.go` are R's alone.
- **Smallest safe decomposition.** Five lanes plus R; F1 and F2 could be one
  lane but split cleanly by file.

### H: harness and baseline (sonnet-lane)
- Owns `scripts/web/admin-perf.ts` (new). It reuses the login in `scripts/web/sweep.ts`.
  - Credentials come from env only.
  - The host is an argument.
- Per route it records the time the shell paints, the time the data lands, and
  each API call's duration. That covers every route on cold load, plus three
  in-app navigations.
- It runs a warm set (median of 5) and a cold set (after 120 s idle, median of 3).
- It prints a table and JSON, including error and work counts.
- Vet it with `/benchmark-checklist`, then freeze it and take the baseline on
  current prod.
- Acceptance: the baseline reproduces the 2.8 s blank overview and a cold
  metrics loader over 15 s.

### F1: shell, saves, bundle (sonnet-lane)
Owns:
- `web/apps/admin/src/main.tsx`
- `scripts/web/gen-routes.ts`
- `web/apps/loyal/src/next-compat/server-action.ts`
- the `router.refresh()` call sites in `dapps/dapp-list.tsx`,
  `library/library-list.tsx`, `communities/community-active-checkbox.tsx`,
  `vault-integrations/earn-max/earn-max-charts.tsx` (the 60 s poll stays, as a
  plain refetch)
- `lib/earn/stablecoin-monitor.shared.ts`.

Changes:
- `defaultPendingComponent` (sidebar plus skeleton) and `defaultPendingMs: 200`,
  plus `defaultStaleTime: 30_000`.
- gen-routes emits `pendingComponent` from a sibling `loading.tsx`.
- `void router.invalidate()` in `callAction`; delete the redundant refresh calls.
  The admins and flags call sites go with lane R if those pages are removed;
  otherwise R edits them.
- Mints from base58 strings.

Gates:
- tsc, the admin build, `gen-routes --check` for both apps.
- The loyal dist is byte-identical before and after (`diff -r`).
- A scratch check that no earn, metrics or rebalance chunk imports `@solana/web3.js`.

### F2: frontend waterfalls (sonnet-lane)
Owns:
- `earn/rebalance/rebalance-monitor-client.tsx`
- `metrics/metrics-progressive-section.tsx`
- `earn/earn-progressive-client.tsx`.

Changes:
- Operations, executions and audit no longer wait for `overview.status`.
- The visibility root margin grows to about 600px.
- The metrics dashboard and earn funding fetches start on mount, in parallel
  with the page data.

Gates: tsc and the admin build. Locally, the network panel shows the sections
starting together.

### B1: heavy queries (opus-lane)
Owns:
- `go/workers/internal/admin/{metrics,metrics_latency,metrics_clickstack,overview,rebalance,rebalance_data,rebalance_sql,rebalance_audit,rebalance_apy,kamino_reserve,earn_max,backyard}.go`
- yield migrations for new indexes.

Steps:
1. **Forensics first.** Explain the cold 25 s against the warm 2 s on metrics
   and backyard. No Go code caches either.
   - Run `EXPLAIN (ANALYZE, BUFFERS)` twice, back to back and after an idle gap,
     read-only, on the metrics latency query and the backyard loader queries.
   - Capture the 278-byte backyard body.
   - Name the limiter before changing anything.
2. **Windows (after O1):**
   - the metrics latency CTE (`metrics_latency.go:162-166`);
   - the overview volume series (`rebalance_sql.go:731-760`);
   - the executions chart points;
   - the audit range filter moved inside each UNION branch.
   Keep all-time totals as single counts where a view shows them.
3. **Indexes from the plans in step 1.** One yield migration, applied through
   `loyal-migrate` with the deploy.
4. **Concurrency.** errgroup the serial reads in `overview.go:28-35`,
   `rebalance.go:66-80, 174-185, 309-323`.
5. **Earn-max.** Serve the cached vault scan straight away and refresh it in the
   background; drop the second share-price fetch (`earn_max.go:765, 979`).
6. **Cache, only where step 5 leaves a query over predicate 3.** One
   singleflight-plus-TTL helper; the census of which queries need it goes in the
   PR body.
7. **Delete `/api/metrics?section=latency`** (no caller).

Gates:
- `go vet ./... && go test -race ./internal/admin`.
- A scratch JSON diff on two endpoints before and after shows the same shape.

### B2: database plumbing (sonnet-lane)
Owns:
- `go/workers/cmd/loyal-admin/main.go`
- `go/workers/internal/admin/{db,router,communities,earn_funding,smart_accounts}.go`.

Changes:
- **Verify the roles.** Before dropping transactions, confirm the yield and
  Timescale roles hold no write privilege. The check runs read-only on the host
  credentials, through `has_table_privilege` over every schema.
- **Pool config.**
  - Set `statement_timeout` in the pool's `RuntimeParams` for the admin pools,
    in `main.go`. `internal/db/pool.go` is shared with the workers and stays
    untouched.
  - Then `readOnly` queries the pool directly for yield and Timescale; the app
    DB keeps its transaction.
  - MinConns 2. MaxConns 4 to 8 after checking Neon's connection limit and
    current use.
- **One log line per request** in `router.go` with path, status and duration,
  so ClickStack shows page times from now on.
- **Concurrency.** errgroup the serial transactions in `communities.go:218-224,
  463-468`.
- **Funding.** Skip signatures older than the window before `getTransaction`
  (`earn_funding.go:690-732`).
- **Smart accounts.** One LATERAL in place of three correlated subqueries
  (`smart_accounts.go:136-162`).

Gates:
- `go vet`, `go test -race`.
- loyal-admin starts locally with the new pool config and serves one page.

### R: removals (sonnet-lane, after O2 and after F1 merges)
Owns:
- the removed pages' route and view files
- `app-sidebar.tsx`
- `pages.go`, `rpc.go`, the removed families' Go files
- `earn.go` (the unreachable `section=monitoring` fallback)
- `features.go` and `push.go` (the RPCs nothing calls:
  `getFeaturesForMatrix`, `sendManualPushNotification`, `checkPushReceipts`).

Steps:
- Delete in one wave per page: route, view, loader, RPCs, nav entry.
- Regenerate the routes.
- Tables stay; dropping them is a separate migration on the owner's go.

Gates: tsc, the build, `gen-routes --check`, `go test`, and the sweep over the
routes that remain.

## Integration and ship

1. Merge order: H, F1, F2, B2, B1, then R.
2. One `review` over the combined diff. Findings on auth, concurrency or the
   read-only change go to the owner.
3. Deploy with `bun run deploy admin <sha>` on the owner's go.
4. Run H on the host and report predicates 1 to 5 through `/benchmark-checklist`.
5. Run the sweep and check predicate 7.

Rollback is redeploying the previous sha. The only schema change is
indexes, which are additive.

## Owner calls

- **O1 Windows.** Recommended: metrics latency covers the last 30 days; the
  overview volume and executions charts cover the last 90 days. All-time totals
  stay as counts.
- **O2 Removals.**
  - From the census: flags and features (one row each since Apr 10, hidden
    from nav since May 18); admins (gates nothing); the manual push panel
    (6 admin sends ever, last May 22).
  - Autonomous vault goes if the vault is no longer funded.
- **O3 Design changes, not in these lanes:**
  - merging backyard with earn-max;
  - folding overview into rebalance;
  - landing on `/earn`.

  Each recomposes an approved page, so each waits for a yes.
- **O4 Deploy go.**

## Risks

- **A skeleton flash on fast loads.** `pendingMinMs` holds it for 500 ms once
  shown; the 200 ms delay keeps it off fast pages.
- **A list may show the old row for a moment after a save,** until the refetch
  lands.
- **The forensic queries run the 25 s query on the prod yield DB,** read-only,
  about four times.
- **More connections against Neon** if MaxConns rises.
- **Dropping the per-read transaction removes one layer of read-only
  enforcement.** The role check in B2 is what keeps it safe; B2 does not proceed
  without it.

## Unknown

- The cause of the cold 25 s. B1 step 1 finds it.
- Real page views: request logs only start at the Oct 22 cutover. Lane B2's
  log line starts that record.
- Whether the autonomous vault is still funded.

## Decisions

The owner's calls of Oct 22.

- **O1 Windows.** Metrics latency covers 30 days. The overview volume chart and
  the executions chart cover 90 days. All-time totals stay, as counts. The
  executions chart points are bucketed hourly per route on the server, to bring
  the response under the 100 kB target.
- **O2 Removals.** Flags, Features (including the feature detail page), Admins
  and Push notifications are removed: routes, views, loaders, RPCs and nav
  entries. The push page goes whole, token counts included; the counts only
  sized the manual send form and nothing else reads them. The autonomous vault
  stays. Every table stays (`runtime_flags`, `feature_registry`,
  `feature_flag_links`, `admins`, `push_tokens`, `push_notification_sends`);
  dropping them is a separate migration on the owner's go. Login does not read
  the `admins` table: auth takes `ADMIN_USER` and `ADMIN_PASSWORD` from the
  credentials directory.
- **O3 Design changes.** Yes to all three, built by a later lane. `/` lands on
  `/earn` and Overview leaves the nav. Backyard and Earn Max merge into one page
  with every section kept, and the old URL redirects. Overview's two charts fold
  into Rebalance, and `/overview`, `overview.go` and its page entry go.
- **O4 Deploy.** The main session deploys after the end-of-feature review, and
  applies migration 0122 with `bun run yield:migrate` at deploy.
- **B2 finding.** The admin database credentials are the database owner, so the
  per-read transaction stays until SELECT-only roles exist. Owner follow-up.
