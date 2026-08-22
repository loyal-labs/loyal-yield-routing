import type { VaultBalancePoint } from "../types";

const WIDTH = 920;
const HEIGHT = 286;
const PADDING_X = 28;
const PADDING_TOP = 22;
const PADDING_BOTTOM = 42;

function rawToUsdc(raw: bigint): number {
  return Number(raw) / 1_000_000;
}

function compactUsdc(value: number): string {
  return new Intl.NumberFormat("en-US", {
    notation: "compact",
    maximumFractionDigits: 1,
  }).format(value);
}

export function VaultBalanceChart({ points }: { points: readonly VaultBalancePoint[] }) {
  const values = points.map((point) => rawToUsdc(point.balanceRaw));
  const maximum = Math.max(...values, 1);
  const minimumObserved = Math.min(...values);
  const minimum = Math.max(0, minimumObserved - (maximum - minimumObserved || maximum) * 0.18);
  const range = Math.max(maximum - minimum, 1);
  const chartWidth = WIDTH - PADDING_X * 2;
  const chartHeight = HEIGHT - PADDING_TOP - PADDING_BOTTOM;
  const coordinates = points.map((point, index) => ({
    x: PADDING_X + (index / Math.max(points.length - 1, 1)) * chartWidth,
    y: PADDING_TOP + (1 - (rawToUsdc(point.balanceRaw) - minimum) / range) * chartHeight,
    point,
  }));
  const line = coordinates.map(({ x, y }) => `${x.toFixed(2)},${y.toFixed(2)}`).join(" ");
  const area = `${PADDING_X},${HEIGHT - PADDING_BOTTOM} ${line} ${WIDTH - PADDING_X},${HEIGHT - PADDING_BOTTOM}`;
  const labelIndexes = [0, Math.floor((points.length - 1) / 2), points.length - 1];

  return (
    <div className="chart-shell">
      <svg
        className="balance-chart"
        role="img"
        aria-label="Thirty day vault balance"
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
      >
        <defs>
          <linearGradient id="balance-fill" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="#65f2b5" stopOpacity="0.32" />
            <stop offset="100%" stopColor="#65f2b5" stopOpacity="0.01" />
          </linearGradient>
        </defs>
        {[0, 0.5, 1].map((ratio) => {
          const y = PADDING_TOP + ratio * chartHeight;
          const value = maximum - ratio * range;
          return (
            <g key={ratio}>
              <line className="chart-grid" x1={PADDING_X} x2={WIDTH - PADDING_X} y1={y} y2={y} />
              <text className="chart-value-label" x={PADDING_X} y={y - 7}>
                ${compactUsdc(value)}
              </text>
            </g>
          );
        })}
        <polygon points={area} fill="url(#balance-fill)" />
        <polyline className="chart-line" points={line} />
        {coordinates.map(({ x, y, point }) => (
          <circle className="chart-point" key={point.date} cx={x} cy={y} r="8">
            <title>{`${point.date}: $${rawToUsdc(point.balanceRaw).toLocaleString("en-US", { maximumFractionDigits: 6 })}${point.depositsRaw > 0n ? ` · deposits +$${rawToUsdc(point.depositsRaw).toLocaleString("en-US")}` : ""}${point.withdrawalsRaw > 0n ? ` · withdrawals -$${rawToUsdc(point.withdrawalsRaw).toLocaleString("en-US")}` : ""}`}</title>
          </circle>
        ))}
        {labelIndexes.map((index) => (
          <text
            className="chart-date-label"
            key={points[index].date}
            x={coordinates[index].x}
            y={HEIGHT - 12}
            textAnchor={index === 0 ? "start" : index === points.length - 1 ? "end" : "middle"}
          >
            {new Date(`${points[index].date}T00:00:00Z`).toLocaleDateString("en-US", {
              month: "short",
              day: "numeric",
              timeZone: "UTC",
            })}
          </text>
        ))}
      </svg>
    </div>
  );
}
