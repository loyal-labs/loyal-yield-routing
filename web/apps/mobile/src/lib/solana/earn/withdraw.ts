import { normalizeLoyalCluster } from "@loyal-labs/actions";
import {
  createSmartAccountVaultsClient,
  hydratePreparedOperation,
  serializePreparedEarnUsdcWithdraw,
  type SmartAccountEarnUsdcCleanupInput,
  type SmartAccountEarnUsdcWithdrawInput,
} from "@loyal-labs/smart-account-vaults";
import { PublicKey } from "@solana/web3.js";

import { getConnection } from "@/lib/solana/rpc/connection";
import { assertSolForFees } from "@/lib/wallet/insufficient-sol-error";
import { isWalletRejection } from "@/lib/wallet/rejection";
import type { Signer } from "@/lib/wallet/signer";
import {
  type LifecycleDiagnostics,
  type LifecycleFlow,
  mapLifecycleErrorCode,
  startLifecycleFlow,
} from "@/services/observability";

import { executeEarnAutodepositClose } from "./autodeposit";
import { withConnectionRetry } from "./connection-retry";
import { tokenProgramForEarnMint } from "./earn-product-mints";
import {
  confirmEarnWithdraw,
  confirmEarnWithdrawCleanup,
  EarnApiError,
  type EarnWithdrawCleanupPrepareContext,
  fetchEarnWithdrawCleanupPrepareContext,
  fetchEarnWithdrawPrepareContext,
  prepareEarnWithdraw,
  type EarnAuthFields,
  type EarnWithdrawMode,
  type EarnWithdrawPrepareContext,
  type EarnWithdrawSource,
  type WirePreparedEarnWithdraw,
} from "./earn-api";
import { signEarnAuth, withEarnAuth } from "./earn-auth";
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

function wireTokenProgram(
  program: string | null | undefined,
  mint: string,
): PublicKey {
  return program ? new PublicKey(program) : tokenProgramForEarnMint(mint);
}

function hydrateEarnWithdrawInput(
  context: EarnWithdrawPrepareContext,
  walletAddress: PublicKey,
): SmartAccountEarnUsdcWithdrawInput {
  const wire = context.withdrawInput;
  const base = {
    amountRaw: BigInt(wire.amountRaw),
    closePoliciesOnFullWithdrawal: wire.closePoliciesOnFullWithdrawal,
    cluster: normalizeLoyalCluster(context.cluster),
    feePayer: walletAddress,
    policySigner: new PublicKey(wire.policySigner),
    settingsPda: new PublicKey(context.settingsPda),
    walletAddress,
    ...(wire.source
      ? {
          source:
            wire.source.type === "idle"
              ? {
                  amountRaw: BigInt(wire.source.amountRaw),
                  id: wire.source.id,
                  mint: new PublicKey(wire.source.mint),
                  tokenAccount: new PublicKey(wire.source.tokenAccount),
                  tokenProgramId: new PublicKey(wire.source.tokenProgramId),
                  type: "idle" as const,
                }
              : {
                  amountRaw: BigInt(wire.source.amountRaw),
                  id: wire.source.id,
                  liquidityMint: new PublicKey(wire.source.liquidityMint),
                  market: new PublicKey(wire.source.market),
                  reserve: new PublicKey(wire.source.reserve),
                  type: "reserve" as const,
                },
        }
      : {}),
    ...(wire.target
      ? {
          target: {
            liquidityMint: new PublicKey(wire.target.liquidityMint),
            liquidityTokenProgram: wireTokenProgram(
              wire.target.liquidityTokenProgram,
              wire.target.liquidityMint,
            ),
            market: new PublicKey(wire.target.market),
            reserve: new PublicKey(wire.target.reserve),
            supplyApyBps: wire.target.supplyApyBps
              ? BigInt(wire.target.supplyApyBps)
              : null,
          },
        }
      : {}),
    ...(wire.fullWithdrawalTargets
      ? {
          fullWithdrawalTargets: wire.fullWithdrawalTargets.map((target) => ({
            ...(target.amountRaw
              ? { amountRaw: BigInt(target.amountRaw) }
              : {}),
            liquidityMint: new PublicKey(target.liquidityMint),
            liquidityTokenProgram: wireTokenProgram(
              target.liquidityTokenProgram,
              target.liquidityMint,
            ),
            market: new PublicKey(target.market),
            reserve: new PublicKey(target.reserve),
            ...(target.reserveCollateralMint
              ? {
                  reserveCollateralMint: new PublicKey(
                    target.reserveCollateralMint,
                  ),
                }
              : {}),
            ...(target.reserveLiquiditySupply
              ? {
                  reserveLiquiditySupply: new PublicKey(
                    target.reserveLiquiditySupply,
                  ),
                }
              : {}),
            supplyApyBps: target.supplyApyBps
              ? BigInt(target.supplyApyBps)
              : null,
            ...(target.vaultCollateralAta
              ? { vaultCollateralAta: new PublicKey(target.vaultCollateralAta) }
              : {}),
          })),
        }
      : {}),
    yieldRoutingPolicy: {
      account: new PublicKey(wire.yieldRoutingPolicy.account),
      seed: BigInt(wire.yieldRoutingPolicy.seed),
      ...(wire.yieldRoutingPolicy.setupPolicy
        ? {
            setupPolicy: {
              account: new PublicKey(
                wire.yieldRoutingPolicy.setupPolicy.account,
              ),
              seed: BigInt(wire.yieldRoutingPolicy.setupPolicy.seed),
            },
          }
        : {}),
    },
  };
  return wire.mode === "full"
    ? { ...base, mode: "full" }
    : { ...base, mode: "partial" };
}

