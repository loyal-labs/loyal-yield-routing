import { expect, test } from "bun:test";

import { buildEarnMaxEarnedBars } from "@/components/wallet-workspace/facelift/earn-max-pane";
import type { EarnMaxViewModel } from "@/features/earn-max/types";

const DAY_MS = 86_400_000;

test("a position older than the window does not book its whole balance as one day's earnings", () => {
  const now = Date.now();
  const at = (daysAgo: number) => new Date(now - daysAgo * DAY_MS).toISOString();
  const view = {
    activity: [],
    performance: [
      { equityUsd: 1000, timestamp: at(60) },
      { equityUsd: 1001, timestamp: at(29) },
      { equityUsd: 1010, timestamp: at(0) },
    ],
  } as unknown as EarnMaxViewModel;

  const total = buildEarnMaxEarnedBars(view).reduce(
    (sum, bar) => sum + bar.earnedUsd,
    0
  );

  expect(total).toBeCloseTo(10, 6);
});
