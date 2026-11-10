import { pushTokens } from "@loyal-labs/db-core/schema";
import { NextResponse } from "next/server";

import { getDatabase } from "@/lib/core/database";

export async function POST(req: Request): Promise<NextResponse> {
  try {
    const body = await req.json();
    const { token, telegramUserId, walletPublicKey, platform, provider } = body;

    // This route is unauthenticated, so it stores only legacy Expo tokens. OneSignal
    // subscriptions are registered through Go, which checks the signed-in wallet.
    if (provider !== undefined && provider !== "expo") {
      return NextResponse.json(
        { error: "Invalid push token for provider" },
        { status: 400 }
      );
    }

    if (!token || (platform !== "ios" && platform !== "android")) {
      return NextResponse.json(
        { error: "Missing required fields: token, platform" },
        { status: 400 }
      );
    }

    if (!telegramUserId && !walletPublicKey) {
      return NextResponse.json(
        {
          error:
            "At least one identity required: telegramUserId or walletPublicKey",
        },
        { status: 400 }
      );
    }

    const telegramUserIdBigInt = telegramUserId ? BigInt(telegramUserId) : null;
    const walletPublicKeyValue =
      typeof walletPublicKey === "string" && walletPublicKey.length > 0
        ? walletPublicKey
        : null;

    const db = getDatabase();

    await db
      .insert(pushTokens)
      .values({
        token,
        telegramUserId: telegramUserIdBigInt,
        walletPublicKey: walletPublicKeyValue,
        platform,
        provider: "expo",
      })
      .onConflictDoUpdate({
        target: pushTokens.token,
        set: {
          telegramUserId: telegramUserIdBigInt,
          walletPublicKey: walletPublicKeyValue,
          platform,
          // A rebound row is never a server push target.
          provider: "expo",
          updatedAt: new Date(),
        },
      });

    return NextResponse.json({ success: true });
  } catch (error) {
    console.error("[api/push-tokens] Failed to register token:", error);
    return NextResponse.json(
      { error: "Failed to register push token" },
      { status: 500 }
    );
  }
}
