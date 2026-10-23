import { executeMobileEarnMax } from "../mobile-earn-max";
import type { Signer } from "@/lib/wallet/signer";
import { PublicKey } from "@solana/web3.js";

const owner = new PublicKey("11111111111111111111111111111111");
const request = jest.fn();
const send = jest.fn();

jest.mock("@loyal-labs/smart-account-vaults", () => ({}), { virtual: true });
jest.mock("@/config/env", () => ({ env: { solanaEnv: "mainnet" } }));
jest.mock("@/lib/solana/rpc/connection", () => ({
  getConnection: () => ({}),
  getSolanaEnv: () => "mainnet",
}));
jest.mock("@/services/api", () => ({
  fetchEarnMax: (...args: unknown[]) => request(...args),
  provisionEarnMaxAccount: (...args: unknown[]) => request(...args),
}));
jest.mock("@/services/observability", () => ({
  startLifecycleFlow: () => ({
    observe: jest.fn(),
    complete: jest.fn(),
    failFrom: jest.fn(),
  }),
}));
jest.mock("../earn-session", () => ({
  deliberateSession: (...args: unknown[]) => request(...args),
}));
jest.mock("../send-prepared", () => ({
  signAndSendPreparedOperation: (...args: unknown[]) => send(...args),
}));

const signer = { publicKey: owner } as Signer;

test("rejects deposits below one dollar before authentication or signing", async () => {
  await expect(
    executeMobileEarnMax(signer, {
      action: "deposit",
      amountRaw: BigInt(999_999),
    })
  ).rejects.toThrow(/Minimum/);
  expect(request).not.toHaveBeenCalled();
  expect(send).not.toHaveBeenCalled();
});
