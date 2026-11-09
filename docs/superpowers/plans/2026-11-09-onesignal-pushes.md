# OneSignal Pushes and Smarter Earnings Notifications Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Send every server push through OneSignal, and make the daily earnings push fire only when a wallet has at least $1 of new combined Earn + Earn MAX earnings, at most 4 times a week.

**Architecture:** `WalletPush` in `go/workers/internal/webapp` keeps its `send(ctx, wallet, payload)` shape. Its Expo body is replaced by one OneSignal REST call per wallet, with a category (Android channel), a delivery mode and an idempotency key. The `earn-yield-joy` cron reads Earn lifetime through the existing earnings read, and Earn MAX performance through `earnmax.Reader.Activity`, the read the Earn MAX tab uses. It decides with a pure function, then claims state with compare-and-swap before sending. A new cron exports confirmed Earn deposits and withdrawals to Mixpanel past a watermark. The mobile app gives the permission prompt and tap routing to OneSignal, in JS only.

**Tech Stack:** Go 1.x (`pgx/v5`, `huma/v2`, `google/uuid`), Postgres (`loyal_app` via drizzle, `loyal_yield`), OneSignal REST API, Mixpanel `/track` ingestion API, Expo / React Native (`react-native-onesignal` 5.2.14, `expo-router`), Jest.

**Spec:** `docs/superpowers/specs/2026-11-08-onesignal-pushes-design.md`. Read it before any task. "Decision N" below means that spec's decision N.

## Global Constraints

- Earnings threshold is a **fixed $1.00** of combined earnings since the last push. No decay, no tiers, no push under $1 (decision 2).
- Combined lifetime = Earn `Ranges.All.LifetimeEarnedUsd` + Earn MAX `Activity.Earnings.PerformanceUsd`. These are the same reads the two tabs show (decision 3, last anti-goal).
- Cap: at most **4** Earnings-category pushes per wallet in any rolling **7 days**, and at most **1** per UTC decision day. The cap counts only the earnings cron. Account alerts never wait for it and never count toward it (decision 5).
- Milestones are **$1, $100, $500, $1,000, $5,000**. `yield-first` is removed. Anniversaries stay, at 6 and 12 months, and need at least $1 lifetime. A milestone or anniversary replaces that day's digest (decision 6).
- Copy fixed by the spec:
  - Digest title `+$1.37 earned`, body `$48.20 earned with Loyal so far.`
  - $1 milestone title `$1 earned with Loyal`, body `Your first dollar, and it keeps going on its own.`
- All other copy in this plan is **proposed**. Rodion approves it verbatim in PR review before merge. Copy rules: no rate, no "more", no "risk-free", no "highest yield", no day names.
- OneSignal targeting: `include_aliases.external_id = [wallet]`, `target_channel: "push"`. Earnings pushes use `delayed_option: "last-active"`. Alerts are sent immediately (decisions 7, 8).
- Two Android categories. Their OneSignal channel IDs come from plain env `ONESIGNAL_EARNINGS_CHANNEL_ID` and `ONESIGNAL_ALERTS_CHANNEL_ID`. When an ID is unset, omit `android_channel_id` (decision 9).
- Secrets `ONESIGNAL_APP_ID`, `ONESIGNAL_REST_API_KEY` and `MIXPANEL_PROJECT_TOKEN` are `systemd-creds` read with `engine.OptionalCredential`. Never log them, and never put them in code, tests or commands.
- Mobile changes are JS-only and ship with `eas update`. No native config, plugin or dependency changes (decision 11).
- Commits use Conventional Commits, `type(scope): description`, with a subject under 100 characters. **No `Co-Authored-By` trailers** (repo AGENTS.md overrides the harness default).
- Before the first PR, rename the branch to the Linear issue (`ASK-NNN-onesignal-pushes`). Renaming a PR head branch later closes the PR.
- The builder never deploys and never touches loyal-db-primary. Rodion applies the migration and deploys (Task 10).

## Review Focus

1. **Earn MAX read fails for one wallet** (RPC timeout, history gap). The wallet must be skipped as `unavailable`, with no state write and no push. Pushing the Earn-only number would mis-baseline the wallet and later report its Earn MAX history as new earnings. Test in Task 4.
2. **Earn MAX performance falls after a push, then recovers.** No push until combined lifetime is at least $1 above the last pushed level, and none while combined lifetime is at or below 0. Test in Task 5.
3. **First run after deploy, on rows written by the old rule** (`rule_version = 1`, Earn MAX history not yet counted). Each row is re-baselined silently with no push. Milestones it has passed are marked sent. A push the old rule sent in the last 7 days still counts toward the cap. Test in Task 5.
4. **Mixpanel export after the cron was down for more than 5 days.** `/track` rejects events older than 5 days. The window must be clamped, so one stale batch can't 400 forever and freeze the watermark. Test in Task 8.
5. **A push whose `data.route` is not one we send** (a hand-made dashboard push, an old payload). The app must ignore it and open normally, never navigating to an arbitrary path. Test in Task 9.

---

## PR 1: OneSignal sender (Tasks 1–2)

### Task 1: OneSignal sender behind `WalletPush`

**Files:**
- Modify: `go/workers/internal/webapp/push_send.go` (replace everything Expo: `expoMessage`, `expoTicket`, `dispatch`, `deliver`, `expo`, `parseExpoTicket`, the `push_tokens` read)
- Modify: `go/workers/internal/webapp/autodeposit_push.go` (payload type, `send`)
- Modify: `go/workers/go.mod` (`github.com/google/uuid` becomes a direct dependency)
- Test: `go/workers/internal/webapp/push_send_test.go` (new)

**Interfaces:**
- Produces:
  - `type pushCategory int` with `pushCategoryAlerts pushCategory = iota` (zero value) and `pushCategoryEarnings`
  - `type pushDelivery int` with `pushDeliveryNow = iota` (zero value) and `pushDeliveryIntelligent`
  - `autodepositWalletPush` gains `Category pushCategory`, `Delivery pushDelivery`, `Route string` and `IdempotencyName string`. It keeps `Body` and `Title`.
  - `type OneSignalConfig struct { AppID, RESTAPIKey, EarningsChannelID, AlertsChannelID string }`
  - `func NewWalletPush(cfg OneSignalConfig, secret string) *WalletPush`
  - `func (p *WalletPush) sendToWallet(ctx context.Context, wallet string, msg autodepositWalletPush, data json.RawMessage) (sent bool, err error)`
  - `func (p *WalletPush) send(ctx context.Context, wallet string, payload autodepositWalletPush) bool`. It returns true only when OneSignal returned a notification id.
  - `func pushIdempotencyKey(name string) string`

