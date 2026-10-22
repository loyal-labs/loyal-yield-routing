import {
  botMessages,
  type BotThread,
  botThreads,
  type EncryptedMessageContent,
  type SenderType,
  type ThreadStatus,
} from "@loyal-labs/db-core/schema";
import { and, asc, count, desc, eq, isNull } from "drizzle-orm";

import { getDatabase } from "@/lib/core/database";
import { decrypt, encrypt } from "@/lib/encryption";
import type { ChatMessage } from "@/lib/redpill";

export type TextContent = { type: "text"; text: string };
export type ImageContent = {
  type: "image";
  url: string;
  metadata?: { fileName?: string; fileSize?: number };
};
export type VoiceContent = {
  type: "voice";
  url: string;
  metadata?: { duration?: number };
};
export type MessageContent = TextContent | ImageContent | VoiceContent;

export type DecryptedMessage = {
  id: string;
  senderType: SenderType;
  content: MessageContent;
  createdAt: Date;
  telegramMessageId: bigint | null;
};

function buildThreadWhereConditions(
  telegramChatId: bigint,
  telegramThreadId?: number
) {
  const chatCondition = eq(botThreads.telegramChatId, telegramChatId);
  const threadCondition =
    telegramThreadId !== undefined
      ? eq(botThreads.telegramThreadId, telegramThreadId)
      : isNull(botThreads.telegramThreadId);

  return and(chatCondition, threadCondition);
}

export async function getOrCreateThread(params: {
  userId: string;
  telegramChatId: bigint;
  telegramThreadId?: number;
  title?: string;
}): Promise<string> {
  const db = getDatabase();

  const whereConditions = buildThreadWhereConditions(
    params.telegramChatId,
    params.telegramThreadId
  );

  const existing = await db.query.botThreads.findFirst({
    where: whereConditions,
  });

  if (existing) {
    return existing.id;
  }

  const result = await db
    .insert(botThreads)
    .values({
      userId: params.userId,
      telegramChatId: params.telegramChatId,
      telegramThreadId: params.telegramThreadId,
      title: params.title,
      status: "active",
    })
    .onConflictDoNothing()
    .returning({ id: botThreads.id });

  if (result.length > 0) {
    return result[0].id;
  }

  const conflictedThread = await db.query.botThreads.findFirst({
    where: whereConditions,
  });

  if (!conflictedThread) {
    throw new Error("Failed to create or find thread after conflict");
  }

  return conflictedThread.id;
}

export async function getThread(threadId: string): Promise<BotThread | null> {
  const db = getDatabase();

  const thread = await db.query.botThreads.findFirst({
    where: eq(botThreads.id, threadId),
  });

  return thread ?? null;
}

export async function updateThread(
  threadId: string,
  updates: {
    title?: string;
    status?: ThreadStatus;
  }
): Promise<void> {
  const db = getDatabase();

  await db
    .update(botThreads)
    .set({
      ...updates,
      updatedAt: new Date(),
    })
    .where(eq(botThreads.id, threadId));
}

export async function getUserLatestThread(
  userId: string
): Promise<{ id: string; title: string | null } | null> {
  const db = getDatabase();

  const thread = await db.query.botThreads.findFirst({
    where: eq(botThreads.userId, userId),
    orderBy: (botThreads, { desc }) => [desc(botThreads.updatedAt)],
  });

  return thread ? { id: thread.id, title: thread.title } : null;
}

export async function addMessage(params: {
  threadId: string;
  senderType: SenderType;
  content: string | MessageContent;
  telegramMessageId?: bigint;
}): Promise<string | null> {
  const db = getDatabase();

  const messageContent: MessageContent =
    typeof params.content === "string"
      ? { type: "text", text: params.content }
      : params.content;

  const plaintext = JSON.stringify(messageContent);
  const encrypted = await encrypt(plaintext);

  if (!encrypted) {
    console.error("Failed to encrypt message content");
    return null;
  }

  const encryptedContent: EncryptedMessageContent = {
    type: messageContent.type,
    ciphertext: encrypted.ciphertext,
    iv: encrypted.iv,
    metadata:
      "metadata" in messageContent ? messageContent.metadata : undefined,
  };

  // neon-http does not support db.transaction(), so use batch for atomic writes.
  const [insertedRows] = await db.batch([
    db
      .insert(botMessages)
      .values({
        threadId: params.threadId,
        senderType: params.senderType,
        encryptedContent,
        telegramMessageId: params.telegramMessageId,
      })
      .returning({ id: botMessages.id }),
    db
      .update(botThreads)
      .set({ updatedAt: new Date() })
      .where(eq(botThreads.id, params.threadId)),
  ]);

  const insertedMessage = insertedRows[0];
  if (!insertedMessage?.id) {
    throw new Error("Failed to insert bot message");
  }

  return insertedMessage.id;
}

export async function getThreadMessages(
  threadId: string,
  options?: { limit?: number; latest?: boolean }
): Promise<DecryptedMessage[]> {
  const db = getDatabase();

  const messages = await db.query.botMessages.findMany({
    where: eq(botMessages.threadId, threadId),
    orderBy: [
      options?.latest
        ? desc(botMessages.createdAt)
        : asc(botMessages.createdAt),
    ],
    limit: options?.limit,
  });

  const decrypted: DecryptedMessage[] = [];

  for (const msg of messages) {
    const encContent = msg.encryptedContent;
    const plaintext = await decrypt({
      ciphertext: encContent.ciphertext,
      iv: encContent.iv,
    });

    if (!plaintext) {
      console.error(`Failed to decrypt message ${msg.id}`);
      continue;
    }

    try {
      const content: MessageContent = JSON.parse(plaintext);
      decrypted.push({
        id: msg.id,
        senderType: msg.senderType,
        content,
        createdAt: msg.createdAt,
        telegramMessageId: msg.telegramMessageId,
      });
    } catch (error) {
      console.error(`Failed to parse message content for ${msg.id}`, error);
    }
  }

  if (options?.latest) {
    decrypted.reverse();
  }

  return decrypted;
}

export async function getThreadMessagesForAI(
  threadId: string,
  options?: { limit?: number }
): Promise<ChatMessage[]> {
  const messages = await getThreadMessages(threadId, {
    limit: options?.limit,
    latest: true,
  });

  return messages
    .filter((msg) => msg.senderType !== "system")
    .map((msg): ChatMessage => {
      const role: "user" | "assistant" =
        msg.senderType === "user" ? "user" : "assistant";

      let content: string;
      if (msg.content.type === "text") {
        content = msg.content.text;
      } else if (msg.content.type === "image") {
        content = `[Image: ${msg.content.metadata?.fileName || "image"}]`;
      } else {
        content = `[Voice message: ${msg.content.metadata?.duration || 0}s]`;
      }

      return { role, content };
    });
}

export async function getThreadMessageCount(threadId: string): Promise<number> {
  const db = getDatabase();

  const [result] = await db
    .select({ count: count() })
    .from(botMessages)
    .where(eq(botMessages.threadId, threadId));

  return Number(result.count);
}
