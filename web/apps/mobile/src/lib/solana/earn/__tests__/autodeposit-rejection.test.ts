
const mockTrack = jest.fn();
const mockFetchState = jest.fn();
const mockConfirmClose = jest.fn();
const mockPrepareClose = jest.fn();
const mockSendPreparedOperation = jest.fn();

jest.mock("expo-updates", () => ({
  channel: "production",
  runtimeVersion: "1.0.0",
  updateId: undefined,
}));
jest.mock("@/config/env", () => ({
  env: { apiBaseUrl: "https://example.test" },
}));
jest.mock(
  "@loyal-labs/actions",
  () => ({
    resolveLoyalClusterForSolanaEnv: (env: unknown) => env,
  }),
  { virtual: true },
);
jest.mock(
  "@loyal-labs/smart-account-vaults",
  () => ({
    createSmartAccountVaultsClient: () => ({
      prepareEarnUsdcAutodepositClose: (...args: unknown[]) =>
        mockPrepareClose(...args),
    }),
  }),
  { virtual: true },
);
jest.mock("@solana/web3.js", () => ({
  PublicKey: class PublicKey {
    constructor(private readonly value: string) {}
    toBase58() {
      return this.value;
    }
  },
}));
jest.mock("@/lib/analytics/analytics", () => ({
  track: (...args: unknown[]) => mockTrack(...args),
}));
jest.mock("@/lib/analytics/earn-events", () => ({
  EARN_EVENTS: {
    autodepositDisabled: "autodeposit_disabled",
    autodepositEnabled: "autodeposit_enabled",
  },
}));
jest.mock("@/lib/solana/rpc/connection", () => ({
  getConnection: () => ({}),
  getSolanaEnv: () => "mainnet",
}));
jest.mock("../earn-api", () => {
  class EarnApiError extends Error {
    constructor(message: string, readonly code?: string) {
      super(message);
    }
  }
  return {
    EarnApiError,
    confirmEarnAutodepositClose: (...args: unknown[]) =>
      mockConfirmClose(...args),
    confirmEarnAutodepositSetup: jest.fn(),
    fetchEarnAutodepositState: (...args: unknown[]) => mockFetchState(...args),
    requestEarnAutodepositSweepExecute: jest.fn(),
    toggleEarnAutodeposit: jest.fn(),
    updateEarnAutodepositFloor: jest.fn(),
  };
});
jest.mock("../earn-session", () => ({
  ensureEarnSession: jest.fn(),
}));
jest.mock("../send-prepared", () => ({
  signAndSendPreparedOperation: (...args: unknown[]) =>
    mockSendPreparedOperation(...args),
  signAndSendPreparedOperations: jest.fn(),
}));
// eslint-disable-next-line import/first
import {
  executeEarnAutodepositClose,
  setEarnAutodepositActive,
} from "../autodeposit";
// eslint-disable-next-line import/first
import { WalletRejectedError } from "@/lib/wallet/rejection";

type Envelope = {
  errorCode?: string;
  flowName: string;
  flowVariant: string;
  outcome: string;
  stage: string;
};

const walletAddress = "11111111111111111111111111111111";
const signer = {
  kind: "mwa" as const,
  publicKey: { toBase58: () => walletAddress },
  signAllTransactions: jest.fn(),
  signMessage: jest.fn(),
  signTransaction: jest.fn(),
} as unknown as Parameters<typeof setEarnAutodepositActive>[0]["signer"];

function captureEnvelopes(): Envelope[] {
  const envelopes: Envelope[] = [];
  global.fetch = jest.fn(async (_url: unknown, init: unknown) => {
    envelopes.push(JSON.parse((init as { body: string }).body) as Envelope);
    return { ok: true, json: async () => ({}) } as unknown as Response;
  }) as unknown as typeof fetch;
  return envelopes;
}

function terminal(envelopes: Envelope[]): Envelope[] {
  return envelopes.filter(
    ({ outcome }) => outcome === "cancelled" || outcome === "failed",
  );
}

beforeEach(() => {
  jest.clearAllMocks();
  mockFetchState.mockResolvedValue({
    policySigner: "policy-signer",
    programId: "program-id",
    settingsPda: "settings-pda",
  });
  mockPrepareClose.mockResolvedValue({ prepared: { instructions: [] } });
  mockSendPreparedOperation.mockResolvedValue({
    confirmedSlot: "42",
    signature: "landed-close-signature",
  });
});

describe("Autodeposit wallet rejection lifecycle", () => {
  it("keeps a pre-submit close rejection as a clean cancellation", async () => {
    const envelopes = captureEnvelopes();
    const rejection = new WalletRejectedError();
    mockSendPreparedOperation.mockRejectedValueOnce(rejection);

    await expect(
      executeEarnAutodepositClose({
        policy: "policy",
        recurringDelegation: "delegation",
        signer,
      }),
    ).rejects.toBe(rejection);

    expect(mockConfirmClose).not.toHaveBeenCalled();
    expect(terminal(envelopes)).toEqual([
      expect.objectContaining({
        errorCode: "wallet_rejected",
        flowName: "earn.autodeposit.configuration",
        flowVariant: "close",
        outcome: "cancelled",
        stage: "wallet_approval",
      }),
    ]);
  });
});
