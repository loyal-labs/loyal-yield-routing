"use client";

import { useEffect, useMemo, useRef, useState } from "react";

export type WeeklyChartPoint = {
  label: string;
  periodLabel: string;
  value: number;
};

type ChartPoint = WeeklyChartPoint & {
  barHeight: number;
  barWidth: number;
  centerX: number;
  height: number;
  index: number;
  slotX: number;
  x: number;
  y: number;
};

type Size = { height: number; width: number };

// The chart draws in CSS pixels, so its box sets the height and the loading
// skeleton reuses this class to hold the same space.
export const chartBoxClass = "mt-6 h-64 md:h-80 lg:h-[26rem]";

const chartPadding = {
  top: 32,
  right: 4,
  bottom: 32,
  left: 52,
};
const tickFontSize = 12;
const dateFontSize = 12;
const valueFontSize = 13;
const labelGap = 10;
const tooltipWidth = 184;
const tooltipHeight = 74;
const brandRed = "#f9363c";

function textWidth(text: string, fontSize: number) {
  return text.length * fontSize * 0.62;
}

function formatCurrencyTick(value: number) {
  if (value === 0) {
    return "$0";
  }

  if (value >= 1_000_000) {
    return `$${value / 1_000_000}M`;
  }

  if (value >= 1_000) {
    return `$${value / 1_000}K`;
  }

  return `$${value}`;
}

function formatCurrency(value: number) {
  return new Intl.NumberFormat("en-US", {
    currency: "USD",
    maximumFractionDigits: 2,
    style: "currency",
  }).format(value);
}

function formatCompactCurrency(value: number) {
  if (value === 0) {
    return "$0";
  }

  return new Intl.NumberFormat("en-US", {
    currency: "USD",
    maximumFractionDigits: value >= 1_000_000 ? 2 : 1,
    notation: "compact",
    style: "currency",
  }).format(value);
}

function formatChange(value: number) {
  if (value === 0) {
    return "$0.00";
  }

  const absolute = formatCurrency(Math.abs(value));
  return value > 0 ? `+${absolute}` : `-${absolute}`;
}

function getNiceChartMax(value: number) {
  if (value <= 0) {
    return 1;
  }

  const magnitude = 10 ** Math.floor(Math.log10(value));
  const normalized = value / magnitude;
  const niceNormalized = normalized <= 2 ? 2 : normalized <= 5 ? 5 : 10;

  return niceNormalized * magnitude;
}

function getChartTicks(maxValue: number) {
  return Array.from({ length: 5 }, (_, index) => (maxValue / 4) * index);
}

function clamp(value: number, min: number, max: number) {
  return Math.min(Math.max(value, min), max);
}

function getChartPoints(points: WeeklyChartPoint[], maxValue: number, size: Size) {
  const plotWidth = size.width - chartPadding.left - chartPadding.right;
  const plotHeight = size.height - chartPadding.top - chartPadding.bottom;
  const baseline = chartPadding.top + plotHeight;
  const slotWidth = plotWidth / Math.max(points.length, 1);
  const barWidth = Math.min(56, slotWidth * 0.64);

  return points.map((point, index) => {
    const rawHeight = (Math.min(point.value, maxValue) / maxValue) * plotHeight;
    const barHeight = point.value > 0 ? Math.max(rawHeight, 2) : 0;
    const slotX = chartPadding.left + index * slotWidth;
    const x = slotX + (slotWidth - barWidth) / 2;
    const y = baseline - barHeight;

    return {
      ...point,
      barHeight,
      barWidth,
      centerX: x + barWidth / 2,
      height: barHeight,
      index,
      slotX,
      x,
      y,
    };
  });
}

function getDateLabelStep(points: WeeklyChartPoint[], slotWidth: number) {
  const widest = Math.max(...points.map((point) => textWidth(point.label, dateFontSize)), 0);
  return Math.max(1, Math.ceil((widest + labelGap) / slotWidth));
}

function getValueLabels(points: ChartPoint[], size: Size) {
  const last = points.length - 1;
  if (last < 0 || points[last].value <= 0) {
    return [];
  }

  const place = (index: number) => {
    const point = points[index];
    const text = formatCompactCurrency(point.value);
    const half = textWidth(text, valueFontSize) / 2;
    const x = clamp(point.centerX, chartPadding.left + half, size.width - half);
    const topUnderLabel = Math.min(
      ...points
        .filter((other) => other.x < x + half && other.x + other.barWidth > x - half)
        .map((other) => other.y)
    );

    return { half, index, text, x, y: Math.max(chartPadding.top - 12, topUnderLabel - 8) };
  };

  const peak = points.reduce((best, point) => (point.value > points[best].value ? point.index : best), 0);
  const latest = place(last);
  if (peak === last) {
    return [latest];
  }

  const top = place(peak);
  const peakFits = Math.abs(latest.x - top.x) >= latest.half + top.half + labelGap;
  return peakFits ? [top, latest] : [latest];
}

function getTooltipPosition(point: ChartPoint, size: Size) {
  const maxX = size.width - chartPadding.right - tooltipWidth;
  const x = clamp(point.centerX - tooltipWidth / 2, chartPadding.left, maxX);
  const preferredY = point.y - tooltipHeight - 12;
  const y =
    preferredY >= chartPadding.top
      ? preferredY
      : point.y + Math.min(point.height + 12, 36);

  return { x, y };
}

