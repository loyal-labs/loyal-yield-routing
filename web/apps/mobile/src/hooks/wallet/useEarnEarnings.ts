import { useEffect, useState } from "react";

import {
  fetchEarnEarnings,
  type EarnEarningsResponse,
} from "@/lib/solana/earn/earn-api";

export type EarnEarningsStatus = "loading" | "ready" | "unavailable";

type CachedEarnings = {
  earnings: EarnEarningsResponse | null;
  fetchedAtMs: number;
  status: Exclude<EarnEarningsStatus, "loading">;
};

let cacheAddress: string | null = null;
let cached: CachedEarnings | null = null;
let inFlight: { address: string; promise: Promise<void> } | null = null;
const pushListeners = new Set<() => void>();

const BACKGROUND_MAX_AGE_MS = 60_000;

function fetchIntoCache(walletAddress: string): Promise<void> {
  if (inFlight?.address === walletAddress) {
    return inFlight.promise;
  }
  const entry: { address: string; promise: Promise<void> } = {
    address: walletAddress,
    promise: Promise.resolve(),
  };
  entry.promise = (async () => {
    try {
      const res = await fetchEarnEarnings(walletAddress);
      cacheAddress = walletAddress;
      cached = { earnings: res, fetchedAtMs: Date.now(), status: "ready" };
    } catch (error) {
      console.error("Failed to fetch Earn earnings", error);
      if (cacheAddress !== walletAddress || cached?.status !== "ready") {
        cacheAddress = walletAddress;
        cached = {
          earnings: null,
          fetchedAtMs: Date.now(),
          status: "unavailable",
        };
      }
    } finally {
      if (inFlight === entry) {
        inFlight = null;
      }
    }
  })();
  inFlight = entry;
  return entry.promise;
}

// Refreshes the cache. Silent by default (mounted charts keep their snapshot);
// `notify: true` also pushes the fresh data into mounted charts — use it only
// when the earn balance explicitly changed.
export async function refreshEarnEarningsCache(
  walletAddress: string,
  { notify = false }: { notify?: boolean } = {},
): Promise<void> {
  const fresh =
    cacheAddress === walletAddress &&
    cached !== null &&
    Date.now() - cached.fetchedAtMs < BACKGROUND_MAX_AGE_MS;
  if (!notify && fresh) {
    return;
  }
  await fetchIntoCache(walletAddress);
  if (notify) {
    for (const listener of pushListeners) {
      listener();
    }
  }
}

type EarnEarningsSnapshot = {
  earnings: EarnEarningsResponse | null;
  fetchedAtMs: number | null;
  status: EarnEarningsStatus;
};

const LOADING: EarnEarningsSnapshot = {
  earnings: null,
  fetchedAtMs: null,
  status: "loading",
};

export function useEarnEarnings(walletAddress: string | null) {
  const [snapshot, setSnapshot] = useState<EarnEarningsSnapshot>(() =>
    walletAddress && cacheAddress === walletAddress && cached
      ? { ...cached }
      : LOADING,
  );

  useEffect(() => {
    if (!walletAddress) {
      setSnapshot(LOADING);
      return;
    }
    let alive = true;
    const syncFromCache = () => {
      if (!alive || cacheAddress !== walletAddress || !cached) {
        return;
      }
      const { earnings, fetchedAtMs, status } = cached;
      setSnapshot((prev) =>
        prev.earnings === earnings &&
        prev.fetchedAtMs === fetchedAtMs &&
        prev.status === status
          ? prev
          : { earnings, fetchedAtMs, status },
      );
    };
    if (cacheAddress === walletAddress && cached) {
      syncFromCache();
    } else {
      void fetchIntoCache(walletAddress).then(syncFromCache);
    }
    pushListeners.add(syncFromCache);
    return () => {
      alive = false;
      pushListeners.delete(syncFromCache);
    };
  }, [walletAddress]);

  return snapshot;
}
