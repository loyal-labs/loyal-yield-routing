"use client";

import {
  getStablecoinMintForCluster,
  resolveLoyalClusterForSolanaEnv,
  Stablecoin,
} from "@loyal-labs/actions";
import { resolveSolanaEnv } from "@loyal-labs/solana-rpc";
import { useMemo } from "react";

import { usePublicEnv } from "@/contexts/public-env-context";
import type { WalletDesktopData } from "@/hooks/use-wallet-desktop-data";
import {
  getStablecoinMintSetForSolanaEnv,
  isStablecoinMint,
} from "@/lib/wallet/stablecoin-classification";

export function useEarnUsdcSourceUsd(
  mainUsdcAmount: number | null,
  positions: WalletDesktopData["positions"],
  portfolioUsdFor: (position: WalletDesktopData["positions"][number]) => number
): number {
  const publicEnv = usePublicEnv();
  const usdcMint = getStablecoinMintForCluster(
    resolveLoyalClusterForSolanaEnv(resolveSolanaEnv(publicEnv.solanaEnv)),
    Stablecoin.USDC
  ).toBase58();
  if (mainUsdcAmount !== null) {
    return mainUsdcAmount;
  }
  const position = positions.find(
    (candidate) => candidate.asset.mint === usdcMint
  );
  return position ? portfolioUsdFor(position) : 0;
}

export function useStablecoinsUsd(
  positions: WalletDesktopData["positions"]
): number {
  const publicEnv = usePublicEnv();
  const stablecoinMints = useMemo(
    () => getStablecoinMintSetForSolanaEnv(publicEnv.solanaEnv),
    [publicEnv.solanaEnv]
  );
  return useMemo(
    () =>
      positions
        .filter((position) =>
          isStablecoinMint(position.asset.mint, stablecoinMints)
        )
        .reduce((sum, position) => sum + (position.totalValueUsd ?? 0), 0),
    [positions, stablecoinMints]
  );
}