export function EarnAumBarChart({
  ariaLabel,
  metricLabel,
  points,
}: {
  ariaLabel: string;
  metricLabel: string;
  points: WeeklyChartPoint[];
}) {
  const boxRef = useRef<HTMLDivElement>(null);
  const [size, setSize] = useState<Size | null>(null);
  const [activeIndex, setActiveIndex] = useState<number | null>(null);

  useEffect(() => {
    const box = boxRef.current;
    if (!box) {
      return;
    }

    const observer = new ResizeObserver(([entry]) => {
      const { height, width } = entry.contentRect;
      setSize({ height: Math.round(height), width: Math.round(width) });
    });
    observer.observe(box);
    return () => observer.disconnect();
  }, []);

  const chartMaxValue = useMemo(
    () => getNiceChartMax(Math.max(...points.map((point) => point.value), 0)),
    [points]
  );
  const chartTicks = useMemo(() => getChartTicks(chartMaxValue), [chartMaxValue]);

  if (!size || size.width === 0) {
    return <div className={chartBoxClass} ref={boxRef} />;
  }

  const chartPoints = getChartPoints(points, chartMaxValue, size);
  const plotHeight = size.height - chartPadding.top - chartPadding.bottom;
  const baseline = size.height - chartPadding.bottom;
  const slotWidth =
    (size.width - chartPadding.left - chartPadding.right) / Math.max(points.length, 1);
  const dateLabelStep = getDateLabelStep(points, slotWidth);
  const valueLabels = getValueLabels(chartPoints, size);
  const lastIndex = chartPoints.length - 1;
  const activePoint =
    activeIndex === null ? null : chartPoints[activeIndex] ?? null;
  const previousPoint =
    activeIndex === null ? null : chartPoints[activeIndex - 1] ?? null;
  const weeklyChange =
    activePoint && previousPoint ? activePoint.value - previousPoint.value : null;
  const tooltipPosition = activePoint ? getTooltipPosition(activePoint, size) : null;

  return (
    <div className={chartBoxClass} ref={boxRef}>
      <svg
        aria-label={ariaLabel}
        className="block overflow-visible"
        height={size.height}
        role="img"
        viewBox={`0 0 ${size.width} ${size.height}`}
        width={size.width}
      >
        {chartTicks.map((tick) => {
          const y = chartPadding.top + plotHeight - (tick / chartMaxValue) * plotHeight;

          return (
            <g key={tick}>
              <line
                stroke="#dce3ea"
                strokeDasharray="4 6"
                strokeWidth="1"
                x1={chartPadding.left}
                x2={size.width - chartPadding.right}
                y1={y}
                y2={y}
              />
              <text
                fill="#6f7782"
                fontSize={tickFontSize}
                textAnchor="end"
                x={chartPadding.left - 10}
                y={y + 4}
              >
                {formatCurrencyTick(tick)}
              </text>
            </g>
          );
        })}

        {chartPoints.map((point, index) => {
          const highlighted = index === activeIndex || index === lastIndex;

          return (
            <g key={point.label}>
              <rect
                fill={highlighted ? brandRed : "#0b0d0f"}
                height={point.barHeight}
                rx={Math.min(6, point.barWidth / 3)}
                x={point.x}
                y={point.y}
                width={point.barWidth}
              />
              {(lastIndex - index) % dateLabelStep === 0 ? (
                <text
                  fill={index === lastIndex ? brandRed : "#6f7782"}
                  fontSize={dateFontSize}
                  textAnchor="middle"
                  x={point.centerX}
                  y={size.height - 10}
                >
                  {point.label}
                </text>
              ) : null}
              <rect
                aria-label={`${point.periodLabel}: ${formatCurrency(point.value)} ${metricLabel}`}
                fill="transparent"
                height={size.height - chartPadding.top}
                onBlur={() => setActiveIndex(null)}
                onFocus={() => setActiveIndex(index)}
                onPointerEnter={() => setActiveIndex(index)}
                onPointerLeave={() => setActiveIndex(null)}
                onPointerMove={() => setActiveIndex(index)}
                role="button"
                tabIndex={0}
                x={point.slotX}
                y={chartPadding.top}
                width={slotWidth}
              />
            </g>
          );
        })}

        {valueLabels.map((label) => (
          <text
            fill={label.index === lastIndex || label.index === activeIndex ? brandRed : "#101418"}
            fontSize={valueFontSize}
            fontWeight="650"
            key={label.index}
            pointerEvents="none"
            textAnchor="middle"
            x={label.x}
            y={label.y}
          >
            {label.text}
          </text>
        ))}

        {activePoint && tooltipPosition ? (
          <g pointerEvents="none">
            <line
              stroke={brandRed}
              strokeDasharray="3 5"
              strokeWidth="1"
              x1={activePoint.centerX}
              x2={activePoint.centerX}
              y1={chartPadding.top}
              y2={baseline}
            />
            <circle
              cx={activePoint.centerX}
              cy={activePoint.y}
              fill="#ffffff"
              r="4"
              stroke={brandRed}
              strokeWidth="2"
            />
            <rect
              fill="#101418"
              height={tooltipHeight}
              rx="8"
              width={tooltipWidth}
              x={tooltipPosition.x}
              y={tooltipPosition.y}
            />
            <text
              fill="#cdd4dc"
              fontSize="12"
              x={tooltipPosition.x + 12}
              y={tooltipPosition.y + 20}
            >
              {activePoint.periodLabel}
            </text>
            <text
              fill="#ffffff"
              fontSize="18"
              fontWeight="700"
              x={tooltipPosition.x + 12}
              y={tooltipPosition.y + 44}
            >
              {formatCurrency(activePoint.value)}
            </text>
            <text
              fill={
                weeklyChange === null || weeklyChange >= 0
                  ? "#8ff0c0"
                  : "#ffb0b0"
              }
              fontSize="12"
              x={tooltipPosition.x + 12}
              y={tooltipPosition.y + 63}
            >
              {weeklyChange === null
                ? "First data point"
                : `${formatChange(weeklyChange)} vs prior week`}
            </text>
          </g>
        ) : null}
      </svg>
    </div>
  );
}