function hydrateEarnWithdrawCleanupInput(
  context: EarnWithdrawCleanupPrepareContext,
  walletAddress: PublicKey,
): SmartAccountEarnUsdcCleanupInput {
  const wire = context.cleanupInput;
  return {
    cluster: normalizeLoyalCluster(context.cluster),
    feePayer: walletAddress,
    policySigner: new PublicKey(wire.policySigner),
    vaultTokenAccounts: wire.vaultTokenAccounts.map((account) => ({
      address: new PublicKey(account.address),
      amountRaw: BigInt(account.amountRaw),
      decimals: account.decimals,
      mint: new PublicKey(account.mint),
      tokenProgramId: new PublicKey(account.tokenProgramId),
    })),
    settingsPda: new PublicKey(context.settingsPda),
    walletAddress,
    yieldRoutingPolicy: {
      account: new PublicKey(wire.yieldRoutingPolicy.account),
      seed: BigInt(wire.yieldRoutingPolicy.seed),
      ...(wire.yieldRoutingPolicy.setupPolicy
        ? {
            setupPolicy: {
              account: new PublicKey(
                wire.yieldRoutingPolicy.setupPolicy.account,
              ),
              seed: BigInt(wire.yieldRoutingPolicy.setupPolicy.seed),
            },
          }
        : {}),
    },
  };
}

const WITHDRAW_NETWORK_MESSAGE =
  "We couldn't reach the network to prepare the withdrawal. Your funds are safe — check your connection and try again.";

const RETRYABLE_CLEANUP_CONTEXT_CODES = new Set([
  "context_failed",
  "resolve_failed",
]);
const CLEANUP_CONTEXT_ATTEMPTS = 8;
const CLEANUP_CONTEXT_RETRY_DELAY_MS = 2_500;

// The cleanup confirm 503s (`full_exit_verification_retryable`) while the
// backend's RPC catches up to the cleanup slot for its zero proof. A dropped
// confirm leaves the position row active-at-$0 (a ghost) until yield routing
// reconciles it, so retry here first.
const RETRYABLE_CLEANUP_CONFIRM_CODES = new Set([
  "full_exit_verification_retryable",
]);
const CLEANUP_CONFIRM_ATTEMPTS = 4;
const CLEANUP_CONFIRM_RETRY_DELAY_MS = 3_000;

// The lifecycle contract has a single coarse `prepare` stage and no substage
// field, so a failed front-half reports WHICH sub-call died via the contract's
// bounded `stageIndex`/`stageCount` ints, in execution order (ASK-2095). The
// cause itself travels as the enum-only `errorDetail` (ASK-2018).
const PREPARE_SUBSTAGES = [
  "wallet_auth",
  "prepare_context",
  "device_prepare",
  "server_prepare",
] as const;

