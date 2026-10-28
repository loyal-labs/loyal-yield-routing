import { fetchMobileEarnState, fetchMobileEarnHoldings, fetchEarnEarnings, fetchEarnTransactions, fetchEarnAutodepositState } from "../earn-api";
import { fetchMobileEarnMax, fetchEarnMax } from "@/services/api";
import { earnWalletRead } from "../earn-session";

jest.mock("@/config/env", () => ({ env: { apiBaseUrl: "https://next.askloyal.com" } }));
jest.mock("@loyal-labs/shared", () => ({ EARN_AUTODEPOSIT_PROGRESS_STATES: [] }));
jest.mock("../earn-session", () => ({ earnWalletRead: jest.fn(), earnHeaders: () => ({}) }));
const request = jest.fn();
const originalFetch = global.fetch;
beforeEach(() => { jest.clearAllMocks(); global.fetch = request as unknown as typeof fetch; });
afterAll(() => { global.fetch = originalFetch; });

const wallet = "HfWgk8ccQBEynbsZJqC3fU4AZMcx5ZFoK78Wo7YJgYCM";
test("all passive Earn reads use the connected wallet without cookies or authentication", async () => {
  request.mockImplementation(async (input: string) => {
    const url = new URL(input);
    const data = url.pathname === "/api/earn-max/apy" ? { apyBps: 1631, history: [{ observedAt: "2026-10-28T13:00:00Z", apyBps: 1631 }] }
      : url.pathname.endsWith("earnings") ? { ranges: { "30D": { rangeEarnedUsd: 0.03 } } }
      : url.pathname.endsWith("transactions") ? { transactions: [] }
      : url.pathname.endsWith("autodeposit/state") ? { autodeposit: null, prepareContext: null, settingsPda: null }
      : url.pathname.endsWith("earn-max") ? (url.searchParams.get("action") === "summary" ? { summary: { balanceUsd: 0.996407, currentApyBps: 1631, forecastApyBps: -1772, realizedApyBps: -1772 } } : { operations: [], performance: [] })
      : url.pathname.endsWith("holdings") ? { currentTotalAmountRaw: "5827459", holdings: [], observedAt: "2026-10-28T13:30:34Z" }
      : { position: { currentAmountRaw: "5790715" } };
    return new Response(JSON.stringify(data));
  });
  await fetchMobileEarnState(wallet);
  expect((await fetchMobileEarnHoldings(wallet)).currentTotalAmountRaw).toBe("5827459");
  await fetchEarnEarnings(wallet);
  await fetchEarnTransactions(wallet);
  expect((await fetchEarnAutodepositState(wallet)).policySigner).toBe("");
  const max = await fetchMobileEarnMax(wallet);
  expect(max.summary?.balanceUsd).toBe(0.996407);
  expect(max.activity.earnings).toBeUndefined();
  expect(max.summary?.forecastApyBps).toBe(1631);
  expect(max.summary?.realizedApyBps).toBe(-1772);
  expect(max.summary?.apyHistory).toEqual([{ date: "2026-10-28", apyBps: 1631 }]);
  expect(request).toHaveBeenCalledTimes(8);
  for (const [input, init] of request.mock.calls) {
    const url = new URL(input);
    if (url.pathname !== "/api/earn-max/apy") {
      expect(url.searchParams.get("walletAddress")).toBe(wallet);
      expect(url.pathname).toMatch(/^\/api\/smart-accounts\/mobile\/earn/);
    }
    expect(init.credentials).toBe("omit");
    expect(init.headers).toBeUndefined();
  }
  expect(earnWalletRead).not.toHaveBeenCalled();
});

test("failed holdings are errors, never a successful zero balance", async () => {
  request.mockImplementation(async () => new Response(JSON.stringify({ error: { message: "RPC unavailable" } }), { status: 502 }));
  await expect(fetchMobileEarnHoldings(wallet)).rejects.toThrow("RPC unavailable");
  await expect(fetchMobileEarnMax(wallet)).rejects.toThrow("RPC unavailable");
});

test("transaction preparation still uses authenticated Earn MAX reads", async () => {
  jest.mocked(earnWalletRead).mockResolvedValue(new Response(JSON.stringify({ config: { walletAddress: wallet } })));
  await fetchEarnMax(wallet, "/api/smart-accounts/earn-max/summary");
  expect(earnWalletRead).toHaveBeenCalledWith(wallet, "https://next.askloyal.com/api/smart-accounts/earn-max/summary");
  expect(request).not.toHaveBeenCalled();
});
