import { EarnApiError } from "../earn-api";
import { executeMobileEarnMax, readMobileEarnMax } from "../mobile-earn-max";
import type { Signer } from "@/lib/wallet/signer";
import { PublicKey, SystemProgram } from "@solana/web3.js";

const owner = new PublicKey("11111111111111111111111111111111");
const outer = SystemProgram.transfer({
  fromPubkey: owner,
  toPubkey: owner,
  lamports: 1,
});
const inner = SystemProgram.transfer({
  fromPubkey: owner,
  toPubkey: owner,
  lamports: 2,
});
const network = jest.fn();
const prepare = jest.fn();
const settings = jest.fn();
const deposit = jest.fn();
const claim = jest.fn();
const send = jest.fn();
const request = jest.fn();
const getToken = jest.fn();
const clearToken = jest.fn();
const signAuth = jest.fn();

jest.mock(
  "@loyal-labs/smart-account-vaults",
  () => ({
    VoltrClaimLiquidityError: class extends Error {},
    createSmartAccountVaultsClient: () => ({
      sdk: { smartAccounts: { queries: { fetchSettings: settings } } },
      prepareCustomInstructionSync: prepare,
    }),
    deriveEarnMaxVoltrAuthority: () => owner,
    EARN_MAX_VOLTR_ACCOUNT_INDEX: 2,
    voltrDepositPlan: (...args: unknown[]) => deposit(...args),
    voltrClaimPlan: (...args: unknown[]) => claim(...args),
  }),
  { virtual: true }
);
jest.mock("@/config/env", () => ({ env: { solanaEnv: "mainnet" } }));
jest.mock("@/lib/solana/rpc/connection", () => ({
  getConnection: () => ({}),
  getSolanaEnv: () => network(),
}));
jest.mock("@/services/api", () => ({
  requestEarnMax: (...args: unknown[]) => request(...args),
}));
jest.mock("@/services/observability", () => ({
  startLifecycleFlow: () => ({
    observe: jest.fn(),
    complete: jest.fn(),
    failFrom: jest.fn(),
  }),
}));
jest.mock("../earn-session", () => ({
  getEarnSessionToken: () => getToken(),
  clearEarnSession: () => clearToken(),
}));
jest.mock("../earn-auth", () => ({
  signEarnAuth: () => signAuth(),
  withEarnAuth: (
    _s: unknown,
    auth: unknown,
    _p: unknown,
    call: (auth: unknown) => unknown
  ) => call(auth),
}));
jest.mock("../send-prepared", () => ({
  signAndSendPreparedOperation: (...args: unknown[]) => send(...args),
}));

const signer = { publicKey: owner } as Signer;
beforeEach(() => {
  jest.resetAllMocks();
  network.mockReturnValue("mainnet");
  getToken.mockResolvedValue("session");
  request.mockResolvedValue({
    cluster: "mainnet",
    walletAddress: owner.toBase58(),
    settingsPda: owner.toBase58(),
    programId: owner.toBase58(),
  });
  settings.mockResolvedValue({ threshold: 1, timeLock: 0 });
  deposit.mockReturnValue({ outer: [outer], vault: [inner] });
  prepare.mockResolvedValue({
    instructions: [inner],
    lookupTableAccounts: [],
    payer: owner,
  });
  send.mockResolvedValue({ signature: "confirmed", confirmedSlot: "1" });
});

test("deposit funds the vault before sync execution and returns confirmed signature", async () => {
  await expect(
    executeMobileEarnMax(signer, {
      action: "deposit",
      amountRaw: BigInt(1_000_000),
    })
  ).resolves.toMatchObject({ signature: "confirmed" });
  expect(prepare.mock.calls[0][0].accountIndex).toBe(2);
  expect(send.mock.calls[0][0].operation.instructions).toEqual([outer, inner]);
});

