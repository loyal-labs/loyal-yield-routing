"use client";

import NumberFlow, { continuous } from "@number-flow/react";
import {
  type PointerEvent,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { maskBalanceText } from "@/components/wallet-workspace/facelift/balance-visibility";
import { ApyRevealText } from "@/components/wallet-workspace/facelift/skeleton-reveal";
import { useEarnForecastApy } from "@/hooks/use-earn-forecast-apy";
import { useEarnForecastApyHistory } from "@/hooks/use-earn-forecast-apy-history";
import {
  type EarnForecastApy,
  type EarnForecastApyHistoryResponse,
  formatEarnApyPercent,
  getEarnForecastTargetMultiplier,
} from "@/lib/kamino/earn-forecast.shared";
import type {
  EarnEarningsResponse,
  EarningsRangeId,
} from "@/lib/yield-optimization/earnings.shared";

const font = "var(--font-geist-sans), sans-serif";
const secondary = "var(--muted-foreground)";
const LOYAL_EARN_BRAND_COLOR = "var(--primary)";
const EARN_BALANCE_DECIMALS = 6;
export const EARN_BALANCE_SAMPLE_MS = 250;
const USDC_RAW_SCALE = BigInt(1_000_000);
const USDC_DISPLAY_DUST_TOLERANCE = 1.5 / Number(USDC_RAW_SCALE);
const SECONDS_PER_YEAR = 365 * 24 * 60 * 60;
const EARN_NUMBER_FLOW_PLUGINS = [continuous];

export type EarnHelpTopic =
  | "autodeposit"
  | "autodepositDelete"
  | "autodepositDestination"
  | "autodepositLoadError"
  | "autodepositLoading"
  | "autodepositPending"
  | "autodepositSetup"
  | "autodepositSource"
  | "autodepositThreshold"
  | "currentPositions"
  | "earn";

export type EarnDepositSourceOption = {
  addressLabel: string;
  balance: number;
  balanceFraction: string;
  balanceWhole: string;
  decimals: number;
  icon: string;
  id: string;
  label: string;
  mint: string | null;
};

export type EarnDepositDraft = {
  amount: number;
  amountLabel: string;
  forecastApyBps: number;
  source: EarnDepositSourceOption;
  symbol: string;
  tokenDecimals: number;
  tokenMint: string | null;
};

export type EarnWithdrawDraft = {
  amount: number;
  amountLabel: string;
  destination: EarnDepositSourceOption;
  // Every reserve the vault still holds. A full exit must unwind all of them —
  // a wallet can hold USDC in a second Kamino market, and leaving one behind
  // strands it and fails the post-withdrawal zero proof.
  fullExitSources: EarnWithdrawSourceOption[];
  isSourceMax?: boolean;
  mode: "partial" | "full";
  source: EarnWithdrawSourceOption;
  symbol: string;
  tokenDecimals: number;
};

export type EarnWithdrawSourceOption = {
  amountRaw: string;
  balance: number;
  id: string;
  icon: string;
  label: string;
  liquidityMint: string;
  market: string | null;
  reserve: string | null;
  sourceId: string;
  supplyApyBps?: string | null;
  tokenAccount: string | null;
  tokenProgramId: string;
  type: "reserve" | "idle";
};

export type EarnAutodepositDraft = {
  amount: number;
  amountLabel: string;
  amountChanged?: boolean;
  existingPolicySeed?: string;
  existingRecurringDelegation?: string;
  expiryTimestamp?: bigint;
  keepAmount: number;
  keepAmountChanged?: boolean;
  keepAmountLabel: string;
  nonce: bigint;
  periodLengthSeconds?: bigint;
  requiresSignature?: boolean;
  source: EarnDepositSourceOption;
  startTimestamp?: bigint;
  symbol: "USDC";
  tokenDecimals: number;
};

type EarnChartPoint = {
  date: string;
  highValue: number;
  index: number;
  lowValue: number;
  value: number;
  yieldUsd: number;
};

function formatMoney(value: number) {
  if (!Number.isFinite(value)) {
    return "0.00";
  }

  return value.toLocaleString("en-US", {
    maximumFractionDigits: 2,
    minimumFractionDigits: 2,
  });
}

function snapDollarDisplayDust(value: number) {
  if (!Number.isFinite(value)) {
    return 0;
  }

  const nearestCent = Math.round(value * 100) / 100;
  return Math.abs(value - nearestCent) <= USDC_DISPLAY_DUST_TOLERANCE
    ? nearestCent
    : value;
}

// Renders the full entered amount. The 6-digit ceiling only absorbs float
// noise — typed input is already capped at USDC's 6 on-chain decimals.
export function formatEarnActionCtaAmount(value: number) {
  if (!Number.isFinite(value)) {
    return "0";
  }

  return value.toLocaleString("en-US", {
    maximumFractionDigits: 6,
    minimumFractionDigits: 0,
  });
}

export function formatEarnWithdrawMaxAmountLabel(value: number): string {
  if (!Number.isFinite(value) || value <= 0) {
    return "0";
  }

  const roundedToCents = value.toFixed(2);
  return Number(roundedToCents) > 0 ? roundedToCents : value.toFixed(6);
}

// Sanitizes free-form amount typing. This helper never compares against a
// balance: deposit inputs allow over-balance amounts (the submit button
// switches to "Insufficient balance"), while the withdraw input clamps to the
// max withdrawable amount at its call site.
export function sanitizeBucksAmountInput(
  rawValue: string,
  previousValue: string
) {
  if (rawValue === "") {
    return "";
  }

  if (rawValue === ".") {
    return "0.";
  }

  if (!/^[\d,]*\.?\d*$/.test(rawValue)) {
    return null;
  }

  const isDeletion = rawValue.length < previousValue.length;
  const nextValue = (
    isDeletion && rawValue.endsWith(".") ? rawValue.slice(0, -1) : rawValue
  ).replace(/^0+(?=\d)/, "");

  // USDC carries 6 on-chain decimals; anything finer can't be transferred.
  const decimals = nextValue.split(".")[1] ?? "";
  if (decimals.length > 6) {
    return null;
  }

  if (nextValue.split(".")[0].replace(/,/g, "").length > 9) {
    return null;
  }

  return nextValue;
}

function forecastMoneySegments(value: number, mutedFraction = false) {
  const [whole, fraction = "00"] = formatMoney(value).split(".");
  return [
    { text: `$${whole}` },
    {
      color: mutedFraction ? "var(--tertiary)" : undefined,
      text: `.${fraction}`,
    },
  ];
}

const FORECAST_DATES = [
  "May 2026",
  "Jun 2026",
  "Jul 2026",
  "Aug 2026",
  "Sep 2026",
  "Oct 2026",
  "Nov 2026",
  "Dec 2026",
  "Jan 2027",
  "Feb 2027",
  "Mar 2027",
  "Apr 2027",
  "May 2027",
];

export function buildEarnChartPoints(
  principal: number,
  apy: EarnForecastApy
): EarnChartPoint[] {
  const months = 12;
  const target = principal * getEarnForecastTargetMultiplier(apy.apyBps);
  const lowTarget =
    principal * getEarnForecastTargetMultiplier(apy.rangeLowBps);
  const highTarget =
    principal * getEarnForecastTargetMultiplier(apy.rangeHighBps);

  return Array.from({ length: months + 1 }, (_, index) => {
    const progress = index / months;
    const eased = progress ** 1.08;
    const value = principal + (target - principal) * eased;
    return {
      date: FORECAST_DATES[index] ?? FORECAST_DATES[FORECAST_DATES.length - 1],
      highValue: principal + (highTarget - principal) * progress,
      index,
      lowValue: principal + (lowTarget - principal) * progress,
      value,
      yieldUsd: value - principal,
    };
  });
}

type EarnComparisonSeriesKey = "loyal" | "mainUsdcReserve" | "tBill";

const EARN_COMPARISON_SERIES: {
  color: string;
  dashed: boolean;
  fixedApyBps: number | null;
  key: EarnComparisonSeriesKey;
  label: string;
}[] = [
  {
    color: LOYAL_EARN_BRAND_COLOR,
    dashed: false,
    fixedApyBps: null,
    key: "loyal",
    label: "Loyal Earn",
  },
  {
    color: "#2688EB",
    dashed: true,
    fixedApyBps: 559,
    key: "mainUsdcReserve",
    label: "Main Kamino USDC",
  },
  {
    color: "#8E8E93",
    dashed: true,
    fixedApyBps: 365,
    key: "tBill",
    label: "T-Bill",
  },
];

const EARN_SERIES_DISPLAY: Record<
  EarnComparisonSeriesKey,
  { color: string; label: string }
> = {
  loyal: { color: LOYAL_EARN_BRAND_COLOR, label: "Loyal Earn" },
  mainUsdcReserve: { color: "#A7B3F6", label: "Main Kamino" },
  tBill: { color: "#B1B1B4", label: "T-Bill" },
};

const EARN_COMPARISON_MIN_APY_BPS = 50;

type EarnComparisonPoint = {
  date: string;
  index: number;
  values: Record<EarnComparisonSeriesKey, number>;
};

type EarnComparisonApyOverrides = Partial<
  Record<Exclude<EarnComparisonSeriesKey, "loyal">, number>
>;

function getEarnComparisonApyBps(
  forecastApyBps: number,
  fixedApyBps: number | null
): number {
  return Math.max(fixedApyBps ?? forecastApyBps, EARN_COMPARISON_MIN_APY_BPS);
}

export function buildEarnComparisonPoints(
  principal: number,
  apy: EarnForecastApy,
  apyOverrides: EarnComparisonApyOverrides = {}
): EarnComparisonPoint[] {
  const months = 12;
  const targets = EARN_COMPARISON_SERIES.reduce((acc, series) => {
    const overrideApyBps =
      series.key === "loyal" ? undefined : apyOverrides[series.key];
    const apyBps = getEarnComparisonApyBps(
      apy.apyBps,
      overrideApyBps ?? series.fixedApyBps
    );
    acc[series.key] = principal * getEarnForecastTargetMultiplier(apyBps);
    return acc;
  }, {} as Record<EarnComparisonSeriesKey, number>);

  return Array.from({ length: months + 1 }, (_, index) => {
    const progress = index / months;
    const eased = progress ** 1.08;
    const values = EARN_COMPARISON_SERIES.reduce((acc, series) => {
      acc[series.key] = principal + (targets[series.key] - principal) * eased;
      return acc;
    }, {} as Record<EarnComparisonSeriesKey, number>);
    return {
      date: FORECAST_DATES[index] ?? FORECAST_DATES[FORECAST_DATES.length - 1],
      index,
      values,
    };
  });
}

export function deriveMainUsdcReserveForecastApyBps(
  history: Pick<EarnForecastApyHistoryResponse, "series">,
  fallbackApyBps = 559
): number {
  const latestSample = history.series
    ?.find((series) => series.key === "mainUsdcReserve")
    ?.samples.at(-1);
  return latestSample && Number.isFinite(latestSample.apyBps)
    ? latestSample.apyBps
    : fallbackApyBps;
}

function getEarnApyRate(apyBps: number): number {
  return apyBps / 10_000;
}

export function getEarningsRatePerSecond(
  apyBps: number,
  principal: number
): number {
  return (principal * getEarnApyRate(apyBps)) / SECONDS_PER_YEAR;
}

export function deriveEarnWithdrawMode({
  amount,
  sources,
}: {
  amount: number;
  sources: EarnWithdrawSourceOption[];
}): "partial" | "full" {
  // Compared against the WHOLE position, never the selected source: "full"
  // closes the position, the policies and the vault, so it may only mean
  // "everything". Maxing one of several sources is a partial withdrawal.
  //
  // Do not round another source away as dust. Max is source-local; any other
  // positive holding keeps this a partial withdrawal and cleanup remains a
  // separate fresh all-zero decision.
  const totalWithdrawableAmount = sources.reduce(
    (total, source) => total + source.balance,
    0
  );

  return amount >= totalWithdrawableAmount ? "full" : "partial";
}

// A withdrawal always targets the selected source. Full means that this was
// the vault's only positive source; it never grants permission to drain other
// reserves discovered by the UI.
export function selectEarnFullExitSources(
  draft: Pick<EarnWithdrawDraft, "fullExitSources" | "mode" | "source">
): EarnWithdrawSourceOption[] {
  if (draft.mode !== "full" || draft.source.type !== "reserve") {
    return [];
  }

  return [draft.source];
}

// Facelift-only: the exact display EarnGrowingBalance renders (dust-snapped,
// 6 decimals, grouped, $-prefixed), split so the redesigned pane can pop the
// digits with a muted fraction. Keep in sync with the NumberFlow format below.
export function splitEarnBalanceDisplay(baseAmount: number): {
  fraction: string;
  whole: string;
} {
  const formatted = snapDollarDisplayDust(baseAmount).toLocaleString("en-US", {
    maximumFractionDigits: EARN_BALANCE_DECIMALS,
    minimumFractionDigits: EARN_BALANCE_DECIMALS,
  });
  const [whole, fraction] = formatted.split(".");
  return { fraction: `.${fraction}`, whole: `$${whole}` };
}

export function EarnGrowingBalance({
  baseAmount,
  isHidden = false,
}: {
  baseAmount: number;
  isHidden?: boolean;
}) {
  const displayValue = snapDollarDisplayDust(baseAmount);

  return (
    <>
      <style jsx>{`
        :global(.earn-growing-balance-flow) {
          --number-flow-mask-height: 0.12em;
          --number-flow-mask-width: 0.24em;
          color: ${isHidden ? "#BBBBC0" : "#000"};
          font-family: ${font};
          font-size: 40px;
          font-variant-numeric: tabular-nums;
          font-weight: 600;
          line-height: 46px;
        }
        :global(.earn-growing-balance-flow::part(decimal)),
        :global(.earn-growing-balance-flow::part(fraction)) {
          color: ${isHidden ? "#BBBBC0" : "rgba(60, 60, 67, 0.4)"};
        }
        @media (max-width: 760px) {
          :global(.earn-growing-balance-flow) {
            font-size: clamp(30px, 9.5vw, 40px);
            line-height: 1.08;
          }
        }
      `}</style>
      <NumberFlow
        className="earn-growing-balance-flow"
        format={{
          maximumFractionDigits: EARN_BALANCE_DECIMALS,
          minimumFractionDigits: EARN_BALANCE_DECIMALS,
          useGrouping: true,
        }}
        opacityTiming={{ duration: 0, easing: "linear" }}
        plugins={EARN_NUMBER_FLOW_PLUGINS}
        prefix="$"
        spinTiming={{ duration: 0, easing: "linear" }}
        transformTiming={{ duration: 0, easing: "linear" }}
        trend={1}
        value={displayValue}
      />
    </>
  );
}
export const EARNINGS_LIFETIME_RANGE_ID = "ALL" satisfies EarningsRangeId;
export const EARNINGS_DAILY_RANGE_ID = "30D" satisfies EarningsRangeId;

export function deriveEstimatedEarnedAmount({
  earningsData,
}: {
  earningsData: EarnEarningsResponse | null;
}) {
  if (!earningsData) {
    return 0;
  }

  return Number.isFinite(earningsData.lifetimeEarnedUsd)
    ? earningsData.lifetimeEarnedUsd
    : 0;
}

function deriveLiveEarnedUsd({
  apyBps,
  generatedAt,
  nowMs = Date.now(),
  principalAmount,
}: {
  apyBps: number;
  generatedAt: string | null;
  nowMs?: number;
  principalAmount: number;
}) {
  const generatedAtMs = generatedAt ? Date.parse(generatedAt) : Number.NaN;
  const elapsedSeconds = Number.isFinite(generatedAtMs)
    ? Math.max(0, (nowMs - generatedAtMs) / 1000)
    : 0;

  return getEarningsRatePerSecond(apyBps, principalAmount) * elapsedSeconds;
}

function normalizeDisplayedEarnedUsd(value: number) {
  if (!Number.isFinite(value) || value <= 0) {
    return 0;
  }

  return Number(value.toFixed(EARN_BALANCE_DECIMALS));
}

export function deriveEstimatedEarnedSummaryAmount({
  apyBps,
  canLiveEstimate = true,
  earningsData,
  generatedAt,
  nowMs,
  principalAmount,
}: {
  apyBps: number;
  canLiveEstimate?: boolean;
  earningsData: EarnEarningsResponse | null;
  generatedAt: string | null;
  nowMs?: number;
  principalAmount: number;
}) {
  if (!earningsData) {
    return 0;
  }

  const lifetimeEarnedUsd = Number.isFinite(earningsData.lifetimeEarnedUsd)
    ? earningsData.lifetimeEarnedUsd
    : 0;
  const estimatedLifetimeEarnedUsd =
    lifetimeEarnedUsd +
    (canLiveEstimate
      ? deriveLiveEarnedUsd({
          apyBps,
          generatedAt,
          nowMs,
          principalAmount,
        })
      : 0);

  // A fresh RPC balance can include an Autodeposit sweep before the confirmed
  // principal history catches up. Treating balance - principal as earned in
  // that window turns new deposits into yield, so use the APY/history path as
  // the display authority.
  return normalizeDisplayedEarnedUsd(estimatedLifetimeEarnedUsd);
}

export function formatEarnedSummaryLabel(value: number) {
  return formatSignedEarningsAmount(value);
}

export function deriveEstimatedEarnedAmountApyBps({
  earningsData,
  fallbackApyBps,
}: {
  earningsData: EarnEarningsResponse | null;
  fallbackApyBps: number;
}) {
  if (!earningsData || earningsData.currentApyBps === null) {
    return Number.isFinite(fallbackApyBps) ? fallbackApyBps : 0;
  }

  return Number.isFinite(earningsData.currentApyBps)
    ? earningsData.currentApyBps
    : 0;
}

function getEarningsFractionDigits(value: number) {
  const absoluteValue = Math.abs(value);
  if (absoluteValue > 0 && absoluteValue < 0.01) {
    return EARN_BALANCE_DECIMALS;
  }
  return 2;
}

export function splitEarningsHeaderValue(value: number): {
  fraction: string;
  whole: string;
} {
  const safeValue = Number.isFinite(value) && value > 0 ? value : 0;
  const [whole, fraction] = safeValue
    .toLocaleString("en-US", {
      maximumFractionDigits: EARN_BALANCE_DECIMALS,
      minimumFractionDigits: EARN_BALANCE_DECIMALS,
    })
    .split(".");
  return { fraction, whole };
}

export function formatMaxDailyEarningsLabel(value: number) {
  if (!Number.isFinite(value) || value < 0.01) {
    return "<$0.01";
  }
  return `$${value.toLocaleString("en-US", {
    maximumFractionDigits: 2,
    minimumFractionDigits: 2,
  })}`;
}

export function formatSignedEarningsAmount(value: number) {
  if (!Number.isFinite(value)) {
    return "+$0.00";
  }
  const fractionDigits = getEarningsFractionDigits(value);
  const sign = value >= 0 ? "+" : "-";
  const formatted = Math.abs(value).toLocaleString("en-US", {
    maximumFractionDigits: fractionDigits,
    minimumFractionDigits: fractionDigits,
  });
  return `${sign}$${formatted}`;
}

export function EarningsChartLoader() {
  const SIZE = 56;
  const STROKE = 5;
  const radius = (SIZE - STROKE) / 2;
  const circumference = 2 * Math.PI * radius;
  const arc = circumference * 0.25;
  return (
    <div
      aria-label="Loading earnings"
      role="status"
      style={{
        alignItems: "center",
        alignSelf: "stretch",
        display: "flex",
        flex: 1,
        justifyContent: "center",
        minHeight: 0,
        width: "100%",
      }}
    >
      <span
        className="animate-spin"
        style={{ display: "inline-flex", height: SIZE, width: SIZE }}
      >
        <svg
          aria-hidden="true"
          height={SIZE}
          viewBox={`0 0 ${SIZE} ${SIZE}`}
          width={SIZE}
        >
          <circle
            cx={SIZE / 2}
            cy={SIZE / 2}
            fill="none"
            r={radius}
            stroke="rgba(18, 18, 18, 0.08)"
            strokeWidth={STROKE}
          />
          <circle
            cx={SIZE / 2}
            cy={SIZE / 2}
            fill="none"
            r={radius}
            stroke={LOYAL_EARN_BRAND_COLOR}
            strokeDasharray={`${arc} ${circumference - arc}`}
            strokeLinecap="round"
            strokeWidth={STROKE}
          />
        </svg>
      </span>
    </div>
  );
}

export function AutodepositToggle({
  ariaLabel,
  disabled = false,
  isOn,
  isPending = false,
  onToggle,
}: {
  ariaLabel?: string;
  disabled?: boolean;
  isOn: boolean;
  isPending?: boolean;
  onToggle?: () => void;
}) {
  // transitions-dev "toggle": .is-init gates the bounce keyframes so the off
  // animation doesn't play at mount — armed once isOn first changes, which
  // covers both click-driven and server-driven flips.
  const [isInit, setIsInit] = useState(false);
  const previousIsOn = useRef(isOn);
  useEffect(() => {
    if (previousIsOn.current !== isOn) {
      setIsInit(true);
    }
    previousIsOn.current = isOn;
  }, [isOn]);

  return (
    <button
      aria-busy={isPending}
      aria-checked={isOn}
      aria-label={
        ariaLabel ?? (isOn ? "Pause Autodeposit" : "Resume Autodeposit")
      }
      className={`t-toggle${isInit ? " is-init" : ""}`}
      data-on={isOn}
      disabled={disabled}
      onClick={onToggle}
      role="switch"
      style={{
        alignItems: "center",
        background: isOn ? LOYAL_EARN_BRAND_COLOR : "rgba(120, 120, 128, 0.32)",
        border: "none",
        borderRadius: "9999px",
        cursor: disabled ? "default" : "pointer",
        display: "inline-flex",
        flexShrink: 0,
        height: "31px",
        padding: "2px",
        width: "51px",
      }}
      type="button"
    >
      {/* Thumb travel (20px) lives in the t-toggle CSS in globals.css. */}
      <span
        className="t-toggle-thumb"
        style={{
          alignItems: "center",
          background: "#fff",
          borderRadius: "9999px",
          boxShadow: "0 3px 8px rgba(0, 0, 0, 0.15)",
          display: "inline-flex",
          height: "27px",
          justifyContent: "center",
          width: "27px",
        }}
      >
        {isPending ? (
          <span
            aria-hidden="true"
            className="autodeposit-toggle-spinner"
            style={{
              border: "2px solid rgba(120, 120, 128, 0.25)",
              borderRadius: "9999px",
              borderTopColor: LOYAL_EARN_BRAND_COLOR,
              display: "inline-block",
              height: "16px",
              width: "16px",
            }}
          />
        ) : null}
      </span>
      <style jsx>{`
        .autodeposit-toggle-spinner {
          animation: autodeposit-toggle-spin 0.6s linear infinite;
        }
        @keyframes autodeposit-toggle-spin {
          to {
            transform: rotate(360deg);
          }
        }
        @media (prefers-reduced-motion: reduce) {
          .autodeposit-toggle-spinner {
            animation-duration: 1.1s;
          }
        }
      `}</style>
    </button>
  );
}

export type HistoricalApySample = {
  apyPercent: number;
  observedAtMs: number;
};

const HISTORICAL_APY_STATIC_BENCHMARKS = EARN_COMPARISON_SERIES.filter(
  (
    series
  ): series is (typeof EARN_COMPARISON_SERIES)[number] & {
    fixedApyBps: number;
  } => series.key !== "loyal" && series.key !== "mainUsdcReserve"
);
const HISTORICAL_RANGE_CONFIG: Record<EarningsRangeId, { spanDays: number }> = {
  "7D": { spanDays: 7 },
  "30D": { spanDays: 30 },
  "1Y": { spanDays: 365 },
  ALL: { spanDays: 540 },
};

function toHistoricalApySamples(
  history: ReturnType<typeof useEarnForecastApyHistory>
): HistoricalApySample[] {
  const loyalSeries = history.series?.find((series) => series.key === "loyal");
  const samples = loyalSeries?.samples.length
    ? loyalSeries.samples
    : history.samples;

  return samples.map((sample) => ({
    apyPercent: sample.apyBps / 100,
    observedAtMs: Date.parse(sample.observedAt),
  }));
}

function toHistoricalBenchmarkSamples(
  history: ReturnType<typeof useEarnForecastApyHistory>,
  key: Exclude<EarnComparisonSeriesKey, "loyal">
): HistoricalApySample[] {
  const series = history.series?.find((item) => item.key === key);
  if (!series) {
    return [];
  }

  return series.samples.map((sample) => ({
    apyPercent: sample.apyBps / 100,
    observedAtMs: Date.parse(sample.observedAt),
  }));
}

function nearestHistoricalApyPercent(
  samples: readonly HistoricalApySample[] | undefined,
  observedAtMs: number,
  fallback: number
): number {
  if (!samples || samples.length === 0) {
    return fallback;
  }

  return samples.reduce((nearest, sample) =>
    Math.abs(sample.observedAtMs - observedAtMs) <
    Math.abs(nearest.observedAtMs - observedAtMs)
      ? sample
      : nearest
  ).apyPercent;
}

function smoothChartLinePath(
  points: readonly { x: number; y: number }[]
): string {
  const first = points[0];
  if (!first) {
    return "";
  }
  const path = [`M${first.x.toFixed(2)},${first.y.toFixed(2)}`];
  for (let index = 0; index < points.length - 1; index += 1) {
    const previous = points[index - 1] ?? points[index];
    const current = points[index];
    const next = points[index + 1];
    const afterNext = points[index + 2] ?? next;
    const span = next.x - current.x;
    const out1 = Math.min(1, span / Math.max((next.x - previous.x) / 6, 1e-9));
    const out2 = Math.min(
      1,
      span / Math.max((afterNext.x - current.x) / 6, 1e-9)
    );
    const control1X = current.x + ((next.x - previous.x) / 6) * out1;
    const control1Y = current.y + ((next.y - previous.y) / 6) * out1;
    const control2X = next.x - ((afterNext.x - current.x) / 6) * out2;
    const control2Y = next.y - ((afterNext.y - current.y) / 6) * out2;
    path.push(
      `C${control1X.toFixed(2)},${control1Y.toFixed(2)} ${control2X.toFixed(
        2
      )},${control2Y.toFixed(2)} ${next.x.toFixed(2)},${next.y.toFixed(2)}`
    );
  }
  return path.join(" ");
}

const HISTORICAL_APY_MAX_LINE_POINTS = 110;

function downsampleHistoricalApySamples(
  samples: HistoricalApySample[]
): HistoricalApySample[] {
  if (samples.length <= HISTORICAL_APY_MAX_LINE_POINTS) {
    return samples;
  }
  const buckets: HistoricalApySample[] = [];
  for (let index = 0; index < HISTORICAL_APY_MAX_LINE_POINTS; index += 1) {
    const start = Math.floor(
      (index * samples.length) / HISTORICAL_APY_MAX_LINE_POINTS
    );
    const end = Math.max(
      Math.floor(
        ((index + 1) * samples.length) / HISTORICAL_APY_MAX_LINE_POINTS
      ),
      start + 1
    );
    const bucket = samples.slice(start, end);
    const observedAtMs =
      index === 0
        ? bucket[0].observedAtMs
        : index === HISTORICAL_APY_MAX_LINE_POINTS - 1
        ? bucket[bucket.length - 1].observedAtMs
        : bucket[Math.floor(bucket.length / 2)].observedAtMs;
    buckets.push({
      apyPercent:
        bucket.reduce((sum, sample) => sum + sample.apyPercent, 0) /
        bucket.length,
      observedAtMs,
    });
  }
  return buckets;
}

const HISTORICAL_AXIS_DATE_FORMAT = new Intl.DateTimeFormat("en-US", {
  day: "numeric",
  month: "short",
});
const HISTORICAL_HOVER_DATE_FORMAT = new Intl.DateTimeFormat("en-US", {
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
  month: "short",
});
const HISTORICAL_MAIN_USDC_FALLBACK_APY_PERCENT =
  (EARN_COMPARISON_SERIES.find((series) => series.key === "mainUsdcReserve")
    ?.fixedApyBps ?? 559) / 100;

function historicalApyValueSegments(
  apyPercent: number,
  mutedFraction: boolean
) {
  const [whole, fraction = "00"] = apyPercent.toFixed(2).split(".");
  return [
    { text: whole },
    {
      color: mutedFraction ? "var(--tertiary)" : undefined,
      text: `.${fraction}%`,
    },
  ];
}

type HistoricalApyChartProps = {
  apyDataRevealed: boolean;
  axisTickCount?: number;
  primaryLabel?: string;
  primarySamples?: HistoricalApySample[];
  rangeId: EarningsRangeId;
};

const subscribeToHistoricalChartHydration = () => () => undefined;
const getHistoricalChartClientSnapshot = () => true;
const getHistoricalChartServerSnapshot = () => false;

export function HistoricalApyChart(props: HistoricalApyChartProps) {
  const isHydrated = useSyncExternalStore(
    subscribeToHistoricalChartHydration,
    getHistoricalChartClientSnapshot,
    getHistoricalChartServerSnapshot
  );
  const { apy: forecast } = useEarnForecastApy();
  const history = useEarnForecastApyHistory();

  const rangeStartMs =
    Date.now() -
    HISTORICAL_RANGE_CONFIG[props.rangeId].spanDays * 24 * 60 * 60 * 1000;
  const visibleSamples =
    props.primarySamples ??
    toHistoricalApySamples(history).filter(
      (sample) => sample.observedAtMs >= rangeStartMs
    );
  if (
    (props.primarySamples === undefined &&
      (!forecast || forecast.availability === "unavailable")) ||
    visibleSamples.length === 0
  ) {
    return (
      <div
        className="flex min-h-0 w-full flex-1 items-center justify-center text-muted-foreground"
        role="status"
      >
        {props.primarySamples === undefined && forecast?.source === "live"
          ? "Historical 7-day APY unavailable; live APY uses 6–24h"
          : "Historical APY unavailable"}
      </div>
    );
  }

  if (!isHydrated) {
    // The fallback history intentionally ends at the current local time. Keep
    // the chart slot empty for SSR and the browser's matching hydration pass,
    // then mount it below with the browser clock and timezone. The parent card
    // owns the dimensions, so this avoids both a layout shift and mismatched
    // date labels without weakening React's hydration checks.
    return <div aria-hidden="true" className="min-h-0 w-full flex-1" />;
  }

  return (
    <>
      {props.primarySamples === undefined && forecast?.availability === "stale" ? (
        <span className="text-xs text-muted-foreground">APY data is stale</span>
      ) : null}
      {visibleSamples[0].observedAtMs > rangeStartMs + 2 * 60 * 60 * 1000 ? (
        <span className="text-xs text-muted-foreground">
          Showing available recorded history
        </span>
      ) : null}
      <HydratedHistoricalApyChart
        {...props}
        apyHistory={history}
        samples={downsampleHistoricalApySamples(visibleSamples)}
      />
    </>
  );
}

function HydratedHistoricalApyChart({
  apyDataRevealed,
  apyHistory,
  axisTickCount = 2,
  primaryLabel,
  rangeId,
  samples,
}: HistoricalApyChartProps & {
  apyHistory: EarnForecastApyHistoryResponse;
  samples: HistoricalApySample[];
}) {
  const revealClipId = `historical-chart-reveal-clip-${useId().replace(
    /[^a-zA-Z0-9_-]/g,
    ""
  )}`;
  const mainUsdcSamples = useMemo(() => {
    if (rangeId !== "30D") {
      return [];
    }

    return downsampleHistoricalApySamples(
      toHistoricalBenchmarkSamples(apyHistory, "mainUsdcReserve")
    );
  }, [apyHistory, rangeId]);
  const [hoverIndex, setHoverIndex] = useState<number | null>(null);
  const chartBoxRef = useRef<HTMLDivElement | null>(null);
  const [chartSize, setChartSize] = useState({ height: 0, width: 0 });

  useEffect(() => {
    const node = chartBoxRef.current;
    if (!node) {
      return;
    }
    const observer = new ResizeObserver(([entry]) => {
      if (!entry) {
        return;
      }
      setChartSize({
        height: entry.contentRect.height,
        width: entry.contentRect.width,
      });
    });
    observer.observe(node);
    return () => observer.disconnect();
  }, []);

  const isHovering = hoverIndex !== null;
  const focusSample =
    samples[Math.min(hoverIndex ?? samples.length - 1, samples.length - 1)];
  const mainUsdcApyPercentAt = (observedAtMs: number) =>
    nearestHistoricalApyPercent(
      mainUsdcSamples,
      observedAtMs,
      HISTORICAL_MAIN_USDC_FALLBACK_APY_PERCENT
    );
  const benchmarks: {
    apyPercentAt: (observedAtMs: number) => number;
    key: EarnComparisonSeriesKey;
    samples: HistoricalApySample[] | null;
  }[] = [
    {
      apyPercentAt: mainUsdcApyPercentAt,
      key: "mainUsdcReserve",
      samples: mainUsdcSamples.length > 0 ? mainUsdcSamples : null,
    },
    ...HISTORICAL_APY_STATIC_BENCHMARKS.map((series) => ({
      apyPercentAt: () => series.fixedApyBps / 100,
      key: series.key,
      samples: null,
    })),
  ];

  const scaleValues = [
    ...samples.map((sample) => sample.apyPercent),
    ...(mainUsdcSamples.length > 0
      ? mainUsdcSamples.map((sample) => sample.apyPercent)
      : [HISTORICAL_MAIN_USDC_FALLBACK_APY_PERCENT]),
    ...HISTORICAL_APY_STATIC_BENCHMARKS.map(
      (series) => series.fixedApyBps / 100
    ),
  ];
  const maxValue = Math.max(...scaleValues);
  const minValue = Math.min(...scaleValues);
  const valueRange = Math.max(maxValue - minValue, 0.01);
  const scaleQuantum = Math.max(
    10 ** Math.floor(Math.log10(valueRange)) / 2,
    0.01
  );
  const scaleMax = Math.ceil(maxValue / scaleQuantum) * scaleQuantum;
  const scaleMin = Math.max(
    Math.floor((minValue - valueRange * 0.2) / scaleQuantum) * scaleQuantum,
    0
  );

  const headerSeries = [
    {
      apyPercent: focusSample.apyPercent,
      color: EARN_SERIES_DISPLAY.loyal.color,
      key: "loyal" as EarnComparisonSeriesKey,
      label: primaryLabel ?? EARN_SERIES_DISPLAY.loyal.label,
    },
    ...benchmarks.map((benchmark) => ({
      apyPercent: benchmark.apyPercentAt(focusSample.observedAtMs),
      color: EARN_SERIES_DISPLAY[benchmark.key].color,
      key: benchmark.key,
      label: EARN_SERIES_DISPLAY[benchmark.key].label,
    })),
  ];

  const chartWidth = chartSize.width;
  const chartHeight = chartSize.height;
  const hasChartArea = chartWidth > 2 && chartHeight > 2;
  const startedAtMs = samples[0]?.observedAtMs ?? 0;
  const endedAtMs = samples[samples.length - 1]?.observedAtMs ?? startedAtMs;
  const xForObservedAtMs = (observedAtMs: number) => {
    const progress =
      endedAtMs > startedAtMs
        ? Math.min(
            Math.max(
              (observedAtMs - startedAtMs) / (endedAtMs - startedAtMs),
              0
            ),
            1
          )
        : 1;
    return 1 + progress * (chartWidth - 2);
  };
  const yForValue = (value: number) => {
    const t = Math.min(
      Math.max((value - scaleMin) / Math.max(scaleMax - scaleMin, 0.01), 0),
      1
    );
    return 1 + (1 - t) * (chartHeight - 2);
  };
  const sampleLinePath = (lineSamples: readonly HistoricalApySample[]) =>
    smoothChartLinePath(
      lineSamples.map((sample) => ({
        x: xForObservedAtMs(sample.observedAtMs),
        y: yForValue(sample.apyPercent),
      }))
    );
  const flatLinePath = (value: number) => {
    const y = yForValue(value).toFixed(2);
    return `M1,${y} L${(chartWidth - 1).toFixed(2)},${y}`;
  };
  const plotLines = [
    ...benchmarks
      .map((benchmark) => ({
        color: EARN_SERIES_DISPLAY[benchmark.key].color,
        d: benchmark.samples
          ? sampleLinePath(benchmark.samples)
          : flatLinePath(benchmark.apyPercentAt(endedAtMs)),
        key: benchmark.key,
      }))
      .reverse(),
    {
      color: EARN_SERIES_DISPLAY.loyal.color,
      d: sampleLinePath(samples),
      key: "loyal" as EarnComparisonSeriesKey,
    },
  ];
  const focusX = xForObservedAtMs(focusSample.observedAtMs);

  const handlePointerMove = (event: PointerEvent<HTMLDivElement>) => {
    const rect = event.currentTarget.getBoundingClientRect();
    if (rect.width <= 0) {
      return;
    }
    const x = Math.min(Math.max(event.clientX - rect.left, 0), rect.width);
    setHoverIndex(Math.round((x / rect.width) * (samples.length - 1)));
  };

  return (
    <div
      style={{
        display: "flex",
        flex: "1 1 auto",
        flexDirection: "column",
        minHeight: 0,
        width: "100%",
      }}
    >
      <style jsx>{`
        .historical-chart-reveal-rect {
          animation: historical-chart-reveal 0.7s cubic-bezier(0.2, 0, 0, 1)
            both;
          transform-origin: 0 0;
        }
        .historical-chart-mode {
          transition: opacity 0.18s ease;
        }
        .historical-chart-dot {
          animation: historical-chart-fade-in 0.25s 0.5s ease both;
        }
        @keyframes historical-chart-reveal {
          0% {
            transform: scaleX(0);
          }
          100% {
            transform: scaleX(1);
          }
        }
        @keyframes historical-chart-fade-in {
          0% {
            opacity: 0;
          }
          100% {
            opacity: 1;
          }
        }
        @media (prefers-reduced-motion: reduce) {
          .historical-chart-reveal-rect {
            animation: none;
          }
          .historical-chart-mode {
            transition: none;
          }
          .historical-chart-dot {
            animation: none;
          }
        }
      `}</style>

      <div
        style={{
          alignItems: "flex-end",
          display: "flex",
          gap: "20px",
          paddingBottom: "8px",
          width: "100%",
        }}
      >
        {headerSeries.map((series) => {
          const isPrimary = series.key === "loyal";
          return (
            <div
              key={series.key}
              style={{
                display: "flex",
                flex: "1 0 0",
                flexDirection: "column",
                gap: "2px",
                minWidth: 0,
              }}
            >
              <p
                style={{
                  color: "var(--foreground)",
                  fontFamily: font,
                  fontSize: isPrimary ? "28px" : "20px",
                  fontWeight: 600,
                  lineHeight: isPrimary ? "32px" : "24px",
                  margin: 0,
                  whiteSpace: "nowrap",
                }}
              >
                <ApyRevealText
                  isRevealed={apyDataRevealed}
                  segments={historicalApyValueSegments(
                    series.apyPercent,
                    !isPrimary
                  )}
                />
              </p>
              <span
                style={{
                  alignItems: "center",
                  display: "flex",
                  gap: "4px",
                  height: "16px",
                  width: "100%",
                }}
              >
                <span
                  aria-hidden="true"
                  style={{
                    background: series.color,
                    borderRadius: "4px",
                    flexShrink: 0,
                    height: "12px",
                    width: "12px",
                  }}
                />
                <span
                  style={{
                    color: secondary,
                    fontFamily: font,
                    fontSize: "13px",
                    lineHeight: "16px",
                    overflow: "hidden",
                    textOverflow: "ellipsis",
                    whiteSpace: "nowrap",
                  }}
                >
                  {series.label}
                </span>
              </span>
            </div>
          );
        })}
      </div>

      <div
        style={{
          display: "flex",
          fontFamily: font,
          fontSize: "13px",
          justifyContent: "space-between",
          lineHeight: "16px",
          paddingBottom: "8px",
          width: "100%",
        }}
      >
        <span style={{ color: "var(--tertiary)", whiteSpace: "nowrap" }}>
          {isHovering
            ? HISTORICAL_HOVER_DATE_FORMAT.format(focusSample.observedAtMs)
            : ""}
        </span>
        <span style={{ color: "var(--tertiary)", whiteSpace: "nowrap" }}>
          {`${scaleMax.toFixed(2)}%`}
        </span>
      </div>

      <div
        onPointerLeave={() => setHoverIndex(null)}
        onPointerMove={handlePointerMove}
        ref={chartBoxRef}
        style={{
          flex: "1 1 auto",
          minHeight: "300px",
          position: "relative",
          width: "100%",
        }}
      >
        {hasChartArea ? (
          <>
            <svg
              aria-label="Historical APY chart"
              height="100%"
              preserveAspectRatio="none"
              role="img"
              style={{ display: "block", overflow: "visible" }}
              viewBox={`0 0 ${chartWidth} ${chartHeight}`}
              width="100%"
            >
              <defs>
                <clipPath clipPathUnits="userSpaceOnUse" id={revealClipId}>
                  <rect
                    className="historical-chart-reveal-rect"
                    height={chartHeight}
                    width={chartWidth}
                    x={0}
                    y={0}
                  />
                </clipPath>
              </defs>
              <g clipPath={`url(#${revealClipId})`}>
                {plotLines.map((line) => (
                  <path
                    d={line.d}
                    fill="none"
                    key={line.key}
                    stroke={line.color}
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    strokeWidth={2}
                  />
                ))}
                <g
                  className="historical-chart-mode"
                  style={{ opacity: isHovering ? 1 : 0 }}
                >
                  <rect
                    fill="var(--card)"
                    fillOpacity={0.6}
                    height={chartHeight}
                    width={Math.max(chartWidth - focusX, 0)}
                    x={focusX}
                    y={0}
                  />
                  <line
                    stroke="var(--foreground)"
                    strokeDasharray="6 6"
                    strokeLinecap="round"
                    strokeOpacity={0.14}
                    x1={focusX}
                    x2={focusX}
                    y1={0.5}
                    y2={chartHeight - 0.5}
                  />
                </g>
              </g>
            </svg>
            {headerSeries.map((series) => (
              <span
                aria-hidden="true"
                className="historical-chart-dot"
                key={`dot-${series.key}`}
                style={{
                  background: series.color,
                  borderRadius: "9999px",
                  boxShadow: "0 0 0 2px var(--card)",
                  height: "8px",
                  left: `${((focusX / chartWidth) * 100).toFixed(2)}%`,
                  pointerEvents: "none",
                  position: "absolute",
                  top: `${(
                    (yForValue(series.apyPercent) / chartHeight) *
                    100
                  ).toFixed(2)}%`,
                  transform: "translate(-50%, -50%)",
                  width: "8px",
                }}
              />
            ))}
          </>
        ) : null}
      </div>

      <div
        style={{
          display: "flex",
          fontFamily: font,
          fontSize: "13px",
          justifyContent: "space-between",
          lineHeight: "16px",
          paddingTop: "8px",
          width: "100%",
        }}
      >
        {Array.from({ length: Math.max(axisTickCount, 2) }, (_, index) => {
          const tickCount = Math.max(axisTickCount, 2);
          const progress = index / (tickCount - 1);
          const tickMs = startedAtMs + progress * (endedAtMs - startedAtMs);
          return (
            <span
              key={index}
              style={{ color: "var(--tertiary)", whiteSpace: "nowrap" }}
            >
              {HISTORICAL_AXIS_DATE_FORMAT.format(tickMs)}
            </span>
          );
        })}
      </div>
    </div>
  );
}

export function ForecastChart({
  apy,
  apyDataRevealed,
  isBalanceHidden = false,
  mainUsdcReserveApyBps = 559,
  principal = 1000,
}: {
  apy: EarnForecastApy;
  apyDataRevealed: boolean;
  isBalanceHidden?: boolean;
  mainUsdcReserveApyBps?: number;
  principal?: number;
}) {
  const maskIfHidden = (text: string, seed: string) =>
    isBalanceHidden ? maskBalanceText(text, seed) : text;
  const revealClipId = `forecast-tab-reveal-clip-${useId().replace(
    /[^a-zA-Z0-9_-]/g,
    ""
  )}`;
  const points = useMemo(
    () =>
      buildEarnComparisonPoints(principal, apy, {
        mainUsdcReserve: mainUsdcReserveApyBps,
      }),
    [apy, mainUsdcReserveApyBps, principal]
  );
  const [hoverIndex, setHoverIndex] = useState<number | null>(null);
  const chartBoxRef = useRef<HTMLDivElement | null>(null);
  const [chartSize, setChartSize] = useState({ height: 0, width: 0 });

  useEffect(() => {
    const node = chartBoxRef.current;
    if (!node) {
      return;
    }
    const observer = new ResizeObserver(([entry]) => {
      if (!entry) {
        return;
      }
      setChartSize({
        height: entry.contentRect.height,
        width: entry.contentRect.width,
      });
    });
    observer.observe(node);
    return () => observer.disconnect();
  }, []);

  const loyalApyBps = getEarnComparisonApyBps(apy.apyBps, null);
  const loyalTarget = principal * getEarnForecastTargetMultiplier(loyalApyBps);
  const scaleRange = Math.max(loyalTarget - principal, 0.01);
  const scaleQuantum = Math.max(
    10 ** (Math.floor(Math.log10(scaleRange)) - 1),
    0.01
  );
  const scaleMax = Math.max(
    Math.ceil(loyalTarget / scaleQuantum) * scaleQuantum,
    principal + scaleRange
  );

  const isHovering = hoverIndex !== null;
  const focusIndex = hoverIndex ?? points.length - 1;
  const focusPoint = points[focusIndex];

  const headerSeries = EARN_COMPARISON_SERIES.map((series) => ({
    apyBps: getEarnComparisonApyBps(
      apy.apyBps,
      series.key === "mainUsdcReserve"
        ? mainUsdcReserveApyBps
        : series.fixedApyBps
    ),
    color: EARN_SERIES_DISPLAY[series.key].color,
    key: series.key,
    label: EARN_SERIES_DISPLAY[series.key].label,
  }));
  const lineSeries = [...EARN_COMPARISON_SERIES].reverse();

  const chartWidth = chartSize.width;
  const chartHeight = chartSize.height;
  const hasChartArea = chartWidth > 2 && chartHeight > 2;
  // The 28px nowrap money values need ~440px across three columns; narrower
  // hosts (e.g. the 400px workspace chart pane) get the compact summary that
  // the <=760px viewport media query alone can't detect.
  const isCompactSummary = chartWidth > 0 && chartWidth < 440;
  const xForIndex = (index: number) =>
    1 + (index / Math.max(points.length - 1, 1)) * (chartWidth - 2);
  const yForValue = (value: number) => {
    const t = Math.min(
      Math.max((value - principal) / (scaleMax - principal), 0),
      1
    );
    return 1 + (1 - t) * (chartHeight - 2);
  };
  const linePath = (key: EarnComparisonSeriesKey) =>
    smoothChartLinePath(
      points.map((point, index) => ({
        x: xForIndex(index),
        y: yForValue(point.values[key]),
      }))
    );
  const focusX = xForIndex(focusIndex);

  const handlePointerMove = (event: PointerEvent<HTMLDivElement>) => {
    const rect = event.currentTarget.getBoundingClientRect();
    if (rect.width <= 0) {
      return;
    }
    const x = Math.min(Math.max(event.clientX - rect.left, 0), rect.width);
    setHoverIndex(Math.round((x / rect.width) * (points.length - 1)));
  };

  return (
    <div
      style={{
        display: "flex",
        flex: "1 1 auto",
        flexDirection: "column",
        minHeight: 0,
        width: "100%",
      }}
    >
      <style jsx>{`
        .forecast-chart-reveal-rect {
          animation: forecast-chart-reveal 0.7s cubic-bezier(0.2, 0, 0, 1) both;
          transform-origin: 0 0;
        }
        .forecast-chart-mode {
          transition: opacity 0.18s ease;
        }
        .forecast-chart-dot {
          animation: forecast-chart-fade-in 0.25s 0.5s ease both;
        }
        @keyframes forecast-chart-reveal {
          0% {
            transform: scaleX(0);
          }
          100% {
            transform: scaleX(1);
          }
        }
        @keyframes forecast-chart-fade-in {
          0% {
            opacity: 0;
          }
          100% {
            opacity: 1;
          }
        }
        @media (prefers-reduced-motion: reduce) {
          .forecast-chart-reveal-rect {
            animation: none;
          }
          .forecast-chart-mode {
            transition: none;
          }
          .forecast-chart-dot {
            animation: none;
          }
        }
        @media (max-width: 760px) {
          .forecast-chart-summary {
            display: grid !important;
            gap: 8px !important;
            grid-template-columns: repeat(3, minmax(0, 1fr));
            padding-bottom: 6px !important;
          }

          .forecast-chart-summary-item {
            min-width: 0;
          }

          .forecast-chart-summary-value {
            font-size: 16px !important;
            line-height: 20px !important;
            min-width: 0;
            overflow: hidden;
          }

          .forecast-chart-summary-value[data-primary="true"] {
            font-size: 24px !important;
            line-height: 28px !important;
          }

          .forecast-chart-summary-label {
            font-size: 12px !important;
            min-width: 0;
          }
        }
        .forecast-chart-summary--compact {
          gap: 12px !important;
        }
        .forecast-chart-summary--compact
          .forecast-chart-summary-item[data-primary="true"] {
          flex: 0 0 auto !important;
        }
        .forecast-chart-summary--compact
          .forecast-chart-summary-value[data-primary="true"] {
          font-size: 24px !important;
          line-height: 28px !important;
        }
      `}</style>

      <div
        className={
          isCompactSummary
            ? "forecast-chart-summary forecast-chart-summary--compact"
            : "forecast-chart-summary"
        }
        style={{
          alignItems: "flex-end",
          display: "flex",
          gap: "20px",
          paddingBottom: "8px",
          width: "100%",
        }}
      >
        {headerSeries.map((series) => {
          const isPrimary = series.key === "loyal";
          return (
            <div
              className="forecast-chart-summary-item"
              data-primary={isPrimary ? "true" : undefined}
              key={series.key}
              style={{
                display: "flex",
                flex: "1 0 0",
                flexDirection: "column",
                gap: "2px",
                minWidth: 0,
              }}
            >
              <p
                className="forecast-chart-summary-value"
                data-primary={isPrimary ? "true" : undefined}
                style={{
                  color: isBalanceHidden
                    ? "var(--tertiary)"
                    : "var(--foreground)",
                  fontFamily: font,
                  fontSize: isPrimary ? "28px" : "16px",
                  fontWeight: 600,
                  lineHeight: isPrimary ? "32px" : "20px",
                  margin: 0,
                  whiteSpace: "nowrap",
                }}
              >
                <ApyRevealText
                  isRevealed={apyDataRevealed}
                  segments={forecastMoneySegments(
                    focusPoint.values[series.key],
                    !isBalanceHidden
                  ).map((segment, segmentIndex) => ({
                    ...segment,
                    text: maskIfHidden(
                      segment.text,
                      `${series.key}:${segmentIndex}`
                    ),
                  }))}
                />
              </p>
              <span
                style={{
                  alignItems: "center",
                  display: "flex",
                  gap: "4px",
                  height: "16px",
                  width: "100%",
                }}
              >
                <span
                  aria-hidden="true"
                  style={{
                    background: series.color,
                    borderRadius: "4px",
                    flexShrink: 0,
                    height: "12px",
                    width: "12px",
                  }}
                />
                <span
                  className="forecast-chart-summary-label"
                  style={{
                    color: secondary,
                    fontFamily: font,
                    fontSize: "13px",
                    lineHeight: "16px",
                    overflow: "hidden",
                    textOverflow: "ellipsis",
                    whiteSpace: "nowrap",
                  }}
                >
                  <ApyRevealText
                    isRevealed={apyDataRevealed}
                    segments={[
                      {
                        text: `${series.label} (${formatEarnApyPercent(
                          series.apyBps
                        )} APY)`,
                      },
                    ]}
                    withPop={false}
                  />
                </span>
              </span>
            </div>
          );
        })}
      </div>

      <div
        style={{
          display: "flex",
          fontFamily: font,
          fontSize: "13px",
          justifyContent: "space-between",
          lineHeight: "16px",
          paddingBottom: "8px",
          width: "100%",
        }}
      >
        <span style={{ color: "var(--tertiary)", whiteSpace: "nowrap" }}>
          {isHovering ? focusPoint.date : ""}
        </span>
        <span
          style={{
            color: "var(--tertiary)",
            whiteSpace: "nowrap",
          }}
        >
          {maskIfHidden(`$${formatMoney(scaleMax)}`, "scale-max")}
        </span>
      </div>

      <div
        onPointerLeave={() => setHoverIndex(null)}
        onPointerMove={handlePointerMove}
        ref={chartBoxRef}
        style={{
          flex: "1 1 auto",
          minHeight: 0,
          position: "relative",
          width: "100%",
        }}
      >
        {hasChartArea ? (
          <>
            <svg
              aria-label="Projected earnings comparison chart"
              height="100%"
              preserveAspectRatio="none"
              role="img"
              style={{ display: "block", overflow: "visible" }}
              viewBox={`0 0 ${chartWidth} ${chartHeight}`}
              width="100%"
            >
              <defs>
                <clipPath clipPathUnits="userSpaceOnUse" id={revealClipId}>
                  <rect
                    className="forecast-chart-reveal-rect"
                    height={chartHeight}
                    width={chartWidth}
                    x={0}
                    y={0}
                  />
                </clipPath>
              </defs>
              <g clipPath={`url(#${revealClipId})`}>
                <g
                  className="forecast-chart-mode"
                  style={{ opacity: isHovering ? 0 : 1 }}
                >
                  {lineSeries.map((series) => (
                    <path
                      d={linePath(series.key)}
                      fill="none"
                      key={`resting-${series.key}`}
                      stroke={EARN_SERIES_DISPLAY[series.key].color}
                      strokeDasharray={
                        series.key === "loyal" ? undefined : "4 4"
                      }
                      strokeLinecap="round"
                      strokeWidth={2}
                    />
                  ))}
                </g>
                <g
                  className="forecast-chart-mode"
                  style={{ opacity: isHovering ? 1 : 0 }}
                >
                  {lineSeries.map((series) => (
                    <path
                      d={linePath(series.key)}
                      fill="none"
                      key={`focused-${series.key}`}
                      stroke={EARN_SERIES_DISPLAY[series.key].color}
                      strokeLinecap="round"
                      strokeWidth={2}
                    />
                  ))}
                  <rect
                    fill="var(--card)"
                    fillOpacity={0.6}
                    height={chartHeight}
                    width={Math.max(chartWidth - focusX, 0)}
                    x={focusX}
                    y={0}
                  />
                  <line
                    stroke="var(--foreground)"
                    strokeDasharray="6 6"
                    strokeLinecap="round"
                    strokeOpacity={0.14}
                    x1={focusX}
                    x2={focusX}
                    y1={0.5}
                    y2={chartHeight - 0.5}
                  />
                </g>
              </g>
            </svg>
            {lineSeries.map((series) => (
              <span
                aria-hidden="true"
                className="forecast-chart-dot"
                key={`dot-${series.key}`}
                style={{
                  background: EARN_SERIES_DISPLAY[series.key].color,
                  borderRadius: "9999px",
                  boxShadow: "0 0 0 2px var(--card)",
                  height: "8px",
                  left: `${((focusX / chartWidth) * 100).toFixed(2)}%`,
                  pointerEvents: "none",
                  position: "absolute",
                  top: `${(
                    (yForValue(focusPoint.values[series.key]) / chartHeight) *
                    100
                  ).toFixed(2)}%`,
                  transform: "translate(-50%, -50%)",
                  width: "8px",
                }}
              />
            ))}
          </>
        ) : null}
      </div>

      <div
        style={{
          display: "flex",
          fontFamily: font,
          fontSize: "13px",
          justifyContent: "space-between",
          lineHeight: "16px",
          paddingTop: "8px",
          width: "100%",
        }}
      >
        <span
          style={{
            color: "var(--tertiary)",
            filter: isBalanceHidden ? "url(#rs-pixelate-sm)" : "none",
            whiteSpace: "nowrap",
          }}
        >
          {`Today · $${formatMoney(principal)}`}
        </span>
        <span style={{ color: "var(--tertiary)", whiteSpace: "nowrap" }}>
          {points[points.length - 1]?.date ?? ""}
        </span>
      </div>
    </div>
  );
}
