import { shouldReloadEarningsChart } from "../chart-reload";

const POLL_MS = 15_000;
const YEAR_MS = 365 * 24 * 60 * 60 * 1000;

function accrualRaw(balanceRaw: number, apy: number): number {
  return Math.round((balanceRaw * apy * POLL_MS) / YEAR_MS);
}

describe("shouldReloadEarningsChart", () => {
  it("ignores one poll of 10% APY accrual on $300k", () => {
    const prevRaw = 300_000 * 1e6;
    const nextRaw = prevRaw + accrualRaw(prevRaw, 0.1);
    expect(nextRaw - prevRaw).toBeGreaterThan(10_000);
    expect(
      shouldReloadEarningsChart({ prevRaw, nextRaw, intervalMs: POLL_MS })
    ).toBe(false);
  });

  it("reloads on a $1 deposit", () => {
    const prevRaw = 300_000 * 1e6;
    expect(
      shouldReloadEarningsChart({
        prevRaw,
        nextRaw: prevRaw + 1_000_000,
        intervalMs: POLL_MS,
      })
    ).toBe(true);
  });
});
