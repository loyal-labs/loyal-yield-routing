import { useEffect, useMemo, useRef, useState } from "react";

import {
  LOYAL_TOKEN_MINT,
  NATIVE_SOL_MINT,
  SOLANA_USDC_MINT_DEVNET,
  SOLANA_USDC_MINT_MAINNET,
} from "@/lib/solana/constants";
import { getSolanaEnv } from "@/lib/solana/rpc/connection";
import {
  fetchTokenDetailMarket,
  type MobileTokenDetailResponse,
} from "@/services/api";

export type TokenDetailsByMint = Record<
  string,
  MobileTokenDetailResponse | undefined
>;

// CoinGecko sometimes returns a first response before priceChange24h has been
// computed. Retry a couple of times so the UI avoids flickering "—" for 24h.
async function fetchTokenDetailWithRetry(
  mint: string,
  maxAttempts = 3,
  retryDelayMs = 250,
): Promise<MobileTokenDetailResponse> {
  let lastDetail: MobileTokenDetailResponse | null = null;
  let lastError: unknown = null;

  for (let attempt = 1; attempt <= maxAttempts; attempt += 1) {
    try {
      const detail = await fetchTokenDetailMarket(mint);
      lastDetail = detail;
      const change = detail.market.priceChange24hPercent;
      if (typeof change === "number" && Number.isFinite(change)) {
        return detail;
      }
    } catch (error) {
      lastError = error;
    }

    if (attempt < maxAttempts && retryDelayMs > 0) {
      await new Promise((resolve) => setTimeout(resolve, retryDelayMs));
    }
  }

  if (lastDetail) return lastDetail;
  throw lastError ?? new Error("Failed to fetch token detail");
}

export function useTokenDetailMints(
  tokenHoldings: readonly { mint: string }[],
  shieldedBalances?: readonly { tokenMint: string }[],
): string[] {
  return useMemo(() => {
    const mints = new Set<string>([
      NATIVE_SOL_MINT,
      LOYAL_TOKEN_MINT,
      getSolanaEnv() === "mainnet"
        ? SOLANA_USDC_MINT_MAINNET
        : SOLANA_USDC_MINT_DEVNET,
    ]);
    for (const holding of tokenHoldings) mints.add(holding.mint);
    if (shieldedBalances !== undefined) {
      for (const balance of shieldedBalances) mints.add(balance.tokenMint);
    }
    return Array.from(mints);
  }, [tokenHoldings, shieldedBalances]);
}

export function useTokenDetails(
  mints: string[],
  resetKey: number = 0,
): TokenDetailsByMint {
  const mintsKey = useMemo(
    () => Array.from(new Set(mints)).sort().join("|"),
    [mints],
  );

  const [detailsByMint, setDetailsByMint] = useState<TokenDetailsByMint>({});

  const attemptedRef = useRef<Set<string>>(new Set());

  // Keep reset synchronous with the fetch effect below so that bumping
  // `resetKey` (manual pull-to-refresh, network switch) always triggers a
  // re-fetch. If we split reset into its own effect, the fetch effect's
  // `mintsKey` dep wouldn't have changed, and the UI would get stuck
  // showing skeletons against an empty state until `mintsKey` next shifts.
  const lastResetKeyRef = useRef(resetKey);

  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  useEffect(() => {
    if (lastResetKeyRef.current !== resetKey) {
      lastResetKeyRef.current = resetKey;
      attemptedRef.current = new Set();
    }

    if (mintsKey.length === 0) return;

    const uniqueMints = mintsKey.split("|");
    const missing = uniqueMints.filter(
      (mint) => !attemptedRef.current.has(mint),
    );
    if (missing.length === 0) return;

    for (const mint of missing) attemptedRef.current.add(mint);

    void Promise.allSettled(
      missing.map(async (mint) => {
        try {
          const detail = await fetchTokenDetailWithRetry(mint);
          return { mint, detail, ok: true as const };
        } catch (error) {
          console.error("[token-detail] fetch failed", {
            mint,
            error: error instanceof Error ? error.message : String(error),
          });
          return { mint, ok: false as const };
        }
      }),
    ).then((results) => {
      if (!mountedRef.current) return;
      const successes = results.flatMap((r) =>
        r.status === "fulfilled" && r.value.ok
          ? [[r.value.mint, r.value.detail] as const]
          : [],
      );
      if (successes.length === 0) return;
      setDetailsByMint((current) => {
        const next = { ...current };
        for (const [mint, detail] of successes) next[mint] = detail;
        return next;
      });
    });
  }, [mintsKey, resetKey]);

  return detailsByMint;
}
