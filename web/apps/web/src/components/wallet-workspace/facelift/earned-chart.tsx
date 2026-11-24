"use client";

import { useEffect, useMemo, useState } from "react";

import {
  deriveEstimatedEarnedAmountApyBps,
  EARN_BALANCE_SAMPLE_MS,
  EARNINGS_DAILY_RANGE_ID,
  EARNINGS_LIFETIME_RANGE_ID,
  EarningsChartLoader,
  formatMaxDailyEarningsLabel,
  formatSignedEarningsAmount,
  splitEarningsHeaderValue,
} from "@/components/wallet-sidebar/earn-detail-view";
import {
  ScrambledPopDigits,
  ScrambleText,
  useBalanceVisibility,
} from "@/components/wallet-workspace/facelift/balance-visibility";
import { getEarnEarningsCacheKey } from "@/components/wallet-workspace/facelift/earn-earnings-prefetch";
import { chartTimestamp, EarnChartDates, EarnChartHeadline } from "@/components/wallet-workspace/facelift/earn-chart-layout";
import { SkeletonReveal } from "@/components/wallet-workspace/facelift/skeleton-reveal";
import type { EarnPositionData } from "@/components/wallet-workspace/facelift/use-earn-position-data";
import { usePublicEnv } from "@/contexts/public-env-context";
import { useEarnEarnings } from "@/hooks/use-earn-earnings";
import { useEarnForecastApy } from "@/hooks/use-earn-forecast-apy";
import { rawTokenAmountToNumber } from "@loyal-labs/shared";
import { deriveEarnEarningsDisplayAmounts } from "@/lib/yield-optimization/earnings-display.shared";

export type EarnedChartBar = {
  apyBps: number | null;
  earnedUsd: number;
  netEarnedUsd?: number;
  endAt: string;
  isCurrent: boolean;
  label: string;
  startAt: string;
};

// Shared wiring for everything derived from the Earn earnings feed — the
// Earned chart consumes all of it, the wallet-home Earn card only the bars.
export function useEarnEarnedData(data: EarnPositionData) {
  const publicEnv = usePublicEnv();
  const { apy: earnForecastApy } = useEarnForecastApy();
  const hasPositiveCurrentBalance = data.hasPosition && data.earnBalanceUsd > 0;
  const earnEarningsRevalidationKey = data.position?.principalAmountRaw ?? "0";
  const earnEarningsCacheKey = getEarnEarningsCacheKey({
    settingsPda: data.settingsPda ?? "no-settings",
    solanaEnv: publicEnv.solanaEnv,
    walletAddress: data.walletAddress ?? "anonymous",
  });
  const {
    data: earningsRangeSet,
    freshness: earningsFreshness,
    isLoading: isEarningsLoading,
    outcome: earningsOutcome,
    refresh: refreshEarnings,
  } = useEarnEarnings({
    cacheKey: earnEarningsCacheKey,
    enabled: hasPositiveCurrentBalance,
    revalidationKey: earnEarningsRevalidationKey,
    settingsPda: data.settingsPda,
    solanaEnv: publicEnv.solanaEnv,
    walletAddress: data.walletAddress,
  });
  const [earnLiveNowMs, setEarnLiveNowMs] = useState(() => Date.now());
  useEffect(() => {
    setEarnLiveNowMs(Date.now());
    const interval = window.setInterval(
      () => setEarnLiveNowMs(Date.now()),
      EARN_BALANCE_SAMPLE_MS
    );
    return () => window.clearInterval(interval);
  }, []);

  const lifetimeData =
    earningsRangeSet?.ranges[EARNINGS_LIFETIME_RANGE_ID] ?? null;
  const dailyData = earningsRangeSet?.ranges[EARNINGS_DAILY_RANGE_ID] ?? null;
  const principalAmount = data.position
    ? rawTokenAmountToNumber(data.position.principalAmountRaw, 6)
    : 0;
  const canLiveEstimate =
    earningsFreshness === "fresh" &&
    earningsRangeSet?.principalMatchesHistory === true &&
    lifetimeData?.currentApyBps !== null &&
    lifetimeData?.currentApyBps !== undefined;
  const estimatedEarnedAmounts = deriveEarnEarningsDisplayAmounts({
    apyBps: deriveEstimatedEarnedAmountApyBps({
      earningsData: lifetimeData,
      fallbackApyBps:
        !earnForecastApy || earnForecastApy.availability === "unavailable"
          ? 0
          : earnForecastApy.apyBps,
    }),
    canLiveEstimate,
    dailyData,
    generatedAt: earningsRangeSet?.generatedAt ?? null,
    lifetimeData,
    nowMs: earnLiveNowMs,
    principalAmount,
  });
  const earningsUnavailable = earningsOutcome === "unavailable";
  const earningsStale = earningsFreshness === "stale";

  const realBars = dailyData?.bars;
  const hasRealBars = (realBars?.length ?? 0) > 0;
  const showEarningsLoader = isEarningsLoading && !hasRealBars;
  const estimatedTodayEarnedUsd = estimatedEarnedAmounts.todayEarnedUsd;
  const dailyBars = useMemo(
    () =>
      (realBars ?? []).map((bar) =>
        bar.isCurrent && bar.earnedUsd >= 0
          ? {
              ...bar,
              earnedUsd: Math.max(0, bar.earnedUsd, estimatedTodayEarnedUsd),
            }
          : bar
      ),
    [realBars, estimatedTodayEarnedUsd]
  );

  return {
    currentApyBps: dailyData?.currentApyBps ?? null,
    dailyBars,
    earningsStale,
    earningsUnavailable,
    rangeEarnedUsd: estimatedEarnedAmounts.rangeEarnedUsd,
    refreshEarnings,
    showEarningsLoader,
  };
}

