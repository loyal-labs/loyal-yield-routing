import type { LaunchResponse, RouteError } from "@loyal-labs/api";

import { env } from "@/config/env";
import { fetchWithTimeout } from "@/lib/network/fetch-with-timeout";
import {
  earnHeaders,
  earnWalletRead,
  getEarnSessionToken,
} from "@/lib/solana/earn/earn-session";

export async function fetchFundingLaunch(
  walletAddress: string,
  signal: AbortSignal
) {
  const token = await getEarnSessionToken(walletAddress);
  const headers = earnHeaders();
  if (token) headers.Authorization = `Bearer ${token}`;
  const response = await fetchWithTimeout(
    `${env.apiBaseUrl}/api/funding/launch`,
    { credentials: "omit", headers, signal }
  );
  if (!response.ok) return false;
  const result: LaunchResponse = await response.json();
  return result?.launched === true;
}

export async function fundingRequest<T>(
  walletAddress: string,
  path: string,
  options: {
    method?: "GET" | "POST";
    body?: unknown;
    interactive?: boolean;
    signal?: AbortSignal;
  } = {}
): Promise<T> {
  const url = `${env.apiBaseUrl}${path}`;
  const response = await earnWalletRead(walletAddress, url, {
    ...options,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    // Native login can leave cookies in the platform jar. Funding belongs to
    // the active wallet's bearer, including after switching accounts.
    credentials: "omit",
    timeoutMs: 35_000,
  });
  const data = await response.json().catch(() => null);
  if (response.ok && data !== null) return data as T;
  const detail = (data as Partial<RouteError> | null)?.error;
  throw Object.assign(
    new Error(
      detail?.message ||
        `The bridge request failed (${response.status}). Please try again.`
    ),
    {
      code: detail?.code,
      requestId: detail?.requestId,
      status: response.status,
    }
  );
}
