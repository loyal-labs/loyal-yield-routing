# Rebuild the remaining loyal-app apps on Go + Vite

Status (Oct 25 2026): kept as the record of the Oct 21 proposal, and in part
overtaken. Later on Oct 21 the owner redirected the move into
loyal-yield-routing, without the Vercel framing. Since then:
- W3 admin is done: admin.askloyal.com has served the Go + Vite loyal-admin
  since Oct 22.
- The dashboard is done: stats.askloyal.com has served loyal-stats since Oct 22.
- W1 and W2's Earn families run on loyal-web at next.askloyal.com (#608–#614).
Retiring the Vercel projects and deleting the Next code each need the owner's
go. Follows `web-go.md`, which moved the web frontend.

Owner decisions in force: every app is rebuilt as Go + Vite on the
loyal-workers host behind test hostnames; Vercel stays production while the
rebuild runs; leaving Vercel is the next phase, after this one is ready
(Oct 21). Databases stay on Neon and loyal-app stays under `web/` (Oct 20).

## Done predicate

Each row is countable on the real artifact.

1. Every app kept in the inventory below has a build from this repo running on
   loyal-workers at a test host, or (mobile, extension) a test build pointed
   at those hosts.
2. Page sweep: every page of each Vite app renders with no page error,
   screenshots beside the Vercel baseline (`scripts/web/sweep.ts`, extended
   per app).
3. API parity: every ported `/api` path shows 0 JSON differences against
   Vercel for the test account (`scripts/web/api-diff.ts`, driven by the
   family manifest below).
4. Transaction parity: every ported prepare endpoint builds the same
   instructions as the TS builder for the recorded inputs (tx-diff harness),
   then one small real deposit and withdraw land on the test account (owner
   go: money).
5. The `/api` proxy to Vercel serves 0 requests for the kept apps over 24 h on
   the test hosts (counted from `loyal-web` request logs).
6. `loyal-engine`, `loyal-observer`, `loyal-realtime` NRestarts unchanged
   across every deploy of the new services.

## Inventory (measured Oct 21 at loyal-app main 552f22c4)

Activity is commits in 90 days and ClickStack telemetry in 30 days. LOC is
non-test TS/TSX.

