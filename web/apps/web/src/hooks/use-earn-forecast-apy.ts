"use client";

import { toForecastApy } from "@/lib/kamino/earn-forecast.client";
import type { EarnForecastApy } from "@/lib/kamino/earn-forecast.shared";

import { useEarnForecastSummary } from "./use-earn-forecast-summary";

export function useEarnForecastApy(): {
  apy: EarnForecastApy | undefined;
  isLoaded: boolean;
} {
  const { summary, isLoaded } = useEarnForecastSummary();
  return {
    apy: summary ? toForecastApy(summary.forecast) : undefined,
    isLoaded,
  };
}
