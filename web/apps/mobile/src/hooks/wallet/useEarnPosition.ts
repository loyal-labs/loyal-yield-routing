import { resolveLoyalClusterForSolanaEnv } from "@loyal-labs/actions";
import {
  type EarnRpcHolding,
  fetchEarnRpcHoldingsSnapshot,
} from "@loyal-labs/smart-account-vaults";
import { PublicKey } from "@solana/web3.js";
import { useCallback, useEffect, useRef, useState } from "react";

import {
  fetchEarnState,
  type EarnPosition,
  type EarnStateResponse,
} from "@/lib/solana/earn/earn-api";
import { getConnection, getSolanaEnv } from "@/lib/solana/rpc/connection";

const MUTATION_TRUST_MS = 20_000;

type LiveHoldings = { currentTotalAmountRaw: string; holdings: EarnRpcHolding[] };

function reconcileBalance(
  position: EarnPosition | null,
  liveTotalRaw: string | null,
  preferReadModel: boolean,
): EarnPosition | null {
  if (liveTotalRaw === null || preferReadModel) {
    return position;
  }
  if (position === null) {
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

async function readLiveHoldings(
  state: EarnStateResponse,
): Promise<LiveHoldings | null> {
  if (!state.policy || !state.programId || !state.settingsPda) {
    return null;
  }
  return fetchEarnRpcHoldingsSnapshot({
    cluster: resolveLoyalClusterForSolanaEnv(getSolanaEnv()),
    connection: getConnection(),
    policy: state.policy,
    programId: new PublicKey(state.programId),
    settingsPda: new PublicKey(state.settingsPda),
  });
}

export function useEarnPosition(walletAddress: string | null) {
  const [position, setPosition] = useState<EarnPosition | null>(null);
  const [holdings, setHoldings] = useState<EarnRpcHolding[]>([]);
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
        let state: EarnStateResponse;
        try {
          state = await fetchEarnState(walletAddress);
        } catch (error) {
          console.error("Failed to fetch Earn position", error);
          if (options?.throwOnError) {
            throw error;
          }
          return;
        }
        let live: LiveHoldings | null = null;
        try {
          live = await readLiveHoldings(state);
        } catch (error) {
          console.error("Failed to read Earn holdings", error);
        }
        if (fetchId !== fetchIdRef.current) {
          if (options?.throwOnError) {
            throw new Error("Earn position refresh was superseded.");
          }
          return;
        }
        setPolicyMissing(state.policy === null);
        if (live) {
          setHoldings(live.holdings);
        }
        setPosition(
          reconcileBalance(
            state.position,
            live?.currentTotalAmountRaw ?? null,
            Date.now() - mutatedAtRef.current < MUTATION_TRUST_MS,
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
