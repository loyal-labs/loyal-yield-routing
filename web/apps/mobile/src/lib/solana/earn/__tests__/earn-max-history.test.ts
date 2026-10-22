import { buildEarnMaxDailySeries } from "@loyal-labs/shared";

beforeEach(() => {
  jest.useFakeTimers();
  jest.setSystemTime(new Date("2026-10-21T12:00:00Z"));
});
afterEach(() => jest.useRealTimers());
const operation = (action: string, amountRaw: string, timestamp: string) => ({
  id: timestamp,
  action,
  amountRaw,
  timestamp,
  signature: null,
  status: "confirmed",
});
test("deposits and withdrawal requests are cash flows, not earnings", () => {
  const days = buildEarnMaxDailySeries({
    performance: [
      { timestamp: "2026-10-20T12:00:00Z", equityUsd: 100 },
      { timestamp: "2026-10-21T12:00:00Z", equityUsd: 76 },
    ],
    activity: [
      operation("deposit", "100000000", "2026-10-20T12:00:00Z"),
      operation("withdraw_request", "25000000", "2026-10-21T11:00:00Z"),
    ],
  }).days;
  expect(days.at(-2)?.earnedUsd).toBe(0);
  expect(days.at(-1)?.earnedUsd).toBe(1);
});
test("positions opened before the chart window do not appear as earned principal", () => {
  const days = buildEarnMaxDailySeries({
    performance: [
      { timestamp: "2026-09-01T00:00:00Z", equityUsd: 100 },
      { timestamp: "2026-10-21T12:00:00Z", equityUsd: 102 },
    ],
    activity: [operation("deposit", "100000000", "2026-09-01T00:00:00Z")],
  }).days;
  expect(days.reduce((sum, day) => sum + day.earnedUsd, 0)).toBe(2);
});
test("real losses survive while sub-cent deposit rounding is ignored", () => {
  const days = buildEarnMaxDailySeries({
    performance: [
      { timestamp: "2026-10-20T12:00:00Z", equityUsd: 99.999 },
      { timestamp: "2026-10-21T12:00:00Z", equityUsd: 95 },
    ],
    activity: [operation("deposit", "100000000", "2026-10-20T12:00:00Z")],
  }).days;
  expect(days.at(-2)?.earnedUsd).toBe(0);
  expect(days.at(-1)?.earnedUsd).toBeCloseTo(-4.999);
});