function prepareSubstage(
  substage: (typeof PREPARE_SUBSTAGES)[number],
): LifecycleDiagnostics {
  return {
    stageCount: PREPARE_SUBSTAGES.length,
    stageIndex: PREPARE_SUBSTAGES.indexOf(substage),
  };
}

function isRetryExhaustion(error: unknown): error is EarnApiError {
  return (
    error instanceof EarnApiError &&
    error.code === undefined &&
    error.status === undefined
  );
}

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

async function fetchCleanupContextWithRetry(args: {
  signer: Signer;
  prepareAuth: EarnAuthFields;
  minContextSlot: string;
  flowId: string;
}): Promise<EarnWithdrawCleanupPrepareContext> {
  return retryEarnApiCall({
    attempts: CLEANUP_CONTEXT_ATTEMPTS,
    delayMs: CLEANUP_CONTEXT_RETRY_DELAY_MS,
    retryableCodes: RETRYABLE_CLEANUP_CONTEXT_CODES,
    run: () =>
      withEarnAuth(
        args.signer,
        args.prepareAuth,
        "earn-withdraw-prepare",
        (auth) =>
          fetchEarnWithdrawCleanupPrepareContext({
            auth,
            flowId: args.flowId,
            minContextSlot: args.minContextSlot,
          }),
      ),
  });
}

