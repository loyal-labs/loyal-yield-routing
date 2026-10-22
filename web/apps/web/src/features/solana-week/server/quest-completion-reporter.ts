import "server-only";

import bs58 from "bs58";
import nacl from "tweetnacl";

import { getOptionalEnv } from "@/lib/core/config/shared";

const ENABLED_ENV = "SOLANA_WEEK_QUESTS_ENABLED";
// Round-end hard stop, checked before the env gate: while true, no completion
// is ever sent to Solana (round 1 ended 2026-07-15).
const ROUND_ENDED = true;
const TESTER_WALLETS_ENV = "SOLANA_WEEK_QUESTS_TESTER_WALLETS";

const ENDPOINT_ENV = "SOLANA_WEEK_QUESTS_COMPLETION_ENDPOINT";
const API_KEY_ENV = "SOLANA_WEEK_QUESTS_API_KEY";
const SIGNING_KEY_ENV = "SOLANA_WEEK_QUESTS_SIGNING_KEY";
const QUEST_ID_EARN_DEPOSIT_ENV = "SOLANA_WEEK_QUEST_ID_EARN_DEPOSIT";
const QUEST_ID_FIRST_AUTODEPOSIT_ENV =
  "SOLANA_WEEK_QUEST_ID_FIRST_AUTODEPOSIT";

const MAX_WALLET_ID_LENGTH = 128;
const MAX_QUEST_ID_LENGTH = 120;

export type QuestKind = "earn_deposit" | "first_autodeposit_sweep";

const QUEST_ID_ENV_BY_KIND: Record<QuestKind, string> = {
  earn_deposit: QUEST_ID_EARN_DEPOSIT_ENV,
  first_autodeposit_sweep: QUEST_ID_FIRST_AUTODEPOSIT_ENV,
};

export type QuestCompletionResult =
  | { status: "completed" | "already_completed" }
  | { status: "disabled"; reason: string }
  | { status: "skipped"; reason: string }
  | {
      status: "permanent_error";
      httpStatus: number;
      error: string;
      message: string;
    }
  | { status: "retryable_error"; httpStatus?: number; error: string };

export type QuestCompletionMetadata = Record<string, unknown>;

type ReporterConfig = {
  endpoint: string;
  apiKey: string;
  signingSecretKey: Uint8Array | null;
  questIdByKind: Record<QuestKind, string | undefined>;
};

function decodeSigningSecretKey(raw: string): Uint8Array {
  const tryDecoders: Array<(value: string) => Uint8Array> = [
    (value) => bs58.decode(value),
    (value) => new Uint8Array(Buffer.from(value, "base64")),
  ];

  for (const decode of tryDecoders) {
    let bytes: Uint8Array;
    try {
      bytes = decode(raw);
    } catch {
      continue;
    }
    if (bytes.length === nacl.sign.secretKeyLength) {
      return bytes;
    }
    if (bytes.length === nacl.sign.seedLength) {
      return nacl.sign.keyPair.fromSeed(bytes).secretKey;
    }
  }

  throw new Error(
    `${SIGNING_KEY_ENV} must be a base58/base64 Ed25519 secret key (32-byte seed or 64-byte key).`
  );
}

function loadReporterConfig(
  env: NodeJS.ProcessEnv = process.env
): ReporterConfig | null {
  const endpoint = getOptionalEnv(env, ENDPOINT_ENV);
  const apiKey = getOptionalEnv(env, API_KEY_ENV);
  if (!endpoint || !apiKey) {
    return null;
  }

  const signingKeyRaw = getOptionalEnv(env, SIGNING_KEY_ENV);

  return {
    endpoint,
    apiKey,
    signingSecretKey: signingKeyRaw
      ? decodeSigningSecretKey(signingKeyRaw)
      : null,
    questIdByKind: {
      earn_deposit: getOptionalEnv(env, QUEST_ID_EARN_DEPOSIT_ENV),
      first_autodeposit_sweep: getOptionalEnv(
        env,
        QUEST_ID_FIRST_AUTODEPOSIT_ENV
      ),
    },
  };
}

function buildHeaders(
  config: ReporterConfig,
  bodyBytes: Uint8Array
): Record<string, string> {
  const headers: Record<string, string> = {
    "content-type": "application/json",
    "x-api-key": config.apiKey,
  };
  if (config.signingSecretKey) {
    const signature = nacl.sign.detached(bodyBytes, config.signingSecretKey);
    headers["x-signature"] = Buffer.from(signature).toString("base64");
  }
  return headers;
}

