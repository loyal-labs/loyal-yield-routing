import {
  getSolanaEndpoints,
  resolveSolanaEnv,
} from "@loyal-labs/solana-rpc";

// loyal-web: one API server for content, Earn and push. In development, use your local network IP or tunnel URL.
const API_BASE_URL =
  process.env.EXPO_PUBLIC_API_BASE_URL ?? "https://next.askloyal.com";
const GRID_AUTH_BASE_URL =
  process.env.EXPO_PUBLIC_GRID_AUTH_BASE_URL ??
  "https://auth.askloyal.com";
const SOLANA_ENV = resolveSolanaEnv(process.env.EXPO_PUBLIC_SOLANA_ENV);

// Vercel deployment-protection bypass — only needed for protected preview
// deploys (e.g. a staging branch). Production askloyal.com is public, so this
// is empty by default; set EXPO_PUBLIC_VERCEL_PROTECTION_BYPASS to test against
// a protected deploy. Sent as the `x-vercel-protection-bypass` header only when
// non-empty.
const VERCEL_PROTECTION_BYPASS =
  process.env.EXPO_PUBLIC_VERCEL_PROTECTION_BYPASS ?? "";

// Hardcoded identity for MVP (auth deferred)
const TELEGRAM_USER_ID = "2131567542";

const MIXPANEL_TOKEN = process.env.EXPO_PUBLIC_MIXPANEL_TOKEN ?? "";

// OneSignal App ID (dashboard > Settings > Keys & IDs). No fallback on
// purpose — OneSignal init is skipped entirely when unset.
const ONESIGNAL_APP_ID = process.env.EXPO_PUBLIC_ONESIGNAL_APP_ID ?? "";

// Privy identity layer (same Privy app as web, mobile app client). No
// fallback: when unset the app boots without a PrivyProvider and the legacy
// signers keep working.
const PRIVY_APP_ID = process.env.EXPO_PUBLIC_PRIVY_APP_ID ?? "";
const PRIVY_CLIENT_ID = process.env.EXPO_PUBLIC_PRIVY_CLIENT_ID ?? "";

export const env = {
  apiBaseUrl: API_BASE_URL,
  vercelProtectionBypass: VERCEL_PROTECTION_BYPASS,
  gridAuthBaseUrl: GRID_AUTH_BASE_URL,
  solanaEnv: SOLANA_ENV,
  solanaRpcEndpoint: getSolanaEndpoints(SOLANA_ENV).rpcEndpoint,
  telegramUserId: TELEGRAM_USER_ID,
  mixpanelToken: MIXPANEL_TOKEN,
  oneSignalAppId: ONESIGNAL_APP_ID,
  privyAppId: PRIVY_APP_ID,
  privyClientId: PRIVY_CLIENT_ID,
} as const;