async function signSendAndConfirmWithdraw(args: {
  signer: Signer;
  prepareAuth: EarnAuthFields;
  operations: HydratedPreparedOperation[];
  hasSteps: boolean;
  wirePreparedWithdraw: WirePreparedEarnWithdraw;
  flow: LifecycleFlow<"earn.withdrawal">;
}): Promise<EarnWithdrawSendResult> {
  const connection = getConnection();

  let confirmLost = false;
  const confirmStep = async (
    withdrawalSignature: string,
    confirmedSlot: string,
    stepIndex?: number,
  ) => {
    try {
      await withEarnAuth(
        args.signer,
        args.prepareAuth,
        "earn-withdraw-confirm",
        (auth) =>
          confirmEarnWithdraw({
            auth,
            preparedWithdraw: args.wirePreparedWithdraw,
            stepIndex,
            withdrawalSignature,
            confirmedSlot,
          }),
      );
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
    operations: args.operations,
  }).catch((error) => {
    args.flow.failFrom("wallet_submit_confirm", error);
    throw error;
  });
  args.flow.observe("wallet_submit_confirm", {
    chainState: "confirmed",
    executionMode: args.operations.length > 1 ? "sequential" : "single",
  });

  const withdrawalSignatures: string[] = [];
  const withdrawalConfirmedSlots: string[] = [];
  for (let i = 0; i < sent.length; i++) {
    withdrawalSignatures.push(sent[i].signature);
    withdrawalConfirmedSlots.push(sent[i].confirmedSlot);
    await confirmStep(
      sent[i].signature,
      sent[i].confirmedSlot,
      args.hasSteps ? i : undefined,
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

async function serverPrepareWithdraw(
  args: {
    signer: Signer;
    amountRaw: string;
    mode: EarnWithdrawMode;
    source?: EarnWithdrawSource | null;
    prepareAuth: EarnAuthFields;
  },
  flow: LifecycleFlow<"earn.withdrawal">,
): Promise<WirePreparedEarnWithdraw> {
  const prepare = () =>
    withConnectionRetry("server prepare", WITHDRAW_NETWORK_MESSAGE, () =>
      withEarnAuth(
        args.signer,
        args.prepareAuth,
        "earn-withdraw-prepare",
        (auth) =>
          prepareEarnWithdraw({
            auth,
            amountRaw: args.amountRaw,
            mode: args.mode,
            source: args.source ?? undefined,
          }),
      ),
    ).catch((error) => {
      flow.failFrom("prepare", error, prepareSubstage("server_prepare"));
      throw error;
    });
  let preparedWithdraw = (await prepare()).preparedWithdraw;

  for (let round = 0; preparedWithdraw.autodepositClosePrepared; round++) {
    if (round >= MAX_WITHDRAW_AUTODEPOSIT_CLOSES) {
      throw new Error(
        "Couldn't remove the Autodeposit tied to this position. Delete the Autodeposit and try again.",
      );
    }
    const close = preparedWithdraw.autodepositClosePrepared;
    await executeEarnAutodepositClose({
      signer: args.signer,
      policy: close.policy.account,
      recurringDelegation: close.subscription.recurringDelegation,
      source: "withdraw",
    }).catch((error) => {
      flow.failFrom("autodeposit_close", error, {
        autodepositCloseRequired: true,
      });
      throw error;
    });
    flow.observe("autodeposit_close", { autodepositCloseRequired: true });
    preparedWithdraw = (await prepare()).preparedWithdraw;
  }
  return preparedWithdraw;
}

// Real on-chain Earn withdrawal from the caller's smart account (same position
// as web). The withdraw transactions are built ON-DEVICE with the
// smart-account-vaults SDK (mirroring the deposit and autodeposit flows): the
// backend `prepare-context` call keeps source selection and reconcile, and the
// RPC-heavy instruction build runs on the device's own RPC/IP allowance. A
// full exit may span multiple Kamino reserves (one signed step each).
//
// `mode` "full" tells the backend to use the exact on-chain source amount (the
// passed `amountUsd` is only authoritative for "partial").
export async function executeEarnWithdraw(args: {
  signer: Signer;
  amountUsd: number;
  flowId?: string;
  mode: EarnWithdrawMode;
  // The chosen source. Omitted/null lets the backend auto-select when there's
  // exactly one source; required (from the picker) when the position spans
  // multiple sources.
  source?: EarnWithdrawSource | null;
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
    source?: EarnWithdrawSource | null;
  },
  flow: LifecycleFlow<"earn.withdrawal">,
): Promise<EarnWithdrawResult> {
  const amountRaw = usdToUsdcRaw(args.amountUsd);
  await assertSolForFees(
    getConnection(),
    args.signer.publicKey,
    WITHDRAW_MIN_FEE_LAMPORTS,
  ).catch((error) => {
    flow.failFrom("prepare", error);
    throw error;
  });
  const prepareAuth = await signEarnAuth(
    args.signer,
    "earn-withdraw-prepare",
  ).catch((error) => {
    flow.failFrom("prepare", error, prepareSubstage("wallet_auth"));
    throw error;
  });
  const fetchContext = () =>
    withConnectionRetry("prepare-context", WITHDRAW_NETWORK_MESSAGE, () =>
      withEarnAuth(args.signer, prepareAuth, "earn-withdraw-prepare", (auth) =>
        fetchEarnWithdrawPrepareContext({
          auth,
          amountRaw,
          flowId: flow.flowId,
          mode: args.mode,
          source: args.source ?? null,
        }),
      ),
    ).catch((error) => {
      flow.failFrom("prepare", error, prepareSubstage("prepare_context"));
      throw error;
    });

  let context = await fetchContext();

  for (let round = 0; context.withdrawInput.autodepositClose; round++) {
    if (round >= MAX_WITHDRAW_AUTODEPOSIT_CLOSES) {
      throw new Error(
        "Couldn't remove the Autodeposit tied to this position. Delete the Autodeposit and try again.",
      );
    }
    const close = context.withdrawInput.autodepositClose;
    await executeEarnAutodepositClose({
      signer: args.signer,
      policy: close.policy,
      recurringDelegation: close.recurringDelegation,
      source: "withdraw",
    }).catch((error) => {
      flow.failFrom("autodeposit_close", error, {
        autodepositCloseRequired: true,
      });
      throw error;
    });
    flow.observe("autodeposit_close", { autodepositCloseRequired: true });
    context = await fetchContext();
  }

  const client = createSmartAccountVaultsClient({
    connection: getConnection(),
    programId: new PublicKey(context.programId),
  });
  const needsSeparateCleanup = context.withdrawInput.mode === "full";
  const withdrawInput = hydrateEarnWithdrawInput(
    context,
    args.signer.publicKey,
  );
  let operations: HydratedPreparedOperation[];
  let hasSteps: boolean;
  let wirePreparedWithdraw: WirePreparedEarnWithdraw;
  try {
    const preparedWithdraw = await withConnectionRetry(
      "device prepare",
      WITHDRAW_NETWORK_MESSAGE,
      () =>
        client.prepareEarnUsdcWithdraw({
          ...withdrawInput,
          closePoliciesOnFullWithdrawal: false,
        }),
    );
    hasSteps = preparedWithdraw.withdrawSteps.length > 0;
    operations = hasSteps
      ? preparedWithdraw.withdrawSteps.map((step) => step.prepared)
      : [preparedWithdraw.prepared];
    wirePreparedWithdraw =
      serializePreparedEarnUsdcWithdraw(preparedWithdraw);
    flow.observe("prepare");
  } catch (error) {
    if (!isRetryExhaustion(error)) {
      flow.failFrom("prepare", error, prepareSubstage("device_prepare"));
      throw error;
    }
    console.warn(
      "[earn-withdraw] device prepare exhausted; recovering via server prepare",
      { errorDetail: error.detail },
    );
    const serverPrepared = await serverPrepareWithdraw(
      {
        signer: args.signer,
        amountRaw,
        mode: args.mode,
        source: args.source,
        prepareAuth,
      },
      flow,
    );
    const steps =
      serverPrepared.withdrawSteps && serverPrepared.withdrawSteps.length > 0
        ? serverPrepared.withdrawSteps
        : null;
    hasSteps = steps !== null;
    operations = (steps ?? [{ prepared: serverPrepared.prepared }]).map(
      (step) => hydratePreparedOperation(step.prepared),
    );
    wirePreparedWithdraw = serverPrepared;
    flow.observe("prepare", {
      ...prepareSubstage("server_prepare"),
      ...(error.detail !== undefined ? { errorDetail: error.detail } : {}),
    });
  }
  const withdrawResult = await signSendAndConfirmWithdraw({
    signer: args.signer,
    prepareAuth,
    operations,
    hasSteps,
    wirePreparedWithdraw,
    flow,
  });
  if (!needsSeparateCleanup) {
    flow.complete("ui_commit");
    return {
      withdrawalSignatures: withdrawResult.withdrawalSignatures,
    };
  }

  const minContextSlot = withdrawResult.withdrawalConfirmedSlots.reduce(
    (latest, slot) => (BigInt(slot) > BigInt(latest) ? slot : latest),
  );
  let cleanupSignature: string | undefined;
  try {
    const cleanupContext = await fetchCleanupContextWithRetry({
      flowId: flow.flowId,
      signer: args.signer,
      prepareAuth,
      minContextSlot,
    });
    flow.observe("full_exit_verify");
    const cleanupClient = createSmartAccountVaultsClient({
      connection: getConnection(),
      programId: new PublicKey(cleanupContext.programId),
    });
    const preparedCleanup = await withConnectionRetry(
      "device cleanup prepare",
      WITHDRAW_NETWORK_MESSAGE,
      () =>
        cleanupClient.prepareEarnUsdcCleanup(
          hydrateEarnWithdrawCleanupInput(
            cleanupContext,
            args.signer.publicKey,
          ),
        ),
    );
    flow.observe("cleanup_prepare", { cleanupRequired: true });
    const [cleanupResult] = await signAndSendPreparedOperations({
      connection: getConnection(),
      signer: args.signer,
      operations: [preparedCleanup.prepared],
    });
    flow.observe("cleanup_wallet_submit_confirm", {
      chainState: "confirmed",
      cleanupRequired: true,
    });
    cleanupSignature = cleanupResult?.signature;
    if (cleanupResult) {
      try {
        await retryEarnApiCall({
          attempts: CLEANUP_CONFIRM_ATTEMPTS,
          delayMs: CLEANUP_CONFIRM_RETRY_DELAY_MS,
          retryableCodes: RETRYABLE_CLEANUP_CONFIRM_CODES,
          run: () =>
            withEarnAuth(
              args.signer,
              prepareAuth,
              "earn-withdraw-confirm",
              (auth) =>
                confirmEarnWithdrawCleanup({
                  auth,
                  cleanupSignature: cleanupResult.signature,
                  confirmedSlot: cleanupResult.confirmedSlot,
                }),
            ),
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
