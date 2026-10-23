import type {
  DepositWirePreparedOperation,
  EarnDepositConfirmRequest,
  EarnPolicyConfirmRequest,
} from "@loyal-labs/api";
import {
  hydratePreparedEarnUsdcDeposit,
  hydratePreparedOperation,
  type SmartAccountPreparedEarnUsdcDeposit,
  type WirePreparedLoyalSmartAccountsOperation,
  type WireSmartAccountPreparedEarnUsdcDeposit,
} from "@loyal-labs/smart-account-vaults";

import { track } from "@/lib/analytics/analytics";
import { EARN_EVENTS } from "@/lib/analytics/earn-events";
import { getConnection } from "@/lib/solana/rpc/connection";
import { assertNativeSolRequirement } from "@/lib/wallet/insufficient-sol-error";
import type { Signer } from "@/lib/wallet/signer";
import {
  type LifecycleFlow,
  startLifecycleFlow,
} from "@/services/observability";

import { withConnectionRetry } from "./connection-retry";
import {
  confirmEarnDeposit,
  confirmEarnPolicy,
  EarnApiError,
  prepareEarnDeposit,
  prepareEarnPolicy,
} from "./earn-api";
import { EARN_PRODUCT_DECIMALS } from "./earn-product-mints";
import { ensureEarnSession } from "./earn-session";
import {
  signAndSendPreparedOperations,
  type HydratedPreparedOperation,
  type SentTransaction,
} from "./send-prepared";

const DEPOSIT_NETWORK_MESSAGE =
  "We couldn't reach the network to prepare the deposit. No funds moved — check your connection and try again.";

function usdToStableRaw(amountUsd: number): string {
  if (!Number.isFinite(amountUsd) || amountUsd <= 0) {
    throw new Error("Deposit amount must be greater than 0.");
  }
  return BigInt(Math.round(amountUsd * 10 ** EARN_PRODUCT_DECIMALS)).toString();
}

export type EarnDepositResult = {
  depositSignature: string;
};

type DepositStageName = "policy" | "policy-finalize" | "deposit";

const NO_ELIGIBLE_RESERVE_ATTEMPTS = 4;
const NO_ELIGIBLE_RESERVE_DELAY_MS = 2_000;
const CONFIRM_ATTEMPTS = 3;
const POLICY_CONFIRMED_BUT_NOT_RECORDED_MESSAGE =
  "Earn setup is confirmed, but Earn is still updating. Refresh Earn, then continue this deposit. No USDC was deposited yet.";

function hydrateOperation(
  wire: DepositWirePreparedOperation,
): HydratedPreparedOperation {
  return hydratePreparedOperation(
    wire as unknown as WirePreparedLoyalSmartAccountsOperation,
  );
}

async function confirmWithRetry(
  label: string,
  run: () => Promise<void>,
): Promise<boolean> {
  for (let attempt = 1; attempt <= CONFIRM_ATTEMPTS; attempt += 1) {
    try {
      await run();
      return true;
    } catch (error) {
      if (attempt === CONFIRM_ATTEMPTS) {
        console.warn(`[earn-deposit] ${label} confirm failed after retries`, error);
        return false;
      }
      await new Promise((resolve) => setTimeout(resolve, 2000 * attempt));
    }
  }
  return false;
}

function policyStageConfirmBody(
  persistence: SmartAccountPreparedEarnUsdcDeposit["persistence"],
  stage: "policy" | "policy-finalize",
  sent: SentTransaction,
): EarnPolicyConfirmRequest {
  return {
    ...persistence,
    confirmedSlot: sent.confirmedSlot,
    policySignature: sent.signature,
    setupPolicyConfirmedSlot:
      stage === "policy-finalize" ? sent.confirmedSlot : undefined,
    setupPolicySignature:
      stage === "policy-finalize" ? sent.signature : undefined,
    stage: stage === "policy" ? "route_policy" : "setup_policy",
  };
}

