# Porting an Earn route to Go

Read this first. The web route (web/apps/web/src/app/api/...) and its JSON are the contract; Go
serves the same path with the same body and status, and both clients (web and mobile) call it.

## The file you own

One family file, `earn_<family>.go`, already in this package. Its header lists the paths and the
Next file each ports. Touch nothing else in `internal/webapp`; a shared change goes in your report.
Delete the header once every path in it is served. The family var at the top is the contract:

- `ready`: replace `notPorted` with what the ops need, e.g. `func(h earnRoutes) bool { return h.signedIn() && h.YieldDB != nil }`.
- `appGrants` / `yieldGrants`: every table your SQL touches, as `{"loyal_yield.table", "SELECT"}`.
  Add `EarnPolicyGrants...` when you call `activePolicyPair`. `CheckEarnGrants` checks them once
  at startup; a missing grant keeps the family proxied to Next.
- `register`: your `registerX(api)` with one `huma.Register` per path. It runs for `OpenAPI()`
  with a zero `earnRoutes`, so never touch a dependency outside the handler body.

## Principal and account

`account, err := h.account(ctx, in.SessionInput)` gives the wallet, settings PDA, smart account,
Earn vault PDA (index 1) and `account.Cluster` (`mainnet-beta`/`devnet`, as the yield DB stores it).
The request struct embeds `SessionInput` (bearer `loyal-mobile-earn` JWT or the web cookie). A
`walletAddress` query param or a signed message in the body is never identity: drop it.
`h.appUserID(ctx, wallet)` and `h.activePolicyPair(ctx, account)` mirror getOrCreateCurrentUser's
read and findActiveYieldRoutePolicyPair (L/yield-deposit-repository.server.ts:3050); nil pair = none.

## Errors

Web's `jsonError(status, code, message)` is `earnError(status, code, message)`: body
`{"error":{"code","message"}}`. Auth refusals from `h.account` are already that envelope
(401 `unauthenticated`, 409 `smart_account_required`, ...); pass `err` through
`earnUnavailable(step, err, code, message)`, which keeps a refusal and turns any other error into
a logged 503 with your code. Declare statuses with `errorResponses[routeError](api, 401, 409, 503)`.
A route whose error body has more fields (earnings) declares its own error type like `push.go`.

## Types

Response and request bodies are named exported Go structs; `bun scripts/web/openapi-types.ts`
turns them into `@loyal-labs/api` types. Lanes do NOT regenerate: the foreman runs it once after
merging. Clients import the generated type and delete their hand copy.

## Transactions

Build exactly what the TS builder builds (web/packages/smart-account-vaults/src/client.ts). Use
`internal/earnprep` for compute budget, idempotent ATA, v0 compile with lookup tables, Squads
execute and the policy signer key; reuse `internal/programs/*`. Prove bytes with
`scripts/web/earn-tx-parity.ts` (its header has the command) and a git-ignored
`earn_<family>_scratch_test.go` that builds the same input in Go and compares. Never send.
`earnprep.CompileV0` orders keys as web3.js `compileToV0Message` does; solana-go's
`NewTransaction` does not, so never use it for a message the parity harness checks.

## Signing

Only `earn_session.go` and `earn_privy.go` call `h.Session.Mint`; only `earn_sponsor.go` (the sponsored provisioning seam, `ensureSmartAccount`) calls
`h.Sponsor.Cosign`. `signer_test.go` fails on any other caller.

## Census

Delete your paths' lines from `scripts/web/earn-next.txt` once Go serves them (a line is
`<METHOD or *> <client path>`, mobile `/mobile/` twins included); the census fails on a stale,
new or missing line. A mobile twin line goes when the RN lane points the client at your path. Do not touch `web/apps/mobile/**`, `web/apps/web/src/**` or Next routes.

## Gates (wrap each in `nice -n 20 lockf -k /tmp/build.lock`, GOMAXPROCS=4 GOTOOLCHAIN=local)

    cd go/workers && go vet ./internal/webapp ./internal/earnprep && go test -race ./internal/webapp ./internal/earnprep
    bun scripts/web/routes-census.ts --check

`bun scripts/web/openapi-types.ts --check` fails once you register a path: expected, the foreman
regenerates after merging the lanes.
