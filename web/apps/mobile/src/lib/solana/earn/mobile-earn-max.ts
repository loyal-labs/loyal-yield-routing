import {
  createSmartAccountVaultsClient,
  prepareEarnMaxVoltr,
  voltrClaimPlan,
  VoltrClaimLiquidityError,
  voltrDepositPlan,
  type VoltrPlanBuilder,
  voltrRequestWithdrawalPlan,
} from "@loyal-labs/smart-account-vaults";
import { type Connection, PublicKey } from "@solana/web3.js";

import { getConnection, getSolanaEnv } from "@/lib/solana/rpc/connection";
import type { Signer } from "@/lib/wallet/signer";
import { fetchEarnMax, provisionEarnMaxAccount } from "@/services/api";
import { startLifecycleFlow } from "@/services/observability";

import { deliberateSession } from "./earn-session";
import { signAndSendPreparedOperation } from "./send-prepared";

async function earnMaxAccount(signer: Signer) {
  const walletAddress = signer.publicKey.toBase58();
  const token = await deliberateSession(signer);
  if (!token) throw new Error("Sign in to use Earn MAX.");
  const summary = await fetchEarnMax(
    walletAddress,
    "/api/smart-accounts/earn-max/summary"
  );
  const account = summary?.config ?? (await provisionEarnMaxAccount(walletAddress));
  if (account.walletAddress !== walletAddress) {
    throw new Error("Earn MAX account does not match this wallet.");
  }
  return account;
}

export type MobileEarnMaxOperation =
  | { action: "deposit"; amountRaw: bigint }
  | { action: "request_withdrawal"; amountRaw: bigint | "max" }
  | { action: "claim" };

function planFor(
  connection: Connection,
  operation: MobileEarnMaxOperation
): VoltrPlanBuilder {
  switch (operation.action) {
    case "deposit":
      return async (context) => voltrDepositPlan(context, operation.amountRaw);
    case "request_withdrawal":
      return (context) =>
        voltrRequestWithdrawalPlan(connection, context, operation.amountRaw);
    case "claim":
      return (context) => voltrClaimPlan(connection, context);
  }
}

const activeWallets = new Set<string>();

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
    const account = await earnMaxAccount(signer);
    if (getSolanaEnv() !== "mainnet")
      throw new Error("Earn MAX requires mainnet.");
    const connection = getConnection();
    const programId = new PublicKey(account.programId);
    const prepared = await prepareEarnMaxVoltr({
      build: planFor(connection, operation),
      client: createSmartAccountVaultsClient({ connection, programId }),
      owner: signer.publicKey,
      programId,
      settingsPda: new PublicKey(account.settingsPda),
    });
    if (getSolanaEnv() !== "mainnet")
      throw new Error("Earn MAX requires mainnet.");
    flow.observe("prepare");
    stage = "wallet_submit_confirm";
    const sent = await signAndSendPreparedOperation({
      connection,
      signer,
      operation: prepared,
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
