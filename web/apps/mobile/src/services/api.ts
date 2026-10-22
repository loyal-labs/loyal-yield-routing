import type { MobileEarnMaxResponses } from "@loyal-labs/shared";
import type {
  Get,
  MobileTokenDetailResponse,
  MobileTrustedDapp,
  Summary,
} from "@loyal-labs/api";

import { earnHeaders } from "@/lib/solana/earn/earn-session";

import { env } from "@/config/env";
import { fetchWithTimeout } from "@/lib/network/fetch-with-timeout";
import {
  EarnApiError,
  type EarnAuthFields,
  type EarnSessionAuth,
} from "@/lib/solana/earn/earn-api";

export async function fetchEarnMaxApy(): Promise<number | null> {
  const response = await fetchWithTimeout(
    `${env.apiBaseUrl}/api/earn-max/apy`,
    { headers: earnHeaders() }
  );
  if (!response.ok) throw new Error("Earn MAX APY is unavailable.");
  const payload = await response.json();
  return typeof payload.apyBps === "number" && Number.isFinite(payload.apyBps)
    ? payload.apyBps
    : null;
}

/** Public on-chain display data, like the ordinary Earn state/earnings reads. */
export async function fetchMobileEarnMax<A extends "summary" | "activity">(
  action: A, walletAddress: string
): Promise<MobileEarnMaxResponses[A]> {
  const response = await fetchWithTimeout(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn-max?action=${action}&walletAddress=${encodeURIComponent(walletAddress)}`,
    { headers: earnHeaders() }
  );
  const payload = await response.json().catch(() => null);
  if (!response.ok || !payload) throw new EarnApiError(
    payload?.error?.message ?? "Unable to update Earn MAX. Please try again.",
    payload?.error?.code ?? "earn_max_unavailable", response.status
  );
  return payload as MobileEarnMaxResponses[A];
}

export async function requestEarnMax<A extends keyof MobileEarnMaxResponses>(
  action: A,
  auth: EarnAuthFields | EarnSessionAuth | { walletAddress: string }
): Promise<MobileEarnMaxResponses[A]> {
  const session = "sessionToken" in auth;
  const response = await fetchWithTimeout(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn-max`,
    {
      method: "POST",
      credentials: "include",
      headers: {
        ...earnHeaders(),
        ...(session ? { Authorization: `Bearer ${auth.sessionToken}` } : {}),
      },
      body: JSON.stringify({
        ...(!session ? auth : {}),
        action,
      }),
    }
  );
  const payload = await response.json().catch(() => null);
  if (!payload || !response.ok) {
    throw new EarnApiError(
      payload?.error?.message ?? "Earn MAX is temporarily unavailable. Please try again later.",
      payload?.error?.code ?? "earn_max_unavailable",
      response.status
    );
  }
  return payload as MobileEarnMaxResponses[A];
}

export type GroupChat = {
  id: string;
  title: string;
  subtitle: string;
  photoUrl?: string;
  // Deprecated compatibility fields; remove after migration window.
  photoBase64?: string;
  photoMimeType?: string;
};

export type TokenDetailTimeframe = "1d" | "1w" | "1m" | "1y";

export type { MobileTokenDetailResponse } from "@loyal-labs/api";

/**
 * Fetch all summaries from the API.
 */
export async function fetchSummaries(): Promise<Summary[]> {
  const response = await fetch(`${env.apiBaseUrl}/api/summaries`);
  if (!response.ok) {
    throw new Error(`Failed to fetch summaries: ${response.status}`);
  }
  const data: Get["/api/summaries"] = await response.json();
  return data.summaries;
}

/**
 * Fetch summaries for a specific group chat.
 */
export async function fetchSummariesByGroup(
  groupChatId: string,
): Promise<Summary[]> {
  const response = await fetch(
    `${env.apiBaseUrl}/api/summaries?groupChatId=${encodeURIComponent(
      groupChatId
    )}`
  );
  if (!response.ok) {
    throw new Error(`Failed to fetch summaries: ${response.status}`);
  }
  const data: Get["/api/summaries"] = await response.json();
  return data.summaries;
}

export async function fetchTokenDetailMarket(
  mint: string,
  timeframe: TokenDetailTimeframe = "1d"
): Promise<MobileTokenDetailResponse> {
  const response = await fetch(
    `${env.apiBaseUrl}/api/mobile/tokens/${encodeURIComponent(
      mint
    )}?timeframe=${timeframe}`
  );

  if (!response.ok) {
    throw new Error(`Failed to fetch token detail: ${response.status}`);
  }

  return response.json();
}

export async function fetchTrustedDapps(): Promise<MobileTrustedDapp[]> {
  const response = await fetch(`${env.apiBaseUrl}/api/mobile/dapps`);

  if (!response.ok) {
    throw new Error(`Failed to fetch trusted dapps: ${response.status}`);
  }

  const data: Get["/api/mobile/dapps"] = await response.json();
  return data.dapps;
}

export async function fetchLibrary(): Promise<Get["/api/mobile/library"]> {
  const response = await fetch(`${env.apiBaseUrl}/api/mobile/library`);

  if (!response.ok) {
    throw new Error(`Failed to fetch library: ${response.status}`);
  }

  return response.json();
}

/**
 * Transform flat summaries array into deduplicated group list.
 * Keeps the most recent summary per group (input assumed sorted newest-first from API).
 */
export function transformSummariesToGroups(
  summaries: Summary[],
): GroupChat[] {
  const groupMap = new Map<string, GroupChat>();

  for (const summary of summaries) {
    const groupKey = summary.chatId ?? summary.title;
    if (!groupMap.has(groupKey)) {
      groupMap.set(groupKey, {
        id: groupKey,
        title: summary.title,
        subtitle: summary.topics[0]?.content ?? "",
        photoUrl: summary.photoUrl,
        photoBase64: summary.photoBase64,
        photoMimeType: summary.photoMimeType,
      });
    }
  }

  return Array.from(groupMap.values());
}
