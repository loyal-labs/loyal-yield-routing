import { useCallback, useEffect, useRef, useState } from "react";

import {
  fetchEarnHoldings,
  fetchEarnState,
  type EarnHoldingItem,
  type EarnPosition,
} from "@/lib/solana/earn/earn-api";

// After a local deposit/withdraw, the backend read-model (`/state`
// `currentAmountRaw`) is written synchronously by the confirm call, so it's the
// correct intended balance immediately. The live `/holdings` read, by contrast,
// LAGS the chain mid-sweep — it dips below the deposit right after a deposit
// (funds still moving idle→Kamino) and reads high right after a withdraw. So for
// a short window after a mutation we trust the read-model; outside it we trust
// the live holdings total, which tracks market drift the read-model misses (the
// reason the live override exists at all). This mirrors the web showing the
// correct value the moment a deposit lands instead of a transient dip.
const MUTATION_TRUST_MS = 20_000;

function reconcileBalance(
  position: EarnPosition | null,
  liveTotalRaw: string | null,
  preferReadModel: boolean,
): EarnPosition | null {
  if (liveTotalRaw === null || preferReadModel) {
    return position;
  }
  if (position === null) {
    // Read-model gap: the vault holds funds on-chain but the deposit was never
    // projected (e.g. first deposit after a full exit, wallet 8p6b… 2026-09-11).
    // Surface the live balance so Withdraw stays reachable instead of showing
    // $0.00 and hiding the user's funds. APY/principal are unknown here.
    return Number(liveTotalRaw) > 0
      ? {
          currentAmountRaw: liveTotalRaw,
          currentSupplyApyBps: null,
          principalAmountRaw: liveTotalRaw,
          status: "active",
        }
      : null;
  }
  return { ...position, currentAmountRaw: liveTotalRaw };
}

export function useEarnPosition(walletAddress: string | null) {
  const [position, setPosition] = useState<EarnPosition | null>(null);
  const [holdings, setHoldings] = useState<EarnHoldingItem[]>([]);
  // True when the last holdings read hit the server's no-active-policy branch
  // (observedAt === null): the route-policy pair is missing — never created,
  // or torn down by a full exit — so the next deposit re-runs first-time setup
  // and needs the setup SOL even if a position balance exists. Stays false on
  // fetch failures (fail-open, like the SOL gate itself).
  const [policyMissing, setPolicyMissing] = useState(false);
  const [isLoading, setIsLoading] = useState(false);
  const [hasLoaded, setHasLoaded] = useState(false);
  const fetchIdRef = useRef(0);
  const mutatedAtRef = useRef(0);

  const markEarnMutation = useCallback(() => {
    mutatedAtRef.current = Date.now();
  }, []);

  const refreshEarnPosition = useCallback(
    async (options?: { throwOnError?: boolean }) => {
      if (!walletAddress) {
        return;
      }
      const fetchId = ++fetchIdRef.current;
      setIsLoading(true);
      try {
        const [stateResult, holdingsResult] = await Promise.allSettled([
          fetchEarnState(walletAddress),
          fetchEarnHoldings(walletAddress),
        ]);
        if (fetchId !== fetchIdRef.current) {
          if (options?.throwOnError) {
            throw new Error("Earn position refresh was superseded.");
          }
          return;
        }
        if (stateResult.status === "rejected") {
          console.error("Failed to fetch Earn position", stateResult.reason);
          if (options?.throwOnError) {
            throw stateResult.reason;
          }
          return;
        }
        let liveTotalRaw: string | null = null;
        if (holdingsResult.status === "fulfilled") {
          // observedAt is null only when the server skipped the chain read (no
          // active Earn policy row yet) and returned a placeholder "0" — treat
          // that as "live read unavailable" so it can't zero a real read-model
          // balance. A genuine snapshot always carries observedAt, even at $0.
          if (holdingsResult.value.observedAt !== null) {
            liveTotalRaw = holdingsResult.value.currentTotalAmountRaw;
          }
          setPolicyMissing(holdingsResult.value.observedAt === null);
          setHoldings(holdingsResult.value.holdings);
        } else {
          console.error("Failed to fetch Earn holdings", holdingsResult.reason);
        }
        const preferReadModel =
          Date.now() - mutatedAtRef.current < MUTATION_TRUST_MS;
        setPosition(
          reconcileBalance(
            stateResult.value.position,
            liveTotalRaw,
            preferReadModel,
          ),
        );
      } finally {
        if (fetchId === fetchIdRef.current) {
          setIsLoading(false);
          setHasLoaded(true);
        }
      }
    },
    [walletAddress],
  );

  useEffect(() => {
    if (walletAddress) {
      refreshEarnPosition();
    } else {
      setPosition(null);
      setHoldings([]);
      setPolicyMissing(false);
      setHasLoaded(false);
    }
  }, [walletAddress, refreshEarnPosition]);

  return {
    position,
    holdings,
    policyMissing,
    isLoading,
    hasLoaded,
    refreshEarnPosition,
    markEarnMutation,
  };
}