| App | Today | Activity | Server surface | LOC | Target |
|---|---|---|---|---|---|
| web | Next on Vercel; Vite copy live on the test host | 445 commits/30d, telemetry | 102 route handlers, 3 crons | 157k | Frontend done; port the API to Go |
| mobile | Expo 54, EAS | 38, telemetry | none (calls web's `/api/smart-accounts/mobile/*`) | 50k | Test build with `EXPO_PUBLIC_EARN_API_BASE_URL` at the Go host |
| admin | Next on Vercel | 23 | 9 routes, 9 server-action files, 20 pages | 35k | Vite SPA + Go admin API |
| telegram | Next + grammy on Vercel | 16 | 20 routes incl. webhook, 3 crons, 2 pages, own drizzle DB | 18k | Bot service on loyal-workers (shape is an owner call) |
| extension | WXT, store build in `extension.yml` | 6 | none | 14k | Test build pointed at the Go host |
| dashboard | Next on Vercel | 3 | 1 route | 0.9k | Fold into `web/apps/loyal` as one route, or drop |
| privy-showcase | Next on Vercel | 6 | 3 routes | 2k | Drop, or leave on Vercel |
| userbot | Bun worker, no deploy config | 1 (last Aug 12) | worker process | 3k | Confirm dead, then drop |

Not apps, kept as they are: `web/packages/*` (15 libraries, 39k LOC, the
TS reuse surface) and `web/observability` (ClickStack and its Telegram relay
on Render, out of scope).

## Lies table

| Lie underneath | The one fix | Deleted |
|---|---|---|
| loyal-app and yield are both the source of `web/` | One writer: Vercel builds from this repo's `web/apps/*`, loyal-app is archived (owner call D1) | `sync.sh`, `check-untouched.sh`, loyal-app |
| The server needs Next | Go serves every `/api` family on the same path; the proxy is a time-boxed adapter for paths not yet ported | The proxy, then the Next route handlers |
| Clients need Next | Vite apps render the existing components through the `next-compat` aliases, as `web/apps/loyal` does | Next pages, after leaving Vercel |
| API shapes are typed twice | huma's OpenAPI generates the TS client (hey-api) the apps call | Hand-written response types, family by family |
| Two writers own `user_yield_positions` (Go observer and TS deposit repository) | Go owns it; the TS write path goes when deposits move to Go. This also fixes the stale-reserve finding at source | TS writes in `yield-deposit-repository.server.ts` |
| The yield root and `web/` are separate TS worlds | One Bun workspace, one Bun version; the root consumes the workspace packages directly | npm, pnpm and yarn lockfiles; the root's registry pins |

## Shape: TS for clients, Go for the server

- **TS reuse.** UI, wallet and Solana client code stays TS and is shared by the
  web and admin Vite apps, mobile and extension through `web/packages/*`.
  Server-only TS (`*.server.ts`, about 100 route handlers and the libraries
  behind them) is what Go replaces; it is not ported to the browser.
- **Go reuse.** Go already owns the data behind the read families
  (`observer/earn` writes positions and APY) and the program builders
  (`programs/{squads,kamino,voltr,jupiter,spl}`, `chain/`, `policy/`). New
  endpoints live in `internal/webapp` and import those packages, never the
  reverse, and add no SQL that duplicates a store.
- **One manifest.** `internal/webapp/families.go` lists every `/api` family:
  path, method, owner (go or proxy), fixture account. It drives huma
  registration, `api-diff.ts`, the proxy request counter and the done
  predicate's row 5. A family moves by flipping one row.
- **Auth.** `internal/webauth` verifies the session cookie that
  `api/auth/privy/complete` issues, and the admin session, so ported routes
  accept the same sessions as Vercel during the overlap.

## Waves

Each wave ends green before the next. Each wave runs under one foreman, with
one `review` at its end.

### W0 Foundation (blocking)
- D1 writer move, if approved.
- One Bun workspace.
- Family manifest, `webauth`, hey-api generation (finish `web-go/wip-g7-heyapi`).
- Client IP from `CF-Connecting-IP`.
- `deployment.environment.name` on Go services, so ClickStack sees them.
- Test hosts for admin and telegram on the existing tunnel.

Proof: the manifest lists all 135 handlers with owner `proxy`;
`api-diff.ts` runs from it; `bun install` at the root resolves the workspace
packages.

### W1 Read families to Go
- Web paths: `smart-accounts/{overview,earn-max,earn-forecast,vault-activity,earn-transactions}`
  and the read routes of `yield-optimization` (earn-state, earnings,
  position).
- Their mobile twins under `mobile/earn/{state,earnings,holdings,transactions}`,
  served by the same handlers where the shapes match.
- dashboard's `/api/earn`.

Proof: rows 3 and 5 for these paths; a mobile test build on the Go host
shows the test account's Earn screen.

### W2 Transaction families to Go
- Families: deposits, withdrawals, autodeposit, policies, policy refunds,
  cross-mint, session, position reconcile; web and mobile variants.
- Blocking: the builder fork F1 is settled first.

Proof: row 4. This wave makes Go the only writer of `user_yield_positions`.

### W3 admin
- A Vite app renders the admin components through the aliases.
- The 9 server-action files become Go endpoints.
- `api/earn/rebalance/*` reuses the fleet and multiply planners.
- Admin login moves to `webauth`.

Proof: rows 2 and 3 for admin.

### W4 telegram
- Webhook, crons and the 2 pages on loyal-workers with a test bot token.
- The app database and its drizzle migrations stay where they are.

Proof: the test bot answers, each cron runs once, and the summaries job writes
its rows.

### W5 Web crons
- `smart-account-reconcile`, `earn-yield-joy` and `telemetry-heartbeat` become
  Go timers, inside the worker that already owns the data where one does.

Proof: each runs once on the host and writes what its Vercel twin writes.

### W6 Clients
- EAS and `extension.yml` build from this repo.
- Test profiles point at the test hosts.

Proof: row 1 for mobile and extension.

## Owner calls

- **D1 Writer move, before W0.**
  - Recommended: repoint the Vercel projects at this repo and archive
    loyal-app. Production stays on Vercel and nothing user-facing changes.
    Without it, `web/apps/web` and `web/packages` stay read-only here, and
    every reuse that needs a package edit waits for loyal-app.
  - The cost: the team commits here instead of in loyal-app.
- **D2 Drop list.** dashboard (fold or drop), privy-showcase, userbot.
- **D3 telegram shape.**
  - Recommended: a Bun service that runs the existing TS (grammy, AI
    summaries, encryption), since Go has no overlap there.
  - The alternative: a Go port of 18k LOC.
- **D4 Money canary in W2:** one small deposit and withdraw on the test account.

## Design forks a run will settle

- **F1 Where user-signed transactions are built** (W2): Go flows on
  `programs/*`, a Bun sidecar running the existing TS builders, or the
  clients building them with the TS packages while Go validates and records.
  Settled by `/arena` on one flow (withdraw prepare), judged by the tx-diff
  harness, before W2 starts.

## Risks

- Moving target while D1 is open: loyal-app ships about 15 web commits a day.
- Host capacity: telegram and admin add processes to loyal-workers. Measure
  RAM before W4.
- Session overlap: a cookie issued by Vercel must verify in Go and the reverse,
  until clients stop calling Vercel.

## Later, the next phase

Switch DNS app by app off Vercel, delete the Next route handlers and pages,
merge drizzle-kit into `loyal-migrate`, move the observability stack off
Render.

## Unknown

- The per-endpoint split of reads, transaction builds and writes inside each
  `yield-optimization` and `mobile/earn` route: inferred from file names, not
  read in full.
- Whether `DATABASE_URL` (app DB) and `NEON_DATABASE_URL` (yield DB) are
  separate Neon projects.
- Which telegram routes the mobile app calls (`api/mobile/*`, `push-tokens`).
- The extension's API base and store-publish credentials.
