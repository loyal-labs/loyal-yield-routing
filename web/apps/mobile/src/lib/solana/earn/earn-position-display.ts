import type { EarnPositionDisplay } from "@loyal-labs/api";
import {
  KAMINO_BITCOIN_MARKET,
  KAMINO_DEVNET_MAIN_MARKET,
  KAMINO_ETHENA_MARKET,
  KAMINO_FIGURE_MARKET,
  KAMINO_HUMA_MARKET,
  KAMINO_JLP_MARKET,
  KAMINO_MAIN_MARKET,
  KAMINO_MAPLE_MARKET,
  KAMINO_ONRE_MARKET,
  KAMINO_SOLSTICE_MARKET,
  KAMINO_SUPERSTATE_OPENING_BELL_MARKET,
  LoyalCluster,
  STABLECOIN_MINTS_BY_CLUSTER,
} from "@loyal-labs/actions";

export type EarnVenueKey = "kamino" | "prime" | "maple" | "onre" | "ethena";

const MARKET_NAMES: Record<string, string> = {
  [KAMINO_MAIN_MARKET.toBase58()]: "Main Kamino",
  [KAMINO_DEVNET_MAIN_MARKET.toBase58()]: "Main Kamino",
  [KAMINO_FIGURE_MARKET.toBase58()]: "Prime Market",
  [KAMINO_MAPLE_MARKET.toBase58()]: "Maple Market",
  [KAMINO_ONRE_MARKET.toBase58()]: "OnRe Market",
  [KAMINO_ETHENA_MARKET.toBase58()]: "Ethena Market",
  [KAMINO_JLP_MARKET.toBase58()]: "JLP Market",
  [KAMINO_BITCOIN_MARKET.toBase58()]: "Bitcoin Market",
  [KAMINO_SUPERSTATE_OPENING_BELL_MARKET.toBase58()]: "Superstate Market",
  [KAMINO_HUMA_MARKET.toBase58()]: "Huma Market",
  [KAMINO_SOLSTICE_MARKET.toBase58()]: "Solstice Market",
};

const MARKET_VENUES: Record<string, EarnVenueKey> = {
  [KAMINO_FIGURE_MARKET.toBase58()]: "prime",
  [KAMINO_MAPLE_MARKET.toBase58()]: "maple",
  [KAMINO_ONRE_MARKET.toBase58()]: "onre",
  [KAMINO_ETHENA_MARKET.toBase58()]: "ethena",
};

const MINT_SYMBOLS: Record<string, string> = Object.fromEntries(
  Object.values(LoyalCluster).flatMap((cluster) =>
    Object.entries(STABLECOIN_MINTS_BY_CLUSTER[cluster]).map(
      ([symbol, mint]) => [mint.toBase58(), symbol],
    ),
  ),
);

function shortenAddress(address: string): string {
  return address.length <= 10
    ? address
    : `${address.slice(0, 4)}…${address.slice(-4)}`;
}

type ResolvedEarnPositionDisplay = Pick<
  EarnPositionDisplay,
  "marketName" | "mintSymbol"
> & { venue: EarnVenueKey };

export function resolveEarnPositionDisplay(args: {
  market: string | null;
  liquidityMint: string;
}): ResolvedEarnPositionDisplay {
  const mintSymbol =
    MINT_SYMBOLS[args.liquidityMint] ?? shortenAddress(args.liquidityMint);

  if (!args.market) {
    return { marketName: "Idle balance", mintSymbol, venue: "kamino" };
  }

  return {
    marketName: MARKET_NAMES[args.market] ?? shortenAddress(args.market),
    mintSymbol,
    venue: MARKET_VENUES[args.market] ?? "kamino",
  };
}
