# loyal-web: the Loyal web app on Go + Vite

## Current production state (Oct 22 2026)

`app.askloyal.com` and `next.askloyal.com` serve Go + Vite on loyal-workers.
`askloyal.com/app` redirects to the app host. Marketing remains on Vercel;
`backend.askloyal.com` keeps the Next API reachable without a proxy loop.
Go serves public Earn statistics; remaining production API families still
proxy to Next. Go auth/Earn MAX activation is a separate rollout.

The retained `loyal-frontend` Vercel project still builds `loyal-app/apps/web`.
Changing its Git source to this repo and root to `web/apps/web` is pending;
verify a preview and API parity before changing its production source.

## Repository consolidation

- Published loyal-app main through `3875075d` is reconciled. The shared Voltr
  SDK import replaces the old app-local import; web gate removal and skeleton
  changes already landed here. `web/LOYAL_APP_BASE` records that published pin.
- Local mobile commit `5fe09749d99d5e52b69d06d5f798f9d574897fed` was ported
  separately, including the updated home, Earn MAX screens, history and shared
  types. Its source checkout and Android OTA channels are unchanged. The local
  dApp Store OTA note was also preserved. This does not publish an OTA.
- Keep both API contracts during consolidation: imported mobile screens use the
  existing `/api/smart-accounts/mobile/earn-max` route through loyal-web; the Go
  `earn-max-v4` client remains available. Both use `env.apiBaseUrl`.
- Commit-style and web-journey checks move to root `.github/workflows` with
  `web/` working directories and path filters. Imported nested workflows do
  not run on GitHub.

### Remaining workflow and deployment handoff

| Workflow | Before activation here |
|---|---|
| Extension CI | Move to root, prefix paths with `web/`, verify extension API/telemetry secrets and release tags |
| Release Packages | Move to root, run in `web/`, set Changesets `cwd: web`; provide `NPM_TOKEN` and `RELEASE_GITHUB_TOKEN`; stop old publisher first |
| Sync Mirror Repos | Move after package publishing, run in `web/`, verify changed-path selection under the prefix and `MIRROR_SYNC_TOKEN` |
| Preview Domain | Move after Vercel source change and provide `VERCEL_TOKEN` |
| Trusted Vercel Previews | Move after Vercel source change and configure `TRUSTED_VERCEL_PREVIEW_USERS` |
| Claude / Pullfrog | Retain inactive until maintainers choose whether to migrate the bots and their credentials |

The destination repository currently lists no repository-level Actions secrets
or variables; organization-level access still needs verification. GitHub cannot
export old secret values. Reuse the approved secret source rather than copying
credentials into files or asking for them in chat.

Before archiving loyal-app: finish those workflow handoffs, reconcile any further
local/unpublished edits with their owner, verify the Vercel preview and production
source switch, then stop the old workflows. The source pin does not claim that
uncommitted changes have been imported.

## Original pilot acceptance criteria

