# Loyal mobile on the shared Go backend

Goal: ship Earn MAX in the React Native app without a second, mobile-only
backend. Web and mobile call the same paths on `loyal-web`. Go answers the
families it already models, the rest proxy to Next unchanged, and one schema
owns each response.

Source read: `web/apps/mobile` at main 5f753a96. It is byte-identical
(tree d8e71610) to loyal-app main 552f22c4, Vlad's latest push (#846, Oct 21),
and to his `ask-2302-integrate-earn-max-with-the-voltr-rwa-vault` branch.
`ota-0.1.2-dappstore` is older (pre-Privy). No pushed branch carries newer
mobile code.

## Owner decisions (2026-10-22)

- Vercel is untouched. Production web stays on loyal-app's Next, so this plan
  makes no production Next edits.
- Focus is React Native mobile.
- The mobile base URL is `next.askloyal.com`. Preview builds go first. The
  production build flips on the owner's go.
- Mobile is invite-gated like web.
- Every per-wallet read requires a session. Passive wallet-keyed reads are not
  ported.
- Builds, EAS and OTA are Vlad's. This plan does no work on EAS workflows,
  `eas.json` or the `loyal-mobile` mirror.

## What the app calls today

Two base URLs (`web/apps/mobile/src/config/env.ts`) plus third parties. The
`GRID_AUTH_BASE_URL` default (`auth.askloyal.com`) is configured but never
called.

### `API_BASE_URL`, the Telegram Next app (solana-telegram-transactions.vercel.app)

Five routes, none authenticated, none used by any other client:

| Call (mobile) | Handler `web/apps/telegram/src/app/api/…` | Data |
|---|---|---|
| GET `/api/summaries[?groupChatId]` (`services/api.ts:63,77`) | `summaries` (106 LOC) | `summaries`, `communities` |
| GET `/api/mobile/tokens/{mint}` (`api.ts:91`) | `mobile/tokens/[mint]` (38) | CoinGecko, in-memory cache |
| GET `/api/mobile/dapps` (`api.ts:116`) | `mobile/dapps` (57) | `trusted_dapps` |
| GET `/api/mobile/library` (`api.ts:151`) | `mobile/library` (124) | `library_sections`, `library_articles` |
| POST `/api/push-tokens` (`services/notifications.ts:137`) | `push-tokens` (62) | `push_tokens` upsert |

Go admin already reads and writes these tables (`internal/admin/dapps.go`,
`library.go`, `push.go`, `communities.go`).

`push-tokens` takes `telegramUserId` and `walletPublicKey` from the body and
overwrites them on conflict (`route.ts:36-52`). Anyone holding a device token
can rebind it to another wallet.

### `EARN_API_BASE_URL`, the web Next app (askloyal.com)

 25 Earn fetches in
`lib/solana/earn/earn-api.ts` (1,313 LOC), plus session, auth, observability:

- Mobile-only route family `smart-accounts/mobile/*`: 24 Earn route files
  plus `earn-max`, 5,918 LOC. Each wraps the same `lib/yield-optimization/*` core as its web
  twin under `smart-accounts/yield-optimization/*`, but with different path
  names (`deposit` vs `deposits`, `withdraw` vs `withdrawals`, `state` vs
  `earn-state`), different auth and different shapes. Mobile `state` drops the
  autodeposit, cross-mint and autoswap fields the web `earn-state` returns.
- Mobile routes with no web route twin: `holdings` and `withdraw/sources`.
  Web reads holdings client-side
  (`lib/yield-optimization/earn-rpc-holdings.client.ts`), and the mobile route
  imports that same reader (`mobile/earn/holdings/route.ts:15`).
  `transactions` has a web twin, `smart-accounts/earn-transactions`
  (`earn-transactions.client.ts:88`). Mobile-only prepare contexts: `deposit/prepare-context`,
  `withdraw/prepare-context`, `withdraw/cleanup/prepare-context`.
