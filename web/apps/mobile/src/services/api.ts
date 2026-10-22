import type {
  Get,
  MobileTokenDetailResponse,
  MobileTrustedDapp,
  Summary,
} from "@loyal-labs/api";

import { env } from "@/config/env";

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
    `${env.apiBaseUrl}/api/summaries?groupChatId=${encodeURIComponent(groupChatId)}`,
  );
  if (!response.ok) {
    throw new Error(`Failed to fetch summaries: ${response.status}`);
  }
  const data: Get["/api/summaries"] = await response.json();
  return data.summaries;
}

export async function fetchTokenDetailMarket(
  mint: string,
  timeframe: TokenDetailTimeframe = "1d",
): Promise<MobileTokenDetailResponse> {
  const response = await fetch(
    `${env.apiBaseUrl}/api/mobile/tokens/${encodeURIComponent(mint)}?timeframe=${timeframe}`,
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
