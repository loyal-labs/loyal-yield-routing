import { Stablecoin } from "@loyal-labs/actions/types";

export const STABLECOIN_DECIMALS = 6;

const STABLECOIN_MINT_ADDRESSES: Record<Stablecoin, string> = {
  [Stablecoin.CASH]: "CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH",
  [Stablecoin.USDG]: "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH",
  [Stablecoin.PYUSD]: "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
  [Stablecoin.USDC]: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
  [Stablecoin.USDT]: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
  [Stablecoin.USDS]: "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA",
};

export const EARN_STABLECOIN_DESCRIPTORS = Object.freeze(
  Object.values(Stablecoin).map((symbol) => ({
    decimals: STABLECOIN_DECIMALS,
    mint: STABLECOIN_MINT_ADDRESSES[symbol],
    symbol,
  }))
);

export type EarnStablecoinSymbol = Stablecoin;
export type StablecoinHealthWarningCode = "no_eligible_reserve";

export type StablecoinHealthWarning = {
  code: StablecoinHealthWarningCode;
  level: "info" | "warning" | "critical";
  message: string;
};

export type StablecoinHealthWarningInput = {
  eligibleReserveCount: number;
  eligibilityReason: string;
  symbol: EarnStablecoinSymbol;
};

const DESCRIPTOR_BY_MINT = new Map(
  EARN_STABLECOIN_DESCRIPTORS.map((descriptor) => [descriptor.mint, descriptor])
);
const DESCRIPTOR_BY_SYMBOL = new Map(
  EARN_STABLECOIN_DESCRIPTORS.map((descriptor) => [
    descriptor.symbol,
    descriptor,
  ])
);

export function getEarnStablecoinByMint(mint: string) {
  return DESCRIPTOR_BY_MINT.get(mint) ?? null;
}

export function getEarnStablecoinBySymbol(symbol: string) {
  return DESCRIPTOR_BY_SYMBOL.get(symbol as EarnStablecoinSymbol) ?? null;
}

export function getEarnStablecoinSymbol(mint: string | null | undefined) {
  if (!mint) {
    return null;
  }

  return getEarnStablecoinByMint(mint)?.symbol ?? null;
}

export function deriveStablecoinHealthWarnings(
  input: StablecoinHealthWarningInput
): StablecoinHealthWarning[] {
  return input.eligibleReserveCount === 0
    ? [
        {
          code: "no_eligible_reserve",
          level: "critical",
          message: `${input.symbol} has no eligible Safe reserve: ${input.eligibilityReason}.`,
        },
      ]
    : [];
}
