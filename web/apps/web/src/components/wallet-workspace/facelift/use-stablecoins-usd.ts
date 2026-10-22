"use client";

import { useMemo } from "react";

import { usePublicEnv } from "@/contexts/public-env-context";
import type { WalletDesktopData } from "@/hooks/use-wallet-desktop-data";
import {
  getStablecoinMintSetForSolanaEnv,
  isStablecoinMint,
} from "@/lib/wallet/stablecoin-classification";

// Same stablecoin bucketing the sidebar's Stablecoins cell uses.
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
