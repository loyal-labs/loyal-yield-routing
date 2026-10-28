import { earnMaxChartEarnings } from "../chart-earnings";
import type { EarnMaxSummary } from "@loyal-labs/api";
import type { EarnMaxActivityResponse } from "@loyal-labs/shared";

const summary = { coverage: "complete", earnedUsd: 0.5 } as EarnMaxSummary;
const activity: EarnMaxActivityResponse = {
  operations: [
    {
      action: "deposit",
      amountRaw: "100000000",
      id: "deposit",
      signature: null,
      status: "confirmed",
      timestamp: "2026-10-27T10:00:00Z",
    },
  ],
  performance: [
    { equityUsd: 100, timestamp: "2026-10-27T10:00:00Z" },
    { equityUsd: 100.5, timestamp: "2026-10-28T10:00:00Z" },
  ],
};
beforeAll(() => {
  jest.useFakeTimers();
  jest.setSystemTime(new Date("2026-10-28T13:00:00Z"));
});
afterAll(() => jest.useRealTimers());

test("public mobile history renders without the newer earnings field and excludes deposits", () => {
  const chart = earnMaxChartEarnings(summary, activity);
  expect(chart?.bars).toHaveLength(30);
  expect(chart?.lifetimeEarnedUsd).toBe(0.5);
  expect(chart?.rangeEarnedUsd).toBe(0.5);
  expect(chart?.todayEarnedUsd).toBe(0.5);
  expect(chart?.bars?.at(-2)?.earnedUsd).toBe(0);
});

test("attributed earnings take precedence over legacy equity history", () => {
  const chart = earnMaxChartEarnings(summary, {
    ...activity,
    earnings: {
      performanceUsd: -0.02,
      netEarnedUsd: -0.03,
      sellingCostChangeUsd: -0.01,
      days: [
        {
          date: "2026-10-28",
          performanceUsd: -0.02,
          netEarnedUsd: -0.03,
          sellingCostChangeUsd: -0.01,
        },
      ],
    },
  });
  expect(chart?.todayEarnedUsd).toBe(-0.02);
  expect(chart?.lifetimeEarnedUsd).toBe(-0.02);
});

test("explicitly unavailable or incomplete earnings stay unavailable", () => {
  expect(
    earnMaxChartEarnings(summary, { ...activity, earnings: null })
  ).toBeNull();
  expect(
    earnMaxChartEarnings(
      { ...summary, coverage: "history_incomplete" },
      activity
    )
  ).toBeNull();
  expect(
    earnMaxChartEarnings({ ...summary, earnedUsd: null }, activity)
  ).toBeNull();
});
