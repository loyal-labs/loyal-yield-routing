# OneSignal pushes and smarter earnings notifications

Date: 2026-11-08 · Owner and reviewer: Rodion · Status: design approved in chat, not built

## Why

Loyal makes money for its users, and a push is the cheapest way to make them feel it. The goal is
retention first ("Loyal quietly made me money, I'm not leaving") and growth second (larger depositors
add more). Most depositors hold more money than they have put into Loyal.

The pushes must stay worth reading. A $10 deposit must never get "you earned $0.01". Large
depositors should hear from us more often, because they earn more. A push that people mute is worse
than no push, because a muted channel serves neither goal.

## What exists today (facts, 2026-11-08)

- The Earn "joy" pushes (ASK-2091, PR #649, Vlad, August) were ported to Go by Chris on 2026-11-04
  (PR #705): `go/workers/internal/webapp/earn_yield_joy.go`. They run as
  `loyal-web cron earn-yield-joy` from `deploy/hetzner/systemd/loyal-web-cron-earn-yield-joy.timer`,
  daily at 15:41 UTC.
- The current rule has a digest threshold that decays: $1 within 7 days of the last push, then $0.10,
  then $0.01 after 30 days. Its milestones are a first-yield push at $0.01 lifetime, then $10, $100
  and $1,000, plus 6- and 12-month anniversaries. Wallets are seeded silently the first time they are
  seen. A run sends at most one push per wallet. State is kept in `earn_yield_push_state`, and claims
  use compare-and-swap.
- The rule counts **Earn only**: `lifetimeEarned` reads `earnprep.EarnVaultIndex`. Earn MAX per-user
  performance is in `go/workers/internal/earnmax/earnings.go`. It **can be negative**, because Earn
  MAX is a leveraged vault.
- **Every server push goes through Expo** (`go/workers/internal/webapp/push_send.go`, which calls
  `exp.host`). Tokens are kept per wallet in the app DB `push_tokens`. Call sites:
  - `autodeposit_confirm.go` (sweep scheduled, autodeposit enabled)
  - `autodeposit_setup.go`
  - `push_campaigns.go` (Mixpanel cohort pushes from ASK-1651, failed-sweep push)
  - `earn_yield_joy.go`
  - the `POST /api/push-tokens/debug-send` route
- The mobile app (`web/apps/mobile`) registers with both systems. It sends an Expo token to
  `/api/push-tokens`, and it calls `OneSignal.login(wallet)` (`src/services/onesignal.ts`). The OS
  permission prompt is owned by the Expo flow (`PushTokenRegistrar`). No server code sends through
  OneSignal yet. A manual REST send to 7 Seeker Loot Party winners on 2026-11-03 delivered 7 of 7.
- `react-native-onesignal` 5.2.14 has been in the binary since about mid-August. The app has
  `expo-updates` with `runtimeVersion: appVersion` and the channels `production` and `dapp-store`, so
  JS-only changes ship over the air.
- Taps are handled today by `addNotificationResponseListener` in `src/services/notifications.ts`,
  which is Expo. The tabs include `(tabs)/earn.tsx` and `(tabs)/earn-max.tsx`. The scheme is `loyal://`.
- Mixpanel: mobile identifies users as `mob:<wallet>`. `Earn Deposit` (`amount_usd`) is tracked
  client-side, for manual mobile deposits only. Withdrawals, autodeposit sweeps and Earn MAX are not
  tracked in Mixpanel.
- The production DB moved from Neon to `loyal-db-primary` on Hetzner (`deploy/hetzner/db/cutover.md`).
  The Neon endpoints are disabled.

## Decisions (made by Rodion)

1. **Purpose: retention plus growth.** The push text never asks for money. The push opens the
   earnings screen, and the "add more" nudge lives there.
2. **Rule: push when there is enough to tell.** Once a day, each wallet's earnings since its last
   push are compared with a **fixed $1** threshold. There is no decay and no tiers. A push is sent
   only at or above $1.
3. **Earn and Earn MAX are combined** into one lifetime number per wallet.
4. **Earnings copy:**
   - Title: `+$1.37 earned`
   - Body: `$48.20 earned with Loyal so far.`
   - It names no day, because delivery can be late.
   - It does not mention a rate, "more" or "risk-free" (Loyal's yield-claim rules).
5. **Cap: at most 4 earnings-category pushes per wallet per rolling 7 days, and at most 1 per day.**
   Earnings keep accruing while capped, so the next push simply shows a bigger number.
6. **Milestones, by lifetime earned:** **$1, $100, $500, $1,000, $5,000.**
   - The $1 milestone is the welcome push. Suggested copy: title `$1 earned with Loyal`, body
     `Your first dollar, and it keeps going on its own.`
   - **The first-yield push at $0.01 is removed.**
   - The 6- and 12-month anniversaries stay. They still require at least $1 lifetime earned.
   - A milestone or anniversary takes the place of that day's earnings push.
7. **Everything moves to OneSignal.** This covers all server pushes, account alerts included. Expo
   sending and Expo token registration are removed. Targeting is
   `include_aliases.external_id = [wallet]` with `target_channel: "push"`.
8. **Earnings pushes use OneSignal intelligent delivery** (`delayed_option: "last-active"`).
   Account alerts (autodeposit, failed sweep, quests, cohort nudges) are sent immediately.
9. **Two Android notification categories in OneSignal: "Earnings" and "Account alerts."** Every push
   names its category (`android_channel_id`), so a user can mute earnings without muting alerts.
10. **No holdout group.** Measurement happens in Mixpanel.
11. **No new store build.** The app changes must be JS-only and ship with `eas update`.
12. **Rodion reviews and deploys.** Chris is not involved, except for adding Rodion's SSH key to the
    workers host.
13. **No combined-earnings UI.** The push's lifetime number is Earn lifetime plus Earn MAX
    performance, and the two tabs keep showing their own parts. No app screen change is made for this.
14. **Retire the Mixpanel cohort campaigns `earning-while-away` and `still-earning`** (ASK-1651).
    The daily earnings cron supersedes them. Remove them from `mixpanelCohortCampaigns` in
    `push_campaigns.go`, and tell whoever owns the Mixpanel cohorts to stop syncing them. The other
    cohort nudges stay, and are sent as Account alerts, immediately.

## Scope

### Backend (`go/workers`)

- **Replace the Expo sender with a OneSignal sender.**
  - Keep the `WalletPush.send(ctx, wallet, payload)` shape so the call sites barely change.
    `payload` gains a category (`earnings` | `alerts`) and an optional delivery mode (`now` |
    `intelligent`).
  - Every send carries an `idempotency_key` derived from (campaign, wallet, run/claim id), so a retry
    can never send twice.
  - Keep the existing claim-before-send pattern in `push_campaigns.go` and `earn_yield_joy.go`.
- **Credentials:** `ONESIGNAL_APP_ID` and `ONESIGNAL_REST_API_KEY` are installed as `systemd-creds`
  on `loyal-web` and on the cron unit. Their drop-ins follow the README pattern.
- **Remove Expo:**
  - `push_send.go`'s Expo dispatch, the ticket handling, and its grants on `push_tokens` and
    `push_notification_*`.
  - **`POST /api/push-tokens/debug-send` stays, backed by OneSignal** (builder's call). It sends as
    Account alerts, immediately. Any non-Go sender still calling it then moves to OneSignal with no
    change of its own. Candidates are the Next `wallet-push.server.ts` callers (reached through
    `MOBILE_PUSH_API_BASE_URL`) and anything proxied to Vercel.
  - **Before Expo is removed, inventory the live senders.** Check `loyal-web` journal logs and the
    Vercel logs for `debug-send` callers and for any Expo sends from `web/apps/telegram`'s own
    sender. "Expo removed" is only claimed once that check is clean.
- **Earnings rule changes in `earn_yield_joy.go` (decisions 2–6):**
  - A fixed $1 digest threshold.
  - The new milestone list. Drop `yield-first`.
  - A 4-per-7-days cap. Store recent push times; for example, add a `recent_pushed_at` timestamp
    array to `earn_yield_push_state`. That needs a migration on loyal-db-primary.
  - Combined lifetime is `earn_lifetime + earn_max_performance`.
- **Negative Earn MAX performance:** the baseline (`last_pushed_earned_usd`) moves only when a push
  is sent. That makes it a high-water mark: after a drop, no push goes out until combined earnings
  exceed the last pushed level by $1. The lifetime number shown in the copy is the current combined
  value. If it is ≤ 0, no earnings or milestone push is sent.
- **One-time re-baseline when Earn MAX is added:** without it, the first run would report a wallet's
  whole Earn MAX history as "+$X earned". On the first run of the new code, every existing state row
  has its baseline set to the current combined total without pushing. Milestones already passed under
  the combined total are marked sent, the same way first-sight seeding works today.
- **Mixpanel server events** on `mob:<wallet>` (builder's call, v1 scope):
  - `Earnings Push Sent`, with type, campaign and amount. Emitted by the cron when it sends.
  - `Earn Deposit Confirmed` and `Earn Withdrawal Confirmed`, with amount and source (manual |
    autodeposit).
    - **Source:** a cron job reading `loyal_yield.user_yield_position_deposits` and
      `user_yield_position_withdrawals` past a watermark. Autodeposit sweeps appear there:
      `balance_sweep_execution_id` is set on the deposit row.
    - **Why a job:** no single request path sees every deposit. Sweeps complete inside the retail
      engine, and the retail engine is not touched.
  - **Out of v1:** Earn MAX deposit and withdrawal events. There is no server confirm for Earn MAX
    deposits today.
- **App DB migration (builder's call):** `earn_yield_push_state` is in `loyal_app`, which drizzle
  manages (`web/packages/db-core/src/schema.ts`, `web/apps/telegram/drizzle/`).
  - The new column is a normal drizzle migration.
  - Rodion applies it to loyal-db-primary **before** the loyal-web release that reads it goes live.
    The 15:41 UTC cron runs that release.
  - `loyal-migrate` and `--migrations-applied` do not apply to loyal-web.

### Mobile app (`web/apps/mobile`, JS only, ships over the air)

- OneSignal owns the permission prompt (`OneSignal.Notifications.requestPermission`). It replaces
  the Expo prompt in `PushTokenRegistrar`. `OneSignal.login(wallet)` stays.
- Remove the Expo token registration (`registerPushToken` to `/api/push-tokens`).
- Add a OneSignal click listener in `src/services/onesignal.ts` that routes on the push's
  `data.route`:
  - Earnings and milestone pushes open `/(tabs)/earn`, or `/(tabs)/earn-max` when the wallet holds
    only Earn MAX. The server picks the route when it sends.
  - Account alerts open what they open today.
- Do not touch the native config. Nothing may require a new binary.

### OneSignal dashboard (Rodion, guided)

- Create Android categories "Earnings" and "Account alerts", and record their channel IDs for the
  backend config.
- Confirm that the plan includes intelligent delivery.

## Constraints

- **Delivery:** intelligent delivery can hold a push for up to about 24 hours. The amount is fixed
  at decision time, which the copy allows for.
- **Old binaries:** phones on builds without `react-native-onesignal` (before about mid-August)
  cannot receive anything after Expo is removed. This is accepted. The builder reports the count.
- **Deploys:** the README says deploys go through `bun run deploy` to `loyal-workers`, which needs
  root SSH. That waits until Rodion's key is on the host. Migrations are applied to loyal-db-primary
  before deploying with `--migrations-applied`.
- **Push copy:** wallet-led, no rate, no "risk-free" or "highest yield". Push copy is approved
  verbatim by Rodion before it ships.
- **Ownership:** the builder never deploys. Rodion deploys.

## Non-goals

- No in-app notification settings screen. The Android categories are enough for now.
- No per-user send-time logic of our own. OneSignal intelligent delivery handles timing.
- No holdout or A/B framework.
- No iOS-specific work. The app ships on Android (Seeker / dApp Store) today.

## Anti-goals (a failure even if it works)

- Any push that reports under $1, apart from anniversaries that already require at least $1.
- Two **Earnings-category** pushes to the same wallet on the same decision day, or a burst of
  backdated milestones after the deploy.
  - The cap counts the earnings cron only. Account alerts never wait for the cap and never count
    toward it.
  - A push held by intelligent delivery may still land on the same calendar day as the next one.
    This is accepted.
- A failed-sweep alert that arrives late, or that a user loses because they muted earnings.
- A push whose number differs from what the screens show. The lifetime number must equal the Earn
  tab's lifetime plus the Earn MAX tab's performance, read through the same services those screens
  use.

## Left to the builder

- What happens to `/api/push-tokens` and `debug-send`: keep a OneSignal-backed debug send for
  testing, or remove it. Remove the `push_tokens` table only after a release with no readers.
- How to ship: one PR or a sequence. The suggested sequence is OneSignal sender, then the rule
  changes and Mixpanel events, then the app change.
- Mixpanel transport from Go (the HTTP ingestion API) and how events are batched.
- Before cutover, measure reach: wallets with an enabled OneSignal push subscription, compared with
  wallets with Expo tokens. This needs a read-only `loyal_app` URL, plus the OneSignal key that is
  already in `loyal-noncritical-env`.
- Check on a test phone that a push naming a new category creates that Android channel with no
  new build.
