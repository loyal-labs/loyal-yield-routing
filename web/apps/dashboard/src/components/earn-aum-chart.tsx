"use client";

import { useEffect, useId, useMemo, useRef, useState } from "react";

export type WeeklyChartPoint = {
  label: string;
  periodLabel: string;
  value: number;
};

type ChartPoint = WeeklyChartPoint & {
  index: number;
  x: number;
  y: number;
};

type Size = { height: number; width: number };

// The chart draws in CSS pixels, so its box sets the height and the loading
// skeleton reuses this class to hold the same space.
export const chartBoxClass = "mt-6 h-64 md:h-80 lg:h-[26rem]";

const chartPadding = {
  top: 32,
  right: 8,
  bottom: 32,
  left: 52,
};
const pointInset = 6;
const tickFontSize = 12;
const axisFontSize = 12;
const valueFontSize = 13;
const labelGap = 10;
const tooltipWidth = 184;
const tooltipHeight = 74;
const ink = "#0b0d0f";
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
  const plotWidth = size.width - chartPadding.left - chartPadding.right - 2 * pointInset;
  const plotHeight = size.height - chartPadding.top - chartPadding.bottom;
  const step = points.length > 1 ? plotWidth / (points.length - 1) : 0;
  const firstX = chartPadding.left + pointInset + (points.length > 1 ? 0 : plotWidth / 2);

  return points.map((point, index) => ({
    ...point,
    index,
    x: firstX + index * step,
    y: chartPadding.top + plotHeight - (Math.min(point.value, maxValue) / maxValue) * plotHeight,
  }));
}

function getMonthLabels(points: ChartPoint[]) {
  const labels: { index: number; text: string }[] = [];
  let month: string | null = null;
  let previousRight = -Infinity;

  for (const point of points) {
    const pointMonth = point.label.split(" ")[0];
    if (pointMonth === month) {
      continue;
    }
    month = pointMonth;

    const text = point.index === 0 ? point.label : pointMonth;
    const half = textWidth(text, axisFontSize) / 2;
    if (point.x - half < previousRight + labelGap) {
      continue;
    }
    previousRight = point.x + half;
    labels.push({ index: point.index, text });
  }

  return labels;
}