- [ ] **Step 1: Write the failing tests** in `push_send_test.go`. Use an `httptest.Server`, and set `p.url = server.URL + "/notifications?c=push"`.

```go
func TestOneSignalEarningsRequestShape(t *testing.T) {
	// send(ctx, "W1", autodepositWalletPush{Title: "+$1.37 earned", Body: "$48.20 earned with Loyal so far.",
	//   Category: pushCategoryEarnings, Delivery: pushDeliveryIntelligent, Route: "/(tabs)/earn", IdempotencyName: "earnings:yield-digest:W1:2026-11-09"})
	// Assert on the captured request:
	//   method POST; header Authorization == "Key test-key"; Content-Type starts with "application/json"
	//   body.app_id == "test-app"; body.target_channel == "push"
	//   body.include_aliases.external_id == ["W1"]
	//   body.headings.en == "+$1.37 earned"; body.contents.en == "$48.20 earned with Loyal so far."
	//   body.data.route == "/(tabs)/earn"
	//   body.android_channel_id == "chan-earnings"; body.delayed_option == "last-active"
	//   body.idempotency_key == pushIdempotencyKey("earnings:yield-digest:W1:2026-11-09")
	// server answers 200 {"id":"n-1"}  => send returns true
}

func TestOneSignalAlertDefaultsAreImmediateAlerts(t *testing.T) {
	// zero-value Category/Delivery, no Route:
	//   android_channel_id == "chan-alerts"; "delayed_option" key absent; "data" key absent
	//   idempotency_key parses with uuid.Parse (random when IdempotencyName is empty)
}

func TestOneSignalOmitsChannelWhenUnset(t *testing.T) {
	// EarningsChannelID == "" => "android_channel_id" key absent
}

func TestOneSignalNotSubscribedIsNotSent(t *testing.T) {
	// 200 {"id":"","errors":["All included players are not subscribed"]} => send false, sendToWallet (false, nil)
}

func TestOneSignalRetriesOnceWithSameKey(t *testing.T) {
	// first response 503, second 200 {"id":"n-2"} => true; exactly 2 requests; both idempotency_key equal
	// 400 => no retry, sendToWallet returns an error whose text does not contain "test-key"
}

func TestPushIdempotencyKeyIsStableUUID(t *testing.T) {
	// pushIdempotencyKey("a") == pushIdempotencyKey("a"); != pushIdempotencyKey("b"); uuid.Parse succeeds
}
```

- [ ] **Step 2: Run the tests and confirm they fail.**
  Run: `cd go/workers && go test ./internal/webapp -run 'TestOneSignal|TestPushIdempotencyKey' -v`
  Expected: FAIL to compile (`OneSignalConfig` and `pushIdempotencyKey` undefined).

- [ ] **Step 3: Implement.**
  - Endpoint const `oneSignalSendURL = "https://api.onesignal.com/notifications?c=push"`. Header `Authorization: Key <RESTAPIKey>`.
  - `pushIdempotencyKey`: when the name is non-empty, use `uuid.NewSHA1` over a fixed package namespace UUID (a literal constant). When it is empty, use `uuid.NewString()`.
  - `data`: debug-send passes raw `data`. Otherwise, `{"route": Route}` only when `Route != ""`.
  - Retry: one retry after 1s, only on a transport error, 429 or 5xx, reusing the same body.
  - Read at most 1 MiB of the response. An error message holds the status and the body, never the key.
  - Raise `autodepositPushSendTimeout` to 10s so the retry fits.
  - Delete the Expo code and the `push_tokens` read. `WalletPush` keeps `Secret` and `HTTP` and gains `cfg OneSignalConfig` and `url string`.
  - `send` logs a failure as today (`[wallet-push] push send failed`). It logs not-subscribed at Info, with the wallet only.

- [ ] **Step 4: Run the tests and confirm they pass.**
  Run: `cd go/workers && go test ./internal/webapp -run 'TestOneSignal|TestPushIdempotencyKey' -v`
  Expected: PASS. Task 1 updates the debug-send handler in `push_send.go` to the new `sendToWallet` signature, so `./internal/webapp` builds. `./cmd/loyal-web` still calls the old `NewWalletPush` until Task 2, so run only `./internal/webapp` here. Tasks 1 and 2 land in the same PR.

- [ ] **Step 5: Commit.**

```bash
git add go/workers/internal/webapp/push_send.go go/workers/internal/webapp/autodeposit_push.go go/workers/internal/webapp/push_send_test.go go/workers/go.mod go/workers/go.sum
git commit -m "feat(push): send wallet pushes through OneSignal instead of Expo"
```

### Task 2: Wire OneSignal, categorize every call site, retire two campaigns

**Files:**
- Modify: `go/workers/cmd/loyal-web/main.go:143,161,299-313,346-353` (`newWalletPush`, remove the `PushSend` verdict)
- Modify: `go/workers/cmd/loyal-web/cron.go:53-56`
- Modify: `go/workers/internal/webapp/grants.go:86-93` (delete `PushSendGrants`)
- Modify: `go/workers/internal/webapp/push_send.go` (debug-send handler)
- Modify: `go/workers/internal/webapp/push_campaigns.go:67-88,312,361`
- Modify: `go/workers/internal/webapp/earn_yield_joy.go:378` (category only; the rule changes are PR 2)
- Modify: `deploy/hetzner/README.md` (credentials table rows for `PUSH_DEBUG_SECRET` and the joy/push paragraphs near line 595)
- Test: `go/workers/internal/webapp/push_send_test.go`, `go/workers/cmd/loyal-web/main_test.go` (fix any `PushSend` references)

