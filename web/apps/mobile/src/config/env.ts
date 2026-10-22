import {
  getSolanaEndpoints,
  resolveSolanaEnv,
} from "@loyal-labs/solana-rpc";

const API_BASE_URL =
  process.env.EXPO_PUBLIC_API_BASE_URL ?? "https://next.askloyal.com";
const GRID_AUTH_BASE_URL =
  process.env.EXPO_PUBLIC_GRID_AUTH_BASE_URL ??
  "https://auth.askloyal.com";
const SOLANA_ENV = resolveSolanaEnv(process.env.EXPO_PUBLIC_SOLANA_ENV);

const VERCEL_PROTECTION_BYPASS =
  process.env.EXPO_PUBLIC_VERCEL_PROTECTION_BYPASS ?? "";

const TELEGRAM_USER_ID = "2131567542";

const MIXPANEL_TOKEN = process.env.EXPO_PUBLIC_MIXPANEL_TOKEN ?? "";

const ONESIGNAL_APP_ID = process.env.EXPO_PUBLIC_ONESIGNAL_APP_ID ?? "";

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
