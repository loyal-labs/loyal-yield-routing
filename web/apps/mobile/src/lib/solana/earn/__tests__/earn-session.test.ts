const store = new Map<string, string>();
const mockGetWalletSigner = jest.fn();

jest.mock("@/config/env", () => ({
  env: { apiBaseUrl: "https://example.test", vercelProtectionBypass: "" },
}));
jest.mock("@/lib/solana/wallet/wallet-details", () => ({
  getWalletSigner: () => mockGetWalletSigner(),
}));
jest.mock(
  "expo-secure-store",
  () => ({
    getItemAsync: async (key: string) => store.get(key) ?? null,
    setItemAsync: async (key: string, value: string) => {
      store.set(key, value);
    },
    deleteItemAsync: async (key: string) => {
      store.delete(key);
    },
  }),
  { virtual: true },
);

import type { Signer } from "@/lib/wallet/signer";

const WALLET = "wallet-privy";
const SESSIONS_KEY = "earn.sessions.v2";

function privySigner(): Signer {
  return {
    kind: "privy",
    publicKey: { toBase58: () => WALLET },
    signMessage: async () => new Uint8Array(64),
  } as unknown as Signer;
}

function session(token: string) {
  return {
    token,
    expiresAt: new Date(Date.now() + 3_600_000).toISOString(),
  };
}

describe("earn session", () => {
  let mints: number;
  let earn: typeof import("../earn-session");

  beforeEach(() => {
    jest.resetModules();
    store.clear();
    mints = 0;
    mockGetWalletSigner.mockResolvedValue(privySigner());
    earn = require("../earn-session");
  });

  it("mints once across reads when the server keeps rejecting the fresh token", async () => {
    store.set(SESSIONS_KEY, JSON.stringify({ [WALLET]: session("stale") }));
    global.fetch = jest.fn(async (_url: string, init?: RequestInit) => {
      if (init?.method === "POST") {
        mints += 1;
        return new Response(JSON.stringify(session(`fresh-${mints}`)), {
          status: 200,
        });
      }
      return new Response(
        JSON.stringify({ error: { code: "invalid_mobile_session" } }),
        { status: 401 },
      );
    }) as unknown as typeof fetch;

    await earn.earnWalletRead(WALLET, "https://example.test/read");
    await earn.earnWalletRead(WALLET, "https://example.test/read");

    expect(mints).toBe(1);
  });

  it("keeps storage empty when a logout lands while a rejected token is dropped", async () => {
    store.set(
      SESSIONS_KEY,
      JSON.stringify({ [WALLET]: session("stale"), other: session("kept") }),
    );

    await earn.getEarnSessionToken(WALLET);

    const dropping = earn.dropRejectedToken(WALLET, "stale");
    const clearing = earn.clearEarnSession();
    await Promise.all([dropping, clearing]);

    expect(store.has(SESSIONS_KEY)).toBe(false);
  });

  it("leaves no mint cooldown when a logout lands during the rejected retry", async () => {
    store.set(SESSIONS_KEY, JSON.stringify({ [WALLET]: session("stale") }));
    let retryStarted: () => void = () => {};
    const started = new Promise<void>((resolve) => (retryStarted = resolve));
    let releaseRetry: () => void = () => {};
    const gate = new Promise<void>((resolve) => (releaseRetry = resolve));
    let gets = 0;
    global.fetch = jest.fn(async (_url: string, init?: RequestInit) => {
      if (init?.method === "POST") {
        mints += 1;
        return new Response(JSON.stringify(session(`fresh-${mints}`)), {
          status: 200,
        });
      }
      gets += 1;
      if (gets === 2) {
        retryStarted();
        await gate;
      }
      return new Response(
        JSON.stringify({ error: { code: "invalid_mobile_session" } }),
        { status: 401 },
      );
    }) as unknown as typeof fetch;

    const reading = earn.earnWalletRead(WALLET, "https://example.test/read");
    await started;
    await earn.clearEarnSession();
    releaseRetry();
    await reading;
    await earn.silentSession(privySigner());

    expect(mints).toBe(2);
  });
});
