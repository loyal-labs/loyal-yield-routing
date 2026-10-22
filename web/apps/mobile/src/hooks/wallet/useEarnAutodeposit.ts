import { useCallback, useEffect, useRef, useState } from "react";

import {
  fetchEarnAutodepositState,
  type EarnAutodepositState,
} from "@/lib/solana/earn/earn-api";

export function useEarnAutodeposit(walletAddress: string | null) {
  const [autodeposit, setAutodeposit] = useState<EarnAutodepositState | null>(
    null,
  );
  const [isLoading, setIsLoading] = useState(false);
  const [hasLoaded, setHasLoaded] = useState(false);
  const fetchIdRef = useRef(0);
  const latestRequestRef = useRef<Promise<EarnAutodepositState | null> | null>(
    null,
  );

  const refreshAutodeposit = useCallback(
    async (options?: {
      throwOnError?: boolean;
    }): Promise<EarnAutodepositState | null> => {
      if (!walletAddress) {
        return null;
      }
      const fetchId = ++fetchIdRef.current;
      setIsLoading(true);
      const request = (async () => {
        try {
          const state = await fetchEarnAutodepositState(walletAddress);
          if (fetchId === fetchIdRef.current) {
            setAutodeposit(state.autodeposit);
          }
          return state.autodeposit;
        } finally {
          if (fetchId === fetchIdRef.current) {
            setIsLoading(false);
            setHasLoaded(true);
          }
        }
      })();
      latestRequestRef.current = request;
      try {
        let newest = request;
        await newest.catch(() => undefined);
        while (latestRequestRef.current !== newest) {
          newest = latestRequestRef.current!;
          await newest.catch(() => undefined);
        }
        return await newest;
      } catch (error) {
        console.error("Failed to fetch Autodeposit state", error);
        if (options?.throwOnError) {
          throw error;
        }
        return null;
      }
    },
    [walletAddress],
  );

  useEffect(() => {
    if (walletAddress) {
      refreshAutodeposit();
    } else {
      setAutodeposit(null);
      setHasLoaded(false);
    }
  }, [walletAddress, refreshAutodeposit]);

  return { autodeposit, isLoading, hasLoaded, refreshAutodeposit };
}
