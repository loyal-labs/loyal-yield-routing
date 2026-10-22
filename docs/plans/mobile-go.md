# Loyal mobile on the shared Go backend

Goal: ship Earn MAX in the React Native app without a second, mobile-only
backend. Web and mobile call the same paths on `loyal-web`. Go answers the
families it already models, the rest proxy to Next unchanged, and one schema
owns each response.

Status: written 2026-10-22 against main 5f753a96 and brought up to the shipped
state at main 35f56e86. M0, M1, M2, M3 and M5 are on main, and so are the app's
Earn MAX reads (see Units). `web/apps/mobile` was byte-identical (tree d8e71610)
to loyal-app main 552f22c4, Vlad's latest push (#846, Oct 21), at the start; the
daily pin job flags anything loyal-app adds after that.

## Owner decisions (2026-10-22)

- Vercel is untouched. Production web stays on loyal-app's Next, so this plan
  makes no production Next edits.
- Focus is React Native mobile.
- The mobile base URL is `next.askloyal.com`. Preview builds go first. The
  production build flips on the owner's go.
- Mobile is invite-gated like web.
- Every per-wallet read requires a session. Passive wallet-keyed reads are not
  ported.
- A push token reassigns to the latest authenticated wallet. Binding a token to
  a device is a follow-up.
- Loyal web bugs are fixed in the Go/Vite frontend (`web/apps/loyal`,
  `loyal-web`), never in loyal-app Next.
- Builds, EAS and OTA are Vlad's. This plan does no work on EAS workflows,
  `eas.json` or the `loyal-mobile` mirror.

## What the app calls today

