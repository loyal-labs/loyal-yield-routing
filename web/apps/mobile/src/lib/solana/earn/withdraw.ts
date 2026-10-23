import type { EarnCleanupConfirmRequest } from "@loyal-labs/api";
import {
  hydratePreparedEarnUsdcAutodepositClose,
  hydratePreparedEarnUsdcWithdraw,
  hydratePreparedOperation,
  type SmartAccountPreparedEarnUsdcWithdraw,
  type WireSmartAccountPreparedEarnUsdcAutodepositClose,
  type WireSmartAccountPreparedEarnUsdcWithdraw,
} from "@loyal-labs/smart-account-vaults";

import { getConnection } from "@/lib/solana/rpc/connection";
import { assertSolForFees } from "@/lib/wallet/insufficient-sol-error";
import { isWalletRejection } from "@/lib/wallet/rejection";
import type { Signer } from "@/lib/wallet/signer";
import {
  type LifecycleFlow,
  mapLifecycleErrorCode,
  startLifecycleFlow,
} from "@/services/observability";

import { executeEarnAutodepositClose } from "./autodeposit";
import { withConnectionRetry } from "./connection-retry";
import {
  confirmEarnWithdraw,
  confirmEarnWithdrawCleanup,
  EarnApiError,
  type EarnWithdrawConfirmBody,
  prepareEarnWithdraw,
  prepareEarnWithdrawCleanup,
  type EarnWithdrawMode,
} from "./earn-api";
import { ensureEarnSession } from "./earn-session";
import {
  signAndSendPreparedOperations,
  type HydratedPreparedOperation,
} from "./send-prepared";

const USDC_DECIMALS = 6;
// The wallet fee-pays every withdrawal transaction itself. Observed prod cost
// is ~57k lamports for a withdraw step + cleanup (priority fees included);
// 0.001 SOL clears that with wide headroom while staying too small to block
// anyone plausible. ponytail: does NOT cover re-creating a closed wallet USDC
// ATA (~0.002 SOL rent) — that rare case still surfaces as the raw prefix
// simulation error; raise the floor if it ever shows up in ClickStack.
const WITHDRAW_MIN_FEE_LAMPORTS = 1_000_000;
const MAX_WITHDRAW_AUTODEPOSIT_CLOSES = 4;

function usdToUsdcRaw(amountUsd: number): string {
  if (!Number.isFinite(amountUsd) || amountUsd <= 0) {
    throw new Error("Withdrawal amount must be greater than 0.");
  }
  return BigInt(Math.round(amountUsd * 10 ** USDC_DECIMALS)).toString();
}

export type EarnWithdrawResult = {
  cleanupSignature?: string;
  withdrawalSignatures: string[];
};

type EarnWithdrawSendResult = EarnWithdrawResult & {
  withdrawalConfirmedSlots: string[];
};

const WITHDRAW_NETWORK_MESSAGE =
  "We couldn't reach the network to prepare the withdrawal. Your funds are safe — check your connection and try again.";

const RETRYABLE_CLEANUP_PREPARE_CODES = new Set([
  "full_withdrawal_projection_pending",
  "full_exit_verification_retryable",
]);
const CLEANUP_PREPARE_ATTEMPTS = 8;
const CLEANUP_PREPARE_RETRY_DELAY_MS = 2_500;

// The cleanup confirm 503s (`full_exit_verification_retryable`) while the
// backend's RPC catches up to the cleanup slot for its zero proof. A dropped
// confirm leaves the position row active-at-$0 (a ghost) until yield routing
// reconciles it, so retry here first.
const RETRYABLE_CLEANUP_CONFIRM_CODES = new Set([
  "full_exit_verification_retryable",
]);
const CLEANUP_CONFIRM_ATTEMPTS = 4;
const CLEANUP_CONFIRM_RETRY_DELAY_MS = 3_000;

