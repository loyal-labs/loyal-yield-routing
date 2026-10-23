"use client";

import { EARN_FORECAST_REALIZED_MAX_AGE_MS } from "@loyal-labs/shared";
import {
  readClientCacheEntry,
  writeClientCache,
} from "@/lib/client-cache/client-cache";

import type {
  EarnForecastApy,
  EarnForecastApyHistoryResponse,
  EarnForecastSummaryResponse,
} from "./earn-forecast.shared";

const CLIENT_CACHE_TTL_MS = 30 * 60 * 1000;
const FAILURE_BACKOFF_MS = 5 * 60 * 1000;
const PERSIST_TTL_MS = 24 * 60 * 60 * 1000;
export const REALIZED_MAX_AGE_MS = EARN_FORECAST_REALIZED_MAX_AGE_MS;

const EARN_FORECAST_CACHE_KEY = "loyal.earnForecastSummary.v2";
const EARN_FORECAST_CACHE_VERSION = 2;
const EARN_FORECAST_CACHE_SCOPE =
  process.env.NEXT_PUBLIC_SOLANA_ENV ?? "mainnet";

function isEarnForecastSummaryResponse(
  data: unknown
): data is EarnForecastSummaryResponse {
  if (typeof data !== "object" || data === null) {
    return false;
  }

  const candidate = data as EarnForecastSummaryResponse;
  return (
    typeof candidate.forecast === "object" &&
    candidate.forecast !== null &&
    typeof candidate.forecast.apyBps === "number" &&
    typeof candidate.history === "object" &&
    candidate.history !== null &&
    Array.isArray(candidate.history.samples)
  );
}

function readPersistedEarnForecastSummary() {
  return readClientCacheEntry<EarnForecastSummaryResponse>({
    key: EARN_FORECAST_CACHE_KEY,
    version: EARN_FORECAST_CACHE_VERSION,
    solanaEnv: EARN_FORECAST_CACHE_SCOPE,
    validate: isEarnForecastSummaryResponse,
  });
}

export const EMPTY_EARN_FORECAST_HISTORY: EarnForecastApyHistoryResponse = {
  feeBps: 1,
  generatedAt: "2026-06-01T00:00:00.000Z",
  riskProfile: "safe",
  samples: [],
  window: {
    endedAt: "2026-06-01T00:00:00.000Z",
    startedAt: "2026-05-02T00:00:00.000Z",
  },
};

let cachedSummary: {
  expiresAt: number;
  value: EarnForecastSummaryResponse;
} | null = null;
let inflightSummary: Promise<EarnForecastSummaryResponse> | null = null;
let failedSummary: { error: unknown; retryAt: number } | null = null;

export { toForecastApy } from "@loyal-labs/shared";

function validCachedSummary(
  summary: EarnForecastSummaryResponse,
  now: number
): boolean {
  if (summary.forecast.strategy !== "realized_7d_share_price") {
    return true;
  }
  const measuredAt = Date.parse(summary.forecast.updatedAt);
  return Number.isFinite(measuredAt) && now - measuredAt < REALIZED_MAX_AGE_MS;
}

export async function fetchEarnForecastSummary(): Promise<EarnForecastSummaryResponse> {
  const now = Date.now();
  if (
    cachedSummary &&
    cachedSummary.expiresAt > now &&
    validCachedSummary(cachedSummary.value, now)
  ) {
    return cachedSummary.value;
  }

  if (inflightSummary) {
    return inflightSummary;
  }

  const persisted = readPersistedEarnForecastSummary();
  if (
    persisted &&
    now - persisted.savedAt < CLIENT_CACHE_TTL_MS &&
    validCachedSummary(persisted.data, now)
  ) {
    cachedSummary = {
      expiresAt: persisted.savedAt + CLIENT_CACHE_TTL_MS,
      value: persisted.data,
    };
    return persisted.data;
  }

  if (failedSummary && failedSummary.retryAt > now) {
    throw failedSummary.error;
  }

  inflightSummary = fetch("/api/smart-accounts/earn-forecast/summary", {
    cache: "no-store",
    signal: AbortSignal.timeout(30_000),
  })
    .then(async (response) => {
      if (!response.ok) {
        throw new Error(
          `Earn forecast summary request failed: ${response.status}`
        );
      }

      return (await response.json()) as EarnForecastSummaryResponse;
    })
    .then((summary) => {
      const receivedAt = Date.now();
      const remainingMs =
        summary.forecast.strategy === "realized_7d_share_price"
          ? REALIZED_MAX_AGE_MS -
            (receivedAt - Date.parse(summary.forecast.updatedAt))
          : CLIENT_CACHE_TTL_MS;
      failedSummary = null;
      cachedSummary = {
        expiresAt:
          receivedAt + Math.max(0, Math.min(CLIENT_CACHE_TTL_MS, remainingMs)),
        value: summary,
      };
      writeClientCache({
        key: EARN_FORECAST_CACHE_KEY,
        version: EARN_FORECAST_CACHE_VERSION,
        solanaEnv: EARN_FORECAST_CACHE_SCOPE,
        data: summary,
        ttlMs: PERSIST_TTL_MS,
      });
      return summary;
    })
    .catch((error) => {
      console.warn("[earn-forecast] failed to load summary", error);
      if (persisted && validCachedSummary(persisted.data, Date.now())) {
        return persisted.data;
      }
      failedSummary = { error, retryAt: Date.now() + FAILURE_BACKOFF_MS };
      throw error;
    })
    .finally(() => {
      inflightSummary = null;
    });

  return inflightSummary;
}

export function getCachedEarnForecastSummaryForTests() {
  return cachedSummary?.value ?? null;
}

export function resetEarnForecastSummaryCacheForTests() {
  cachedSummary = null;
  inflightSummary = null;
  failedSummary = null;
}
