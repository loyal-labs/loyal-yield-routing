import { describe, expect, test } from "bun:test";
import { LoyalCluster, Stablecoin } from "@loyal-labs/actions";
import { TOKEN_2022_PROGRAM_ID, TOKEN_PROGRAM_ID } from "@solana/spl-token";
import { parseEarnDepositPrepareRequestBody } from "./earn-deposit-prepare-contracts.shared";
import {
  buildEarnDepositIntent,
  getEarnProductAssetsForCluster,
  resolveEarnProductAsset,
} from "./earn-product-mints.shared";

describe("Earn asset registry and public deposit intent", () => {
  test("contains exactly the six mainnet products with trusted programs", () => {
    const assets = getEarnProductAssetsForCluster(LoyalCluster.MainnetBeta);
    expect(assets.map((asset) => asset.symbol)).toEqual([
      Stablecoin.CASH,
      Stablecoin.USDG,
      Stablecoin.PYUSD,
      Stablecoin.USDC,
      Stablecoin.USDT,
      Stablecoin.USDS,
    ]);
    expect(assets.map((asset) => asset.tokenProgramId.toBase58())).toEqual([
      TOKEN_2022_PROGRAM_ID.toBase58(),
      TOKEN_2022_PROGRAM_ID.toBase58(),
      TOKEN_2022_PROGRAM_ID.toBase58(),
      TOKEN_PROGRAM_ID.toBase58(),
      TOKEN_PROGRAM_ID.toBase58(),
      TOKEN_PROGRAM_ID.toBase58(),
    ]);
    expect(assets.every((asset) => asset.decimals === 6)).toBe(true);
  });

  test("browser intent contains only mint and raw amount", () => {
    for (const product of getEarnProductAssetsForCluster(
      LoyalCluster.MainnetBeta
    )) {
      const asset = resolveEarnProductAsset({
        cluster: LoyalCluster.MainnetBeta,
        mint: product.mint,
      });
      const intent = buildEarnDepositIntent({
        amountRaw: BigInt(1_000_000),
        mint: asset.mint,
      });
      expect(Object.keys(intent).sort()).toEqual(["amountRaw", "mint"]);
      expect(parseEarnDepositPrepareRequestBody(intent)).toEqual({
        amountRaw: BigInt(1_000_000),
        mint: asset.mint.toBase58(),
      });
    }
  });

});