async function runEarnPolicyUpdate(signer: Signer): Promise<void> {
  const walletAddress = signer.publicKey.toBase58();
  const { preparedPolicy } = await withConnectionRetry(
    "deposit policy update prepare",
    DEPOSIT_NETWORK_MESSAGE,
    () => prepareEarnPolicy(walletAddress),
  );
  if (!preparedPolicy.finalizePrepared) {
    throw new Error("Prepared Earn setup policy is missing. Try again.");
  }
  const [route, setup] = await signAndSendPreparedOperations({
    connection: getConnection(),
    signer,
    operations: [
      hydrateOperation(preparedPolicy.prepared),
      hydrateOperation(preparedPolicy.finalizePrepared),
    ],
  });
  await confirmEarnPolicy({
    walletAddress,
    body: {
      ...preparedPolicy.persistence,
      confirmedSlot: route.confirmedSlot,
      policySignature: route.signature,
      stage: "route_policy",
    },
  });
  await confirmEarnPolicy({
    walletAddress,
    body: {
      ...preparedPolicy.persistence,
      confirmedSlot: setup.confirmedSlot,
      policySignature: setup.signature,
      setupPolicyConfirmedSlot: setup.confirmedSlot,
      setupPolicySignature: setup.signature,
      stage: "setup_policy",
    },
  });
}

async function prepareDeposit(args: {
  signer: Signer;
  amountRaw: string;
  mint: string;
  flowId: string;
}): Promise<SmartAccountPreparedEarnUsdcDeposit> {
  const walletAddress = args.signer.publicKey.toBase58();
  for (let attempt = 1; ; attempt += 1) {
    try {
      const response = await withConnectionRetry(
        "deposit prepare",
        DEPOSIT_NETWORK_MESSAGE,
        () =>
          prepareEarnDeposit({
            walletAddress,
            amountRaw: args.amountRaw,
            mint: args.mint,
            flowId: args.flowId,
          }),
      );
      return hydratePreparedEarnUsdcDeposit(
        response.preparedDeposit as unknown as WireSmartAccountPreparedEarnUsdcDeposit,
      );
    } catch (error) {
      if (
        !(error instanceof EarnApiError) ||
        error.code !== "no_eligible_reserve" ||
        attempt >= NO_ELIGIBLE_RESERVE_ATTEMPTS
      ) {
        throw error;
      }
      await new Promise((resolve) =>
        setTimeout(resolve, NO_ELIGIBLE_RESERVE_DELAY_MS),
      );
    }
  }
}

