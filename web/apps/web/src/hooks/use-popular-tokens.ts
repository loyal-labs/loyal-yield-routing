"use client";

import type { SwapToken } from "@loyal-labs/wallet-core/types";
import { useEffect, useState } from "react";

import {
  getCachedTrendingTokens,
  prefetchTrendingTokens,
  resetTokenSearchCacheForTests,
} from "@/lib/market/token-search.client";

export async function fetchPopularTokens(): Promise<SwapToken[]> {
  return prefetchTrendingTokens();
}

export function resetPopularTokensCacheForTests() {
  resetTokenSearchCacheForTests();
}

export function usePopularTokens(options: { enabled?: boolean } = {}) {
  const enabled = options.enabled ?? true;
  const [tokens, setTokens] = useState<SwapToken[]>(
    getCachedTrendingTokens() ?? []
  );
  const [isLoading, setIsLoading] = useState(
    enabled && !getCachedTrendingTokens()
  );

  useEffect(() => {
    if (!enabled) {
      setIsLoading(false);
      return;
    }

    let cancelled = false;
    setIsLoading(!getCachedTrendingTokens());
    void fetchPopularTokens()
      .then((result) => {
        if (!cancelled) setTokens(result);
      })
      .finally(() => {
        if (!cancelled) setIsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [enabled]);

  return { tokens, isLoading };
}
