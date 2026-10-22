import type { MobileEarnMaxResponses } from "@loyal-labs/shared";
import {
  createSmartAccountVaultsClient,
  deriveEarnMaxVoltrAuthority,
  EARN_MAX_VOLTR_ACCOUNT_INDEX,
  voltrClaimPlan,
  VoltrClaimLiquidityError,
  voltrDepositPlan,
  voltrRequestWithdrawalPlan,
} from "@loyal-labs/smart-account-vaults";
import { PublicKey } from "@solana/web3.js";

import { getConnection, getSolanaEnv } from "@/lib/solana/rpc/connection";
import type { Signer } from "@/lib/wallet/signer";
import { requestEarnMax } from "@/services/api";
import { startLifecycleFlow } from "@/services/observability";

import { EarnApiError } from "./earn-api";
import { signEarnAuth, withEarnAuth } from "./earn-auth";
import { clearEarnSession, getEarnSessionToken } from "./earn-session";
import { signAndSendPreparedOperation } from "./send-prepared";

/** No wallet prompt while a usable mobile Earn session exists. */
export async function readMobileEarnMax<A extends keyof MobileEarnMaxResponses>(
  signer: Signer,
  action: A
): Promise<MobileEarnMaxResponses[A]> {
  const token = await getEarnSessionToken(signer.publicKey.toBase58());
  if (token) {
    try {
      return await requestEarnMax(action, { sessionToken: token });
    } catch (error) {
      if (
        !(error instanceof EarnApiError) ||
        error.code !== "invalid_mobile_session"
      ) {
        throw error;
      }
      await clearEarnSession();
    }
  }
  try {
    return await requestEarnMax(action, {
      walletAddress: signer.publicKey.toBase58(),
    });
  } catch (error) {
    if (!(error instanceof EarnApiError) || error.status !== 401) throw error;
  }
  const auth = await signEarnAuth(signer, "earn-max");
  return withEarnAuth(signer, auth, "earn-max", (fresh) =>
    requestEarnMax(action, fresh)
  );
}

export type MobileEarnMaxOperation =
  | { action: "deposit"; amountRaw: bigint }
  | { action: "request_withdrawal"; amountRaw: bigint | "max" }
  | { action: "claim" };

const activeWallets = new Set<string>();

/** One atomic wallet-signed transaction; history is subsequently read from chain. */
export async function executeMobileEarnMax(
  signer: Signer,
  operation: MobileEarnMaxOperation
) {
  const walletAddress = signer.publicKey.toBase58();
  if (activeWallets.has(walletAddress)) {
    throw new Error("An Earn MAX transaction is already in progress.");
  }
  if (getSolanaEnv() !== "mainnet") {
    throw new Error("Earn MAX requires mainnet.");
  }
  if (
    "amountRaw" in operation &&
    operation.amountRaw !== "max" &&
    (operation.amountRaw <= BigInt(0) ||
      operation.amountRaw > BigInt("18446744073709551615"))
  ) {
    throw new Error("Amount must be a positive u64 in raw USDC units.");
  }
  if (
    operation.action === "deposit" &&
    operation.amountRaw < BigInt(1_000_000)
  ) {
    throw new Error("Minimum Earn MAX deposit is $1.");
  }
  activeWallets.add(walletAddress);
  const flow = startLifecycleFlow({
    flowName: "earn_max.vault",
    flowVariant: operation.action,
  });
  flow.observe("intent");
  let stage: "prepare" | "wallet_submit_confirm" = "prepare";
  try {
    const context = await readMobileEarnMax(signer, "context");
    if (
      context.cluster !== "mainnet" ||
      context.walletAddress !== walletAddress
    ) {
      throw new Error(
        "Earn MAX context does not match this wallet and cluster."
      );
    }
    if (getSolanaEnv() !== "mainnet")
      throw new Error("Earn MAX requires mainnet.");
    const connection = getConnection();
    // SDK and app installs have distinct private web3 type fields but the same Connection API.
    const sdkConnection = connection as unknown as Parameters<
      typeof createSmartAccountVaultsClient
    >[0]["connection"];
    const settingsPda = new PublicKey(context.settingsPda);
    const client = createSmartAccountVaultsClient({
      connection: sdkConnection,
      programId: new PublicKey(context.programId),
    });
    const settings = await client.sdk.smartAccounts.queries.fetchSettings(
      settingsPda
    );
    if (Number(settings.threshold) !== 1 || Number(settings.timeLock) !== 0) {
      throw new Error(
        "Earn MAX needs a single-signer smart account without a time lock."
      );
    }
    const planContext = {
      authority: deriveEarnMaxVoltrAuthority(settingsPda, context.programId),
      owner: signer.publicKey,
    };
    const plan =
      operation.action === "deposit"
        ? voltrDepositPlan(planContext, operation.amountRaw)
        : operation.action === "request_withdrawal"
        ? await voltrRequestWithdrawalPlan(
            sdkConnection,
            planContext,
            operation.amountRaw
          )
        : await voltrClaimPlan(sdkConnection, planContext);
    const prepared = await client.prepareCustomInstructionSync({
      accountIndex: EARN_MAX_VOLTR_ACCOUNT_INDEX,
      feePayer: signer.publicKey,
      instructions: plan.vault,
      settingsPda,
      signer: signer.publicKey,
    });
    if (!prepared) throw new Error("Earn MAX transaction is too large.");
    if (getSolanaEnv() !== "mainnet")
      throw new Error("Earn MAX requires mainnet.");
    flow.observe("prepare");
    stage = "wallet_submit_confirm";
    const sent = await signAndSendPreparedOperation({
      connection,
      signer,
      operation: {
        ...prepared,
        instructions: [...plan.outer, ...prepared.instructions],
      },
    });
    flow.complete("wallet_submit_confirm", { chainState: "confirmed" });
    return sent;
  } catch (error) {
    if (error instanceof VoltrClaimLiquidityError) {
      flow.fail("prepare", {
        chainState: "not_submitted",
        errorCode: "earn_max_liquidity_unavailable",
      });
    } else {
      flow.failFrom(stage, error);
    }
    throw error;
  } finally {
    activeWallets.delete(walletAddress);
  }
}
