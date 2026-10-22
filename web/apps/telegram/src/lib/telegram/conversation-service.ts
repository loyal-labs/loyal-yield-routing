import type { Bot, Context } from "grammy";

import {
  addMessage,
  getOrCreateThread,
  getThreadMessagesForAI,
  updateThread,
} from "@/lib/telegram/bot-thread-service";
import { streamResponse } from "@/lib/telegram/streaming-service";
import { getOrCreateUser } from "@/lib/telegram/user-service";
import { getTelegramDisplayName, isPrivateChat } from "@/lib/telegram/utils";

const CONVERSATION_HISTORY_LIMIT = 20;

const SYSTEM_PROMPT = `You are a helpful AI assistant integrated into Telegram. You provide concise, friendly, and accurate responses.

Guidelines:
- Be helpful and conversational
- Keep responses focused and not overly long unless the user asks for detailed explanations
- Use Telegram-compatible formatting when helpful (bold with *text*, italic with _text_, code with \`code\`)
- If you don't know something, say so honestly
- Be respectful and professional`;

export async function handleDirectTopicCreatedMessage(
  ctx: Context
): Promise<void> {
  const message = ctx.message;
  const chat = ctx.chat;
  const from = message?.from;

  if (!chat || !isPrivateChat(chat.type)) {
    return;
  }

  const telegramThreadId = message?.message_thread_id;
  const topicName = message?.forum_topic_created?.name;

  if (!message || !from || telegramThreadId === undefined || !topicName) {
    console.warn("Missing required fields in direct topic created message");
    return;
  }

  try {
    const displayName = getTelegramDisplayName(from);
    const telegramChatId = BigInt(chat.id);
    const telegramUserId = BigInt(from.id);
    const userId = await getOrCreateUser(telegramUserId, {
      username: from.username || null,
      displayName,
    });

    const threadId = await getOrCreateThread({
      userId,
      telegramChatId,
      telegramThreadId,
      title: topicName,
    });

    await updateThread(threadId, { title: topicName });
  } catch (error) {
    console.error("Error handling direct topic created message:", error);
  }
}

export async function handleDirectMessage(
  ctx: Context,
  bot: Bot
): Promise<void> {
  const message = ctx.message;
  const chat = ctx.chat;
  const from = message?.from;

  if (!message?.text || !chat || !from) {
    console.error("Missing required fields in direct message");
    return;
  }

  const userMessage = message.text;
  const chatId = chat.id;
  const telegramMessageId = BigInt(message.message_id);
  const telegramChatId = BigInt(chatId);
  const telegramUserId = BigInt(from.id);

  const telegramThreadId = message.message_thread_id;

  try {
    const displayName = getTelegramDisplayName(from);
    const userId = await getOrCreateUser(telegramUserId, {
      username: from.username || null,
      displayName,
    });

    const threadId = await getOrCreateThread({
      userId,
      telegramChatId,
      telegramThreadId,
      title: `Conversation with ${displayName}`,
    });

    const userMsgId = await addMessage({
      threadId,
      senderType: "user",
      content: userMessage,
      telegramMessageId,
    });

    if (!userMsgId) {
      console.error("Failed to store user message");
      await ctx.reply(
        "Sorry, I couldn't process your message. Please try again.",
        {
          message_thread_id: telegramThreadId,
        }
      );
      return;
    }

    const conversationHistory = await getThreadMessagesForAI(threadId, {
      limit: CONVERSATION_HISTORY_LIMIT,
    });

    await streamResponse({
      bot,
      chatId,
      threadId,
      telegramThreadId,
      context: conversationHistory,
      systemPrompt: SYSTEM_PROMPT,
    });
  } catch (error) {
    console.error("Error handling direct message:", error);

    try {
      await ctx.reply(
        "Sorry, I encountered an error processing your message. Please try again.",
        {
          message_thread_id: telegramThreadId,
        }
      );
    } catch (replyError) {
      console.error("Failed to send error reply:", replyError);
    }
  }
}
