import { env } from "@/config/env";
import { earnHeaders, silentEarnToken } from "@/lib/solana/earn/earn-session";

/**
 * Tell the server which OneSignal push subscription belongs to the signed-in
 * wallet. The server only pushes to subscriptions registered here, through
 * the Earn session; it never targets the OneSignal external_id, which the app
 * sets itself with OneSignal.login and so proves nothing about the wallet.
 *
 * Resolves to whether the server stored the subscription. Skips (false) when
 * the wallet has no Earn session yet.
 */
export async function registerOneSignalSubscription(
  subscriptionId: string,
  walletPublicKey: string,
): Promise<boolean> {
  try {
    const bearer = await silentEarnToken(walletPublicKey);
    if (!bearer) {
      console.log("[push] No Earn session for this wallet yet, skipping");
      return false;
    }
    const response = await fetch(`${env.apiBaseUrl}/api/push-tokens`, {
      method: "POST",
      headers: { ...earnHeaders(), Authorization: `Bearer ${bearer}` },
      body: JSON.stringify({
        token: subscriptionId,
        platform: process.env.EXPO_OS ?? "unknown",
        provider: "onesignal",
      }),
    });
    if (!response.ok) {
      const bodyText = await response.text().catch(() => "");
      console.error(
        `[push] Backend rejected push subscription (${response.status}):`,
        bodyText.slice(0, 200),
      );
      return false;
    }
    return true;
  } catch (error) {
    console.error("[push] Failed to POST push subscription:", error);
    return false;
  }
}
