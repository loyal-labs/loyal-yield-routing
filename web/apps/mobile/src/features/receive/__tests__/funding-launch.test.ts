import { getWalletSigner } from "@/lib/solana/wallet/wallet-details";
import { fetchFundingLaunch } from "../funding-api";

jest.mock("@/config/env", () => ({
  env: { apiBaseUrl: "https://bridge.test" },
}));
jest.mock("@/lib/solana/wallet/wallet-details", () => ({
  getWalletSigner: jest.fn(),
}));
jest.mock("expo-secure-store", () => ({
  getItemAsync: async (key: string) =>
    key === "earn.sessions.v2"
      ? JSON.stringify({
          listed: { token: "listed-token", expiresAt: "2099-01-01T00:00:00Z" },
        })
      : null,
}));

afterEach(() => {
  jest.restoreAllMocks();
});

it.each([
  { wallet: "fresh", all: true, allowed: true, bearer: null },
  { wallet: "fresh", all: false, allowed: false, bearer: null },
  { wallet: "listed", all: false, allowed: true, bearer: "Bearer listed-token" },
])(
  "reads launch for $wallet with public rollout $all without requesting a signer",
  async ({ wallet, all, allowed, bearer }) => {
    const fetch = jest.spyOn(globalThis, "fetch").mockImplementation(
      async (_url: unknown, init?: RequestInit) => {
        const authorization = new Headers(init?.headers).get("Authorization");
        return Response.json({
          launched: all || authorization === "Bearer listed-token",
        });
      }
    );
    const signal = new AbortController().signal;
    await expect(fetchFundingLaunch(wallet, signal)).resolves.toBe(allowed);
    expect(fetch).toHaveBeenCalledTimes(1);
    const [url, init] = fetch.mock.calls[0];
    expect(url).toBe("https://bridge.test/api/funding/launch");
    expect(init?.credentials).toBe("omit");
    expect(new Headers(init?.headers).get("Authorization")).toBe(bearer);
    expect(getWalletSigner).not.toHaveBeenCalled();
  }
);
