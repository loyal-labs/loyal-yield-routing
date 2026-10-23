import type {
  EarnApySample,
  EarnForecastSummary,
} from "@/lib/solana/earn/earn-api";
import {
  buildEarnForecastProjection,
  EARN_TBILL_APY_BPS,
  deriveMainUsdcReserveApyBps,
} from "@loyal-labs/shared";

export const TBILL_APY_BPS = EARN_TBILL_APY_BPS;

function findSeries(
  summary: EarnForecastSummary | null,
  key: "loyal" | "mainUsdcReserve",
): EarnApySample[] {
  const series = summary?.history.series?.find((s) => s.key === key);
  if (series?.samples?.length) {
    return series.samples;
  }
  if (key === "loyal" && summary?.history.samples?.length) {
    return summary.history.samples;
  }
  return [];
}

export function getLoyalSamples(
  summary: EarnForecastSummary | null,
): EarnApySample[] {
  return findSeries(summary, "loyal");
}

export function getMainSamples(
  summary: EarnForecastSummary | null,
): EarnApySample[] {
  return findSeries(summary, "mainUsdcReserve");
}

export function getLoyalApyBps(
  summary: EarnForecastSummary | null,
): number | null {
  const apyBps = summary?.forecast.apyBps;
  return apyBps !== undefined && Number.isFinite(apyBps) && apyBps >= 0
    ? apyBps
    : null;
}

export function getMainApyBps(summary: EarnForecastSummary | null): number {
  return deriveMainUsdcReserveApyBps(summary?.history);
}

export function resampleSamples(
  samples: EarnApySample[],
  n: number,
): EarnApySample[] {
  if (samples.length <= n) {
    return samples;
  }
  const out: EarnApySample[] = [];
  for (let i = 0; i < n; i += 1) {
    const idx = Math.round((i / (n - 1)) * (samples.length - 1));
    out.push(samples[idx]);
  }
  return out;
}

export const buildProjection = buildEarnForecastProjection;
