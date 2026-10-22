const store = new Map<string, string>();

jest.mock("@/config/env", () => ({
  env: { apiBaseUrl: "https://example.test", vercelProtectionBypass: "" },
}));
jest.mock("@/lib/solana/wallet/wallet-details", () => ({
  getWalletSigner: async () => {
    throw new Error("no active signer");
  },
}));
jest.mock("@/lib/solana/rpc/connection", () => ({ getConnection: () => ({}) }));
jest.mock("../send-prepared", () => ({ signAndSendPreparedOperation: jest.fn() }));
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
jest.mock(
  "@loyal-labs/smart-account-vaults",
  () => ({
    createSmartAccountVaultsClient: jest.fn(),
    prepareEarnMaxVoltr: jest.fn(),
    voltrClaimPlan: jest.fn(),
    voltrDepositPlan: jest.fn(),
    voltrRequestWithdrawalPlan: jest.fn(),
  }),
  { virtual: true },
);
jest.mock("@solana/web3.js", () => ({ PublicKey: class {} }));

import type { Signer } from "@/lib/wallet/signer";

const WALLET = "wallet-mwa";

function mwaSigner(): Signer {
  return {
    kind: "mwa",
    publicKey: { toBase58: () => WALLET },
    signMessage: async () => new Uint8Array(64),
  } as unknown as Signer;
}

function json(body: unknown, status: number, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), { status, headers });
}

describe("Earn MAX read without an Earn session", () => {
  let mints: number;
  let earnMaxRead: typeof import("../earn-max-read");

  beforeEach(() => {
    jest.resetModules();
    store.clear();
    mints = 0;
    global.fetch = jest.fn(async (url: string, init?: RequestInit) => {
      if (init?.method === "POST") {
        mints += 1;
        return json(
          { token: "t", expiresAt: new Date(Date.now() + 3_600_000).toISOString() },
          200,
        );
      }
      const headers = init?.headers as Record<string, string>;
      if (!headers.Authorization) {
        return json({ error: { code: "unauthenticated" } }, 401);
      }
      const body = url.endsWith("/invite")
        ? { redeemed: true }
        : url.endsWith("/activity")
          ? { operations: [] }
          : { summary: null, config: {} };
      return json(body, 200, { "x-loyal-earn-max-contract": "earn-max-v4" });
    }) as unknown as typeof fetch;
    earnMaxRead = require("../earn-max-read");
  });

  it("asks for a session instead of failing, and one tap mints once and reads", async () => {
    const first = await earnMaxRead.readEarnMax(WALLET);
    expect(first.kind).toBe("session_required");
    expect(mints).toBe(0);

    const connected = await earnMaxRead.connectEarnMax(WALLET, mwaSigner());

    expect(mints).toBe(1);
    expect(connected.kind).toBe("ready");
  });
});
