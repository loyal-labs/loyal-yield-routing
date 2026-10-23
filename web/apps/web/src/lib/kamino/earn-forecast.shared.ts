export type {
  EarnForecastApyHistoryResponse,
  EarnForecastApyHistorySample,
  EarnForecastApyHistorySeries,
  EarnForecastResponse,
  EarnForecastSummaryResponse,
} from "@loyal-labs/shared";

import type { EarnForecastResponse } from "@loyal-labs/shared";

export type EarnForecastApy = Pick<
  EarnForecastResponse,
  "apyBps" | "availability" | "rangeHighBps" | "rangeLowBps" | "source"
>;

export function formatEarnApyLabel(apy: EarnForecastApy | undefined): string {
  if (!apy || apy.availability === "unavailable") {
    return "APY unavailable";
  }
  const { apyBps, availability, source } = apy;
  const qualifiers = [
    ...(source === "live" ? ["6–24h live"] : []),
    ...(availability === "stale" ? ["stale"] : []),
  ];
  if (qualifiers.length > 0) {
    return `${(apyBps / 100).toFixed(2)}% APY (${qualifiers.join(", ")})`;
  }
  return `${(apyBps / 100).toFixed(2)}% APY`;
}

export function formatEarnApyPercent(apyBps: number): string {
  return `${(apyBps / 100).toFixed(2)}%`;
}

export function getEarnForecastTargetMultiplier(apyBps: number): number {
  return 1 + apyBps / 10_000;
}
