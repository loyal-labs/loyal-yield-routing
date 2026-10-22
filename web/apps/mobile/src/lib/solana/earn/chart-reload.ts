const YEAR_MS = 365 * 24 * 60 * 60 * 1000;
const CHART_RELOAD_FLOOR_RAW = 10_000;
const ASSUMED_MAX_APY = 0.25;

export function earnAccrualCeilingRaw(
  balanceRaw: number,
  intervalMs: number
): number {
  return (balanceRaw * ASSUMED_MAX_APY * intervalMs) / YEAR_MS;
}

export function shouldReloadEarningsChart(args: {
  prevRaw: number;
  nextRaw: number;
  intervalMs: number;
}): boolean {
  const threshold = Math.max(
    CHART_RELOAD_FLOOR_RAW,
    earnAccrualCeilingRaw(args.prevRaw, args.intervalMs)
  );
  return Math.abs(args.nextRaw - args.prevRaw) >= threshold;
}
