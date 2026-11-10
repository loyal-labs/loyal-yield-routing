import { registerOneSignalSubscription } from "../push-registration";

jest.mock("@/config/env", () => ({
  env: { apiBaseUrl: "https://api.test" },
}));

const silentEarnToken = jest.fn();
jest.mock("@/lib/solana/earn/earn-session", () => ({
  earnHeaders: () => ({ "Content-Type": "application/json" }),
  silentEarnToken: (wallet: string) => silentEarnToken(wallet),
}));

const fetchMock = jest.fn();
global.fetch = fetchMock as unknown as typeof fetch;

beforeEach(() => {
  silentEarnToken.mockReset();
  fetchMock.mockReset();
});

describe("registerOneSignalSubscription", () => {
  it("stores the subscription through the wallet's Earn session", async () => {
    silentEarnToken.mockResolvedValue("bearer-1");
    fetchMock.mockResolvedValue({ ok: true });

    await expect(
      registerOneSignalSubscription("sub-1", "Wallet1"),
    ).resolves.toBe(true);

    expect(silentEarnToken).toHaveBeenCalledWith("Wallet1");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://api.test/api/push-tokens");
    expect(init.method).toBe("POST");
    expect(init.headers.Authorization).toBe("Bearer bearer-1");
    expect(JSON.parse(init.body)).toMatchObject({
      token: "sub-1",
      provider: "onesignal",
    });
  });

  it("does not post without an Earn session", async () => {
    silentEarnToken.mockResolvedValue(null);
    await expect(
      registerOneSignalSubscription("sub-1", "Wallet1"),
    ).resolves.toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("reports a rejected registration as not stored", async () => {
    silentEarnToken.mockResolvedValue("bearer-1");
    fetchMock.mockResolvedValue({ ok: false, status: 400, text: async () => "bad" });
    await expect(
      registerOneSignalSubscription("sub-1", "Wallet1"),
    ).resolves.toBe(false);
  });
});