**Interfaces:**
- Consumes: Task 1's `NewWalletPush`, `OneSignalConfig`, the `autodepositWalletPush` fields, and `send` returning a bool.
- Produces:
  - `func newWalletPush() *webapp.WalletPush` in `cmd/loyal-web`. It returns nil, logging `degraded("wallet push", …)`, when `ONESIGNAL_APP_ID` or `ONESIGNAL_REST_API_KEY` is missing.
  - `func (h earnRoutes) claimFailurePush(ctx context.Context, wallet string, rawKey any) (claimed bool, key string)`. Today it returns only the bool.
  - `debug-send` answers `{"success":true,"sentTo":1}` when sent. When the wallet has no subscription it answers 404 with the unchanged message `No push tokens registered for this wallet`, kept for callers.

- [ ] **Step 1: Write the failing tests.**

```go
func TestDebugSendGoesThroughOneSignalAsAlert(t *testing.T) {
	// register the route on a humatest API with a WalletPush pointed at httptest; POST with
	// "Authorization: Bearer s3cret" {"walletPublicKey":"W1","title":"t","body":"b","data":{"k":"v"}}
	// => 200 {"success":true,"sentTo":1}; captured android_channel_id == "chan-alerts"; no delayed_option; data.k == "v"
	// server answers {"id":""} => 404 {"error":"No push tokens registered for this wallet","sentTo":0}
}

func TestRetiredCohortCampaignsAreUnknown(t *testing.T) {
	// _, ok := mixpanelCohortCampaigns["earning-while-away"] => false; same for "still-earning"
	// "quest-1-nudge", "quest-2-nudge", "seeker-summer-ending" still present
}

func TestCohortAndSweepPushesAreImmediateAlerts(t *testing.T) {
	// for every payload in mixpanelCohortCampaigns and autodepositSweepFailedPush(0|5_000_000),
	// autodepositEnabledPush(), autodepositSweepScheduledPush(1): Category == pushCategoryAlerts, Delivery == pushDeliveryNow
}
```

- [ ] **Step 2: Run the tests and confirm they fail.**
  Run: `cd go/workers && go test ./internal/webapp -run 'TestDebugSend|TestRetiredCohort|TestCohortAndSweep' -v`
  Expected: FAIL (`earning-while-away` still present; debug-send still reads `push_tokens`).

- [ ] **Step 3: Implement.**
  - Remove `earning-while-away` and `still-earning` from `mixpanelCohortCampaigns`.
  - Cohort sends set `IdempotencyName: "cohort:"+campaign+":"+wallet`. The failed-sweep send sets `"autodeposit-failed:"+key+":"+wallet`, where `key` is the claim key `claimFailurePush` already computes; return it from that function.
  - The joy send sets `Category: pushCategoryEarnings, Delivery: pushDeliveryIntelligent`.
  - `newWalletPush()` reads the two credentials and `os.Getenv` for the two channel IDs, plus the existing `PUSH_DEBUG_SECRET`.
  - `run()` sets `cfg.WalletPush = newWalletPush()` with no grants condition. `cron.go` sets `CampaignPush` when it is non-nil.
  - Delete `PushSendGrants` and `grantVerdicts.PushSend`.
  - README:
    - Replace "push-send grants" wording with the two OneSignal credentials (files `loyal-web-onesignal-app-id` and `loyal-web-onesignal-rest-api-key`, on `loyal-web` and `loyal-web-cron@`), plus the two channel-ID env lines.
    - Say that without them all wallet pushes are off and the joy cron refuses to run.

- [ ] **Step 4: Run the package tests, vet and build.**
  Run: `cd go/workers && go vet ./cmd/loyal-web ./internal/webapp && go test -race ./cmd/loyal-web ./internal/webapp`
  Expected: PASS, no vet findings.

- [ ] **Step 5: Commit.**

```bash
git add go/workers deploy/hetzner/README.md
git commit -m "feat(push): categorize every push and retire the earnings cohort campaigns"
```

---

## PR 2: Earnings rule, Earn MAX, Mixpanel (Tasks 3–8)

### Task 3: App DB migration (cap times, rule version, export watermarks)

**Files:**
- Modify: `web/packages/db-core/src/schema.ts:605-628` (`earnYieldPushState`), plus a new table after it
- Create: `web/apps/telegram/drizzle/0032_earn_push_cap.sql` and its `meta/` snapshot and journal entry (generated)

**Interfaces:**
- Produces these columns and table, used by Tasks 6 and 8:
  - `earn_yield_push_state.recent_pushed_at timestamptz[] NOT NULL DEFAULT '{}'`
  - `earn_yield_push_state.rule_version smallint NOT NULL DEFAULT 1`
  - table `analytics_export_watermarks (stream text PRIMARY KEY, confirmed_at timestamptz NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`

- [ ] **Step 1: Edit `schema.ts`.**
  - `recentPushedAt: timestamp("recent_pushed_at", { withTimezone: true }).array().notNull().default(sql\`'{}'::timestamptz[]\`)`
  - `ruleVersion: smallint("rule_version").notNull().default(1)`
  - `export const analyticsExportWatermarks = pgTable("analytics_export_watermarks", { … })`, with the three columns above.

- [ ] **Step 2: Generate the migration.**
  Run: `cd web/apps/telegram && bunx drizzle-kit generate --name earn_push_cap`
  Expected: one new `0032_earn_push_cap.sql` holding exactly two `ALTER TABLE "earn_yield_push_state" ADD COLUMN` statements and one `CREATE TABLE "analytics_export_watermarks"`. If drizzle emits anything else (a diff from an older drift), stop and report it. Don't hand-edit it away.

- [ ] **Step 3: Check that the packages still build.**
  Run: `cd web && bun run build:packages`
  Expected: success.

- [ ] **Step 4: Commit.**

```bash
git add web/packages/db-core/src/schema.ts web/apps/telegram/drizzle
git commit -m "feat(db): add earnings push cap columns and analytics export watermarks"
```

### Task 4: Combined earnings read (Earn + Earn MAX) and route choice

**Files:**
- Create: `go/workers/internal/webapp/earn_yield_joy_earnings.go` (move `lifetimeEarned` here and rename it)
- Modify: `go/workers/internal/webapp/crons.go:19-22` (`Crons` fields)
- Modify: `go/workers/cmd/loyal-web/cron.go` (build the Earn MAX reader)
- Test: `go/workers/internal/webapp/earn_yield_joy_test.go` (new)

