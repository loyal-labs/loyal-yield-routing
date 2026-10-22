import type { EarnMaxActivityItem, EarnMaxPerformancePoint } from "./earn-max";
const EARNED_WINDOW_DAYS = 30;

// UTC days: Voltr's daily share prices close at 00:00 UTC, so a local-day
// split would show each day's yield on the next local day east of UTC.
function dayKey(date: Date): string {
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
  // First and last confirmed equity per calendar day — the first one anchors
  // the position's opening day so its intra-day earnings still chart.
  const equityByDay = new Map<string, { first: number; last: number }>();
  for (const point of view.performance) {
    const date = new Date(point.timestamp);
    if (Number.isNaN(date.getTime())) {
      continue;
    }
    const key = dayKey(date);
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
    const key = dayKey(date);
    flowsByDay.set(
      key,
      (flowsByDay.get(key) ?? 0) +
        (sign * Number(operation.amountRaw)) / 1_000_000
    );
  }
  // Fixed window ending today, like the Earn earnings API — days without a
  // position render as empty bars so young positions still read as 30 days.
  const now = new Date();
  const days: {
    date: Date;
    earnedUsd: number;
    equityUsd: number | null;
    isCurrent: boolean;
  }[] = [];
  const windowStart = dayKey(
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
    const key = dayKey(date);
    const dayEquity = equityByDay.get(key);
    const equity: number | null = dayEquity?.last ?? carryEquity;
    // The opening day starts from zero, so every flow that day (the first
    // deposit, a same-day top-up or withdrawal) is subtracted, never earned.
    const raw =
      equity === null
        ? 0
        : equity - (carryEquity ?? 0) - (flowsByDay.get(key) ?? 0);
    // Deposit rounding dust (< 1 cent) is not a loss; real losses still chart.
    const earned = raw < 0 && raw > -0.01 ? 0 : raw;
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
