import { normalizeLoyalCluster } from "@loyal-labs/actions";
import {
  createSmartAccountVaultsClient,
  hydratePreparedOperation,
  isEarnPolicyUpdateRequiredError,
  serializePreparedEarnUsdcDeposit,
} from "@loyal-labs/smart-account-vaults";
import { PublicKey } from "@solana/web3.js";

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
  fetchEarnDepositPrepareContext,
  type EarnAuthFields,
  type WirePreparedEarnDeposit,
} from "./earn-api";
import {
  EARN_PRODUCT_DECIMALS,
} from "./earn-product-mints";
import { signEarnAuth, withEarnAuth } from "./earn-auth";
import {
  signAndSendPreparedOperations,
  type HydratedPreparedOperation,
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

type EarnDepositStages = {
  policySetup: HydratedPreparedOperation | null;
  policyFinalize: HydratedPreparedOperation | null;
  deposit: HydratedPreparedOperation;
};

async function signSendAndConfirmDeposit(args: {
  signer: Signer;
  prepareAuth: EarnAuthFields;
  amountUsd: number;
  stages: EarnDepositStages;
  wirePreparedDeposit: WirePreparedEarnDeposit;
  flow: LifecycleFlow<"earn.deposit">;
}): Promise<EarnDepositResult> {
  assertNativeSolRequirement(args.wirePreparedDeposit.nativeSolRequirement);

  const connection = getConnection();
  const operations = [
    args.stages.policySetup,
    args.stages.policyFinalize,
    args.stages.deposit,
  ].filter((operation) => operation != null);
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
    executionMode: operations.length > 1 ? "batch" : "single",
  });
  const deposit = sent[sent.length - 1];

  const CONFIRM_ATTEMPTS = 3;
  let confirmRecorded = false;
  for (let attempt = 1; attempt <= CONFIRM_ATTEMPTS; attempt += 1) {
    try {
      await withEarnAuth(
        args.signer,
        args.prepareAuth,
        "earn-deposit-confirm",
        (auth) =>
          confirmEarnDeposit({
            auth,
            preparedDeposit: args.wirePreparedDeposit,
            depositSignature: deposit.signature,
            confirmedSlot: deposit.confirmedSlot,
          }),
      );
      confirmRecorded = true;
      break;
    } catch (error) {
      if (attempt === CONFIRM_ATTEMPTS) {
        console.warn(
          "[earn-deposit] confirm failed after retries; yield routing will adopt this deposit",
          error,
        );
        break;
      }
      await new Promise((resolve) => setTimeout(resolve, 2000 * attempt));
    }
  }
  args.flow.observe("backend_confirm", {
    chainState: "confirmed",
    ...(confirmRecorded
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

// Policies are immutable permission records, so a legacy (classic-token-only)
// Earn route policy is "updated" by creating a NEW owner-neutral route+setup
// pair at the next seed — two signed transactions, one wallet prompt (the
// Seed Vault batches them). The legacy pair stays on-chain and only strands
// its rent. No dedicated confirm endpoint: `deposit/confirm` adopts a
// chain-discovered policy pair (creation signature resolved from chain) when
// the re-prepared deposit references one the DB doesn't know.
async function runEarnPolicyUpdate(args: {
  client: ReturnType<typeof createSmartAccountVaultsClient>;
  context: { cluster: string; policySigner: string; settingsPda: string };
  signer: Signer;
}): Promise<void> {
  const preparedPolicy = await withConnectionRetry(
    "deposit policy update prepare",
    DEPOSIT_NETWORK_MESSAGE,
    () =>
      args.client.prepareEarnUsdcYieldRoutingPolicy({
        cluster: normalizeLoyalCluster(args.context.cluster),
        feePayer: args.signer.publicKey,
        settingsPda: new PublicKey(args.context.settingsPda),
        signer: new PublicKey(args.context.policySigner),
        walletAddress: args.signer.publicKey,
      }),
  );
  await signAndSendPreparedOperations({
    connection: getConnection(),
    signer: args.signer,
    operations: [preparedPolicy.prepared, preparedPolicy.finalizePrepared].filter(
      (operation) => operation != null,
    ),
  });
}

function hydrateWireStages(
  preparedDeposit: WirePreparedEarnDeposit,
): EarnDepositStages {
  return {
    policySetup: preparedDeposit.policySetupPrepared
      ? hydratePreparedOperation(preparedDeposit.policySetupPrepared)
      : null,
    policyFinalize: preparedDeposit.policyFinalizePrepared
      ? hydratePreparedOperation(preparedDeposit.policyFinalizePrepared)
      : null,
    deposit: hydratePreparedOperation(preparedDeposit.prepared),
  };
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
  const walletAddress = args.signer.publicKey;
  const prepareAuth = await signEarnAuth(args.signer, "earn-deposit-prepare");

  const context = await withConnectionRetry(
    "deposit prepare-context",
    DEPOSIT_NETWORK_MESSAGE,
    () =>
      fetchEarnDepositPrepareContext({
        auth: prepareAuth,
        amountRaw,
        mint: args.mint,
        flowId: flow.flowId,
      }),
  );
  if (context.yieldRoutingPolicy) {
    flow.setVariant("top_up");
  }
  const client = createSmartAccountVaultsClient({
    connection: getConnection(),
    programId: new PublicKey(context.programId),
  });
  // After a policy update the DB-known route (the legacy pair) is stale, so
  // the retry prepares WITHOUT the known route: the SDK's policy scan sorts
  // compatible pairs first and picks up the freshly created one from chain.
  const prepareOnDevice = (options: { scanPolicies: boolean }) =>
    withConnectionRetry("deposit device prepare", DEPOSIT_NETWORK_MESSAGE, () =>
      client.prepareEarnUsdcDeposit({
        amountRaw: BigInt(amountRaw),
        cluster: normalizeLoyalCluster(context.cluster),
        feePayer: walletAddress,
        initializeYieldRoutingPolicy:
          options.scanPolicies || !context.yieldRoutingPolicy,
        policySigner: new PublicKey(context.policySigner),
        revokeStrayUsdcDelegate: context.revokeStrayUsdcDelegate,
        settingsPda: new PublicKey(context.settingsPda),
        walletAddress,
        ...(context.target
          ? {
              target: {
                liquidityMint: new PublicKey(context.target.liquidityMint),
                liquidityTokenProgram: new PublicKey(
                  context.target.liquidityTokenProgram,
                ),
                market: new PublicKey(context.target.market),
                reserve: new PublicKey(context.target.reserve),
                supplyApyBps: context.target.supplyApyBps
                  ? BigInt(context.target.supplyApyBps)
                  : null,
              },
            }
          : {}),
        ...(!options.scanPolicies && context.yieldRoutingPolicy
          ? {
              yieldRoutingPolicy: {
                account: new PublicKey(context.yieldRoutingPolicy.account),
                seed: BigInt(context.yieldRoutingPolicy.seed),
                ...(context.yieldRoutingPolicy.setupPolicy
                  ? {
                      setupPolicy: {
                        account: new PublicKey(
                          context.yieldRoutingPolicy.setupPolicy.account,
                        ),
                        seed: BigInt(
                          context.yieldRoutingPolicy.setupPolicy.seed,
                        ),
                      },
                    }
                  : {}),
              },
            }
          : {}),
      }),
    );
  let preparedDeposit;
  try {
    preparedDeposit = await prepareOnDevice({ scanPolicies: false });
  } catch (error) {
    if (!isEarnPolicyUpdateRequiredError(error)) {
      throw error;
    }
    console.log("[earn-deposit] route policy predates Token-2022; updating");
    await runEarnPolicyUpdate({ client, context, signer: args.signer });
    preparedDeposit = await prepareOnDevice({ scanPolicies: true });
  }
  flow.observe("prepare", {
    policyMode: context.yieldRoutingPolicy ? "reuse" : "create",
  });
  return signSendAndConfirmDeposit({
    signer: args.signer,
    prepareAuth,
    amountUsd: args.amountUsd,
    stages: {
      policySetup: preparedDeposit.policySetupPrepared ?? null,
      policyFinalize: preparedDeposit.policyFinalizePrepared ?? null,
      deposit: preparedDeposit.prepared,
    },
    wirePreparedDeposit: serializePreparedEarnUsdcDeposit(preparedDeposit),
    flow,
  });
}