**Interfaces:**
- Consumes: `earnmax.NewReader(client *chain.Client, yield earnmax.Querier) *earnmax.Reader` and `(*earnmax.Reader).Activity(ctx, settings solana.PublicKey) (earnmax.Activity, error)`. `Activity.Earnings` is a `*Earnings` with a `PerformanceUsd` field. `Activity.Performance` is a `[]PerformancePoint` whose last entry's `EquityUsd` is the current position value.
- Produces:
  - `type joyEarnings struct { earnLifetimeUSD, earnPrincipalUSD, earnMaxPerformanceUSD, earnMaxEquityUSD float64 }`
  - `func (e joyEarnings) lifetimeUSD() float64` returns `earnLifetimeUSD + earnMaxPerformanceUSD`
  - `func (e joyEarnings) route() string` returns `"/(tabs)/earn-max"` when `earnPrincipalUSD <= 0 && earnMaxEquityUSD > 0`, else `"/(tabs)/earn"`
  - `Crons.Earnings func(ctx context.Context, cluster, settings, wallet string) (joyEarnings, bool, error)` (test seam; the return type changes)
  - `Crons.EarnMax *earnmax.Reader`
  - `func (c Crons) walletEarnings(ctx context.Context, cluster string, candidate joyCandidate) (joyEarnings, bool, error)`
  - `func earnMaxEarnings(activity earnmax.Activity) (performanceUSD, equityUSD float64, available bool)`

- [ ] **Step 1: Write the failing tests.**

```go
func TestJoyEarningsCombinesAndRoutes(t *testing.T) {
	e := joyEarnings{earnLifetimeUSD: 40, earnPrincipalUSD: 1000, earnMaxPerformanceUSD: -2.5, earnMaxEquityUSD: 300}
	// e.lifetimeUSD() == 37.5; e.route() == "/(tabs)/earn"
	// joyEarnings{earnMaxPerformanceUSD: 3, earnMaxEquityUSD: 300}.route() == "/(tabs)/earn-max"
	// joyEarnings{}.route() == "/(tabs)/earn"
}

func TestEarnMaxEarningsAvailability(t *testing.T) {
	// Activity{Earnings: &Earnings{PerformanceUsd: 4.2}, Performance: [{EquityUsd: 0}, {EquityUsd: 310}]} => (4.2, 310, true)
	// Activity{Earnings: nil, Performance: [{EquityUsd: 0}]}  => (0, 0, true)   // never held Earn MAX
	// Activity{Earnings: nil, Performance: []}                => (0, 0, true)
	// Activity{Earnings: nil, Performance: [{EquityUsd: 310}]} => (_, _, false) // holds it but earnings unknown
}

// Review Focus 1
func TestWalletEarningsUnavailableWhenEarnMaxFails(t *testing.T) {
	// Crons with EarnMax == nil and Earnings == nil => walletEarnings returns an error naming Earn MAX, never Earn-only numbers
}
```

- [ ] **Step 2: Run the tests and confirm they fail.**
  Run: `cd go/workers && go test ./internal/webapp -run 'TestJoyEarnings|TestEarnMaxEarnings|TestWalletEarnings' -v`
  Expected: FAIL to compile.

- [ ] **Step 3: Implement.**
  - `walletEarnings` uses the `Earnings` hook when it is set. Otherwise it does the existing Earn read (`readEarningsRangeSetResuming`, `All.LifetimeEarnedUsd` and `All.PrincipalUsd`), then `c.EarnMax.Activity(ctx, solana.MustPublicKeyFromBase58(...))`. Parse with `solana.PublicKeyFromBase58` and return the error; don't panic.
  - An `earningsUnavailableError` from Earn, or `available == false` from Earn MAX, returns `(joyEarnings{}, false, nil)`. An Earn MAX call error returns `(…, false, nil)` and logs a warning with the wallet. A nil `c.EarnMax` returns an error.
  - In `cron.go`, for `earn-yield-joy`, require `rpcClient != nil` (return `errors.New("earn-yield-joy needs WEB_SOLANA_RPC_URL for Earn MAX")`), then set `crons.EarnMax = earnmax.NewReader(rpcClient, yieldDB)`.

