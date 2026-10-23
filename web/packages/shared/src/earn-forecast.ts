export const EARN_TBILL_APY_BPS = 365;
export const EARN_MAIN_USDC_RESERVE_FALLBACK_APY_BPS = 559;
export const EARN_FORECAST_MIN_APY_BPS = 50;
export const EARN_FORECAST_EASE_EXPONENT = 1.08;

export type EarnApyHistorySeriesSample = {
  apyBps: number;
};

export type EarnApyHistorySeriesLike = {
  key: string;
  samples: readonly EarnApyHistorySeriesSample[];
};

export function clampEarnForecastApyBps(apyBps: number): number {
  return Math.max(
    Number.isFinite(apyBps) ? apyBps : 0,
    EARN_FORECAST_MIN_APY_BPS,
  );
}

export function getEarnForecastTargetMultiplier(apyBps: number): number {
  return 1 + apyBps / 10_000;
}

export function deriveMainUsdcReserveApyBps(
  history:
    | { series?: readonly EarnApyHistorySeriesLike[] | null }
    | null
    | undefined,
  fallbackApyBps: number = EARN_MAIN_USDC_RESERVE_FALLBACK_APY_BPS,
): number {
  const series = history?.series?.find(
    (candidate) => candidate.key === "mainUsdcReserve",
  );
  const latestSample =
    series && series.samples.length > 0
      ? series.samples[series.samples.length - 1]
      : undefined;
  return latestSample && Number.isFinite(latestSample.apyBps)
    ? latestSample.apyBps
    : fallbackApyBps;
}

export function buildEarnForecastProjection(
  principal: number,
  apyBps: number,
  points: number,
): number[] {
  const target =
    principal *
    getEarnForecastTargetMultiplier(clampEarnForecastApyBps(apyBps));
  return Array.from({ length: points }, (_, index) => {
    const progress = index / (points - 1);
    const eased = progress ** EARN_FORECAST_EASE_EXPONENT;
    return principal + (target - principal) * eased;
  });
}

export function earnForecastChartAxisCeiling(
  principal: number,
  loyalTarget: number,
): number {
  const scaleRange = Math.max(loyalTarget - principal, 0.01);
  const scaleQuantum = Math.max(
    10 ** (Math.floor(Math.log10(scaleRange)) - 1),
    0.01,
  );
  return Math.max(
    Math.ceil(loyalTarget / scaleQuantum) * scaleQuantum,
    principal + scaleRange,
  );
}
