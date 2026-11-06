import type {
  EarnMaxContextResponse,
  Get,
  MobileTokenDetailResponse,
  MobileTrustedDapp,
  Post,
  ReferralClaimRequest,
  Summary,
} from "@loyal-labs/api";
import type { VoltrWithdrawalConsent, ProtectedVoltrClaimBroadcast } from "@loyal-labs/smart-account-vaults";
import { Buffer } from "buffer";

import { earnHeaders, earnWalletRead } from "@/lib/solana/earn/earn-session";

import { env } from "@/config/env";
import { fetchWithTimeout } from "@/lib/network/fetch-with-timeout";
import { EarnApiError } from "@/lib/solana/earn/earn-api";

type EarnMaxReadPath =
  | "/api/smart-accounts/earn-max/withdrawal-consent"
  | "/api/smart-accounts/earn-max/summary"
  | "/api/smart-accounts/earn-max/activity";

export async function saveEarnMaxWithdrawalConsent(
  walletAddress: string,
  consent: VoltrWithdrawalConsent,
) {
  const response = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/earn-max/withdrawal-consent`,
    { method: "POST", body: JSON.stringify(consent) },
  );
  if (!response.ok)
    throw await earnMaxFailure(response, "Unable to save withdrawal approval.");
}

export async function reserveEarnMaxWithdrawalClaim(walletAddress: string, claim: ProtectedVoltrClaimBroadcast) {
  const response = await earnWalletRead(walletAddress, `${env.apiBaseUrl}/api/smart-accounts/earn-max/withdrawal-claim-attempt`, { method: "POST", body: JSON.stringify({ ...claim, wire: undefined, wireBase64: Buffer.from(claim.wire).toString("base64") }) });
  if (!response.ok) throw await earnMaxFailure(response, "This withdrawal claim could not be reserved. Check status before signing again.");
}

async function earnMaxFailure(response: Response, fallback: string) {
  const payload = await response.json().catch(() => null);
  return new EarnApiError(
    payload?.error?.message || fallback,
    payload?.error?.code ?? "earn_max_unavailable",
    response.status,
  );
}

export async function fetchEarnMax<P extends EarnMaxReadPath>(
  walletAddress: string,
  path: P,
): Promise<Get[P] | null> {
  const response = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}${path}`,
  );
  if (response.ok) return (await response.json()) as Get[P];
  const failure = await earnMaxFailure(
    response,
    "Unable to update Earn MAX. Please try again.",
  );
  if (failure.code === "smart_account_required") return null;
  throw failure;
}

export async function provisionEarnMaxAccount(
  walletAddress: string,
): Promise<EarnMaxContextResponse> {
  const response = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/earn-max/context`,
    { method: "POST", body: "{}" },
  );
  if (response.ok) return (await response.json()) as EarnMaxContextResponse;
  throw await earnMaxFailure(
    response,
    "Earn MAX is temporarily unavailable. Please try again later.",
  );
}

export async function fetchReferrals(
  walletAddress: string,
): Promise<Get["/api/referrals"]> {
  const response = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/referrals`,
  );
  if (!response.ok) {
    throw new Error(`Failed to fetch referrals: ${response.status}`);
  }
  return (await response.json()) as Get["/api/referrals"];
}

export type ReferralClaimAnswer =
  | { answered: true; result: Post["/api/referrals/claim"] }
  | { answered: false };

export async function claimReferral(
  walletAddress: string,
  request: ReferralClaimRequest,
): Promise<ReferralClaimAnswer> {
  try {
    const response = await earnWalletRead(
      walletAddress,
      `${env.apiBaseUrl}/api/referrals/claim`,
      { method: "POST", body: JSON.stringify(request) },
    );
    if (response.ok) {
      return {
        answered: true,
        result: (await response.json()) as Post["/api/referrals/claim"],
      };
    }
    if (response.status === 400) {
      return { answered: true, result: { refusal: "unknown_code" } };
    }
  } catch {}
  return { answered: false };
}

export async function lookupReferralCode(
  code: string,
): Promise<boolean | null> {
  try {
    const response = await fetchWithTimeout(
      `${env.apiBaseUrl}/api/referrals/codes/${encodeURIComponent(code)}`,
      { headers: earnHeaders() },
    );
    if (!response.ok) return null;
    const data: Get["/api/referrals/codes/{code}"] = await response.json();
    return data.valid;
  } catch {
    return null;
  }
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

export async function fetchSummaries(): Promise<Summary[]> {
  const response = await fetch(`${env.apiBaseUrl}/api/summaries`);
  if (!response.ok) {
    throw new Error(`Failed to fetch summaries: ${response.status}`);
  }
  const data: Get["/api/summaries"] = await response.json();
  return data.summaries;
}

export async function fetchSummariesByGroup(
  groupChatId: string,
): Promise<Summary[]> {
  const response = await fetch(
    `${env.apiBaseUrl}/api/summaries?groupChatId=${encodeURIComponent(
      groupChatId,
    )}`,
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
    `${env.apiBaseUrl}/api/mobile/tokens/${encodeURIComponent(
      mint,
    )}?timeframe=${timeframe}`,
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
export function transformSummariesToGroups(summaries: Summary[]): GroupChat[] {
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
