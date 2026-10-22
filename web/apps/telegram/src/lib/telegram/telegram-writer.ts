import type { Bot } from "grammy";

export interface SendDraftParams {
  chatId: number;
  draftId: number; // Must be non-zero; same ID = animated updates
  text: string;
  messageThreadId?: number;
  parseMode?: "Markdown" | "MarkdownV2" | "HTML";
}

export interface SendMessageParams {
  chatId: number;
  text: string;
  messageThreadId?: number;
  parseMode?: "Markdown" | "MarkdownV2" | "HTML";
}

const MAX_RETRIES = 3;
const BASE_RETRY_DELAY_MS = 1000;

async function withRetry<T>(
  fn: () => Promise<T>,
  retries: number = MAX_RETRIES
): Promise<T> {
  let lastError: Error | undefined;

  for (let attempt = 0; attempt < retries; attempt++) {
    try {
      return await fn();
    } catch (error) {
      lastError = error as Error;

      const apiError = error as {
        error_code?: number;
        parameters?: { retry_after?: number };
      };
      if (apiError.error_code === 429) {
        const waitTime = (apiError.parameters?.retry_after || 1) * 1000;
        console.warn(`Rate limited, waiting ${waitTime}ms before retry`);
        await sleep(waitTime);
        continue;
      }

      if (apiError.error_code === 403) {
        throw new Error("User blocked the bot");
      }

      if (attempt < retries - 1) {
        const delay = BASE_RETRY_DELAY_MS * Math.pow(2, attempt);
        console.warn(`Telegram API error, retrying in ${delay}ms:`, error);
        await sleep(delay);
      }
    }
  }

  throw lastError || new Error("Retry failed");
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Sends a message draft for streaming display.
 * Uses Telegram Bot API 9.3+ sendMessageDraft method via Grammy.
 * Same draft_id produces animated updates for smooth streaming.
 *
 * @param bot - Grammy bot instance
 * @param params - Draft parameters including draftId for tracking
 * @returns true on success
 */
export async function sendDraft(
  bot: Bot,
  params: SendDraftParams
): Promise<boolean> {
  return withRetry(async () => {
    const result = await bot.api.sendMessageDraft(
      params.chatId,
      params.draftId,
      params.text,
      {
        message_thread_id: params.messageThreadId,
        parse_mode: params.parseMode,
      }
    );

    return result === true;
  });
}

export function generateDraftId(): number {
  return (
    (Date.now() & 0x7fffffff) ^ Math.floor(Math.random() * 0x7fffffff) || 1
  );
}

export async function sendMessage(
  bot: Bot,
  params: SendMessageParams
): Promise<number> {
  return withRetry(async () => {
    const result = await bot.api.sendMessage(params.chatId, params.text, {
      message_thread_id: params.messageThreadId,
      parse_mode: params.parseMode,
    });

    return result.message_id;
  });
}

export async function editMessage(
  bot: Bot,
  chatId: number,
  messageId: number,
  text: string,
  parseMode?: "Markdown" | "MarkdownV2" | "HTML"
): Promise<void> {
  return withRetry(async () => {
    await bot.api.editMessageText(chatId, messageId, text, {
      parse_mode: parseMode,
    });
  });
}