1. `web/` holds loyal-app with its history. (Done: #399.) `scripts/web/sync.sh`
   was deleted in #490, so nothing syncs, but loyal-app still receives pushes.
2. `scripts/web/check-untouched.sh` was deleted in #490. The pin file
   `web/LOYAL_APP_BASE` and the daily `loyal-app-pin` job fail when loyal-app
   main gains commits touching `apps/mobile` or the moved package paths.
3. `loyal-web` runs under systemd on loyal-workers; `/readyz` is 200 at the
   test host and `X-Loyal-Release` is the deployed main sha.
4. `scripts/web/sweep.ts` renders all 25 pages of the route table on the test
   host with no page error, screenshots beside the Vercel baseline.
5. One `/api` read family is answered by Go from `go/workers/internal` code,
   its JSON equal to Vercel's for the test account.
6. `loyal-engine`, `loyal-observer`, `loyal-realtime` NRestarts unchanged
   across every loyal-web deploy.

## Shape

- `web/apps/loyal` renders apps/web's components **unchanged**. `vite.config.ts`
  aliases `next/{link,image,navigation,dynamic,font/google}`, `geist/font/*`
  and `server-only` to `src/next-compat/*`, and swaps Node-only modules for
  `src/overrides/*`. `process.env` in the bundle is the `NEXT_PUBLIC_*` set,
  as Next inlines it; `Buffer` is injected for the Solana libraries.
- `src/routes.table.ts` lists every Next page and layout and its route;
  `scripts/web/gen-routes.ts --check` fails when they disagree.
- `go/workers/cmd/loyal-web` (in the workers module: only it may import
  `go/workers/internal`) serves the embedded build (`internal/webserve`),
  the `/api` routes Go owns (`internal/webapp`, huma on chi), and forwards
  every other `/api/*` to the Next deployment (`LOYAL_WEB_UPSTREAM`, set to
  `https://backend.askloyal.com` in production).
- `bun run web:build` builds it all into `bin/loyal-web`, including the normalized
  frontend app environment. Go serves `/500` with HTTP 500 and the disabled
  `/dev/txv1` page with HTTP 404 in production; both retain the SPA shell and
  use `Cache-Control: no-store`. Development/local builds keep the probe at 200.

## Facts the seed established

- Vercel overwrites `x-forwarded-host`: Next sets the session cookie with
  `Domain=askloyal.com` whatever host fronts it (checked through the proxy on
  /api/auth/logout). The test host must be under askloyal.com:
  `next.askloyal.com`, served through the Cloudflare tunnel `loyal-workers`
  as robinx.trade is (a DNS-only record to the host loaded at ~70 KB/s from
  some networks; deploy/hetzner/README.md, "Cloudflare tunnel").
- `/api/observability/events` checks `Origin` against its own URL; the proxy
  must present a same-origin request's Origin as the upstream origin.
- Wallet challenge and completion keep the browser's original `Origin`, so
  SIWS names the public app even when Next is reached through a separate
  backend hostname. Both requests must use the same origin.
- `web/` packages ship from `dist/`: the app's build runs `build:packages`
  first, as apps/web's `prebuild` does.

## Lanes

| Lane | Owns | Acceptance |
|---|---|---|
| G1 next-compat | `web/apps/loyal/src/next-compat/**` | Link routes in-app hrefs through TanStack (external → `<a>`); Image handles fill/priority/static imports; navigation matches Next semantics; dynamic; Geist and IBM Plex faces load and define their CSS variables; `headFromMetadata` maps title (incl. templates), description, openGraph, twitter, alternates.canonical, icons |
| G2 app routes | `src/routes/app/**`, `src/routes/dev/**` | `/app`, `/app/policies`, `/app/settings`, `/app/cherry`, `/dev/txv1` render with apps/web's providers; signed-in dashboard shows data on the test host |
| G3 site routes | `src/routes/{index.tsx,500,agents,earn,landing,privacy-policy,risks,trust,ru,zh}/**` | every site page renders with head equal to Vercel's; locale redirect behaviour of apps/web `features/marketing/i18n/locale-redirect.ts` reproduced (G3 writes the spec, G5 implements the server half) |
| G4 blog + static | `src/routes/blog/**`, `src/overrides/blog-data.ts`, `scripts/web/static-files.ts` | blog index (with `?page=`) and every slug render from `public/blog/*/post.md` read at build; sitemap.xml, robots.txt, llms.txt, llms-full.txt emitted into dist equal to Vercel's |
| G5 Go server | `go/workers/cmd/loyal-web/**`, `go/workers/internal/{webapp/{router,proxy,headers}.go,webserve/**}` | proxy contract (method/body/query/cookies through, Host to upstream, Set-Cookie untouched, SSE flushes, same-origin Origin rewritten to upstream's); `/ingest/*` as apps/web next.config rewrites; CSP Report-Only and frame-ancestors equal to next.config.ts:44-72,150-167; cache tiers of wen webserve.go; `go vet`, `go test -race` green |
| G6 deploy | `deploy/hetzner/systemd/loyal-web.service`, the `web` case in `scripts/deploy-workers.sh`, `deploy/hetzner/caddy/loyal-web.caddy`, README section | `bun run deploy web <sha>` builds linux/amd64, ships, activates under the host lock, checks `/readyz` and the release, restores the previous binary on failure; unit limits (MemoryMax, CPUQuota, Nice); no IPs in the repo |
| G7 Go API pilot | `go/workers/internal/webapp/api_*.go`, `web/packages/api` generation, `scripts/web/api-diff.ts` | one yield read family on the same path as its Next handler, JSON equal to Vercel's for the test account |

Shared files (`routes.table.ts`, `__root.tsx`, `main.tsx`, `vite.config.ts`,
`index.html`, `go.mod`, `internal/webapp/api.go`) belong
to the foreman: a lane asks, the foreman edits on `web-go/seed`.

## First API pilot: public Earn statistics

`GET /api/earn/stats` reads the existing App Neon snapshot in Go, preserving
the Next JSON fields, cache policy, freshness limit, and unavailable error.
It is opt-in through the `WEB_APP_DATABASE_URL` systemd credential; without
that credential the route still proxies. Activation and rollback are in
`deploy/hetzner/README.md`. `scripts/web/api-diff.ts` verifies equal JSON at
the same snapshot timestamp and rejects a comparison against a proxy response.
Implementation alone does not satisfy criterion 5: activation and live parity
verification remain deployment steps. No generated client is introduced for
this pilot; existing frontend callers keep the same URL and response contract.

Mobile feature work can proceed against the existing backend. The pin in
`web/LOYAL_APP_BASE` and the daily loyal-app-pin job (PR #527) tell us when
loyal-app changes need porting here. Those API families are outside this public
read pilot.

## Remaining migration sequence

1. Complete repository/workflow consolidation and the retained Next Vercel source
   switch described above. Archive loyal-app only after those checks pass.
2. Move remaining API families to Go in separate contract-checked rollouts,
   keeping mobile endpoints compatible. Remove their Next proxies after parity.
3. Prerender and verify marketing for SEO, then switch the marketing hostname.
4. Remove the Next compatibility layer and retired Next implementation once
   neither production traffic nor retained API handlers need them.

Live deposit/withdrawal testing belongs to the owner and is not an agent-side
blocker for repository consolidation.