async function signSendAndConfirmDeposit(args: {
  signer: Signer;
  amountUsd: number;
  prepared: SmartAccountPreparedEarnUsdcDeposit;
  flow: LifecycleFlow<"earn.deposit">;
}): Promise<EarnDepositResult> {
  const { prepared } = args;
  assertNativeSolRequirement(prepared.nativeSolRequirement);

  const walletAddress = args.signer.publicKey.toBase58();
  const stages: { name: DepositStageName; operation: HydratedPreparedOperation }[] =
    [];
  if (prepared.policySetupPrepared) {
    stages.push({ name: "policy", operation: prepared.policySetupPrepared });
  }
  if (prepared.policyFinalizePrepared) {
    stages.push({
      name: "policy-finalize",
      operation: prepared.policyFinalizePrepared,
    });
  }
  stages.push({ name: "deposit", operation: prepared.prepared });

  const sentBy = new Map<DepositStageName, SentTransaction>();
  for (const stage of stages) {
    const [landed] = await signAndSendPreparedOperations({
      connection: getConnection(),
      signer: args.signer,
      operations: [stage.operation],
    }).catch((error) => {
      args.flow.failFrom("wallet_submit_confirm", error);
      throw error;
    });
    sentBy.set(stage.name, landed);
    if (stage.name === "deposit") {
      continue;
    }
    const stageName = stage.name;
    const recorded = await confirmWithRetry(`${stageName} stage`, () =>
      confirmEarnPolicy({
        walletAddress,
        body: policyStageConfirmBody(prepared.persistence, stageName, landed),
      }),
    );
    if (!recorded) {
      const error = new Error(POLICY_CONFIRMED_BUT_NOT_RECORDED_MESSAGE);
      args.flow.failFrom("backend_confirm", error);
      throw error;
    }
  }
  args.flow.observe("wallet_submit_confirm", {
    chainState: "confirmed",
    executionMode: stages.length > 1 ? "sequential" : "single",
  });

  const deposit = sentBy.get("deposit");
  if (!deposit) {
    throw new Error("Deposit transaction did not land.");
  }

  const policy = sentBy.get("policy");
  const setupPolicy = sentBy.get("policy-finalize");
  const body: EarnDepositConfirmRequest = {
    ...prepared.persistence,
    confirmedSlot: deposit.confirmedSlot,
    depositSignature: deposit.signature,
    policyConfirmedSlot: policy?.confirmedSlot,
    policySignature: policy?.signature ?? deposit.signature,
    setupPolicyConfirmedSlot: setupPolicy?.confirmedSlot,
    setupPolicySignature: setupPolicy?.signature,
    smartAccountAddress: prepared.vault.pubkey.toBase58(),
    targetSupplyApyBps: prepared.persistence.targetSupplyApyBps ?? undefined,
  };
  const depositRecorded = await confirmWithRetry("deposit", () =>
    confirmEarnDeposit({ walletAddress, body, flowId: args.flow.flowId }),
  );
  if (!depositRecorded) {
    console.warn("[earn-deposit] yield routing will adopt this deposit");
  }
  args.flow.observe("backend_confirm", {
    chainState: "confirmed",
    ...(depositRecorded
      ? { persistenceState: "recorded" as const }
      : {
          errorCode: "backend_confirmation_failed" as const,
          persistenceState: "failed" as const,
          recoveryRequired: true,
        }),
  });

  track(EARN_EVENTS.earnDeposit, { amount_usd: args.amountUsd });

  args.flow.complete("ui_commit");
  return { depositSignature: deposit.signature };
}

export async function executeEarnDeposit(args: {
  signer: Signer;
  amountUsd: number;
  mint: string;
  flowId?: string;
}): Promise<EarnDepositResult> {
  const flow = startLifecycleFlow({
    ...(args.flowId ? { flowId: args.flowId } : {}),
    flowName: "earn.deposit",
    flowVariant: "initial",
    reportUnexpectedErrors: true,
    walletAddress: args.signer.publicKey.toBase58(),
  });
  flow.start("prepare");
  try {
    return await runEarnDeposit(args, flow);
  } catch (error) {
    flow.failFrom("prepare", error);
    throw error;
  }
}

async function runEarnDeposit(
  args: {
    signer: Signer;
    amountUsd: number;
    mint: string;
  },
  flow: LifecycleFlow<"earn.deposit">,
): Promise<EarnDepositResult> {
  const amountRaw = usdToStableRaw(args.amountUsd);
  await ensureEarnSession(args.signer);

  const prepare = () =>
    prepareDeposit({
      signer: args.signer,
      amountRaw,
      mint: args.mint,
      flowId: flow.flowId,
    });
  let prepared: SmartAccountPreparedEarnUsdcDeposit;
  try {
    prepared = await prepare();
  } catch (error) {
    if (
      !(error instanceof EarnApiError) ||
      error.code !== "earn_policy_update_required"
    ) {
      throw error;
    }
    console.log("[earn-deposit] route policy predates Token-2022; updating");
    await runEarnPolicyUpdate(args.signer);
    prepared = await prepare();
  }
  if (!prepared.policySetupPrepared) {
    flow.setVariant("top_up");
  }
  flow.observe("prepare", {
    policyMode: prepared.policySetupPrepared ? "create" : "reuse",
  });
  return signSendAndConfirmDeposit({
    signer: args.signer,
    amountUsd: args.amountUsd,
    prepared,
    flow,
  });
}