export function EarnedChart({ data }: { data: EarnPositionData }) {
  const earned = useEarnEarnedData(data);
  return (
    <EarnedBarsChart
      bars={earned.dailyBars}
      isLoading={earned.showEarningsLoader}
      isStale={earned.earningsStale}
      isUnavailable={earned.earningsUnavailable}
      rangeEarnedUsd={earned.rangeEarnedUsd}
      onRetry={earned.refreshEarnings}
    />
  );
}

export function EarnedBarsChart({
  bars,
  isLoading,
  isStale,
  isUnavailable,
  rangeEarnedUsd,
  metric = "earnings",
  onRetry,
}: {
  bars: EarnedChartBar[];
  isLoading: boolean;
  isStale: boolean;
  isUnavailable: boolean;
  rangeEarnedUsd?: number;
  metric?: "earnings" | "performance";
  onRetry?: () => void;
}) {
  const { isBalanceHidden } = useBalanceVisibility();
  const [hoveredBar, setHoveredBar] = useState<number | null>(null);
  const active = hoveredBar === null ? undefined : bars[hoveredBar];
  const value =
    active?.earnedUsd ?? rangeEarnedUsd ?? bars.reduce((sum, bar) => sum + bar.earnedUsd, 0);
  const { whole, fraction } = splitEarningsHeaderValue(Math.abs(value));
  const max = bars.reduce((highest, bar) => Math.max(highest, bar.earnedUsd), 0);
  const label = active
    ? chartTimestamp.format(new Date(active.startAt))
    : isStale
    ? "Updating earnings…"
    : "Past 30 days";
  const tickCount = Math.min(bars.length, 7);
  const dates = Array.from(
    { length: tickCount },
    (_, i) => bars[Math.round((i * (bars.length - 1)) / Math.max(1, tickCount - 1))].label
  );

  return (
    <>
      <EarnChartHeadline label={label}>
        {isUnavailable ? (
          "Unavailable"
        ) : (
          <SkeletonReveal isRevealed={!isLoading}>
            <ScrambledPopDigits
              isHidden={isBalanceHidden}
              popOnChange={false}
              segments={[
                { text: `${value < 0 ? "-" : ""}$${whole}` },
                { color: "var(--tertiary)", text: `.${fraction}` },
              ]}
            />
          </SkeletonReveal>
        )}
      </EarnChartHeadline>
      {isUnavailable ? (
        <div
          className="m-auto flex flex-col items-center gap-2 text-[13px] text-muted-foreground"
          role="status"
        >
          Earnings are temporarily unavailable.
          {onRetry && (
            <button
              className="t-hover rounded-full bg-accent px-3 py-1.5"
              onClick={onRetry}
              type="button"
            >
              Try again
            </button>
          )}
        </div>
      ) : isLoading ? (
        <EarningsChartLoader />
      ) : bars.length === 0 ? (
        <p className="m-auto text-[13px] text-muted-foreground">
          {metric === "performance" ? "Daily performance unavailable" : "No earnings yet"}
        </p>
      ) : (
        <>
          <p className="pb-2 text-right text-[13px] leading-4 text-muted-foreground">
            <ScrambleText
              isHidden={isBalanceHidden}
              text={max === 0 ? "$0.00" : formatMaxDailyEarningsLabel(max)}
            />
          </p>
          <div
            className="flex min-h-0 flex-1 items-end gap-1.5 @min-[640px]:gap-3"
            onPointerLeave={() => setHoveredBar(null)}
          >
            {bars.map((bar, i) => (
              <button
                aria-label={
                  isBalanceHidden
                    ? bar.label
                    : `${bar.label} ${
                        metric === "performance" ? "performance" : "earned"
                      } ${formatSignedEarningsAmount(bar.earnedUsd)}`
                }
                className="flex h-full min-w-0 flex-1 cursor-pointer items-end rounded-sm"
                key={bar.startAt}
                onBlur={() => setHoveredBar(null)}
                onFocus={() => setHoveredBar(i)}
                onPointerEnter={() => setHoveredBar(i)}
                onClick={() => setHoveredBar(i)}
                type="button"
              >
                {bar.earnedUsd > 0 && (
                  <span
                    aria-hidden="true"
                    className={`earn-chart-bar w-full rounded-[4px] transition-opacity ${
                      bar.isCurrent
                        ? "border border-dashed border-chart-positive"
                        : "bg-chart-positive"
                    } ${active && hoveredBar !== i ? "opacity-20" : "opacity-50"}`}
                    style={{ height: `${(bar.earnedUsd / max) * 100}%`, minHeight: 4 }}
                  />
                )}
              </button>
            ))}
          </div>
          <EarnChartDates labels={dates} />
        </>
      )}
    </>
  );
}