async function retryEarnApiCall<T>(args: {
  attempts: number;
  delayMs: number;
  retryableCodes: Set<string>;
  run: () => Promise<T>;
}): Promise<T> {
  let lastError: unknown;
  for (let attempt = 0; attempt < args.attempts; attempt++) {
    if (attempt > 0) {
      await new Promise((resolve) => setTimeout(resolve, args.delayMs));
    }
    try {
      return await args.run();
    } catch (error) {
      if (isWalletRejection(error)) {
        throw error;
      }
      if (
        error instanceof EarnApiError &&
        (error.code === undefined || !args.retryableCodes.has(error.code))
      ) {
        throw error;
      }
      lastError = error;
    }
  }
  throw lastError;
}

function withdrawConfirmBody(args: {
  confirmedSlot: string;
  prepared: SmartAccountPreparedEarnUsdcWithdraw;
  signature: string;
  step?: SmartAccountPreparedEarnUsdcWithdraw["withdrawSteps"][number];
}): EarnWithdrawConfirmBody {
  const { autodepositClose: _autodepositClose, ...persistence } = (
    args.step ?? args.prepared
  ).persistence;
  const targetReserve =
    persistence.targetReserve ?? persistence.sourceTokenAccount;
  if (!targetReserve) {
    throw new Error("Prepared Earn withdrawal is missing its source identity.");
  }
  return {
    ...persistence,
    confirmedSlot: args.confirmedSlot,
    market: persistence.market ?? undefined,
    smartAccountAddress: persistence.vaultPubkey,
    targetReserve,
    withdrawalSignature: args.signature,
  };
}

async function signSendAndConfirmWithdraw(args: {
  signer: Signer;
  prepared: SmartAccountPreparedEarnUsdcWithdraw;
  flow: LifecycleFlow<"earn.withdrawal">;
}): Promise<EarnWithdrawSendResult> {
  const connection = getConnection();
  const steps = args.prepared.withdrawSteps ?? [];
  const operations: HydratedPreparedOperation[] =
    steps.length > 0 ? steps.map((step) => step.prepared) : [args.prepared.prepared];

  let confirmLost = false;
  const confirmStep = async (
    signature: string,
    confirmedSlot: string,
    step?: (typeof steps)[number],
  ) => {
    const confirm = () =>
      confirmEarnWithdraw({
        body: withdrawConfirmBody({
          confirmedSlot,
          prepared: args.prepared,
          signature,
          step,
        }),
        walletAddress: args.signer.publicKey.toBase58(),
      });
    try {
      try {
        await confirm();
      } catch (error) {
        if (error instanceof EarnApiError && (error.status ?? 0) < 500) {
          throw error;
        }
        await confirm();
      }
    } catch (error) {
      confirmLost = true;
      console.warn(
        "[earn-withdraw] confirm failed; reconciler will backfill",
        error,
      );
    }
  };

  const sent = await signAndSendPreparedOperations({
    connection,
    signer: args.signer,
    operations,
  }).catch((error) => {
    args.flow.failFrom("wallet_submit_confirm", error);
    throw error;
  });
  args.flow.observe("wallet_submit_confirm", {
    chainState: "confirmed",
    executionMode: operations.length > 1 ? "sequential" : "single",
  });

  const withdrawalSignatures: string[] = [];
  const withdrawalConfirmedSlots: string[] = [];
  for (let i = 0; i < sent.length; i++) {
    withdrawalSignatures.push(sent[i].signature);
    withdrawalConfirmedSlots.push(sent[i].confirmedSlot);
    await confirmStep(
      sent[i].signature,
      sent[i].confirmedSlot,
      steps.length > 0 ? steps[i] : undefined,
    );
  }
  args.flow.observe("backend_confirm", {
    chainState: "confirmed",
    ...(confirmLost
      ? {
          errorCode: "backend_confirmation_failed" as const,
          persistenceState: "failed" as const,
          recoveryRequired: true,
        }
      : { persistenceState: "recorded" as const }),
  });

  return { withdrawalConfirmedSlots, withdrawalSignatures };
}

