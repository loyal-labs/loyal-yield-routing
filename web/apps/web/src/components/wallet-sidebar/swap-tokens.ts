import type { PortfolioPosition } from "@loyal-labs/solana-wallet";

import { LOYL_LIST_INDEX } from "@/hooks/use-wallet-desktop-data";
import { getTokenIconUrl } from "@/lib/token-icon";

import {
  LOYL_TOKEN,
  type SwapToken,
  swapTokens as fallbackSwapTokens,
} from "./types";

export function deriveSwapTokens(
  positions: readonly PortfolioPosition[] | undefined
): SwapToken[] {
  if (!positions || positions.length === 0) {
    return fallbackSwapTokens;
  }

  const tokens: SwapToken[] = positions
    .filter(
      (position) =>
        position.publicBalance > 0 ||
        ["SOL", "USDC"].includes(position.asset.symbol)
    )
    .map((position) => ({
      balance: position.publicBalance,
      icon: position.asset.imageUrl ?? getTokenIconUrl(position.asset.symbol),
      mint: position.asset.mint,
      price: position.priceUsd ?? 0,
      symbol: position.asset.symbol,
    }));

  if (!tokens.some((token) => token.mint === LOYL_TOKEN.mint)) {
    const loylPosition = positions.find(
      (position) => position.asset.mint === LOYL_TOKEN.mint
    );
    tokens.splice(
      LOYL_LIST_INDEX,
      0,
      loylPosition
        ? {
            ...LOYL_TOKEN,
            balance: loylPosition.publicBalance,
            price: loylPosition.priceUsd ?? 0,
          }
        : LOYL_TOKEN
    );
  }

  return tokens;
}

export function selectSwapToken(
  side: "from" | "to",
  token: SwapToken,
  pair: { from: SwapToken; to: SwapToken }
): { from: SwapToken; to: SwapToken } {
  if (side === "from") {
    return {
      from: token,
      to: pair.to.symbol === token.symbol ? pair.from : pair.to,
    };
  }
  return {
    from: pair.from.symbol === token.symbol ? pair.to : pair.from,
    to: token,
  };
}