- [ ] **Step 4: Run the tests and confirm they pass.** Same command. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add go/workers/internal/webapp/earn_yield_joy_earnings.go go/workers/internal/webapp/earn_yield_joy_test.go go/workers/internal/webapp/crons.go go/workers/cmd/loyal-web/cron.go
git commit -m "feat(earn-joy): count Earn MAX performance in the earnings push total"
```

### Task 5: New push rule and copy (pure)

**Files:**
- Modify: `go/workers/internal/webapp/earn_yield_joy.go:22-187` (constants, `joyState`, `joyDecision`, `joyQualifiedCampaigns`, `selectJoyPush`, `joyPushPayload`; delete `joyDigestThresholdUSD(days)` and everything `joyKindFirst`)
- Test: `go/workers/internal/webapp/earn_yield_joy_test.go`

**Interfaces:**
- Consumes: Task 4's `joyEarnings.route()`.
- Produces:
  - Constants: `joyRuleVersion = 2`, `joyThresholdUSD = 1.0`, `joyWeeklyCap = 4`, `joyCapWindow = 7 * 24 * time.Hour`. `joyMilestonesUSD = [...]float64{1, 100, 500, 1000, 5000}`. Campaign names stay `"yield-total-"+jsNumberString(m)`.
  - `joyState` gains `recentPushedAt []time.Time` and `ruleVersion int`.
  - `joyDecision.kind` gains `joyDecisionRebaseline = "rebaseline"`. `joyDecision` gains `recentPushedAt []time.Time`, the value to store.
  - `joyPush` gains `lifetimeUSD float64` and `route string`.
  - `func selectJoyPush(accountAgeDays, lifetimeUSD float64, now time.Time, state *joyState) joyDecision` (same name, new rules)
  - `func joyPushPayload(push joyPush, now time.Time, wallet string) autodepositWalletPush`
  - `func formatUSDWhole(amount float64) string` (`5000` gives `"$5,000"`)

Rule order inside `selectJoyPush`:
1. `state == nil` gives **seed**: campaigns = qualified, `recentPushedAt = []`.
2. `state.ruleVersion < joyRuleVersion` gives **rebaseline**: campaigns = union(state.sentCampaigns, qualified). `recentPushedAt` = `[state.lastPushedAt]` when it is within `joyCapWindow` of now, else `[]`. No push.
3. `lifetimeUSD <= 0` gives **none**.
4. Any `recentPushedAt` with the same UTC date as `now`, or `joyWeeklyCap` or more entries within `joyCapWindow`, gives **none**.
5. Otherwise, in this order: the highest pending milestone, then a pending anniversary (needs `lifetimeUSD >= 1`), then the digest when `lifetimeUSD - lastPushedEarnedUSD >= joyThresholdUSD`. Each gives **push**, with `recentPushedAt` = entries inside the window plus `now`.

`joyQualifiedCampaigns` keeps today's shape: only the single highest milestone reached qualifies, so lower ones can never fire later as a burst.

- [ ] **Step 1: Write the failing tests** (table-driven; `now := time.Date(2026, 11, 9, 15, 41, 0, 0, time.UTC)`).

```go
func TestSelectJoyPushRule(t *testing.T) {
	// v2 := func(baseline float64, sent []string, recent ...time.Time) *joyState { ruleVersion: 2, lastPushedEarnedUSD: baseline, sentCampaigns: sent, recentPushedAt: recent }
	// cases (accountAgeDays, lifetime, state) => want:
	//   new wallet, 37.2, nil => seed, campaigns ["yield-total-1"], recent []
	//   0.99 above baseline:  20.99, v2(20, ["yield-total-1"]) => none
	//   exactly 1.00 above:   21.00, v2(20, ["yield-total-1"]) => push digest, amount 1.00, lifetime 21.00
	//   first dollar:         1.20,  v2(0.10, []) => push milestone "yield-total-1"
	//   jump past two milestones: 520, v2(90, ["yield-total-1"]) => push "yield-total-500" (never 100 later: qualified has only 500)
	//   anniversary needs $1: age 200, 0.80, v2(0, []) => none; age 200, 3, v2(1.5, ["yield-total-1"]) => push "yield-six-months"
	//   same UTC day cap:     30, v2(20, ["yield-total-1"], now.Add(-2*time.Hour)) => none
	//   yesterday is fine:    30, v2(20, ["yield-total-1"], now.Add(-23*time.Hour)) => push digest
	//   weekly cap:           30, v2(20, [..], now-1d, now-2d, now-3d, now-4d) => none
	//   cap window rolls:     30, v2(20, [..], now-1d, now-2d, now-3d, now-8d) => push; stored recent == [now-1d, now-2d, now-3d, now]
}

// Review Focus 2
func TestSelectJoyPushHighWaterMark(t *testing.T) {
	// baseline 50 (last push), Earn MAX drop => lifetime 47 => none (and no state change)
	// recovery to 50.90 => none; 51.00 => push digest amount 1.00
	// lifetime -3 with baseline 0 => none, even with an anniversary due
}

// Review Focus 3
func TestSelectJoyPushRebaselinesOldRows(t *testing.T) {
	// state{ruleVersion: 1, lastPushedEarnedUSD: 12, sentCampaigns: ["yield-first","yield-total-10"], lastPushedAt: now-3d}
	// lifetime 640 (Earn 12 + Earn MAX 628) =>
	//   kind rebaseline; campaigns ⊇ {"yield-first","yield-total-10","yield-total-500"}; recent == [now-3d]
	// same with lastPushedAt now-9d => recent == []
}

func TestJoyPushPayloadCopy(t *testing.T) {
	// digest {amount 1.37, lifetime 48.2, route "/(tabs)/earn"} =>
	//   Title "+$1.37 earned", Body "$48.20 earned with Loyal so far.", Category earnings, Delivery intelligent, Route "/(tabs)/earn"
	//   IdempotencyName "earnings:yield-digest:W1:2026-11-09"
	// milestone 1 => Title "$1 earned with Loyal", Body "Your first dollar, and it keeps going on its own."
	// milestone 5000, lifetime 5012.4 => Title "$5,000 earned with Loyal", Body "And it keeps going on its own."   // PROPOSED copy
	// anniversary 6 => Title "Six months with Loyal", Body "$48.20 earned since you joined."            // PROPOSED copy
	// anniversary 12 => Title "A year with Loyal"                                                     // PROPOSED copy
	// formatUSDWhole(1000) == "$1,000"; formatUSDWhole(1) == "$1"
}
```

- [ ] **Step 2: Run the tests and confirm they fail.**
  Run: `cd go/workers && go test ./internal/webapp -run 'TestSelectJoyPush|TestJoyPushPayload' -v`
  Expected: FAIL.

- [ ] **Step 3: Implement the rule and copy above** in `earn_yield_joy.go`. `processJoyCandidate` keeps compiling against the new signatures; its state writes change in Task 6.

- [ ] **Step 4: Run the tests and confirm they pass.** Same command. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add go/workers/internal/webapp/earn_yield_joy.go go/workers/internal/webapp/earn_yield_joy_test.go
git commit -m "feat(earn-joy): push on a fixed \$1 with a 4-per-week cap and new milestones"
```

### Task 6: State persistence, claim and cron wiring

**Files:**
- Modify: `go/workers/internal/webapp/earn_yield_joy.go:253-379` (`joyStates`, `claimJoyState`, `processJoyCandidate`, `EarnYieldJoy`)
- Test: `go/workers/internal/webapp/earn_yield_joy_db_test.go` (new, opt-in DB test)
- Modify: `.github/workflows/workers-v2-verify.yml:99-126,191` (add the DB and env var, and the test name to the "ran and were not skipped" list)

**Interfaces:**
- Consumes: Task 3's columns, Task 4's `walletEarnings`, and Task 5's `selectJoyPush` and `joyPushPayload`.
- Produces:
  - `func (c Crons) claimJoyState(ctx context.Context, wallet string, previous *joyState, decision joyDecision, now time.Time, lifetimeUSD float64) (bool, error)`
  - `EarnYieldJoySummary` gains `Rebaselined int`, JSON `rebaselined`.
  - `EarnYieldJoyOutcome` gains `Route string`, JSON `route,omitempty`.