async function prepareWithdraw(
  args: {
    amountRaw: string;
    mode: EarnWithdrawMode;
    signer: Signer;
    sourceId: string;
  },
  flow: LifecycleFlow<"earn.withdrawal">,
): Promise<SmartAccountPreparedEarnUsdcWithdraw> {
  const prepare = async () => {
    const response = await withConnectionRetry(
      "server prepare",
      WITHDRAW_NETWORK_MESSAGE,
      () =>
        prepareEarnWithdraw({
          amountRaw: args.mode === "full" ? "max" : args.amountRaw,
          flowId: flow.flowId,
          sourceId: args.sourceId,
          walletAddress: args.signer.publicKey.toBase58(),
        }),
    ).catch((error) => {
      flow.failFrom("prepare", error);
      throw error;
    });
    return hydratePreparedEarnUsdcWithdraw(
      response.preparedWithdraw as unknown as WireSmartAccountPreparedEarnUsdcWithdraw,
    );
  };
  let prepared = await prepare();

  for (let round = 0; prepared.autodepositClosePrepared; round++) {
    if (round >= MAX_WITHDRAW_AUTODEPOSIT_CLOSES) {
      throw new Error(
        "Couldn't remove the Autodeposit tied to this position. Delete the Autodeposit and try again.",
      );
    }
    const close = prepared.autodepositClosePrepared;
    await executeEarnAutodepositClose({
      signer: args.signer,
      policy: close.policy.account.toBase58(),
      recurringDelegation: close.subscription.recurringDelegation.toBase58(),
      source: "withdraw",
    }).catch((error) => {
      flow.failFrom("autodeposit_close", error, {
        autodepositCloseRequired: true,
      });
      throw error;
    });
    flow.observe("autodeposit_close", { autodepositCloseRequired: true });
    prepared = await prepare();
  }
  return prepared;
}

// Real on-chain Earn withdrawal from the caller's smart account (same position
// as web). Go prepares the operation; the wallet signs and pays. A full exit
// may span multiple Kamino reserves (one signed step each).
//
// `mode` "full" sends "max" so the backend uses the exact on-chain source
// amount (the passed `amountUsd` is only authoritative for "partial").
export async function executeEarnWithdraw(args: {
  signer: Signer;
  amountUsd: number;
  flowId?: string;
  mode: EarnWithdrawMode;
  sourceId: string;
}): Promise<EarnWithdrawResult> {
  const flow = startLifecycleFlow({
    ...(args.flowId ? { flowId: args.flowId } : {}),
    flowName: "earn.withdrawal",
    flowVariant: args.mode === "full" ? "full" : "partial",
    reportUnexpectedErrors: true,
    walletAddress: args.signer.publicKey.toBase58(),
  });
  flow.start("prepare");
  try {
    return await runEarnWithdraw(args, flow);
  } catch (error) {
    flow.failFrom("prepare", error);
    throw error;
  }
}

