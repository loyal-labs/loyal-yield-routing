import type { EarnStatePolicy, EarnStatePosition } from "@loyal-labs/api";
import { resolveLoyalClusterForSolanaEnv } from "@loyal-labs/actions";
import {
  type EarnRpcHolding,
  type EarnRpcPolicyMetadata,
  fetchEarnRpcHoldingsSnapshot,
} from "@loyal-labs/smart-account-vaults";
import { PublicKey } from "@solana/web3.js";
import { useCallback, useEffect, useRef, useState } from "react";

import {
  EarnApiError,
  fetchEarnState,
  type EarnStateResponse,
} from "@/lib/solana/earn/earn-api";
import { deliberateSession } from "@/lib/solana/earn/earn-session";
import { getConnection, getSolanaEnv } from "@/lib/solana/rpc/connection";
import type { Signer } from "@/lib/wallet/signer";

type EarnPosition = Pick<
  EarnStatePosition,
  "currentTotalAmountRaw" | "principalAmountRaw" | "status"
>;

const MUTATION_TRUST_MS = 20_000;

type LiveHoldings = {
  currentTotalAmountRaw: string;
  holdings: EarnRpcHolding[];
};

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

function toRpcPolicy(policy: EarnStatePolicy): EarnRpcPolicyMetadata {
  return {
    account: policy.account,
    delegatedSigners: policy.delegatedSigners ?? undefined,
    id: policy.id,
    kaminoLiquidityMints: policy.kaminoLiquidityMints ?? undefined,
    kaminoMarkets: policy.kaminoMarkets ?? undefined,
    riskProfile: policy.riskProfile,
    routeModes: policy.routeModes ?? undefined,
    seed: policy.seed,
    setupPolicy: policy.setupPolicy
      ? {
          account: policy.setupPolicy.account,
          delegatedSigners: policy.setupPolicy.delegatedSigners ?? undefined,
          id: policy.setupPolicy.id,
          seed: policy.setupPolicy.seed,
        }
      : null,
    stableMints: policy.stableMints ?? undefined,
    universePreset: policy.universePreset,
    vaultIndex: policy.vaultIndex,
    vaultPubkey: policy.vaultPubkey,
  };
}

async function readLiveHoldings(
  state: EarnStateResponse
): Promise<LiveHoldings | null> {
  if (!state.policy || !state.programId || !state.settingsPda) {
    return null;
  }
  return fetchEarnRpcHoldingsSnapshot({
    cluster: resolveLoyalClusterForSolanaEnv(getSolanaEnv()),
    connection: getConnection(),
    policy: toRpcPolicy(state.policy),
    programId: new PublicKey(state.programId),
    settingsPda: new PublicKey(state.settingsPda),
  });
}

export function useEarnPosition(
  walletAddress: string | null,
  signer?: Signer | null
) {
  const [position, setPosition] = useState<EarnPosition | null>(null);
  const [holdings, setHoldings] = useState<EarnRpcHolding[]>([]);
  const [policyMissing, setPolicyMissing] = useState(false);
  const [isLoading, setIsLoading] = useState(false);
  const [loadedWallet, setLoadedWallet] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);
  const fetchIdRef = useRef(0);
  const signingWalletRef = useRef<string | null>(null);
  const mutatedAtRef = useRef(0);
  const walletRef = useRef(walletAddress);
  walletRef.current = walletAddress;
  const signerRef = useRef(signer);
  signerRef.current = signer;

  const markEarnMutation = useCallback(() => {
    mutatedAtRef.current = Date.now();
  }, []);

  const refreshEarnPosition = useCallback(
    async (options?: { signIn?: boolean; throwOnError?: boolean }) => {
      if (!walletAddress) {
        return;
      }
      if (signingWalletRef.current === walletAddress) {
        if (options?.throwOnError) {
          throw new Error("Earn sign-in is still in progress.");
        }
        return;
      }
      const fetchId = ++fetchIdRef.current;
      if (options?.signIn) signingWalletRef.current = walletAddress;
      setIsLoading(true);
      try {
        if (options?.signIn) {
          const signer = signerRef.current;
          if (
            !signer ||
            signer.publicKey.toBase58() !== walletAddress ||
            !(await deliberateSession(signer))
          ) {
            throw new EarnApiError(
              "Sign in to view your balance.",
              undefined,
              401
            );
          }
        }
        if (
          fetchId !== fetchIdRef.current ||
          walletRef.current !== walletAddress
        ) {
          return;
        }
        const state = await fetchEarnState(walletAddress);
        if (state.loadErrors?.position) {
          throw new Error("Earn position is unavailable.");
        }
        let live: LiveHoldings | null = null;
        try {
          live = await readLiveHoldings(state);
        } catch (error) {
          console.error("Failed to read Earn holdings", error);
        }
        if (
          fetchId !== fetchIdRef.current ||
          walletRef.current !== walletAddress
        ) {
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
            Date.now() - mutatedAtRef.current < MUTATION_TRUST_MS
          )
        );
        setError(null);
      } catch (error) {
        if (
          fetchId === fetchIdRef.current &&
          walletRef.current === walletAddress
        ) {
          console.error("Failed to fetch Earn position", error);
          setError(error);
        }
        if (options?.throwOnError) {
          throw error;
        }
      } finally {
        if (options?.signIn && signingWalletRef.current === walletAddress) {
          signingWalletRef.current = null;
        }
        if (
          fetchId === fetchIdRef.current &&
          walletRef.current === walletAddress
        ) {
          setIsLoading(false);
          setLoadedWallet(walletAddress);
        }
      }
    },
    [walletAddress]
  );

  useEffect(() => {
    setPosition(null);
    setHoldings([]);
    setPolicyMissing(false);
    setLoadedWallet(null);
    setError(null);
    setIsLoading(false);
    mutatedAtRef.current = 0;
    if (walletAddress) {
      void refreshEarnPosition();
    }
    return () => {
      fetchIdRef.current += 1;
    };
  }, [walletAddress, refreshEarnPosition]);

  const hasLoaded = walletAddress !== null && loadedWallet === walletAddress;
  const needsSignIn =
    hasLoaded && error instanceof EarnApiError && error.status === 401;
  return {
    position: hasLoaded ? position : null,
    holdings: hasLoaded ? holdings : [],
    policyMissing: hasLoaded && policyMissing,
    error:
      hasLoaded && error
        ? needsSignIn
          ? "Sign in to view your balance."
          : "Couldn't load your Earn balance."
        : null,
    needsSignIn,
    isLoading,
    hasLoaded,
    refreshEarnPosition,
    markEarnMutation,
  };
}