Writes per decision:

| Decision | `last_pushed_at` | `last_pushed_earned_usd` | `sent_campaigns` | `recent_pushed_at` | `rule_version` |
|---|---|---|---|---|---|
| seed (INSERT … ON CONFLICT DO NOTHING) | now | lifetime | qualified | `{}` | 2 |
| rebaseline (UPDATE, CAS) | unchanged | lifetime | union | decision value | 2 |
| push (UPDATE, CAS) | now | lifetime | + campaign unless digest | decision value | 2 |

The CAS `WHERE` clause adds `AND rule_version = $N` to today's `sent_campaigns` and `last_pushed_at` match. A push is sent only when the claim returns true, as today.

- [ ] **Step 1: Write the failing DB test.** Follow the `internal/earnmax/earnings_db_test.go` guard. The env var is `EARN_JOY_TEST_DATABASE_URL`, the host must be `127.0.0.1`, the database name must start with `earn_joy_test`, otherwise skip. Create `earn_yield_push_state` inline, matching `schema.ts` after Task 3.

```go
func TestEarnJoyClaimIsCompareAndSwap(t *testing.T) {
	// seed W1 at lifetime 10 => row rule_version 2, recent {}
	// two claims racing from the same read state with a push decision => exactly one returns true
	// a claim whose previous.ruleVersion is 1 against a rule_version 2 row => false
	// rebaseline write keeps last_pushed_at unchanged and sets recent_pushed_at to the decision value
}

func TestEarnJoyRunSkipsUnavailableWallet(t *testing.T) {
	// Crons with Earnings hook returning (joyEarnings{}, false, nil) for W2 => outcome "unavailable", no row for W2
}
```

- [ ] **Step 2: Run the test locally against a disposable database and confirm it fails.**
  Run: `createdb -h 127.0.0.1 earn_joy_test && cd go/workers && EARN_JOY_TEST_DATABASE_URL=postgres://127.0.0.1/earn_joy_test?sslmode=disable go test ./internal/webapp -run 'TestEarnJoy' -count=1 -v`
  Expected: FAIL (column `rule_version` not written / signature mismatch).

- [ ] **Step 3: Implement.**
  - `joyStates` selects the two new columns.
  - `processJoyCandidate` handles `rebaseline` like `seed`: a write, no push, status `"rebaselined"`, counted in the summary's `switch`. It passes `earnings.route()` into the push, and sets `outcome.Route`.
  - `EarnYieldJoy` returns an error when `c.Earnings == nil && c.EarnMax == nil`.
  - CI: add `earn_joy_test` to the `for db in …` loop, add `EARN_JOY_TEST_DATABASE_URL: postgres://postgres:postgres@127.0.0.1:5432/earn_joy_test?sslmode=disable` to the job env, and add both test names to `tests=`.

- [ ] **Step 4: Run the tests and confirm they pass.** Run the same command, then `go test -race ./internal/webapp`. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add go/workers/internal/webapp/earn_yield_joy.go go/workers/internal/webapp/earn_yield_joy_db_test.go .github/workflows/workers-v2-verify.yml
git commit -m "feat(earn-joy): store push times for the weekly cap and re-baseline old rows"
```

### Task 7: Mixpanel tracker and `Earnings Push Sent`

**Files:**
- Create: `go/workers/internal/webapp/mixpanel_track.go`
- Modify: `go/workers/internal/webapp/crons.go` (field), `go/workers/internal/webapp/earn_yield_joy.go` (emit), `go/workers/cmd/loyal-web/cron.go` (credential)
- Modify: `deploy/hetzner/README.md` (`MIXPANEL_PROJECT_TOKEN`, file `loyal-web-mixpanel-project-token`, cron unit only)
- Test: `go/workers/internal/webapp/mixpanel_track_test.go`

**Interfaces:**
- Produces:
  - `type mixpanelEvent struct { Name, DistinctID, InsertID string; Time time.Time; Properties map[string]any }`
  - `func NewMixpanelTracker(token string) *MixpanelTracker`. It returns nil for an empty token. Its URL is `https://api.mixpanel.com/track?strict=1`, and it posts in batches of 50.
  - `func (m *MixpanelTracker) track(ctx context.Context, events []mixpanelEvent) error`. A nil receiver is a no-op returning nil.
  - `Crons.Mixpanel *MixpanelTracker`

- [ ] **Step 1: Write the failing tests.**

```go
func TestMixpanelTrackBody(t *testing.T) {
	// one event {Name "Earnings Push Sent", DistinctID "mob:W1", InsertID "earnings:yield-digest:W1:2026-11-09", Time 2026-11-09T15:41Z,
	//   Properties {"type":"digest","campaign":"yield-digest","amount_usd":1.37,"lifetime_usd":48.2,"route":"/(tabs)/earn"}}
	// request body is a JSON array; [0].event == "Earnings Push Sent";
	// [0].properties: token == "tok", distinct_id == "mob:W1", $insert_id == InsertID, time == 1794238860000 (ms), amount_usd == 1.37
	// 120 events => 3 requests (50, 50, 20); a 400 response => error, and later batches are not sent
}

func TestEarnJoyEmitsOnlyAcceptedPushes(t *testing.T) {
	// run earnYieldJoy (dryRun false) with Earnings hook, a WalletPush on httptest answering {"id":"n"} for W1 and {"id":""} for W2,
	// and a tracker on httptest => one "Earnings Push Sent" with distinct_id "mob:W1"; none for W2; dry run sends none
	// (uses EARN_JOY_TEST_DATABASE_URL like Task 6; skipped without it; add the name to the CI list)
}
```

- [ ] **Step 2: Run the tests and confirm they fail.**
  Run: `cd go/workers && go test ./internal/webapp -run 'TestMixpanelTrack|TestEarnJoyEmits' -v`
  Expected: FAIL to compile.

- [ ] **Step 3: Implement.**
  - The joy run collects an event after each `send` that returns true, then calls `c.Mixpanel.track` once at the end of a non-dry run. A track error is logged (`[earn-yield-joy] mixpanel track failed`) and does not fail the run.
  - The event's `InsertID` is the push's `IdempotencyName`.
  - `cron.go` reads `MIXPANEL_PROJECT_TOKEN` with `engine.OptionalCredential`. When it is absent, log `degraded("earnings analytics", …)`.