function getValueLabels(points: ChartPoint[], size: Size) {
  const last = points.length - 1;
  if (last < 0 || points[last].value <= 0) {
    return [];
  }

  const reach = points.length > 1 ? points[1].x - points[0].x : 0;
  const place = (index: number) => {
    const text = formatCompactCurrency(points[index].value);
    const half = textWidth(text, valueFontSize) / 2;
    const x = clamp(points[index].x, chartPadding.left + half, size.width - half);
    const lineTop = Math.min(
      ...points.filter((point) => Math.abs(point.x - x) <= half + reach).map((point) => point.y)
    );

    return { half, index, text, x, y: Math.max(chartPadding.top - 12, lineTop - 12) };
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

function linePath(points: ChartPoint[]) {
  return points.map((point, index) => `${index ? "L" : "M"}${point.x},${point.y}`).join(" ");
}

function getTooltipPosition(point: ChartPoint, size: Size) {
  const maxX = size.width - chartPadding.right - tooltipWidth;
  const x = clamp(point.x - tooltipWidth / 2, chartPadding.left, maxX);
  const preferredY = point.y - tooltipHeight - 12;
  const y = preferredY >= chartPadding.top ? preferredY : point.y + 14;

  return { x, y };
}

export function EarnAumChart({
  ariaLabel,
  metricLabel,
  points,
}: {
  ariaLabel: string;
  metricLabel: string;
  points: WeeklyChartPoint[];
}) {
  const boxRef = useRef<HTMLDivElement>(null);
  const svgRef = useRef<SVGSVGElement>(null);
  const fillId = `earn-aum-fill-${useId().replace(/[^a-zA-Z0-9_-]/g, "")}`;
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

  if (!size || size.width === 0 || points.length === 0) {
    return <div className={chartBoxClass} ref={boxRef} />;
  }

  const chartPoints = getChartPoints(points, chartMaxValue, size);
  const plotHeight = size.height - chartPadding.top - chartPadding.bottom;
  const baseline = size.height - chartPadding.bottom;
  const lastIndex = chartPoints.length - 1;
  const lastPoint = chartPoints[lastIndex];
  const settled = chartPoints.slice(0, Math.max(lastIndex, 1));
  const thisWeek = chartPoints.slice(Math.max(lastIndex - 1, 0));
  const areaPath = `${linePath(chartPoints)} L${lastPoint.x},${baseline} L${chartPoints[0].x},${baseline} Z`;
  const monthLabels = getMonthLabels(chartPoints);
  const valueLabels = getValueLabels(chartPoints, size);
  const activePoint =
    activeIndex === null ? null : chartPoints[activeIndex] ?? null;
  const previousPoint =
    activeIndex === null ? null : chartPoints[activeIndex - 1] ?? null;
  const weeklyChange =
    activePoint && previousPoint ? activePoint.value - previousPoint.value : null;
  const tooltipPosition = activePoint ? getTooltipPosition(activePoint, size) : null;

  const nearestIndex = (clientX: number) => {
    const x = clientX - (svgRef.current?.getBoundingClientRect().left ?? 0);
    return chartPoints.reduce(
      (best, point) => (Math.abs(point.x - x) < Math.abs(chartPoints[best].x - x) ? point.index : best),
      0
    );
  };

  return (
    <div className={chartBoxClass} ref={boxRef}>
      <svg
        aria-label={ariaLabel}
        className="block overflow-visible"
        height={size.height}
        ref={svgRef}
        role="img"
        viewBox={`0 0 ${size.width} ${size.height}`}
        width={size.width}
      >
        <defs>
          <linearGradient id={fillId} x1="0" x2="0" y1="0" y2="1">
            <stop offset="0%" stopColor={ink} stopOpacity="0.14" />
            <stop offset="100%" stopColor={ink} stopOpacity="0.01" />
          </linearGradient>
        </defs>

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

        <path d={areaPath} fill={`url(#${fillId})`} />
        {lastIndex > 0 ? (
          <>
            <path
              d={linePath(settled)}
              fill="none"
              stroke={ink}
              strokeLinecap="round"
              strokeLinejoin="round"
              strokeWidth="2.25"
            />
            <path
              d={linePath(thisWeek)}
              fill="none"
              stroke={brandRed}
              strokeDasharray="5 5"
              strokeLinecap="round"
              strokeWidth="2.25"
            />
          </>
        ) : null}
        <circle cx={lastPoint.x} cy={lastPoint.y} fill={brandRed} r="4.5" stroke="#ffffff" strokeWidth="2" />

        {valueLabels.map((label) => (
          <text
            fill={label.index === lastIndex ? brandRed : "#101418"}
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

        {monthLabels.map((label) => (
          <text
            fill="#6f7782"
            fontSize={axisFontSize}
            key={label.index}
            textAnchor="middle"
            x={chartPoints[label.index].x}
            y={size.height - 10}
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
              x1={activePoint.x}
              x2={activePoint.x}
              y1={chartPadding.top}
              y2={baseline}
            />
            <circle
              cx={activePoint.x}
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

        <rect
          aria-label={
            activePoint
              ? `${activePoint.periodLabel}: ${formatCurrency(activePoint.value)} ${metricLabel}`
              : `${metricLabel} by week. Use the arrow keys to step through weeks.`
          }
          fill="transparent"
          height={plotHeight}
          onBlur={() => setActiveIndex(null)}
          onKeyDown={(event) => {
            if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") {
              return;
            }
            event.preventDefault();
            const start = activeIndex ?? lastIndex;
            setActiveIndex(clamp(start + (event.key === "ArrowRight" ? 1 : -1), 0, lastIndex));
          }}
          onPointerDown={(event) => setActiveIndex(nearestIndex(event.clientX))}
          onPointerLeave={() => setActiveIndex(null)}
          onPointerMove={(event) => setActiveIndex(nearestIndex(event.clientX))}
          role="button"
          tabIndex={0}
          width={size.width - chartPadding.left - chartPadding.right}
          x={chartPadding.left}
          y={chartPadding.top}
        />
      </svg>
    </div>
  );
}
