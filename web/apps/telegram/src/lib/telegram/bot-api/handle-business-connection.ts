import {
  type BusinessBotRights,
  businessConnections,
} from "@loyal-labs/db-core/schema";
import type { BusinessConnection as TelegramBusinessConnection } from "grammy/types";

import { getDatabase } from "@/lib/core/database";
import { getBot } from "@/lib/telegram/bot-api/bot";
import { getOrCreateUser } from "@/lib/telegram/user-service";

import { isNotificationsEnabledForUserId } from "./user-settings";

const bot = await getBot();

export async function handleBusinessConnection(
  connection: TelegramBusinessConnection
): Promise<void> {
  const {
    id: connectionId,
    user,
    user_chat_id: userChatId,
    date,
    is_enabled: isEnabled,
    rights,
  } = connection;

  const userId = await getOrCreateUser(BigInt(user.id), {
    username: user.username ?? null,
    displayName: user.first_name + (user.last_name ? ` ${user.last_name}` : ""),
  });

  await upsertBusinessConnection(userId, {
    businessConnectionId: connectionId,
    userChatId: BigInt(userChatId),
    isEnabled,
    rights: (rights as BusinessBotRights) ?? {},
    connectedAt: new Date(date * 1000),
  });

  const notificationsEnabled = await isNotificationsEnabledForUserId(userId);
  if (!notificationsEnabled) {
    return;
  }

  await sendBusinessConnectionMessage(userChatId, isEnabled);
}

async function upsertBusinessConnection(
  userId: string,
  data: {
    businessConnectionId: string;
    userChatId: bigint;
    isEnabled: boolean;
    rights: BusinessBotRights;
    connectedAt: Date;
  }
): Promise<void> {
  const db = getDatabase();

  await db
    .insert(businessConnections)
    .values({
      userId,
      businessConnectionId: data.businessConnectionId,
      userChatId: data.userChatId,
      isEnabled: data.isEnabled,
      rights: data.rights,
      connectedAt: data.connectedAt,
    })
    .onConflictDoUpdate({
      target: businessConnections.userId,
      set: {
        businessConnectionId: data.businessConnectionId,
        userChatId: data.userChatId,
        isEnabled: data.isEnabled,
        rights: data.rights,
        connectedAt: data.connectedAt,
        updatedAt: new Date(),
      },
    });
}

async function sendBusinessConnectionMessage(
  userChatId: number,
  isEnabled: boolean
): Promise<void> {
  try {
    if (isEnabled) {
      await bot.api.sendMessage(
        userChatId,
        "You've connected Loyal to messages."
      );
    } else {
      await bot.api.sendMessage(
        userChatId,
        "You've disconnected Loyal from messages."
      );
    }
  } catch (error) {
    console.warn("Failed to send business connection notification", error);
  }
}