const MAX_COMPLETION_ATTEMPTS = 3;
const RETRY_BASE_DELAY_MS = 250;

function retryDelayMs(retryIndex: number): number {
  return RETRY_BASE_DELAY_MS * 2 ** retryIndex;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

async function attemptCompletion(
  config: ReporterConfig,
  questId: string,
  walletId: string,
  metadata: QuestCompletionMetadata | undefined
): Promise<QuestCompletionResult> {
  const body = JSON.stringify({
    wallet_id: walletId,
    quest_id: questId,
    ...(metadata ? { metadata } : {}),
  });
  const bodyBytes = new TextEncoder().encode(body);

  let response: Response;
  try {
    response = await fetch(config.endpoint, {
      method: "POST",
      headers: buildHeaders(config, bodyBytes),
      body,
    });
  } catch (cause) {
    return {
      status: "retryable_error",
      error: cause instanceof Error ? cause.message : "Network request failed.",
    };
  }

  if (response.status === 201 || response.status === 202) {
    return { status: "completed" };
  }
  if (response.status === 200) {
    return { status: "already_completed" };
  }

  const payload = (await response.json().catch(() => null)) as {
    error?: string;
    message?: string;
  } | null;

  if (response.status >= 500) {
    return {
      status: "retryable_error",
      httpStatus: response.status,
      error: payload?.error ?? "internal_error",
    };
  }

  return {
    status: "permanent_error",
    httpStatus: response.status,
    error: payload?.error ?? "unknown_error",
    message: payload?.message ?? response.statusText,
  };
}

async function postCompletion(
  config: ReporterConfig,
  questId: string,
  walletId: string,
  metadata: QuestCompletionMetadata | undefined
): Promise<QuestCompletionResult> {
  let lastResult: QuestCompletionResult = {
    status: "retryable_error",
    error: "no_attempt_made",
  };

  for (let attempt = 0; attempt < MAX_COMPLETION_ATTEMPTS; attempt++) {
    if (attempt > 0) {
      await sleep(retryDelayMs(attempt - 1));
    }
    lastResult = await attemptCompletion(config, questId, walletId, metadata);
    if (lastResult.status !== "retryable_error") {
      return lastResult;
    }
  }

  return lastResult;
}

function isTesterWallet(env: NodeJS.ProcessEnv, walletId: string): boolean {
  const raw = getOptionalEnv(env, TESTER_WALLETS_ENV);
  if (!raw) {
    return false;
  }
  return raw
    .split(",")
    .some((entry) => entry.trim().length > 0 && entry.trim() === walletId);
}

export async function reportQuestCompletion(args: {
  kind: QuestKind;
  walletId: string;
  metadata?: QuestCompletionMetadata;
  env?: NodeJS.ProcessEnv;
}): Promise<QuestCompletionResult> {
  if (ROUND_ENDED) {
    return {
      status: "disabled",
      reason: "Seeker Summer round 1 ended — Solana-side reporting retired",
    };
  }

  const env = args.env ?? process.env;
  const globallyEnabled = getOptionalEnv(env, ENABLED_ENV) === "true";
  if (!globallyEnabled && !isTesterWallet(env, args.walletId.trim())) {
    return {
      status: "disabled",
      reason: `${ENABLED_ENV} not "true" and wallet not allowlisted — quests hidden until reveal`,
    };
  }

  const config = loadReporterConfig(args.env);
  if (!config) {
    return {
      status: "disabled",
      reason: `${ENDPOINT_ENV}/${API_KEY_ENV} not configured`,
    };
  }

  const questId = config.questIdByKind[args.kind];
  if (!questId) {
    return {
      status: "disabled",
      reason: `${QUEST_ID_ENV_BY_KIND[args.kind]} not configured`,
    };
  }
  if (questId.length > MAX_QUEST_ID_LENGTH) {
    return { status: "skipped", reason: "quest_id exceeds 120 chars" };
  }

  const walletId = args.walletId.trim();
  if (!walletId) {
    return { status: "skipped", reason: "empty wallet_id" };
  }
  if (walletId.length > MAX_WALLET_ID_LENGTH) {
    return { status: "skipped", reason: "wallet_id exceeds 128 chars" };
  }

  return postCompletion(config, questId, walletId, args.metadata);
}
