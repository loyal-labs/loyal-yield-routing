# Mobile Quests development preview — 2026-10-28

**Development only. Do not merge or release this branch to production.**
Vlad explicitly asked to preserve this work on GitHub for future work while
keeping it out of production because Quests is mocked. A GitHub push is not
approval for a production build, OTA, backend deployment, or merge.

Branch: `dev/mobile-quests-preview` in `loyal-labs/loyal-yield-routing`.
Branch base: `10ba80b2d`.

## What is preserved

- Android Quests now matches the web Bones preview and the
  [mobile Figma design](https://www.figma.com/design/cCAe3qzifxJqBT5yYQZDWh/?node-id=763-11868).
  `src/features/quests/QuestsScreen.tsx` owns the UI and its sample fixtures.
  `/quests` shows the in-progress fixture; `/quests?state=default` shows empty.
- Bones totals, Earn/LOYAL amounts inside Quests, rank, referral code,
  eligibility, streaks and cooldowns are examples. There is no rewards backend.
  Claim/invite actions show notices; copying the example referral code works.
  The existing iOS visibility gate remains.
- Quests top spacing follows Activity. Both large Claim buttons share
  `alignContent: "center"`: wrapping rows otherwise had 10 points above the
  content and 16 below in native Yoga; the correction measures 13/13.
- The separate wallet Home/Earn/Earn MAX balances use real wallet data.
  Passive reads use public wallet-address-keyed mobile endpoints without
  cookies or an extra Seed Vault signature. Transaction preparation retains
  authentication. Wallet-keyed snapshots and request generations discard
  old-wallet responses after reset or switching. Unknown totals show a dash.
- The public mobile Earn MAX API has an older contract. Its forecast APY is
  historical annualized share-price growth, not the current rate. The adapter
  uses `/api/earn-max/apy` for the current rate/history, with `currentApyBps`
  as the badge fallback. Verified current APY was 16.31%; the historical metric
  was about -17%. Those must not be treated as interchangeable.
- Earn MAX chart conversion accepts the existing production daily-series
  format when attributed earnings are absent. New attributed earnings take
  precedence; explicit null or incomplete history remains unavailable.

## Test installation and update

- App: **Loyal (Dev) 0.6.0**, Android build **12**, `com.loyal.app.dev`.
- EAS channel: `dapp-store-development`; runtime: `0.6.0` (app-version policy).
- [Dev APK build](https://expo.dev/accounts/loyal-labs/projects/loyal-app/builds/cca6371b-91ce-4938-86d6-a5aa4d3d5bd2).
- [Latest verified development OTA](https://expo.dev/accounts/loyal-labs/projects/loyal-app/updates/39e73e53-3f99-4cdb-8dde-271c4d2428bc).
  Android update ID: `01a182cb-3921-7381-9763-f5e03675ceed`.
  Launch asset SHA-256 (base64url): `ygC46PSlMIu0mcZ4qIqjYtkLC1cIcZDF3Ag4HQruJGc`.
- OTA source was exported from an isolated checkout based on `8583fb4d9`
  plus these mobile changes, before this preservation commit. Its EAS commit
  field therefore does not identify this branch's eventual preservation commit.
- Apply through the dev app's **Update available → Restart** banner. No wallet
  reset or reconnect is required. Never target production channel `dapp-store`.
- EAS `preview` must use `EXPO_PUBLIC_API_BASE_URL=https://next.askloyal.com`.
  That remote environment was corrected during this work. Clear Metro's cache
  when exporting, inspect the actual Hermes bundle and source map, then publish
  with `eas update --skip-bundler`. Verify the served manifest hash afterwards.
  The old Vercel host must not reappear through stale environment inlining.

## Verification and remaining work

13 targeted tests passed across chart earnings, passive reads, Earn sessions
and Earn MAX transaction preparation. Scoped ESLint and Android export passed.
Live no-auth reads returned the connected wallet's correct balances, current
APY and a ready 30-day chart series. Browser wallet-switch tests discarded late
old-wallet responses. Quests navigation and fixture checks passed; the final
Earn MAX browser screen check was inconclusive. Native Yoga verified Claim row
geometry, but final physical Android rendering was not verified.

Full TypeScript checking still reports two existing missing constant exports
in `web/packages/solana-wallet/src/parsers/activity-parser.ts`:
`KAMINO_FARMS_PROGRAM_ID` and `SMART_ACCOUNTS_PROGRAM_ID`.

Before any future production proposal: replace or gate Quests fixtures, connect
and verify real rewards/eligibility/referral behavior, complete physical-device
verification, and obtain explicit release approval. Wallet fixes can be reviewed
separately from mocked Quests. Unrelated Go work in the original working tree
is intentionally not part of this branch.
