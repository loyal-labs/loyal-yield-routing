import {
  getStablecoinMintForCluster,
  getStablecoinsForCluster,
  getStablecoinTokenProgramForCluster,
  type LoyalCluster,
  Stablecoin,
} from "@loyal-labs/actions";
import { PublicKey } from "@solana/web3.js";

export const EARN_PRODUCT_STABLECOINS = [
  Stablecoin.CASH,
  Stablecoin.USDG,
  Stablecoin.PYUSD,
  Stablecoin.USDC,
  Stablecoin.USDT,
  Stablecoin.USDS,
] as const;

export type EarnProductStablecoin = (typeof EARN_PRODUCT_STABLECOINS)[number];

export interface EarnProductAsset {
  decimals: 6;
  mint: PublicKey;
  stablecoin: EarnProductStablecoin;
  symbol: EarnProductStablecoin;
  tokenProgramId: PublicKey;
}

export function getEarnProductAssetsForCluster(
  cluster: LoyalCluster
): readonly EarnProductAsset[] {
  const clusterStablecoins = new Set(getStablecoinsForCluster(cluster));
  return EARN_PRODUCT_STABLECOINS.filter((stablecoin) =>
    clusterStablecoins.has(stablecoin)
  ).map((stablecoin) => ({
    decimals: 6,
    mint: getStablecoinMintForCluster(cluster, stablecoin),
    stablecoin,
    symbol: stablecoin,
    tokenProgramId: getStablecoinTokenProgramForCluster(cluster, stablecoin),
  }));
}

export function resolveEarnProductAsset(args: {
  cluster: LoyalCluster;
  mint: string | PublicKey;
}): EarnProductAsset {
  const mint =
    typeof args.mint === "string" ? new PublicKey(args.mint) : args.mint;
  const asset = getEarnProductAssetsForCluster(args.cluster).find((candidate) =>
    candidate.mint.equals(mint)
  );
  if (!asset) {
    throw new Error("mint is not a supported Earn product mint.");
  }
  return asset;
}

/** The browser sends only money intent. Trusted code derives all metadata. */
export function buildEarnDepositIntent(args: {
  amountRaw: bigint;
  mint: PublicKey;
}): { amountRaw: string; mint: string } {
  if (args.amountRaw <= BigInt(0)) {
    throw new Error("amountRaw must be greater than 0.");
  }
  return { amountRaw: args.amountRaw.toString(), mint: args.mint.toBase58() };
}
