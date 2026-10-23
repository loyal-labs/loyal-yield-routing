import { PublicKey } from "@solana/web3.js";

// web3.js initializes its WebSocket client eagerly, but these tests only use
// PublicKey. Keep Jest from evaluating rpc-websockets' ESM-only nested uuid.
jest.mock("rpc-websockets", () => ({
  CommonClient: class CommonClient {},
  WebSocket: jest.fn(),
}));

const prepareEarnWithdrawCleanup = jest.fn();
const confirmEarnWithdraw = jest.fn();
const confirmEarnWithdrawCleanup = jest.fn();
const signAndSendPreparedOperations = jest.fn();
const observeLifecycle = jest.fn();
const failFromLifecycle = jest.fn();
const prepareEarnWithdraw = jest.fn();

// Mirrors the real EarnApiError so withdraw.ts's instanceof/code checks that
// gate cleanup-context retries stay exercised.
class MockEarnApiError extends Error {
  readonly code?: string;
  readonly status?: number;
  readonly detail?: string;

  constructor(message: string, code?: string, status?: number, detail?: string) {
    super(message);
    this.name = "EarnApiError";
    this.code = code;
    this.status = status;
    this.detail = detail;
  }
}

jest.mock(
  "@loyal-labs/actions",
  () => ({
    ...jest.requireActual("@loyal-labs/actions"),
    normalizeLoyalCluster: (cluster: string) => cluster,
  }),
  { virtual: true },
);

jest.mock(
  "@loyal-labs/smart-account-vaults",
  () => ({
    hydratePreparedEarnUsdcAutodepositClose: (wire: unknown) => wire,
    hydratePreparedEarnUsdcWithdraw: (wire: unknown) => wire,
    hydratePreparedOperation: (wire: unknown) => wire,
  }),
  { virtual: true },
);

jest.mock("@/lib/solana/rpc/connection", () => ({
  getConnection: () => ({ rpc: true }),
}));

// Kept inert: the real module pulls expo-updates, and lifecycle telemetry is
// fire-and-forget by contract.
jest.mock("@/services/observability", () => ({
  mapLifecycleErrorCode: () => "unexpected_error",
  startLifecycleFlow: () => ({
    complete: () => {},
    fail: () => {},
    failFrom: failFromLifecycle,
    flowId: "test-flow-id",
    observe: observeLifecycle,
    setVariant: () => {},
    start: () => {},
  }),
}));

jest.mock("../autodeposit", () => ({
  executeEarnAutodepositClose: jest.fn(),
}));

jest.mock("../earn-api", () => ({
  EarnApiError: MockEarnApiError,
  confirmEarnWithdraw,
  confirmEarnWithdrawCleanup,
  earnNetworkError: (message: string, detail?: string) =>
    new MockEarnApiError(message, undefined, undefined, detail),
  prepareEarnWithdrawCleanup,
  prepareEarnWithdraw,
}));

jest.mock("../send-prepared", () => ({
  signAndSendPreparedOperations,
}));

// Keep the subject import after mock initialization: this test uses virtual
// workspace-package mocks that cannot be referenced before their declarations.
// eslint-disable-next-line import/first
import { executeEarnWithdraw } from "../withdraw";
// eslint-disable-next-line import/first
import { WalletRejectedError } from "@/lib/wallet/rejection";

const address = PublicKey.default.toBase58();
const withdrawOperation = { operation: "withdraw" };
const cleanupOperation = { operation: "cleanup" };
const signer = { publicKey: PublicKey.default } as never;

const preparedWithdraw = {
  autodepositClosePrepared: null,
  mode: "full",
  persistence: {
    sourceTokenAccount: address,
    targetReserve: address,
    vaultPubkey: address,
  },
  prepared: withdrawOperation,
  withdrawSteps: [
    {
      persistence: {
        sourceTokenAccount: address,
        targetReserve: address,
        vaultPubkey: address,
      },
      prepared: withdrawOperation,
    },
  ],
};

