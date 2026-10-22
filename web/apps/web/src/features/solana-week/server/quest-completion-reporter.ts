import "server-only";

import bs58 from "bs58";
import nacl from "tweetnacl";

import { getOptionalEnv } from "@/lib/core/config/shared";

/**
 * Reports Solana Week "challenge quest" completions to the Solana dApp Store
 * Quests API (see `mobile/solana-week.md` → "Challenge Quest Completion API").
 *
 * Backend-only by mandate: the completion API key must never reach the mobile
 * client. Only call this from server code (route handlers, server actions,
 * workers).
 *
 * Feature-gated: if the endpoint, API key, or the relevant quest id is not
 * configured the reporter no-ops (`status: "disabled"`). That makes it safe to
 * ship before Solana finalizes the per-integration credentials and quest ids —
 * fill the env vars in and it starts reporting with no code change.
 *
 * The completion API is idempotent per `(wallet_id, quest_id)`; both
 * `completed` (201) and `already_completed` (200) are success.
 */

// Server-only secrets/config. Never expose any of these with NEXT_PUBLIC_.
// Master kill-switch for ALL Solana-side reporting. Fail-safe OFF: a completion
// is sent to Solana only when SOLANA_WEEK_QUESTS_ENABLED is exactly "true" OR
// the wallet is in the tester allowlist (TESTER_WALLETS_ENV below). Quests are
// hidden for the Solana dApp Store launch until the official reveal; flip this
// env var (no code change) to go live — see mobile/docs/quests-launch-toggle.md.
// Every send path (Quest 1 deposit-confirm, Quest 2 sweep-notify, the reconcile
// cron) funnels through reportQuestCompletion below, so this one gate covers
// them all. The external sweep worker keeps calling /sweep-notify regardless;
// while disabled those reports just no-op here ("disabled"), leaving the local
// row pending.
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

// Accepts a base58 or base64 Ed25519 secret key. A 64-byte value is used as the
// full secret key; a 32-byte value is treated as a seed and expanded.
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
    // The API accepts base64, base58, or hex; base64 is the simplest universal.
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

  // 201 = recorded synchronously; 202 = accepted for asynchronous processing
  // (the API started responding "202 Accepted" when the round opened on
  // 2026-07-08 — previously misclassified as a permanent error, stranding
  // rows as `failed`). Both mean Solana took the completion.
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
  // Seeker Summer round 1 ended 2026-07-15: reporting to the Solana
  // completions API is permanently off, overriding the env kill-switch and
  // the tester carve-out. Local completion rows still record (in-app progress
  // display keeps working); flip this back for a future round.
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
