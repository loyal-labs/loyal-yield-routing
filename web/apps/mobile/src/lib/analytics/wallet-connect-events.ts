export const WALLET_CONNECT_EVENTS = {
  pressed: "wallet_connect_pressed",
  returned: "wallet_connect_returned",
  failed: "wallet_connect_failed",
} as const;

export type WalletConnectProvider = "mwa" | "phantom" | "solflare";

export type WalletConnectSurface = "onboarding";