test("rejects a mismatched wallet before preparing or signing", async () => {
  request.mockResolvedValue({
    cluster: "mainnet",
    walletAddress: "another wallet",
  });
  await expect(
    executeMobileEarnMax(signer, {
      action: "deposit",
      amountRaw: BigInt(1_000_000),
    })
  ).rejects.toThrow(/does not match/);
  expect(prepare).not.toHaveBeenCalled();
  expect(send).not.toHaveBeenCalled();
});

test("rejects multisigner accounts and invalid amounts before sending", async () => {
  settings.mockResolvedValue({ threshold: 2, timeLock: 0 });
  await expect(
    executeMobileEarnMax(signer, {
      action: "deposit",
      amountRaw: BigInt(1_000_000),
    })
  ).rejects.toThrow(/single-signer/);
  await expect(
    executeMobileEarnMax(signer, { action: "deposit", amountRaw: BigInt(0) })
  ).rejects.toThrow(/positive/);
  expect(send).not.toHaveBeenCalled();
});

test("claim liquidity rejection never reaches signing and releases the operation lock", async () => {
  claim.mockRejectedValue(new Error("Insufficient liquidity"));
  await expect(
    executeMobileEarnMax(signer, { action: "claim" })
  ).rejects.toThrow(/liquidity/);
  expect(send).not.toHaveBeenCalled();
  await expect(
    executeMobileEarnMax(signer, {
      action: "deposit",
      amountRaw: BigInt(1_000_000),
    })
  ).resolves.toMatchObject({ signature: "confirmed" });
});

test("existing sign-in session avoids a wallet prompt after bearer expiry", async () => {
  request
    .mockRejectedValueOnce(
      new EarnApiError("expired", "invalid_mobile_session", 401)
    )
    .mockResolvedValueOnce({ summary: null });
  await expect(readMobileEarnMax(signer, "summary")).resolves.toEqual({
    summary: null,
  });
  expect(clearToken).toHaveBeenCalledTimes(1);
  expect(signAuth).not.toHaveBeenCalled();
});

test("missing sessions sign once; server failures never trigger a wallet prompt", async () => {
  getToken.mockResolvedValue(null);
  request
    .mockRejectedValueOnce(new EarnApiError("sign in", "unauthenticated", 401))
    .mockResolvedValueOnce({ summary: null });
  signAuth.mockResolvedValue({ signature: "signed" });
  await readMobileEarnMax(signer, "summary");
  expect(signAuth).toHaveBeenCalledTimes(1);
  request.mockRejectedValue(
    new EarnApiError("unavailable", "earn_max_unavailable", 404)
  );
  await expect(readMobileEarnMax(signer, "summary")).rejects.toThrow(
    /unavailable/
  );
  expect(signAuth).toHaveBeenCalledTimes(1);
});

test("active network override and a network change during prepare prevent signing", async () => {
  network.mockReturnValue("devnet");
  await expect(
    executeMobileEarnMax(signer, { action: "claim" })
  ).rejects.toThrow(/mainnet/);
  expect(request).not.toHaveBeenCalled();
  network
    .mockReturnValueOnce("mainnet")
    .mockReturnValueOnce("mainnet")
    .mockReturnValue("devnet");
  await expect(
    executeMobileEarnMax(signer, {
      action: "deposit",
      amountRaw: BigInt(1_000_000),
    })
  ).rejects.toThrow(/mainnet/);
  expect(send).not.toHaveBeenCalled();
});

test("concurrent operations for one wallet cannot submit twice", async () => {
  let finish!: (value: unknown) => void;
  send.mockImplementation(
    () =>
      new Promise((resolve) => {
        finish = resolve;
      })
  );
  const first = executeMobileEarnMax(signer, {
    action: "deposit",
    amountRaw: BigInt(1_000_000),
  });
  await expect(
    executeMobileEarnMax(signer, { action: "claim" })
  ).rejects.toThrow(/already in progress/);
  while (!finish) await new Promise((resolve) => setImmediate(resolve));
  finish({ signature: "confirmed", confirmedSlot: "1" });
  await first;
  expect(send).toHaveBeenCalledTimes(1);
});

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
