# loyal-web: the Loyal web app on Go + Vite

Owner decisions (Oct 20 2026): loyal-app lives in this repo under `web/`;
databases stay on Neon; Vercel stays production. Both sites (app and
marketing) are built with Vite + TanStack Router and served by the Go binary
`loyal-web` on the loyal-workers host, at a test hostname, for the owner to
open and test. No DNS flip, no move off Vercel in this phase.

## Acceptance criteria

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
  every other `/api/*` to the Next deployment (`LOYAL_WEB_UPSTREAM`, default
  `https://app.askloyal.com`).
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

Mobile feature work can proceed against the existing backend. New mobile
Earn MAX backend/shared SDK changes from loyal-app must be synced here before
porting those API families; they are outside this public read pilot.

## Later, not in this phase

Repoint Vercel and archive loyal-app (then loyal-app's workflows move here);
codemod next-compat away and delete the Next pages; prerender marketing for
SEO; DNS flip; move the remaining `/api` families to Go.
