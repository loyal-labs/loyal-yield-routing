import type { Bot } from "grammy";

import { chatCompletionStream, type ChatMessage } from "@/lib/redpill";
import { addMessage } from "@/lib/telegram/bot-thread-service";
import {
  editMessage,
  generateDraftId,
  sendDraft,
  sendMessage,
} from "@/lib/telegram/telegram-writer";

export interface StreamResponseParams {
  bot: Bot;
  chatId: number;
  threadId: string;
  telegramThreadId?: number; // Telegram's message_thread_id for forum topics
  context: ChatMessage[];
  systemPrompt: string;
  model?: string;
}

export interface StreamResponseResult {
  messageId: string;
  fullText: string;
  telegramMessageId?: number;
}

// Keep this aligned with the model used in RedPill summary generation.
const DEFAULT_MODEL = "phala/gpt-oss-120b";
const UPDATE_INTERVAL_MS = 500; // Update draft every 500ms
const MAX_MESSAGE_LENGTH = 4096; // Telegram limit
const CHUNK_BUFFER = 100; // Buffer before max length for safety

export function splitAtSentenceBoundary(
  text: string,
  maxLength: number = MAX_MESSAGE_LENGTH - CHUNK_BUFFER
): string[] {
  if (text.length <= maxLength) {
    return [text];
  }

  const parts: string[] = [];
  let remaining = text;

  while (remaining.length > maxLength) {
    const chunk = remaining.substring(0, maxLength);

    const sentenceEndings = [
      chunk.lastIndexOf(". "),
      chunk.lastIndexOf("! "),
      chunk.lastIndexOf("? "),
      chunk.lastIndexOf(".\n"),
      chunk.lastIndexOf("!\n"),
      chunk.lastIndexOf("?\n"),
    ];

    const splitIndex = Math.max(...sentenceEndings);

    if (splitIndex > maxLength * 0.5) {
      parts.push(remaining.substring(0, splitIndex + 1).trim());
      remaining = remaining.substring(splitIndex + 2).trim();
    } else {
      const lastSpace = chunk.lastIndexOf(" ");
      if (lastSpace > maxLength * 0.8) {
        parts.push(remaining.substring(0, lastSpace).trim());
        remaining = remaining.substring(lastSpace + 1).trim();
      } else {
        parts.push(chunk.trim());
        remaining = remaining.substring(maxLength).trim();
      }
    }
  }

  if (remaining.length > 0) {
    parts.push(remaining);
  }

  return parts;
}

export async function streamResponse(
  params: StreamResponseParams
): Promise<StreamResponseResult> {
  const {
    bot,
    chatId,
    threadId,
    telegramThreadId,
    context,
    systemPrompt,
    model,
  } = params;

  let fullText = "";
  let currentChunkText = "";
  let lastUpdateTime = 0;
  let fallbackMessageId: number | undefined;
  let currentDraftId = generateDraftId();
  const sentMessageIds: number[] = [];

  const messages: ChatMessage[] = [
    { role: "system", content: systemPrompt },
    ...context,
  ];

  const stream = chatCompletionStream({
    model: model || DEFAULT_MODEL,
    messages,
    temperature: 0.7,
  });

  try {
    for await (const token of stream) {
      fullText += token;
      currentChunkText += token;

      const now = Date.now();
      const shouldUpdate = now - lastUpdateTime >= UPDATE_INTERVAL_MS;

      if (shouldUpdate && currentChunkText.length > 0) {
        if (currentChunkText.length > MAX_MESSAGE_LENGTH - CHUNK_BUFFER) {
          const splitChunks = splitAtSentenceBoundary(currentChunkText);
          const finalText = splitChunks[0];
          const remainingText = splitChunks.slice(1).join("");

          if (fallbackMessageId) {
            await editMessage(bot, chatId, fallbackMessageId, finalText);
            sentMessageIds.push(fallbackMessageId);
          } else {
            const msgId = await sendMessage(bot, {
              chatId,
              text: finalText,
              messageThreadId: telegramThreadId,
            });
            sentMessageIds.push(msgId);
          }

          currentChunkText = remainingText;
          fallbackMessageId = undefined;
          currentDraftId = generateDraftId();
        } else {
          try {
            await sendDraft(bot, {
              chatId,
              draftId: currentDraftId,
              text: currentChunkText,
              messageThreadId: telegramThreadId,
            });
          } catch (draftError) {
            console.warn(
              "sendMessageDraft failed, using fallback:",
              draftError
            );

            if (fallbackMessageId) {
              try {
                await editMessage(
                  bot,
                  chatId,
                  fallbackMessageId,
                  currentChunkText
                );
              } catch (editError) {
                console.warn("Failed to edit fallback message:", editError);
              }
            } else {
              try {
                fallbackMessageId = await sendMessage(bot, {
                  chatId,
                  text: currentChunkText,
                  messageThreadId: telegramThreadId,
                });
              } catch (sendError) {
                console.error("Failed to send fallback message:", sendError);
              }
            }
          }
        }

        lastUpdateTime = now;
      }
    }

    if (currentChunkText.length > 0) {
      const chunks = splitAtSentenceBoundary(currentChunkText);

      for (let i = 0; i < chunks.length; i++) {
        const chunk = chunks[i];
        const isFirst = i === 0;

        if (isFirst && fallbackMessageId) {
          await editMessage(bot, chatId, fallbackMessageId, chunk);
          sentMessageIds.push(fallbackMessageId);
        } else {
          const msgId = await sendMessage(bot, {
            chatId,
            text: chunk,
            messageThreadId: telegramThreadId,
          });
          sentMessageIds.push(msgId);
        }
      }
    }

    const messageId = await addMessage({
      threadId,
      senderType: "bot",
      content: fullText,
      telegramMessageId:
        sentMessageIds.length > 0 ? BigInt(sentMessageIds[0]) : undefined,
    });

    if (!messageId) {
      throw new Error("Failed to store bot response");
    }

    return {
      messageId,
      fullText,
      telegramMessageId: sentMessageIds[0],
    };
  } catch (error) {
    console.error("Stream error:", error);

    if (fullText.length > 0) {
      try {
        await addMessage({
          threadId,
          senderType: "bot",
          content: fullText,
        });
      } catch (saveError) {
        console.error("Failed to save partial response:", saveError);
      }
    }

    throw error;
  }
}