One base URL: `env.apiBaseUrl` (`web/apps/mobile/src/config/env.ts`), from
`EXPO_PUBLIC_API_BASE_URL`, default `https://next.askloyal.com` (loyal-web).
M-base (#543) deleted `API_BASE_URL` and `EARN_API_BASE_URL`. The `eas.json`
profiles still carry the old value until Vlad flips them (see Deploy
sequence). `GRID_AUTH_BASE_URL` (default `auth.askloyal.com`) is configured but
never called.

The routes census (`scripts/web/routes-census.ts --check`, in CI) reads every
`${env.apiBaseUrl}/api/...` path in the app and matches it to a server route.
Today that is 38 client paths.

### Go-native on loyal-web

Nine calls, the list `scripts/web/mobile-go-check.ts` preflights
(`goServedMobileCalls()` in the census):

| Call (mobile) | Go handler |
|---|---|
| GET `/api/smart-accounts/earn-max/{summary,activity,invite}`, POST `…/invite` | `internal/webapp/earnmax_routes.go`, session required |
| GET `/api/summaries[?groupChatId]` | `internal/webapp/content.go` |
| GET `/api/mobile/{dapps,library,tokens/{mint}}` | `content.go`, `token_detail.go` (CoinGecko) |
| POST `/api/push-tokens` | `internal/webapp/push.go`, wallet from the session |

`summaries`, `mobile/*` and `push-tokens` exist only in the Telegram Next app.
Go's proxy upstream is the web Next app, so when Go does not register one of
them the request reaches Next and 404s. See Known limits.

### Proxied to the web Next app

The other 29 paths reach `web/apps/web` through the Go proxy unchanged:

- The mobile-only family `smart-accounts/mobile/earn/*` (24 routes listed in
  `scripts/web/mobile-twins.txt`, including `session`, `state`, `holdings`,
  `earnings`, `transactions`, deposit, withdraw, autodeposit and refund) and
  `smart-accounts/mobile/earn-max`. Each wraps the same
  `lib/yield-optimization/*` core as its web twin under
  `smart-accounts/yield-optimization/*` with different path names, auth and
  shapes.
- Server-built transaction routes still called as fallbacks when a context is
  missing: `deposit/prepare`, `withdraw/prepare`. `policy-refunds/prepare` has
  no on-device path yet, so refund still builds on the server. The
  `web/AGENTS.md` rule (prepare on-device with the shared SDK) holds for
  deposit, withdraw and autodeposit.
- `POST /api/auth/privy/complete` (header `privy-id-token`),
  `POST /api/observability/mobile/{metrics,errors,events}`, and the shared
  `GET /api/smart-accounts/earn-forecast/summary` and
  `GET /api/solana-week/progress`.
- `deposit/confirm/sponsored` (`earn-api.ts`) has no handler anywhere. It sits
  behind `EXPO_PUBLIC_EARN_SPONSORED_DEPOSITS` (off) and waits on the #452
  backend.

Earn MAX itself is on the first list: the app reads summary, activity and
invite from Go, builds Voltr deposits and withdrawals on the device with
`@loyal-labs/smart-account-vaults` (#531, #538) and sends them. No Earn MAX
screen design exists yet (Owner calls).

### Auth

 A wallet-signed message `Loyal Mobile Earn / purpose / wallet /
issuedAt` travels in the JSON body (5 min window, 60 s skew,
`identity/server/mobile-wallet-auth.ts`). Its first use mints a 30-day
session JWT (POST `mobile/earn/session`, audience `loyal-mobile-earn`,
`identity/server/mobile-earn-session.ts`), which the app keeps in SecureStore
per wallet (`earn.sessions.v2`) and sends as `Authorization: Bearer`. The app
mints silently at Privy sign-in and on a rejected token for Privy and local
signers; a hardware signer (mwa, Seed Vault, deeplink) mints on its first Earn
action, since minting asks the wallet to sign. A mint that is still rejected
starts a 60 s cooldown. The web cookie
`loyal_wallet_session` is signed by the same code path and differs only by
audience. Both follow one env switch. With `AUTH_JWT_RS256_PRIVATE_KEY` set the
token is RS256. Otherwise it is HS256 under `AUTH_JWT_SECRET`
(`identity/server/session-cookie.ts:209-224`,
`identity/server/mobile-earn-session.ts:54-58`).

Settled 2026-10-21. Production currently signs mobile session tokens HS256. One
live mint against askloyal.com with the test wallet returned header alg
HS256, and the web cookie uses the same switch.

Owner decision, 2026-10-22, option A. Go holds `AUTH_JWT_SECRET` until Vercel
moves to RS256, then the secret is dropped. RS256 stays the target, where one
verifier holding the public key authenticates both clients. Until then:

- Go verifies only and never signs. Minting stays in Next.
- Each key verifies only its own algorithm: the RS256 public key when
  `AUTH_JWT_RS256_PUBLIC_KEY` is configured (optional), and HS256 under
  `sha256(AUTH_JWT_SECRET)`. The header's `alg` picks the key, which accepts
  the same tokens as Next's RS256-then-HS256 order and never checks one
  algorithm with the other's key.
- `exp` is required, `nbf` is enforced, with no clock tolerance (jose's
  default). A cookie token must carry no `aud`; a bearer must carry
  `loyal-mobile-earn`.
- `AUTH_JWT_SECRET` arrives as a systemd `LoadCredentialEncrypted` credential.
- Without it the Principal routes are not registered. Earn MAX reads then
  proxy to a Next answer the app refuses (no `config.walletAddress`), and
  `push-tokens` reaches the Telegram-only shape or 404s (Known limits).
- Installing the credential on the host waits on the owner's go.

### Third parties, direct from the device

 Jupiter lite search and swap,
CoinGecko SOL price, Helius RPC, Magicblock PER (`lib/solana/rpc/constants.ts`).
These stay on the device and are not part of this plan.

### Types

Reads from Go routes are typed from `@loyal-labs/api`, generated from the Go
OpenAPI (`scripts/web/openapi-types.ts --check`, #532, #543). The routes census
fails a hand-written exported type named like a generated one. The Next-served
Earn types are still hand-copied (`EarnDepositPrepareResponse`,
`EarnWithdrawPrepareResponse`, `EarnTransactionItem`, `EarnEarningsResponse` and
others, defined in `apps/web/src/lib/yield-optimization/*.shared.ts` and again
in `apps/mobile/src/lib/solana/earn/earn-api.ts`). M6 removes them.

### Go today

`loyal-web` serves the routes above natively and reverse-proxies every other
`/api/*` call to `LOYAL_WEB_UPSTREAM` (default `https://app.askloyal.com`, the
web Next app). The proxy rewrites `Origin` and passes every other header
through, `Authorization` included (`internal/webapp/proxy.go`). One edge
verifier (`internal/webapp/auth.go`) turns the cookie or bearer into a
`Principal`. Each group of Go routes registers only when its credentials and
database grants are present; otherwise `loyal-web` logs a WARN line
(`… not served by Go`) at startup and the paths fall through to the proxy.

## Lies table

| Lie underneath | The one fix | Deleted |
|---|---|---|
| Mobile is a different API client from web | One path per operation for both clients, with the caller taken from the cookie or the bearer | `smart-accounts/mobile/earn/*` twins (5,918 LOC) once old builds age out; mobile-only auth branches |
| Each client owns the response shape | The server's schema owns it: Go structs → Huma OpenAPI → generated TS in `web/packages/api` (G7's generator); Next-served routes move their `*.shared.ts` contracts into a package | Hand-copied types in `earn-api.ts` |
| Mobile needs two backends | One base URL, `loyal-web`; Go serves the Telegram content reads from tables it already owns | `API_BASE_URL` (deleted, #543); telegram `mobile/*`, `summaries` and `push-tokens` routes (still live, not deleted) |
| `web/apps/mobile` belongs to loyal-app | Yield owns it the way it owns `web/apps/loyal`; nothing syncs it, and a pin plus a daily job flags loyal-app pushes that touch it | loyal-app's copy (archived later) |
| The server builds mobile transactions | The device builds them with the shared SDK; the server returns prepare contexts | `deposit/prepare`, `withdraw/prepare`, then `policy-refunds/prepare` once refund builds on device |
| Earn MAX's Voltr plans are web code | `features/earn-max/voltr/{program,plans}.ts` move to a package; their imports are web3.js, spl-token, buffer and `@loyal-labs/loyal-smart-accounts`, with no React | The web-local copies |
| A push token's owner is whoever posts it | The owner is the session's wallet | Body identity fields |

## Target shape

```
RN app ──► loyal-web (Go) ──native──► Earn MAX reads, Earn reads, content reads, push
   │             └──proxy───► app.askloyal.com (Next) for every family not yet in Go
   └─ device ──► Jupiter, CoinGecko, Helius, Magicblock (unchanged)
```

- One edge verifier in Go, `internal/webapp/auth.go`, reads the
  `loyal_wallet_session` cookie or `Authorization: Bearer`, verifies the token
  (see Auth) and yields `Principal{Wallet, Settings, SmartAccount}`, always
  fully resolved. The cookie carries the settings account. The bearer path does
  one SQL lookup in ready smart accounts (`app_users`,
  `app_user_smart_accounts`). Both paths check
  `squads.SmartAccountAddress(settings, 0)` and that the signers contain the
  wallet, else 409 `smart_account_required`. Email-only cookie sessions get 401
  `wallet_required`. A refused cookie is 401 `unauthenticated` and a refused
  bearer 401 `invalid_mobile_session`, the code mobile drops its cached token
  on. Handlers take a `Principal` and never parse tokens.
  Minting stays in Next (`mobile/earn/session`, `auth/privy/complete`) until a
  later unit, so Go holds no private key.
- Paths: Go serves the web paths (`/api/smart-accounts/earn-max/summary`,
  `…/yield-optimization/earn-state`, and so on). Mobile moves to them. Go
  implements a `/mobile/` path only for content with no web twin
  (`/api/mobile/{dapps,library,tokens/{mint}}`, listed in `GO_MOBILE_ROUTES` of
  the census); any other Go route under `/mobile/` fails CI. Installed builds on
  older JS keep calling Next directly until they update.
- Responses: the superset. Mobile `state` becomes web `earn-state`, and
  the app ignores fields it does not render.
- CORS: none needed. React Native's `fetch` is not a browser, so no
  preflight runs.
- Location: the app stays at `web/apps/mobile`. It already resolves
  `web/packages/*` through tsconfig and Metro (`metro.config.js:9-62`). Moving
  the path would buy nothing.

## Units

Each unit ends green before the next. "Parity" means `scripts/web/api-diff.ts`
shows equal JSON for the test account against the Next handler it replaces.

| Unit | Change | Done when |
|---|---|---|
| M0 ownership | `web/LOYAL_APP_BASE` pins the loyal-app commit (552f22c4). A daily job (`.github/workflows/loyal-app-pin.yml`, `scripts/web/loyal-app-pin.ts`) fails when loyal-app main gains commits touching `apps/mobile` or the moved package paths. `sync.sh` and `check-untouched.sh` were deleted in #490, so nothing syncs. EAS, builds and the mirror stay Vlad's | The job is green on the pin and red when loyal-app main moves a mobile path. Its error says to port the range into `web/` and bump the pin |
| M1 one base URL | `API_BASE_URL` in `src/config/env.ts` defaults to `https://next.askloyal.com`. A codemod renames `earnApiBaseUrl` to `apiBaseUrl`, and `EARN_API_BASE_URL` is deleted. `eas.json` stays Vlad's, with no per-build edits from us. The production build flips on the owner's go. Proxy only; Go serves nothing new | Every Earn flow on a preview APK passes through Go (`X-Loyal-Release` on the responses); deposit and withdraw land on mainnet with the test wallet |
| M2 Go verifier | `internal/webapp/auth.go`, verify only, matching `mobile-earn-session.ts` and `session-token.ts`. RS256 public key first when configured, then HS256 under `sha256(AUTH_JWT_SECRET)` (see Auth). Without the secret the Principal routes stay unregistered | A token minted by Next verifies in Go for both audiences; a tampered, expired or cross-audience one is refused |
| M3 Earn MAX reads | `summary` and `activity` in Go from the `earn_max.go` loaders and `programs/voltr`, on the web paths | Parity with `features/earn-max/server/mobile.ts`; the web Earn MAX page reads Go too |
| M4 Earn MAX on device | `voltr/{program,plans}.ts` and the Earn MAX types to `web/packages/smart-account-vaults/src/voltr/`, with the wire serializer in `src/wire/`; RN Earn MAX screen builds Voltr deposits and withdrawals on device; `context` served by Go | A preview APK deposits into and withdraws from Earn MAX on mainnet with the test wallet |
| M5 content and push | `dapps`, `library`, `summaries`, `tokens/{mint}` and `push-tokens` native in Go; push takes the wallet from the session | `API_BASE_URL` is gone from `env.ts`; the rebind repro is refused |
| M6 Earn reads | `earn-state`, `holdings`, `earnings`, `transactions`, `withdraw/sources`, `earn-forecast/summary` and `solana-week/progress` in Go, with types generated from OpenAPI | Parity per route; the hand-copied types are gone from `earn-api.ts` |
| M7 delete | Once Vercel `/api/smart-accounts/mobile/*` traffic is 0 for 14 days, delete the Next mobile twins and the server-built deposit and withdraw routes. The delete lands in loyal-app, which Vercel builds | Vercel `/api/smart-accounts/mobile/*` traffic is 0 for 14 days; `smart-accounts/mobile/` has only routes still minted in Next |

Status at main 35f56e86:

- M0 done (#527).
- M1 is in the app (#543) and waits on the deploy sequence below. Its done-when
  (a preview APK through Go) is not yet observed.
- M2 done (#528, #535).
- M3 done (#537, #541). The money fixes are in #539.
- M4 is partial: the SDK move (#531) and the app's Earn MAX reads, Voltr send
  and invite entry (#538) are on main. The screen design is an owner call.
- M5 done (#530 content, #541 push).
- M6 and M7 not started. Minting stays in Next.

Prepare contexts and confirms (signing with `deployment-policy-signer`,
repository writes) stay in Next and are reached through the proxy. They move to
Go only if the owner wants Go to hold that key. That is a separate plan.

## Principles that changed a decision

- **Attack the premise.** I read "move mobile to yield routing" as a code move
  and tested it: the tree is already here, byte-identical. So M0 flips
  ownership and keeps the path. The Go survey also listed CORS as a blocker
  for mobile; native `fetch` does not preflight, so CORS work is out of the
  plan.
- **Subtract before you add.** Go serves the web paths, never `/mobile/`
  copies, and the content reads move to Go so `API_BASE_URL` can go. Both
  remove a layer instead of porting one.
- **Type discipline.** TS types for Go routes come from the OpenAPI the Go
  structs emit. The plan writes no new hand-written TS contracts.
- **Boundary discipline.** The caller is verified once at the Go edge into a
  `Principal`. Today each of the 24 mobile handlers verifies its own caller.
- **Fix at source.** The push-token rebind is fixed where the identity comes
  from (the session), not with a rate limit or a debug guard.
- **Sequence verifiable units.** M1 sends mobile traffic through Go with
  nothing native, so a failure that appears in M3 points at the new handler.
- **Migrate callers, then delete legacy APIs.** The Next mobile routes are the
  time-boxed adapter for builds we cannot update (installed apps on old JS).
  M7 deletes them once Vercel mobile traffic has been 0 for 14 days.

## Earn MAX activity bugs: fixed in Go/Vite, still live in loyal-app Next until cutover

Per the owner (2026-10-22), Loyal web bugs are fixed in the Vite+Go frontend
(`web/apps/loyal` and `loyal-web`), not in loyal-app Next. These live in
`web/apps/web/src/features/earn-max/voltr/` and stay until cutover.

1. Truncated history reported complete (`activity.server.ts:174-178`, `:203`).
   The page is 100 signatures but failed ones are dropped before the count, so
   `complete` reads true. Repro: 100 signatures, one failed, an older $100
   deposit outside the page, $100 still held. Result: "$100 earned" instead of
   unknown.
2. A claim is charged the whole transaction's idle-account change
   (`activity.server.ts:66-79`, used at `:147`). Repro: one tx claims $100 and
   deposits $40, so the claim shows $60. Several users' claims in one tx share
   the total.
3. Claim and request are paired by instruction order (`activity.server.ts:193-199`).
   Repro: claim an old $100 withdrawal, then request $40 in the same tx; the
   request shows $100.
4. Batch reorder pairing (`activity.server.ts:182-188`, results paired with
   `signatures[index]`). Repro: a 09-28 transaction (zgk1Rw, slot 451324332)
   showed as a 10-11 deposit when the RPC reordered the batch.
5. A transaction with no blockTime anywhere is written as the 1970 epoch
   (`activity.server.ts:132`, `?? 0`). Go skips it and marks history
   incomplete.
6. A finite annualized APY of 2^63 bps or more (`apy.server.ts:86-88`). Next prints the
   number, Go emits null.

The Go fixes for 1 to 4 landed in #539: earned is unknown once a full page of
signatures was read, a claim is charged only the idle transfer under its own
instruction and pairs with the request whose escrow its burn matches, and a
transaction served for another signature is rejected. Items 5 and 6 already
behaved correctly in Go.

The browser fix for the same reorder bug in the wallet activity provider
(`web/packages/solana-wallet/src/providers/default-activity-provider.ts`) keys
each parsed transaction by its own `transaction.signatures[0]` and drops one
whose signature was not requested. Its sibling in the mobile app,
`web/apps/mobile/src/lib/solana/rpc/get-account-txn-history.ts`,
pairs by index too and was fixed the same way in #540, with a
reordered-batch test beside it.

Eight tests in `web/packages/smart-account-vaults/src/client.test.ts` failed
identically at 568b6632 and at d719f7b6 (not rerun at 35f56e86): `prepareEarnUsdcDeposit` x5 and
`prepareEarnUsdcWithdraw` x3. Messages include "Cannot safely prepare an Earn
policy without scanning on-chain policies." and "Unable to find Settings
account at 11111111111111111111111111111112". Nothing is filed in Linear.

## Deploy sequence

Before Vlad's next Earn build from main, in this order:

1. The owner's go to deploy `loyal-web`, with every credential installed
   (`AUTH_JWT_SECRET`, `WEB_APP_DATABASE_URL`, `WEB_YIELD_DATABASE_URL`,
   `WEB_SOLANA_RPC_URL`, `EARN_MAX_INVITE_CODE_HASHES`, `COINGECKO_API_KEY`,
   and the database grants). The startup log has no `not served by Go` WARN
   line.
2. The preflight passes against https://next.askloyal.com: the `preflight`
   step of `scripts/web/mobile-go-check.ts` (see Post-deploy verification).
3. Vlad flips `EXPO_PUBLIC_API_BASE_URL` in `eas.json` to
   https://next.askloyal.com, preview profile first, then makes the next Earn
   build.

A build from main before step 3 sends Earn calls to the telegram Vercel app.
The preflight exists because Go proxies what it does not register, and the
proxy target (the web Next app) has none of `summaries`, `mobile/*` or
`push-tokens`, and answers Earn MAX summary without `config.walletAddress`,
which the app rejects as "answered for a different wallet".

## Known limits

- A push token reassigns to whichever wallet last posted it with a valid
  session; binding a token to a device is a follow-up.
- After the flip, a `loyal-web` restart that loses a credential (for example
  `COINGECKO_API_KEY`, `AUTH_JWT_SECRET` or a database grant) makes the Go-only
  mobile paths 404 or answer in a shape the app refuses, instead of proxying
  to something that works. Watch the `not served by Go` WARN line at startup.
  Rerun the preflight after any restart that changed credentials.
- The preflight sends `{}` to the two POST paths, so it proves routing and
  the `X-Loyal-API-Source` header, not that a push token saves or an invite
  redeems. It needs the Go toolchain (the census reads the Go OpenAPI).
- Minting a mobile session stays in Next. A Next outage stops sign-in and
  hardware-wallet Earn actions, not reads with a cached token.

## Post-deploy verification

Run by the owner after the approved deploy of `loyal-web` to next.askloyal.com.
Neither script runs in CI. Both print no key, token or cookie. Setup once:
`cd web && bun install --frozen-lockfile`.

Parity, Go (next.askloyal.com) against Next (app.askloyal.com). Take the web
session cookie from a signed-in browser on app.askloyal.com (DevTools,
Application, Cookies, the `loyal_wallet_session` value) and paste it at the
silent prompt. `POLICY_KEYPAIR` (the test wallet) comes from `op run`; the
bearer is minted from it at the Next origin.

```sh
read -rs LOYAL_WEB_SESSION_COOKIE && export LOYAL_WEB_SESSION_COOKIE
op run --env-file=.env.1password -- bun scripts/web/api-diff.ts https://next.askloyal.com https://app.askloyal.com
```

It prints PASS or KNOWN per row and exits 1 on any FAIL. Each passing row
asserts `X-Loyal-API-Source: go`; summary and activity fail as "parity
unverified: fixture has no position" when the test wallet holds nothing.
KNOWN rows are the Go fixes listed above. `--auth bearer` skips the cookie rows;
`--writes` adds the invite POST and push-token rows, off by default.
`bun scripts/web/api-diff.ts --help` prints the route table.

Mobile against Go. Mints a session, then the preflight: every mobile client
path the routes census lists as Go-served (`goServedMobileCalls()`, never a
hand copy) is requested with the bearer and must answer with
`X-Loyal-API-Source: go` and not 404; every failing path is named and the
run stops. Then it reads summary, activity and invite, builds a 1 USDC Earn MAX
deposit with `prepareEarnMaxVoltr` and simulates it. It never signs or sends. `SOLANA_RPC_URL` comes from the same env
file. The deposit needs the test wallet to hold 1 USDC and SOL for fees.

```sh
op run --env-file=.env.1password -- bun --tsconfig-override web/apps/mobile/tsconfig.json scripts/web/mobile-go-check.ts
```

The on-device mainnet deposit and withdraw stay on Vlad's preview build.

## Owner calls

1. Decided 2026-10-22 (M2): Go holds `AUTH_JWT_SECRET` until Vercel moves to
   RS256, then the secret is dropped.
2. Go to deploy `loyal-web` and to install the credentials and grants,
   including the `AUTH_JWT_SECRET` credential (Deploy sequence, step 1).
3. Vlad's `eas.json` flip of `EXPO_PUBLIC_API_BASE_URL` to
   https://next.askloyal.com, preview profile first (step 3), and the owner's go
   to flip the production profile after the preview canary. The build is Vlad's.
4. An Earn MAX mobile screen design (none exists).
5. RS256 on Vercel, then remove `AUTH_JWT_SECRET` from Go.
6. An empty performance chart for 100 or more signatures: the Go reader drops
   every running-principal point from the first unknown amount on (#539), so a
   wallet whose history fills the 100-signature page shows no chart. Accept
   that or change it.
7. A cancelled withdrawal request shows $0: `cancel_request_withdraw_vault`
   closes the open request at zero (#539). Accept $0 or change it.
8. Push device binding: a token reassigns to the latest authenticated wallet
   today. Decide whether to bind a token to a device.
9. Fall back to the telegram app for Go-served telegram paths when Go does not
   register them. Rejected for now: Telegram's `push-tokens` takes the old
   body-identity shape and Next's Earn MAX summary lacks `config`, so a
   fallback would not work. The preflight gates the flip instead.
10. Decide whether Go may ever hold the deployment-policy signer (the follow-on
    plan for prepare and confirm).

## Unknown

- The Expo project's GitHub link, build credentials and OTA channel names.
  `eas.json` was read, the Expo dashboard was not.
- How the refund flow builds its transaction (`refund.ts` beyond line 28) and
  the `wire.ts` serializers were not read.
- Whether the Go proxy carries the app's request bodies (confirm payloads
  hold several base64 transactions) inside its limits. This is untested and
  is M1's check.
- An Earn MAX screen design for mobile. None exists in the app. Figma and
  Linear were not searched.