- [ ] **Step 4: Run the tests and confirm they pass.** Same command. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add go/workers deploy/hetzner/README.md .github/workflows/workers-v2-verify.yml
git commit -m "feat(earn-joy): report sent earnings pushes to Mixpanel"
```

### Task 8: Earn deposit and withdrawal export cron

**Files:**
- Create: `go/workers/internal/webapp/earn_events_export.go`
- Modify: `go/workers/cmd/loyal-web/cron.go` (new `case "earn-events-mixpanel"` and its usage string)
- Create: `deploy/hetzner/systemd/loyal-web-cron-earn-events-mixpanel.timer` (copy the joy timer; `OnCalendar=*:0/15`, `RandomizedDelaySec=60`)
- Modify: `deploy/hetzner/README.md` (the new timer, the watermark table grant)
- Test: `go/workers/internal/webapp/earn_events_export_test.go`

**Interfaces:**
- Consumes: Task 3's `analytics_export_watermarks` and Task 7's `MixpanelTracker`.
- Produces:
  - `func (c Crons) EarnEventsMixpanel(ctx context.Context, dryRun bool) (EarnEventsSummary, error)`. It runs under `withRunLock(…, "earn-events-mixpanel", …)`.
  - `type EarnEventsSummary struct { Deposits, Withdrawals int; Seeded, DryRun bool; From, To time.Time }`
  - `func exportWindowStart(watermark, now time.Time) time.Time` returns `max(watermark-1h, now-4d)`

Behaviour:
- Two streams: `earn-deposits` and `earn-withdrawals`.
- **No watermark row:** insert `confirmed_at = now()` and emit nothing (`Seeded = true`). There is no history backfill.
- **Otherwise:** read rows with `vault_index = earnprep.EarnVaultIndex AND confirmed_at > exportWindowStart(wm, now) AND confirmed_at <= now()`, ordered by `confirmed_at, id`, limit 2000.
  - Deposit event: `Earn Deposit Confirmed` with `amount_usd = principal_amount_raw/1e6`, `source = "autodeposit"` when `balance_sweep_execution_id IS NOT NULL` else `"manual"`, and `deposit_id`. `$insert_id` is `earn-deposit:<id>`.
  - Withdrawal event: `Earn Withdrawal Confirmed` with `amount_usd = withdrawn_amount_raw/1e6`, `source = "manual"`, `source_type` (the column as-is) and `withdrawal_id`. `$insert_id` is `earn-withdrawal:<id>`.
  - Every event: `distinct_id = "mob:"+wallet_address`, `time = confirmed_at`.
- Advance a stream's watermark to the max `confirmed_at` it sent, and only after `track` succeeds. The one-hour overlap, deduplicated by `$insert_id`, covers rows that are confirmed slightly out of order.
- The cron needs `WEB_YIELD_DATABASE_URL` and `MIXPANEL_PROJECT_TOKEN`. When either is missing it returns an error.

- [ ] **Step 1: Write the failing tests.**

```go
// Review Focus 4
func TestExportWindowStartClampsToTrackLimit(t *testing.T) {
	// now 2026-11-09T12:00Z; wm 2026-11-09T11:30Z => 2026-11-09T10:30Z; wm 2026-10-30T00:00Z => 2026-11-05T12:00Z
}

func TestEarnEventsExportSeedsThenExports(t *testing.T) {
	// opt-in DB test (EARN_JOY_TEST_DATABASE_URL); creates loyal_yield.user_yield_position_deposits/withdrawals with the
	// columns this query reads, plus analytics_export_watermarks
	// first run: Seeded true, 0 events, watermark row exists
	// insert deposit id 7 (sweep id set, 25_000_000 raw, vault EarnVaultIndex) and one in another vault =>
	//   one "Earn Deposit Confirmed" {amount_usd 25, source "autodeposit", $insert_id "earn-deposit:7"}; the other vault ignored
	// tracker answering 400 => watermark unchanged, run returns error
}
```

- [ ] **Step 2: Run the tests and confirm they fail.**
  Run: `cd go/workers && EARN_JOY_TEST_DATABASE_URL=postgres://127.0.0.1/earn_joy_test?sslmode=disable go test ./internal/webapp -run 'TestExportWindow|TestEarnEventsExport' -count=1 -v`
  Expected: FAIL to compile.

- [ ] **Step 3: Implement it as specified above.** Add the DB test name to the CI list.

- [ ] **Step 4: Run the tests, plus a dry run with no DB.**
  - Run: the same command, then `go test -race ./internal/webapp ./cmd/loyal-web`. Expected: PASS.
  - Run: `go run ./cmd/loyal-web cron earn-events-mixpanel --dry-run`. Expected: exits non-zero with `cron needs WEB_APP_DATABASE_URL`, which proves the subcommand is wired.

- [ ] **Step 5: Commit.**

```bash
git add go/workers deploy/hetzner .github/workflows/workers-v2-verify.yml
git commit -m "feat(analytics): export confirmed Earn deposits and withdrawals to Mixpanel"
```

---

## PR 3: Mobile (Task 9)

### Task 9: OneSignal permission prompt and tap routing

**Files:**
- Create: `web/apps/mobile/src/services/notification-route.ts`
- Modify: `web/apps/mobile/src/services/onesignal.ts`
- Modify: `web/apps/mobile/src/components/PushTokenRegistrar.tsx`
- Modify: `web/apps/mobile/src/services/notifications.ts` (delete `registerForPushNotifications` and `registerPushToken`; keep `setupNotificationHandler`)
- Modify: `web/apps/mobile/app/_layout.tsx:52` (attach the click listener after `initOneSignal()`)
- Test: `web/apps/mobile/src/services/__tests__/notification-route.test.ts`

**Interfaces:**
- Consumes: the server's `data.route` values `"/(tabs)/earn"` and `"/(tabs)/earn-max"` (Task 4).
- Produces:
  - `export function notificationRoute(data: unknown): "/(tabs)/earn" | "/(tabs)/earn-max" | null`
  - `export async function requestOneSignalPermission(): Promise<boolean>`, which calls `OneSignal.Notifications.requestPermission(true)` after init has settled and returns false when the module is missing
  - `export async function addOneSignalClickListener(onRoute: (route: string) => void): Promise<(() => void) | null>`

