jest.mock("@/config/env", () => ({
  env: { apiBaseUrl: "https://example.test" },
}));
jest.mock("@/lib/solana/rpc/connection", () => ({ getConnection: () => ({}) }));
jest.mock("../send-prepared", () => ({ signAndSendPreparedOperation: jest.fn() }));
jest.mock("../earn-session", () => ({
  earnHeaders: () => ({}),
  ensureEarnSession: async () => "token",
  earnWalletRead: async (_wallet: string, url: string) => fetch(url),
}));
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

import { fetchEarnMaxSummary, redeemEarnMaxInvite } from "../earn-max";

function refuse(status: number, code: string) {
  global.fetch = jest.fn(
    async () => new Response(JSON.stringify({ error: { code } }), { status }),
  ) as unknown as typeof fetch;
}

describe("Earn MAX errors", () => {
  it("reads smart_account_required by its code, not its 409 status", async () => {
    refuse(409, "smart_account_required");

    await expect(
      redeemEarnMaxInvite({} as Signer, "ABC123"),
    ).rejects.toMatchObject({ code: "smart_account_required" });
    await expect(fetchEarnMaxSummary("wallet")).rejects.toMatchObject({
      code: "smart_account_required",
      message: expect.not.stringContaining("request failed"),
    });
  });
});
