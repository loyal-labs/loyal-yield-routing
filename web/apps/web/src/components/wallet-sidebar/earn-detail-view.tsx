"use client";

import NumberFlow, { continuous } from "@number-flow/react";
import {
  useEffect,
  useId,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { ScrambleText, ScrambledPopDigits } from "@/components/wallet-workspace/facelift/balance-visibility";
import { chartTimestamp, EarnChartDates, EarnChartHeadline } from "@/components/wallet-workspace/facelift/earn-chart-layout";
import { ApyRevealText } from "@/components/wallet-workspace/facelift/skeleton-reveal";
import { useEarnForecastApy } from "@/hooks/use-earn-forecast-apy";
import { useEarnForecastApyHistory } from "@/hooks/use-earn-forecast-apy-history";
import {
  type EarnForecastApy,
  formatEarnApyPercent,
} from "@/lib/kamino/earn-forecast.shared";
import type {
  EarnEarningsResponse,
  EarningsRangeId,
} from "@/lib/yield-optimization/earnings.shared";
import {
  buildEarnForecastProjection,
  clampEarnForecastApyBps,
} from "@loyal-labs/shared";

export { deriveMainUsdcReserveApyBps as deriveMainUsdcReserveForecastApyBps } from "@loyal-labs/shared";

const font = "var(--font-geist-sans), sans-serif";
const LOYAL_EARN_BRAND_COLOR = "var(--primary)";
const USDC_DECIMALS = 6;
const EARN_BALANCE_DECIMALS = USDC_DECIMALS;
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

export function formatEarnActionCtaAmount(value: number) {
  if (!Number.isFinite(value)) {
    return "0";
  }

  return value.toLocaleString("en-US", {
    maximumFractionDigits: USDC_DECIMALS,
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

  const decimals = nextValue.split(".")[1] ?? "";
  if (decimals.length > USDC_DECIMALS) {
    return null;
  }

  if (nextValue.split(".")[0].replace(/,/g, "").length > 9) {
    return null;
  }

  return nextValue;
}

function forecastMoneySegments(value: number) {
  const [whole, fraction = "00"] = formatMoney(value).split(".");
  return [
    { text: `$${whole}` },
    {
      color: "var(--tertiary)",
      text: `.${fraction}`,
    },
  ];
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

const EARN_BALANCE_DISPLAY_FORMAT = {
  maximumFractionDigits: EARN_BALANCE_DECIMALS,
  minimumFractionDigits: EARN_BALANCE_DECIMALS,
  useGrouping: true,
} as const;

export function splitEarnBalanceDisplay(baseAmount: number): {
  fraction: string;
  whole: string;
} {
  const formatted = snapDollarDisplayDust(baseAmount).toLocaleString(
    "en-US",
    EARN_BALANCE_DISPLAY_FORMAT
  );
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
        }
        :global(.earn-growing-balance-flow::part(decimal)),
        :global(.earn-growing-balance-flow::part(fraction)) {
          color: ${isHidden ? "#BBBBC0" : "rgba(60, 60, 67, 0.4)"};
        }
      `}</style>
      <NumberFlow
        className="earn-growing-balance-flow earn-growing-balance-type"
        format={EARN_BALANCE_DISPLAY_FORMAT}
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
const axisDate = new Intl.DateTimeFormat("en-US", {
  day: "numeric",
  month: "short",
  timeZone: "UTC",
});
const forecastMonth = new Intl.DateTimeFormat("en-US", { month: "short", year: "numeric" });

type HistoricalApyChartProps = {
  apyDataRevealed: boolean;
  currentApyBps?: number | null;
  primarySamples?: HistoricalApySample[];
  rangeId: EarningsRangeId;
};

const subscribeToHistoricalChartHydration = () => () => undefined;
const getHistoricalChartClientSnapshot = () => true;
const getHistoricalChartServerSnapshot = () => false;

export function HistoricalApyChart({
  apyDataRevealed,
  currentApyBps,
  primarySamples,
  rangeId,
}: HistoricalApyChartProps) {
  const isHydrated = useSyncExternalStore(
    subscribeToHistoricalChartHydration,
    getHistoricalChartClientSnapshot,
    getHistoricalChartServerSnapshot
  );
  const { apy: forecast } = useEarnForecastApy();
  const history = useEarnForecastApyHistory();
  if (!isHydrated || !apyDataRevealed) {
    return <div aria-hidden="true" className="min-h-0 w-full flex-1" />;
  }

  const available =
    primarySamples !== undefined || (forecast && forecast.availability !== "unavailable");
  const current = primarySamples === undefined ? forecast?.apyBps : currentApyBps;
  const dayMs = 86_400_000;
  const todayUtc = Math.floor(Date.now() / dayMs) * dayMs;
  const rangeStartMs = todayUtc - (HISTORICAL_RANGE_CONFIG[rangeId].spanDays - 1) * dayMs;
  const samples = (primarySamples ?? toHistoricalApySamples(history)).filter(
    (sample) => sample.observedAtMs >= rangeStartMs && Number.isFinite(sample.apyPercent)
  );
  if (!available || !samples.length) {
    return (
      <>
        {available && current != null && (
          <EarnChartHeadline label="Current APY">{formatEarnApyPercent(current)}</EarnChartHeadline>
        )}
        <div className="m-auto text-center text-muted-foreground" role="status">
          {primarySamples === undefined && forecast?.source === "live"
            ? "Historical 7-day APY unavailable; live APY uses 6–24h"
            : "Historical APY unavailable"}
        </div>
      </>
    );
  }
  // Keep real observations at both ends and at every selectable point on the line.
  const plottedSamples =
    samples.length <= HISTORICAL_APY_MAX_LINE_POINTS
      ? samples
      : Array.from(
          { length: HISTORICAL_APY_MAX_LINE_POINTS },
          (_, i) =>
            samples[Math.round((i * (samples.length - 1)) / (HISTORICAL_APY_MAX_LINE_POINTS - 1))]
        );
  return (
    <>
      {primarySamples === undefined && forecast?.availability === "stale" && (
        <span className="text-xs text-muted-foreground">APY data is stale</span>
      )}
      <HistoricalApyPlot currentApyBps={current} samples={plottedSamples} />
    </>
  );
}

function HistoricalApyPlot({
  currentApyBps,
  samples,
}: {
  currentApyBps?: number | null;
  samples: HistoricalApySample[];
}) {
  const gradientId = useId();
  const [activeIndex, setActiveIndex] = useState<number | null>(null);
  const last = samples.length - 1;
  const index = Math.min(activeIndex ?? last, last);
  const selected = samples[index];
  const start = samples[0].observedAtMs;
  const end = samples[last].observedAtMs;
  const max = samples.reduce((value, s) => Math.max(value, s.apyPercent), 0);
  const min = samples.reduce((value, s) => Math.min(value, s.apyPercent), 0);
  const step = Math.max(0.01, 10 ** Math.floor(Math.log10(Math.max(max - min, 0.01))) / 5);
  const bottom = Math.floor(min / step) * step;
  const top = Math.max(bottom + step, Math.ceil(max / step) * step);
  const x = (point: HistoricalApySample) =>
    end > start ? ((point.observedAtMs - start) / (end - start)) * 100 : 100;
  const y = (point: HistoricalApySample) => 1 + ((top - point.apyPercent) / (top - bottom)) * 98;
  const line = smoothChartLinePath(
    samples.map((point) => ({ x: x(point) * 10, y: y(point) * 10 }))
  );
  const tickCount = Math.min(7, samples.length);
  const dates = Array.from({ length: tickCount }, (_, i) =>
    axisDate.format(start + ((end - start) * i) / Math.max(1, tickCount - 1))
  );
  const displayApy =
    activeIndex === null && currentApyBps != null
      ? formatEarnApyPercent(currentApyBps)
      : `${selected.apyPercent.toFixed(2)}%`;
  const label =
    activeIndex !== null
      ? chartTimestamp.format(selected.observedAtMs)
      : currentApyBps != null
      ? "Current APY"
      : "Latest recorded APY";

  return (
    <>
      <EarnChartHeadline label={label}>
        <ApyRevealText isRevealed segments={[{ text: displayApy }]} />
      </EarnChartHeadline>
      <div className="pb-2 text-right text-[13px] leading-4 text-muted-foreground">
        {top.toFixed(2)}%
      </div>
      <div
        aria-label="Historical APY chart"
        aria-valuemax={samples.length}
        aria-valuemin={1}
        aria-valuenow={index + 1}
        aria-valuetext={`${chartTimestamp.format(
          selected.observedAtMs
        )}, ${selected.apyPercent.toFixed(2)}%`}
        className="relative -ml-6 min-h-0 flex-1 touch-pan-y"
        onBlur={() => setActiveIndex(null)}
        onFocus={() => setActiveIndex(last)}
        onKeyDown={(event) => {
          const destinations: Record<string, number> = {
            ArrowLeft: index - 1,
            ArrowRight: index + 1,
            Home: 0,
            End: last,
          };
          const next = destinations[event.key];
          if (next === undefined) return;
          event.preventDefault();
          setActiveIndex(Math.min(last, Math.max(0, next)));
        }}
        onPointerLeave={() => setActiveIndex(null)}
        onPointerMove={(event) => {
          const rect = event.currentTarget.getBoundingClientRect();
          const time = start + ((end - start) * (event.clientX - rect.left)) / rect.width;
          setActiveIndex(
            samples.reduce(
              (nearest, point, i) =>
                Math.abs(point.observedAtMs - time) < Math.abs(samples[nearest].observedAtMs - time)
                  ? i
                  : nearest,
              0
            )
          );
        }}
        role="slider"
        tabIndex={0}
      >
        <svg
          aria-hidden="true"
          className="earn-apy-line absolute inset-0 size-full overflow-visible"
          preserveAspectRatio="none"
          viewBox="0 0 1000 1000"
        >
          <defs>
            <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
              <stop stopColor="var(--chart-positive)" stopOpacity="0.08" />
              <stop offset="1" stopColor="var(--chart-positive)" stopOpacity="0" />
            </linearGradient>
          </defs>
          {samples.length > 1 && (
            <path d={`${line} L1000,1000 L0,1000 Z`} fill={`url(#${gradientId})`} />
          )}
          <path
            d={line}
            fill="none"
            stroke="var(--chart-positive)"
            strokeLinecap="round"
            strokeWidth="2"
            vectorEffect="non-scaling-stroke"
          />
        </svg>
        <div
          aria-hidden="true"
          className="pointer-events-none absolute inset-y-0 right-0 border-l border-dashed border-border bg-card/60"
          style={{ left: `${x(selected)}%` }}
        />
        <span
          aria-hidden="true"
          className="pointer-events-none absolute size-3 -translate-x-1/2 -translate-y-1/2 rounded-full border-2 border-card bg-chart-positive"
          style={{ left: `${x(selected)}%`, top: `${y(selected)}%` }}
        />
        <span className="pointer-events-none absolute right-0 top-1/2 -translate-y-1/2 text-[13px] leading-4 text-muted-foreground">
          {((top + bottom) / 2).toFixed(2)}%
        </span>
      </div>
      <EarnChartDates labels={dates} />
    </>
  );
}

export function ForecastChart({
  apy,
  isBalanceHidden = false,
  principal,
}: {
  apy: EarnForecastApy;
  isBalanceHidden?: boolean;
  principal: number;
}) {
  const [activeMonth, setActiveMonth] = useState<number | null>(null);
  const rate = clampEarnForecastApyBps(apy.apyBps);
  const amounts = buildEarnForecastProjection(principal, rate, 13).slice(1);
  const now = new Date();
  const monthLabel = (index: number) =>
    forecastMonth.format(new Date(now.getFullYear(), now.getMonth() + index + 1, 1));
  const last = amounts.length - 1;
  const focus = activeMonth ?? last;
  const gain = amounts[focus] - principal;
  const maxGain = amounts[last] - principal;
  const startAmount = principal.toLocaleString("en-US", { maximumFractionDigits: 2 });

  return (
    <>
      <EarnChartHeadline label={monthLabel(focus)}>
        <ScrambledPopDigits
          isHidden={isBalanceHidden}
          popOnChange={false}
          segments={forecastMoneySegments(amounts[focus])}
        />
      </EarnChartHeadline>
      <p className="text-[13px] leading-4 text-muted-foreground">
        <span className="font-medium text-positive">
          <ScrambleText isHidden={isBalanceHidden} text={`+$${formatMoney(gain)}`} />
        </span>{" "}
        estimated yield
      </p>
      <div
        className="relative flex min-h-0 flex-1 items-end gap-1.5 pt-2 @min-[640px]:gap-3"
        onPointerLeave={() => setActiveMonth(null)}
      >
        <p className="pointer-events-none absolute left-0 top-0 z-10 text-[13px] leading-4 text-muted-foreground">
          with {formatEarnApyPercent(rate)} simulated APY
        </p>
        {amounts.map((amount, i) => (
          <button
            aria-label={
              isBalanceHidden
                ? monthLabel(i)
                : `${monthLabel(i)}, projected balance $${formatMoney(amount)}`
            }
            className="flex h-full min-w-0 flex-1 cursor-pointer items-end rounded-sm"
            key={i}
            onBlur={() => setActiveMonth(null)}
            onFocus={() => setActiveMonth(i)}
            onPointerEnter={() => setActiveMonth(i)}
            onClick={() => setActiveMonth(i)}
            type="button"
          >
            <span
              className={`earn-chart-bar w-full rounded-[4px] bg-chart-positive transition-opacity ${
                i > focus ? "opacity-20" : "opacity-50"
              }`}
              style={{ height: `${maxGain > 0 ? ((amount - principal) / maxGain) * 100 : 0}%` }}
            />
          </button>
        ))}
      </div>
      <div className="flex justify-between pt-2 text-[13px] leading-4 text-muted-foreground">
        <span>
          Today · <ScrambleText isHidden={isBalanceHidden} text={`$${startAmount}`} />
        </span>
        <span>{monthLabel(last)}</span>
      </div>
    </>
  );
}