describe("executeEarnWithdraw", () => {
  beforeEach(() => {
    jest.clearAllMocks();
    prepareEarnWithdraw.mockResolvedValue({ preparedWithdraw });
    prepareEarnWithdrawCleanup.mockResolvedValue({
      preparedCleanup: {
        autodepositClosePrepared: null,
        prepared: cleanupOperation,
      },
    });
    // clearAllMocks does not drain queued mockResolvedValueOnce values; reset
    // so tests that send only the withdraw don't leak a stale cleanup entry.
    signAndSendPreparedOperations.mockReset();
    signAndSendPreparedOperations
      .mockResolvedValueOnce([
        { confirmedSlot: "101", signature: "withdraw-signature" },
      ])
      .mockResolvedValueOnce([
        { confirmedSlot: "102", signature: "cleanup-signature" },
      ]);
  });

  test("defers final-exit cleanup until after the full withdrawal confirms", async () => {
    const result = await executeEarnWithdraw({
      amountUsd: 1,
      mode: "full",
      signer,
      sourceId: "source-1",
    });

    expect(prepareEarnWithdraw).toHaveBeenCalledWith(
      expect.objectContaining({ amountRaw: "max", sourceId: "source-1" }),
    );
    expect(confirmEarnWithdraw).toHaveBeenCalledTimes(1);
    expect(confirmEarnWithdrawCleanup).toHaveBeenCalledWith({
      body: expect.objectContaining({
        cleanupSignature: "cleanup-signature",
        confirmedSlot: "102",
      }),
      walletAddress: address,
    });
    expect(prepareEarnWithdrawCleanup).toHaveBeenCalledTimes(1);
    expect(prepareEarnWithdrawCleanup).toHaveBeenCalledWith(
      expect.objectContaining({ minContextSlot: "101" }),
    );
    expect(signAndSendPreparedOperations).toHaveBeenCalledTimes(2);
    expect(signAndSendPreparedOperations.mock.calls[1]?.[0]).toMatchObject({
      operations: [cleanupOperation],
    });
    expect(result).toEqual({
      cleanupSignature: "cleanup-signature",
      withdrawalSignatures: ["withdraw-signature"],
    });
  });

  test("returns the landed withdrawal when cleanup fails", async () => {
    const warn = jest.spyOn(console, "warn").mockImplementation(() => {});
    // Code-less EarnApiError = terminal (e.g. backend without the endpoint):
    // no retry, and the landed withdrawal must still resolve as a success.
    prepareEarnWithdrawCleanup.mockRejectedValue(
      new MockEarnApiError("Failed to prepare Earn account cleanup."),
    );

    const result = await executeEarnWithdraw({
      amountUsd: 1,
      mode: "full",
      signer,
      sourceId: "source-1",
    });

    expect(prepareEarnWithdrawCleanup).toHaveBeenCalledTimes(1);
    expect(signAndSendPreparedOperations).toHaveBeenCalledTimes(1);
    expect(confirmEarnWithdraw).toHaveBeenCalledTimes(1);
    expect(result).toEqual({
      withdrawalSignatures: ["withdraw-signature"],
    });
    // Message-matched, not counted: the fixture's placeholder mint also draws
    // a benign catalog-miss warning from tokenProgramForEarnMint.
    expect(warn).toHaveBeenCalledWith(
      "[earn-withdraw] cleanup skipped after landed withdrawal",
      expect.anything(),
    );
    warn.mockRestore();
  });

  test("returns the cleanup signature when cleanup confirm fails", async () => {
    const warn = jest.spyOn(console, "warn").mockImplementation(() => {});
    // Code-less EarnApiError = terminal (non-EarnApiError failures are treated
    // as network errors and retried; see retryEarnApiCall).
    confirmEarnWithdrawCleanup.mockRejectedValueOnce(
      new MockEarnApiError("Failed to confirm Earn account cleanup."),
    );

    const result = await executeEarnWithdraw({
      amountUsd: 1,
      mode: "full",
      signer,
      sourceId: "source-1",
    });

    expect(result).toEqual({
      cleanupSignature: "cleanup-signature",
      withdrawalSignatures: ["withdraw-signature"],
    });
    expect(warn).toHaveBeenCalledWith(
      "[earn-withdraw] cleanup confirm failed; backend will reconcile",
      expect.any(Error),
    );
    expect(observeLifecycle).toHaveBeenCalledWith(
      "cleanup_backend_confirm",
      expect.objectContaining({
        chainState: "confirmed",
        errorCode: "backend_confirmation_failed",
        persistenceState: "failed",
        recoveryRequired: true,
      }),
    );
    warn.mockRestore();
  });

  test("retries the cleanup prepare while the backend RPC catches up", async () => {
    prepareEarnWithdrawCleanup.mockRejectedValueOnce(
      new MockEarnApiError("Minimum context slot not reached", "full_withdrawal_projection_pending"),
    );

    const result = await executeEarnWithdraw({
      amountUsd: 1,
      mode: "full",
      signer,
      sourceId: "source-1",
    });

    expect(prepareEarnWithdrawCleanup).toHaveBeenCalledTimes(2);
    expect(result).toEqual({
      cleanupSignature: "cleanup-signature",
      withdrawalSignatures: ["withdraw-signature"],
    });
  });

  test("does not retry cleanup after the user rejects a wallet prompt", async () => {
    const warn = jest.spyOn(console, "warn").mockImplementation(() => {});
    prepareEarnWithdrawCleanup.mockRejectedValueOnce(
      new WalletRejectedError(),
    );

    const result = await executeEarnWithdraw({
      amountUsd: 1,
      mode: "full",
      signer,
      sourceId: "source-1",
    });

    expect(prepareEarnWithdrawCleanup).toHaveBeenCalledTimes(1);
    expect(signAndSendPreparedOperations).toHaveBeenCalledTimes(1);
    expect(result).toEqual({
      withdrawalSignatures: ["withdraw-signature"],
    });
    warn.mockRestore();
  });

  test("keeps partial withdrawals single-phase", async () => {
    const result = await executeEarnWithdraw({
      amountUsd: 1,
      mode: "partial",
      signer,
      sourceId: "source-1",
    });

    expect(prepareEarnWithdrawCleanup).not.toHaveBeenCalled();
    expect(signAndSendPreparedOperations).toHaveBeenCalledTimes(1);
    expect(result).toEqual({
      withdrawalSignatures: ["withdraw-signature"],
    });
  });
});