- [ ] **Step 1: Write the failing test.**

```ts
// Review Focus 5
describe("notificationRoute", () => {
  it("accepts only the two earnings routes", () => {
    expect(notificationRoute({ route: "/(tabs)/earn" })).toBe("/(tabs)/earn");
    expect(notificationRoute({ route: "/(tabs)/earn-max" })).toBe("/(tabs)/earn-max");
  });
  it("ignores anything else", () => {
    for (const data of [undefined, null, {}, { route: "/settings" }, { route: "https://evil.example" }, { route: 42 }, "x"]) {
      expect(notificationRoute(data)).toBeNull();
    }
  });
});
```

- [ ] **Step 2: Run the test and confirm it fails.**
  Run: `cd web/apps/mobile && bunx jest src/services/__tests__/notification-route.test.ts`
  Expected: FAIL (module not found).

- [ ] **Step 3: Implement.**
  - `notification-route.ts` is pure: an allow-list lookup with no imports.
  - In `onesignal.ts`, the click listener reads `event.notification.additionalData`. The listener is removed by the returned function.
  - `_layout.tsx` routes with `router.push(route)` from `expo-router` when `notificationRoute` returns non-null, and otherwise does nothing.
  - `PushTokenRegistrar` keeps `loginOneSignal(publicKey)`, then awaits `requestOneSignalPermission()`, and drops the Expo token code. Update its log lines and the doc comment on `initOneSignal`, which now owns the prompt.

- [ ] **Step 4: Run the tests, lint and typecheck.**
  Run: `cd web/apps/mobile && bunx jest src/services/__tests__/notification-route.test.ts && bun run lint && bunx tsc --noEmit`
  Expected: PASS, no new lint errors.

- [ ] **Step 5: Commit.**

```bash
git add web/apps/mobile
git commit -m "feat(mobile): let OneSignal own the push prompt and route earnings taps"
```

---

## Task 10: Rollout checklist (Rodion; no code)

Do these in order. Each item says what proves it worked.

- [ ] **Copy:** approve, word for word, every string marked PROPOSED in Task 5, or replace it.
- [ ] **OneSignal dashboard:**
  - Create the Android categories "Earnings" and "Account alerts", and note their channel IDs.
  - Confirm the plan includes intelligent delivery.
- [ ] **Reach before cutover:** count wallets with Expo tokens (`SELECT count(DISTINCT wallet_public_key) FROM push_tokens`) and compare with wallets that have an enabled OneSignal push subscription (dashboard → Audience → Subscriptions, export). The gap is the old-binary count the spec asks to report.
- [ ] **Live-sender inventory:**
  - `journalctl -u loyal-web --since -14d | grep debug-send` on the host.
  - Vercel logs for `/api/push-tokens/debug-send`.
  - Any Expo sends from `web/apps/telegram`.
  - Write down each caller. "Expo removed" is claimed only when this check is clean.
- [ ] **Credentials:** seal `ONESIGNAL_APP_ID` and `ONESIGNAL_REST_API_KEY` for `loyal-web` and `loyal-web-cron@`, and `MIXPANEL_PROJECT_TOKEN` for `loyal-web-cron@`. Use drop-ins `60-onesignal.conf` and `60-mixpanel.conf` holding `LoadCredentialEncrypted=` lines plus `Environment=ONESIGNAL_EARNINGS_CHANNEL_ID=…` and `Environment=ONESIGNAL_ALERTS_CHANNEL_ID=…`. Channel IDs are required for the joy cron: it refuses to run without the earnings one. Proof: `loyal-web` logs no `degraded("wallet push"…)` line.
- [ ] **Deploy PR 1:** `bun run deploy` (web family). Proof: `debug-send` to your own wallet arrives under "Account alerts".
- [ ] **Test phone:** a push naming the Earnings channel creates that channel on an existing install, with no new build.
- [ ] **Migration** (before PR 2 goes live):
  - Check that `drizzle.__drizzle_migrations` has 32 rows, then run `bun run db:migrate` in `web/apps/telegram` against loyal-db-primary `loyal_app` as its owner.
  - The branch may ship as one PR. If it does, apply the migration before that single deploy. Skipping this fails loudly at the 15:41 run without sending anything.
  - Grant SELECT, INSERT and UPDATE on `analytics_export_watermarks` to the loyal-web App role, mirroring `\dp earn_yield_push_state`. Only needed when the loyal-web credentials are not table owners.
- [ ] **Deploy PR 2, then dry run:** `loyal-web cron earn-yield-joy --dry-run`. Proof:
  - `rebaselined` ≈ the number of state rows, `pushed` is empty, and `truncated` is **false**. If it is true, raise `joyTimeBudget` before the 15:41 run, because the same tail of wallets would be skipped every day.
  - Then enable `loyal-web-cron-earn-events-mixpanel.timer`. Its first run reports `seeded: true`.
- [ ] **Rollback warning:** before rolling loyal-web back past this release, run `systemctl disable --now loyal-web-cron-earn-yield-joy.timer`. The old joy code would push `yield-first` and `yield-total-10` to wallets the new code seeded, and would send its sub-$1 digests.
- [ ] **Next day:** the joy summary's `pushed` amounts are all ≥ $1, and Mixpanel shows `Earnings Push Sent` and `Earn Deposit Confirmed`. If `Earn Deposit Confirmed` shows almost no `source=autodeposit`, the observer-before-finalization labelling race needs a fix.
- [ ] **Ship PR 3:** `eas update` on the `production` and `dapp-store` channels. Proof: tapping an earnings push opens Earn (or Earn MAX).
- [ ] **Follow-ups** (not in this plan):
  - Delete the unscheduled legacy Next route `web/apps/web/src/app/api/cron/earn-yield-joy`, because a manual call would push under the old rules.
  - Remove `/api/push-tokens` and the `push_tokens` table after a release with no readers.
  - Ask whoever owns the Mixpanel cohorts to stop syncing `push: earning-while-away` and `push: still-earning`. Until then, those syncs get a 400 "known campaign key" refusal from Go, and Mixpanel pauses them.
