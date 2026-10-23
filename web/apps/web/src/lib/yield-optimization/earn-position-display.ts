import {
  KAMINO_DEVNET_MAIN_MARKET,
  KAMINO_ETHENA_MARKET,
  KAMINO_FIGURE_MARKET,
  KAMINO_MAIN_MARKET,
  KAMINO_MAPLE_MARKET,
  KAMINO_ONRE_MARKET,
} from "@loyal-labs/actions";

export {
  resolveEarnPositionDisplay,
  resolveEarnTransactionMarketLabel,
} from "@loyal-labs/smart-account-vaults";
export type { EarnPositionDisplay } from "@loyal-labs/smart-account-vaults";

const DEFAULT_MARKET_ICON = "/wallet-workspace/earn-kamino.png";

const MARKET_ICONS = new Map<string, string>([
  [KAMINO_MAIN_MARKET.toBase58(), DEFAULT_MARKET_ICON],
  [KAMINO_DEVNET_MAIN_MARKET.toBase58(), DEFAULT_MARKET_ICON],
  [KAMINO_FIGURE_MARKET.toBase58(), "/wallet-workspace/earn-prime.png"],
  [KAMINO_MAPLE_MARKET.toBase58(), "/wallet-workspace/earn-maple.svg"],
  [KAMINO_ONRE_MARKET.toBase58(), "/wallet-workspace/earn-onre.png"],
  [KAMINO_ETHENA_MARKET.toBase58(), "/wallet-workspace/earn-ethena.png"],
]);

export function resolveEarnTransactionMarketIcon(args: {
  market: string | null | undefined;
}): string {
  return args.market
    ? MARKET_ICONS.get(args.market) ?? DEFAULT_MARKET_ICON
    : DEFAULT_MARKET_ICON;
}
