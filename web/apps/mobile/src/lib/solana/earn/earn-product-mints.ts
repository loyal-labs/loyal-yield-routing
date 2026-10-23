import {
  getStablecoinMintForCluster,
  getStablecoinsForCluster,
  getStablecoinTokenProgramForCluster,
  LoyalCluster,
  STABLECOIN_TOKEN_PROGRAMS,
  Stablecoin,
} from "@loyal-labs/actions";
import type { PublicKey } from "@solana/web3.js";

export const EARN_PRODUCT_DECIMALS = 6;

export type EarnProductAsset = {
  symbol: string;
  mint: string;
  tokenProgramId: PublicKey;
};

function assetsForCluster(cluster: LoyalCluster): EarnProductAsset[] {
  return getStablecoinsForCluster(cluster).map((stablecoin) => ({
    symbol: stablecoin,
    mint: getStablecoinMintForCluster(cluster, stablecoin).toBase58(),
    tokenProgramId: getStablecoinTokenProgramForCluster(cluster, stablecoin),
  }));
}

const MAINNET_ASSETS = assetsForCluster(LoyalCluster.MainnetBeta);
const DEVNET_ASSETS = assetsForCluster(LoyalCluster.Devnet);

export function getEarnProductAssets(solanaEnv: string): EarnProductAsset[] {
  if (solanaEnv === "devnet") {
    return DEVNET_ASSETS;
  }
  if (solanaEnv === "localnet") {
    console.warn(
      "[earn] localnet has no Earn product mints; showing the mainnet catalog",
    );
  }
  return MAINNET_ASSETS;
}

export function tokenProgramForEarnMint(mint: string): PublicKey {
  const asset =
    MAINNET_ASSETS.find((candidate) => candidate.mint === mint) ??
    DEVNET_ASSETS.find((candidate) => candidate.mint === mint);
  if (!asset) {
    console.warn(
      `[earn] ${mint} is not in the Earn product catalog; assuming the classic token program`,
    );
    return STABLECOIN_TOKEN_PROGRAMS[Stablecoin.USDC];
  }
  return asset.tokenProgramId;
}
