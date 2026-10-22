import { stripSecrets } from "./redact.ts";

export interface CredentialValues {
  clickStackWebhookSecret: string;
  telegramBotToken: string;
  telegramChatId: string;
  slackWebhookUrl: string;
}

export class RelayCredentials {
  readonly #secrets: string[];

  readonly clickStackWebhookSecret: string;
  readonly telegramBotToken: string;
  readonly telegramChatId: string;
  readonly slackWebhookUrl: string;

  constructor(values: CredentialValues) {
    this.clickStackWebhookSecret = values.clickStackWebhookSecret;
    this.telegramBotToken = values.telegramBotToken;
    this.telegramChatId = values.telegramChatId;
    this.slackWebhookUrl = values.slackWebhookUrl;

    this.#secrets = [
      values.clickStackWebhookSecret,
      values.telegramBotToken,
      values.slackWebhookUrl,
    ];
  }

  /**
   * Strips these credentials out of text the relay did not author. Called as a
   * method, never detached: it reads `#secrets` off `this`.
   */
  redactLogText(text: string): string {
    return stripSecrets(text, this.#secrets);
  }
}
