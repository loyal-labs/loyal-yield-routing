import type { EarnStatePosition } from "@loyal-labs/api";
import type { EarnRpcHolding } from "@loyal-labs/smart-account-vaults";
import { useCallback, useEffect, useRef, useState } from "react";
import { fetchMobileEarnState, fetchMobileEarnHoldings } from "@/lib/solana/earn/earn-api";

type EarnPosition = Pick<
  EarnStatePosition,
  "currentTotalAmountRaw" | "principalAmountRaw" | "status"
>;

const MUTATION_TRUST_MS = 20_000;

function reconcileBalance(
  position: EarnPosition | null,
  liveTotalRaw: string | null,
  preferReadModel: boolean
): EarnPosition | null {
  if (liveTotalRaw === null || preferReadModel) {
    return position;
  }
  if (position === null) {
    return Number(liveTotalRaw) > 0
      ? {
          currentTotalAmountRaw: liveTotalRaw,
          principalAmountRaw: liveTotalRaw,
          status: "active",
        }
      : null;
  }
  return { ...position, currentTotalAmountRaw: liveTotalRaw };
}

const EMPTY_POSITION = {
  position: null as EarnPosition | null,
  holdings: [] as EarnRpcHolding[],
  policyMissing: false,
  isLoading: false,
  hasLoaded: false,
  error: null as string | null,
};

export function useEarnPosition(
  walletAddress: string | null
) {
  const [snapshot, setSnapshot] = useState({
    walletAddress,
    ...EMPTY_POSITION,
  });
  const currentWallet = useRef(walletAddress);
  currentWallet.current = walletAddress;
  const fetchIdRef = useRef(0);
  const mutatedAtRef = useRef(0);

  const markEarnMutation = useCallback(() => {
    mutatedAtRef.current = Date.now();
  }, []);

  const refreshEarnPosition = useCallback(
    async (options?: { throwOnError?: boolean }) => {
      if (!walletAddress || currentWallet.current !== walletAddress) return;
      const fetchId = ++fetchIdRef.current;
      const isCurrent = () =>
        currentWallet.current === walletAddress &&
        fetchId === fetchIdRef.current;
      setSnapshot((prev) => ({
        ...(prev.walletAddress === walletAddress ? prev : EMPTY_POSITION),
        walletAddress,
        isLoading: true,
      }));
      try {
        const [state, live] = await Promise.all([
          fetchMobileEarnState(walletAddress),
          fetchMobileEarnHoldings(walletAddress),
        ]);
        if (!isCurrent()) {
          if (options?.throwOnError)
            throw new Error("Earn position refresh was superseded.");
          return;
        }
        setSnapshot({
          walletAddress,
          position: reconcileBalance(
            state.position ? { ...state.position, currentTotalAmountRaw: state.position.currentAmountRaw } : null,
            live.observedAt ? live.currentTotalAmountRaw : null,
            Date.now() - mutatedAtRef.current < MUTATION_TRUST_MS
          ),
          holdings: live.holdings,
          policyMissing: live.observedAt === null,
          isLoading: false,
          hasLoaded: true,
          error: null,
        });
      } catch (error) {
        if (isCurrent()) {
          setSnapshot({
            ...EMPTY_POSITION,
            walletAddress,
            hasLoaded: true,
            error: error instanceof Error
              ? error.message
              : "Unable to load Earn holdings.",
          });
        }
        if (options?.throwOnError) throw error;
      }
    },
    [walletAddress]
  );

  useEffect(() => {
    mutatedAtRef.current = 0;
    setSnapshot({ walletAddress, ...EMPTY_POSITION });
    void refreshEarnPosition();
    return () => {
      fetchIdRef.current += 1;
    };
  }, [walletAddress, refreshEarnPosition]);

  const visible =
    snapshot.walletAddress === walletAddress ? snapshot : EMPTY_POSITION;
  return { ...visible, refreshEarnPosition, markEarnMutation };
}
