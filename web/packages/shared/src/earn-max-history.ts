import type { EarnMaxActivityItem, EarnMaxPerformancePoint } from "./earn-max";
const EARNED_WINDOW_DAYS = 30;
const ROUNDING_DUST_USD = 0.01;

function utcDayKey(date: Date): string {
  return date.toISOString().slice(0, 10);
}

export function buildEarnMaxDailySeries(view: {
  performance: EarnMaxPerformancePoint[];
  activity: EarnMaxActivityItem[];
}): {
  days: {
    date: Date;
    earnedUsd: number;
    equityUsd: number | null;
    isCurrent: boolean;
  }[];
} {
  const equityByDay = new Map<string, { first: number; last: number }>();
  for (const point of view.performance) {
    const date = new Date(point.timestamp);
    if (Number.isNaN(date.getTime())) {
      continue;
    }
    const key = utcDayKey(date);
    const existing = equityByDay.get(key);
    if (existing) {
      existing.last = point.equityUsd;
    } else {
      equityByDay.set(key, { first: point.equityUsd, last: point.equityUsd });
    }
  }
  const flowsByDay = new Map<string, number>();
  for (const operation of view.activity) {
    if (!(operation.amountRaw && operation.timestamp)) {
      continue;
    }
    const date = new Date(operation.timestamp);
    if (Number.isNaN(date.getTime())) {
      continue;
    }
    const sign =
      operation.action === "deposit" || operation.action === "install"
        ? 1
        : operation.action === "withdraw_request"
        ? -1
        : 0;
    if (sign === 0) {
      continue;
    }
    const key = utcDayKey(date);
    flowsByDay.set(
      key,
      (flowsByDay.get(key) ?? 0) +
        (sign * Number(operation.amountRaw)) / 1_000_000
    );
  }
  const now = new Date();
  const days: {
    date: Date;
    earnedUsd: number;
    equityUsd: number | null;
    isCurrent: boolean;
  }[] = [];
  const windowStart = utcDayKey(
    new Date(
      Date.UTC(
        now.getUTCFullYear(),
        now.getUTCMonth(),
        now.getUTCDate() - EARNED_WINDOW_DAYS + 1
      )
    )
  );
  let carryEquity: number | null = null;
  for (const key of [...equityByDay.keys()].sort()) {
    if (key < windowStart) carryEquity = equityByDay.get(key)!.last;
  }
  for (let offset = EARNED_WINDOW_DAYS - 1; offset >= 0; offset -= 1) {
    const date = new Date(
      Date.UTC(
        now.getUTCFullYear(),
        now.getUTCMonth(),
        now.getUTCDate() - offset
      )
    );
    const key = utcDayKey(date);
    const dayEquity = equityByDay.get(key);
    const equity: number | null = dayEquity?.last ?? carryEquity;
    const raw =
      equity === null
        ? 0
        : equity - (carryEquity ?? 0) - (flowsByDay.get(key) ?? 0);
    const earned = raw < 0 && raw > -ROUNDING_DUST_USD ? 0 : raw;
    days.push({
      date,
      earnedUsd: earned,
      equityUsd: equity,
      isCurrent: offset === 0,
    });
    carryEquity = equity;
  }
  return { days };
}
