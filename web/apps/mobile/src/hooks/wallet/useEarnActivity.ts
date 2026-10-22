import { useCallback, useEffect, useRef, useState } from "react";

import {
  fetchEarnTransactions,
  type EarnTransactionItem,
} from "@/lib/solana/earn/earn-api";
import { mmkv } from "@/lib/storage";

const cacheKey = (walletAddress: string): string => `earn:txns:${walletAddress}`;

function readCachedEarnTransactions(
  walletAddress: string,
): EarnTransactionItem[] | null {
  const raw = mmkv.getString(cacheKey(walletAddress));
  if (!raw) return null;
  try {
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as EarnTransactionItem[]) : null;
  } catch {
    return null;
  }
}

function writeCachedEarnTransactions(
  walletAddress: string,
  transactions: EarnTransactionItem[],
): void {
  mmkv.setString(cacheKey(walletAddress), JSON.stringify(transactions));
}

export function useEarnActivity(walletAddress: string | null) {
  const [earnTransactions, setEarnTransactions] = useState<
    EarnTransactionItem[]
  >([]);
  const [isLoading, setIsLoading] = useState(false);
  const [hasLoaded, setHasLoaded] = useState(false);
  const fetchIdRef = useRef(0);

  const refresh = useCallback(async () => {
    if (!walletAddress) return;
    const fetchId = ++fetchIdRef.current;
    setIsLoading(true);
    try {
      const res = await fetchEarnTransactions(walletAddress);
      if (fetchId === fetchIdRef.current) {
        setEarnTransactions(res.transactions);
        writeCachedEarnTransactions(walletAddress, res.transactions);
      }
    } catch (error) {
      console.warn("Failed to fetch Earn transactions", error);
    } finally {
      if (fetchId === fetchIdRef.current) {
        setIsLoading(false);
        setHasLoaded(true);
      }
    }
  }, [walletAddress]);

  useEffect(() => {
    if (!walletAddress) {
      setEarnTransactions([]);
      setHasLoaded(false);
      return;
    }
    const cached = readCachedEarnTransactions(walletAddress);
    setEarnTransactions(cached ?? []);
    setHasLoaded(cached !== null);
    void refresh();
  }, [walletAddress, refresh]);

  return { earnTransactions, isLoading, hasLoaded, refresh };
}
