import type { EarnMaxSummary } from "@loyal-labs/api";
import {
  buildEarnMaxDailySeries,
  type EarnMaxActivityResponse,
} from "@loyal-labs/shared";
import type { EarnEarningsResponse } from "@/lib/solana/earn/earn-api";

export function earnMaxChartEarnings(
  summary: EarnMaxSummary | null,
  activity: EarnMaxActivityResponse | null
): EarnEarningsResponse | null {
  if (!summary || !activity || summary.coverage !== "complete") return null;
  // Public mobile history predates the attributed earnings response.
  const earnings =
    activity.earnings === undefined && summary.earnedUsd !== null
      ? {
          performanceUsd: summary.earnedUsd,
          days: buildEarnMaxDailySeries({
            performance: activity.performance,
            activity: activity.operations,
          }).days.map((day) => ({
            date: day.date.toISOString().slice(0, 10),
            performanceUsd: day.earnedUsd,
          })),
        }
      : activity.earnings;
  if (!earnings) return null;
  const { performanceUsd, days } = earnings;
  const today = new Date().toISOString().slice(0, 10);
  return {
    bars: days.map((day) => ({
      apyBps: null,
      avgPrincipalUsd: 0,
      earnedUsd: day.performanceUsd,
      endAt: `${day.date}T23:59:59.000Z`,
      isCurrent: day.date === today,
      label: String(Number(day.date.slice(-2))),
      principalAmountRaw: "0",
      principalUsd: 0,
      startAt: `${day.date}T00:00:00.000Z`,
    })),
    currentApyBps: null,
    lastDepositAt: null,
    lifetimeEarnedUsd: performanceUsd,
    principalAmountRaw: "0",
    principalUsd: 0,
    rangeEarnedUsd: days.reduce((sum, day) => sum + day.performanceUsd, 0),
    sinceLastDepositEarnedUsd: performanceUsd,
    todayEarnedUsd: days.find((day) => day.date === today)?.performanceUsd ?? 0,
  };
}
