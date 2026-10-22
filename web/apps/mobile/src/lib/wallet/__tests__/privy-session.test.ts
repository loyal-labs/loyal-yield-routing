const mockSignMessage = jest.fn(async () => new Uint8Array(64));

jest.mock("@/config/env", () => ({
  env: { apiBaseUrl: "https://example.test", vercelProtectionBypass: "" },
}));
jest.mock("@/lib/solana/wallet/wallet-details", () => ({
  getWalletSigner: jest.fn(),
}));
jest.mock(
  "expo-secure-store",
  () => ({
    getItemAsync: async () => null,
    setItemAsync: async () => undefined,
    deleteItemAsync: async () => undefined,
  }),
  { virtual: true },
);

import { exchangePrivySession } from "@/lib/wallet/privy-session";
import type { Signer } from "@/lib/wallet/signer";

function signerOf(kind: Signer["kind"]): Signer {
  return {
    kind,
    publicKey: { toBase58: () => `wallet-${kind}` },
    signMessage: mockSignMessage,
  } as unknown as Signer;
}

describe("exchangePrivySession", () => {
  beforeEach(() => {
    mockSignMessage.mockClear();
    global.fetch = jest.fn(async (url: string) =>
      url.endsWith("/api/auth/privy/complete")
        ? new Response("{}", { status: 200 })
        : new Response(
            JSON.stringify({
              token: "t",
              expiresAt: new Date(Date.now() + 3_600_000).toISOString(),
            }),
            { status: 200 },
          ),
    ) as unknown as typeof fetch;
  });

  it("does not ask a hardware wallet to sign the Earn message", async () => {
    await exchangePrivySession("identity-token", signerOf("mwa"));
    expect(mockSignMessage).not.toHaveBeenCalled();
  });
});
