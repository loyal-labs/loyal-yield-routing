export type {
  EarnForecastApyHistoryResponse,
  EarnForecastApyHistorySample,
  EarnForecastApyHistorySeries,
  EarnForecastResponse,
  EarnForecastSummaryResponse,
} from "@loyal-labs/shared";

export {
  formatEarnApyLabel,
  formatEarnApyPercent,
} from "@loyal-labs/shared";
export type { EarnForecastApy } from "@loyal-labs/shared";

export {
  deriveMainUsdcReserveApyBps as deriveMainUsdcReserveForecastApyBps,
  getEarnForecastTargetMultiplier,
} from "@loyal-labs/shared";
