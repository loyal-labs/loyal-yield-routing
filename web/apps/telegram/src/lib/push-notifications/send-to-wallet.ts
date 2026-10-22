import { pushTokens } from "@loyal-labs/db-core/schema";
import { eq } from "drizzle-orm";

import { getDatabase } from "@/lib/core/database";

import { sendExpoPushNotifications } from "./send";

export type WalletPushPayload = {
  title: string;
  body: string;
  data?: Record<string, unknown>;
};

export async function sendPushToWallet(
  walletPublicKey: string,
  payload: WalletPushPayload
): Promise<{ sentTo: number }> {
  const db = getDatabase();
  const rows = await db
    .select({ token: pushTokens.token })
    .from(pushTokens)
    .where(eq(pushTokens.walletPublicKey, walletPublicKey));

  if (rows.length === 0) {
    return { sentTo: 0 };
  }

  await sendExpoPushNotifications(
    rows.map((row) => ({
      to: row.token,
      title: payload.title,
      body: payload.body,
      sound: "default",
      data: payload.data,
    })),
    { source: "wallet" }
  );

  return { sentTo: rows.length };
}