async function runEarnWithdraw(
  args: {
    signer: Signer;
    amountUsd: number;
    mode: EarnWithdrawMode;
    sourceId: string;
  },
  flow: LifecycleFlow<"earn.withdrawal">,
): Promise<EarnWithdrawResult> {
  const amountRaw = usdToUsdcRaw(args.amountUsd);
  await ensureEarnSession(args.signer);
  await assertSolForFees(
    getConnection(),
    args.signer.publicKey,
    WITHDRAW_MIN_FEE_LAMPORTS,
  ).catch((error) => {
    flow.failFrom("prepare", error);
    throw error;
  });
  const prepared = await prepareWithdraw(
    {
      amountRaw,
      mode: args.mode,
      signer: args.signer,
      sourceId: args.sourceId,
    },
    flow,
  );
  flow.observe("prepare");
  const withdrawResult = await signSendAndConfirmWithdraw({
    flow,
    prepared,
    signer: args.signer,
  });
  if (args.mode !== "full") {
    flow.complete("ui_commit");
    return {
      withdrawalSignatures: withdrawResult.withdrawalSignatures,
    };
  }

  const walletAddress = args.signer.publicKey.toBase58();
  const minContextSlot = withdrawResult.withdrawalConfirmedSlots.reduce(
    (latest, slot) => (BigInt(slot) > BigInt(latest) ? slot : latest),
  );
  let cleanupSignature: string | undefined;
  try {
    const { preparedCleanup: wireCleanup } = await retryEarnApiCall({
      attempts: CLEANUP_PREPARE_ATTEMPTS,
      delayMs: CLEANUP_PREPARE_RETRY_DELAY_MS,
      retryableCodes: RETRYABLE_CLEANUP_PREPARE_CODES,
      run: () =>
        prepareEarnWithdrawCleanup({
          flowId: flow.flowId,
          minContextSlot,
          walletAddress,
        }),
    });
    flow.observe("full_exit_verify");
    flow.observe("cleanup_prepare", { cleanupRequired: true });
    let autodepositCloseSignature: string | undefined;
    let autodepositCloseConfirmedSlot: string | undefined;
    if (wireCleanup.autodepositClosePrepared) {
      const [closeResult] = await signAndSendPreparedOperations({
        connection: getConnection(),
        signer: args.signer,
        operations: [
          hydratePreparedEarnUsdcAutodepositClose(
            wireCleanup.autodepositClosePrepared as WireSmartAccountPreparedEarnUsdcAutodepositClose,
          ).prepared,
        ],
      });
      autodepositCloseSignature = closeResult?.signature;
      autodepositCloseConfirmedSlot = closeResult?.confirmedSlot;
    }
    const [cleanupResult] = await signAndSendPreparedOperations({
      connection: getConnection(),
      signer: args.signer,
      operations: [
        hydratePreparedOperation(wireCleanup.prepared),
      ],
    });
    flow.observe("cleanup_wallet_submit_confirm", {
      chainState: "confirmed",
      cleanupRequired: true,
    });
    cleanupSignature = cleanupResult?.signature;
    if (cleanupResult) {
      const confirmBody: EarnCleanupConfirmRequest = {
        autodepositCloseConfirmedSlot,
        autodepositCloseSignature,
        cleanupSignature: cleanupResult.signature,
        confirmedSlot: cleanupResult.confirmedSlot,
        preparedCleanup: wireCleanup,
      };
      try {
        await retryEarnApiCall({
          attempts: CLEANUP_CONFIRM_ATTEMPTS,
          delayMs: CLEANUP_CONFIRM_RETRY_DELAY_MS,
          retryableCodes: RETRYABLE_CLEANUP_CONFIRM_CODES,
          run: () =>
            confirmEarnWithdrawCleanup({ body: confirmBody, walletAddress }),
        });
        flow.observe("cleanup_backend_confirm", {
          chainState: "confirmed",
          cleanupRequired: true,
          persistenceState: "recorded",
        });
      } catch (error) {
        flow.observe("cleanup_backend_confirm", {
          chainState: "confirmed",
          cleanupRequired: true,
          errorCode: "backend_confirmation_failed",
          persistenceState: "failed",
          recoveryRequired: true,
        });
        console.warn(
          "[earn-withdraw] cleanup confirm failed; backend will reconcile",
          error,
        );
      }
    }
    flow.observe("cleanup", { cleanupRequired: true });
  } catch (error) {
    flow.observe("cleanup", {
      cleanupRequired: true,
      errorCode: mapLifecycleErrorCode(error),
    });
    console.warn("[earn-withdraw] cleanup skipped after landed withdrawal", {
      errorMessage: error instanceof Error ? error.message : String(error),
      errorName: error instanceof Error ? error.name : typeof error,
      minContextSlot,
      withdrawalSignatures: withdrawResult.withdrawalSignatures,
    });
  }
  flow.complete("ui_commit");
  return {
    ...(cleanupSignature !== undefined ? { cleanupSignature } : {}),
    withdrawalSignatures: withdrawResult.withdrawalSignatures,
  };
}