- Server-built transaction routes still called as fallbacks when a context is
  missing: `deposit/prepare` (`deposit.ts:378`), `withdraw/prepare`
  (`withdraw.ts:442`). `policy-refunds/prepare` (`refund.ts:28`) has no
  on-device path yet. The `web/AGENTS.md` rule ("prepare … on-device with the
  shared SDK … server-prepared transaction routes are legacy fallbacks")
  holds for deposit, withdraw and autodeposit. Refund still builds on the server.
- `deposit/confirm/sponsored` (`earn-api.ts:1159`) has no handler anywhere. It
  sits behind `EXPO_PUBLIC_EARN_SPONSORED_DEPOSITS` (off) and waits on the
  #452 backend.
- Shared with web: `GET /api/smart-accounts/earn-forecast/summary`,
  `GET /api/solana-week/progress`.
- `POST /api/auth/privy/complete` (header `privy-id-token`),
  `POST /api/observability/mobile/{metrics,errors,events}` (web has its own
  `observability/{metrics,errors,events}` twins).
- Earn MAX: `smart-accounts/mobile/earn-max` (#845, #846) answers
  `summary`, `activity`, `context` (`features/earn-max/server/mobile.ts`).
  The app does not call it yet: no Earn MAX or Voltr code exists in
  `web/apps/mobile`.

### Auth

 A wallet-signed message `Loyal Mobile Earn / purpose / wallet /
issuedAt` travels in the JSON body (5 min window, 60 s skew,
`identity/server/mobile-wallet-auth.ts`). Its first use mints a 30-day
session JWT (POST `mobile/earn/session`, audience `loyal-mobile-earn`,
`identity/server/mobile-earn-session.ts`), which the app keeps in SecureStore
(`earn.session.v1`) and sends as `Authorization: Bearer`. The web cookie
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
- Without it the Principal routes are not registered, and those paths keep
  proxying to Next.
- Installing the credential on the host waits on the owner's go.

### Third parties, direct from the device

 Jupiter lite search and swap,
CoinGecko SOL price, Helius RPC, Magicblock PER (`lib/solana/rpc/constants.ts`).
These stay on the device and are not part of this plan.

### Types

 Response types are hand-copied on each side, for example
`EarnDepositPrepareResponse`, `EarnWithdrawPrepareResponse`,
`EarnAutodepositSetupStage`, `EarnTransactionItem`, `EarnTransactionKind`,
`EarnEarningsBar` and `EarnEarningsResponse`. They are defined in
`apps/web/src/lib/yield-optimization/*.shared.ts` and again in
`apps/mobile/src/lib/solana/earn/earn-api.ts`. Nothing in `web/packages`
holds them.

### Go today

 `loyal-web` serves `/api/earn/stats` natively and
reverse-proxies every other `/api/*` call to `app.askloyal.com`. The proxy
rewrites `Origin` and passes every other header through, `Authorization`
included (`internal/webapp/proxy.go`). It has no user auth. Go already holds
the Earn MAX readers (`internal/admin/earn_max.go`: vault, share price, APY,
NAV, history), `programs/voltr`, autodeposit and `earnstats`.

## Lies table

| Lie underneath | The one fix | Deleted |
|---|---|---|
| Mobile is a different API client from web | One path per operation for both clients, with the caller taken from the cookie or the bearer | `smart-accounts/mobile/earn/*` twins (5,918 LOC) once old builds age out; mobile-only auth branches |
| Each client owns the response shape | The server's schema owns it: Go structs → Huma OpenAPI → generated TS in `web/packages/api` (G7's generator); Next-served routes move their `*.shared.ts` contracts into a package | Hand-copied types in `earn-api.ts` |
| Mobile needs two backends | One base URL, `loyal-web`; Go serves the Telegram content reads from tables it already owns | `API_BASE_URL`; telegram `mobile/*`, `summaries` and `push-tokens` routes |
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
  `…/yield-optimization/earn-state`, and so on). Mobile moves to them. Go never
  implements a `/mobile/` path. Installed builds on older JS keep calling
  Next directly until they update.
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

The Go fixes for 1 to 3 land in a separate PR (G-money, branch
`mobile-go/g-earnmax-money`), and the Go reorder keying is proved there.

The browser fix for the same reorder bug in the wallet activity provider
(`web/packages/solana-wallet/src/providers/default-activity-provider.ts`) keys
each parsed transaction by its own `transaction.signatures[0]` and drops one
whose signature was not requested. Its sibling in the mobile app,
`web/apps/mobile/src/lib/solana/rpc/get-account-txn-history.ts:658-678`,
pairs by index too and is left to the mobile owner (outside this lane).

Eight tests in `web/packages/smart-account-vaults/src/client.test.ts` already
fail identically at 568b6632 and at d719f7b6: `prepareEarnUsdcDeposit` x5 and
`prepareEarnUsdcWithdraw` x3. Messages include "Cannot safely prepare an Earn
policy without scanning on-chain policies." and "Unable to find Settings
account at 11111111111111111111111111111112". Nothing is filed in Linear.

## Owner calls

1. Decided 2026-10-22 (M2): Go holds `AUTH_JWT_SECRET` until Vercel moves to
   RS256, then the secret is dropped.
2. Go to deploy `loyal-web` and to install the credentials and grants,
   including the `AUTH_JWT_SECRET` credential.
3. Go to flip the production mobile build's base URL, after the preview canary
   (M1). The build is Vlad's.
4. Decide whether Go may ever hold the deployment-policy signer (the
   follow-on plan for prepare and confirm).
5. RS256 on Vercel, then remove `AUTH_JWT_SECRET` from Go.
6. An Earn MAX mobile screen design (none exists).

## Unknown

- The Expo project's GitHub link, build credentials and OTA channel names.
  `eas.json` was read, the Expo dashboard was not.
- Which database connection Go admin uses for the Telegram tables, and whether
  `loyal-web` can hold it.
- How the refund flow builds its transaction (`refund.ts` beyond line 28) and
  the `wire.ts` serializers were not read.
- Whether the Go proxy carries the app's request bodies (confirm payloads
  hold several base64 transactions) inside its limits. This is untested and
  is M1's check.
- An Earn MAX screen design for mobile. None exists in the app. Figma and
  Linear were not searched.
