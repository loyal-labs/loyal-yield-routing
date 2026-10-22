import { NextResponse } from "next/server";
import { pda } from "@loyal-labs/loyal-smart-accounts";
import type { SolanaEnv } from "@loyal-labs/solana-rpc";
import { PublicKey } from "@solana/web3.js";

import { getOrCreateCurrentUser } from "@/features/chat/server/app-user";
import { authenticateMobileWalletRequest } from "@/features/identity/server/mobile-wallet-auth";
import { WalletAuthError } from "@/features/identity/server/wallet-auth-errors";
import { findReadyCurrentUserSmartAccount } from "@/features/smart-accounts/server/service";
import { reportEarnDepositQuestCompletion } from "@/features/solana-week/server/quest-completion-service";
import { resolveLoyalWebSolanaEnvFromEnv } from "@/lib/core/config/solana-env-override";
import {
  buildEarnDepositConfirmRequestBody,
  parseEarnDepositConfirmRequestBody,
} from "@/lib/yield-optimization/earn-confirm-contracts.shared";
import {
  EarnDepositConfirmError,
  verifyConfirmedEarnDeposit,
} from "@/lib/yield-optimization/earn-deposit-confirm.server";
import { citeEarnDepositPolicies } from "@/lib/yield-optimization/earn-deposit-policy-citation.server";
import {
  hydratePreparedEarnUsdcDeposit,
  type WireSmartAccountPreparedEarnUsdcDeposit,
} from "@/lib/yield-optimization/earn-deposit-prepare-contracts.shared";

const EARN_DEPOSIT_VAULT_INDEX = 1;

type MobileConfirmFields = {
  preparedDeposit: WireSmartAccountPreparedEarnUsdcDeposit;
  depositSignature: string;
  confirmedSlot: string;
};

function jsonError(
  status: number,
  code: string,
  message: string
): NextResponse {
  return NextResponse.json({ error: { code, message } }, { status });
}

function parseMobileConfirmFields(body: unknown): MobileConfirmFields {
  if (typeof body !== "object" || body === null) {
    throw new Error("Request body must be an object.");
  }
  const record = body as Record<string, unknown>;
  if (
    typeof record.preparedDeposit !== "object" ||
    record.preparedDeposit === null
  ) {
    throw new Error("preparedDeposit is required.");
  }
  if (typeof record.depositSignature !== "string" || !record.depositSignature) {
    throw new Error("depositSignature is required.");
  }
  if (typeof record.confirmedSlot !== "string" || !record.confirmedSlot) {
    throw new Error("confirmedSlot is required.");
  }
  return {
    preparedDeposit:
      record.preparedDeposit as WireSmartAccountPreparedEarnUsdcDeposit,
    depositSignature: record.depositSignature,
    confirmedSlot: record.confirmedSlot,
  };
}

function getConfiguredSolanaEnv(): SolanaEnv {
  return resolveLoyalWebSolanaEnvFromEnv(process.env);
}

export async function POST(request: Request) {
  let body: unknown;
  try {
    body = await request.json();
  } catch {
    return jsonError(400, "invalid_request", "Invalid request body.");
  }

  let walletAddress: string;
  try {
    ({ walletAddress } = await authenticateMobileWalletRequest({
      body,
      // Accepts the flow's prepare signature too — the device signs one auth
      // message per deposit flow (see authenticateMobileWalletRequest).
      purpose: ["earn-deposit-confirm", "earn-deposit-prepare"],
    }));
  } catch (error) {
    if (error instanceof WalletAuthError) {
      return jsonError(error.status, error.code, error.message);
    }
    return jsonError(401, "unauthenticated", "Mobile wallet auth failed.");
  }

  let fields: MobileConfirmFields;
  try {
    fields = parseMobileConfirmFields(body);
  } catch (error) {
    return jsonError(
      400,
      "invalid_request",
      error instanceof Error ? error.message : "Invalid request body."
    );
  }

  let smartAccountAddress: string;
  let settingsPda: string;
  try {
    const user = await getOrCreateCurrentUser({
      provider: "solana",
      authMethod: "wallet",
      subjectAddress: walletAddress,
      walletAddress,
    });
    const existing = await findReadyCurrentUserSmartAccount({
      userId: user.id,
      walletAddress,
    });
    if (!existing) {
      return jsonError(
        409,
        "smart_account_not_ready",
        "No provisioned smart account for this wallet."
      );
    }
    settingsPda = existing.settingsPda;
    // Earn confirm keys on the vault (smart-account index 1), not the wallet's
    // main account (index 0) that findReadyCurrentUserSmartAccount returns. The
    // web client derives the vault from the prepared op; mirror that here so the
    // canonical `smartAccountAddress` check passes and the vault-keyed position
    // is matched. (Same derivation the deposit-confirm canonical uses.)
    smartAccountAddress = pda
      .getSmartAccountPda({
        settingsPda: new PublicKey(settingsPda),
        accountIndex: EARN_DEPOSIT_VAULT_INDEX,
      })[0]
      .toBase58();
  } catch (error) {
    console.error("[mobile-earn-deposit-confirm] resolve failed", {
      errorMessage:
        error instanceof Error ? error.message : "Unknown resolve error.",
      errorName: error instanceof Error ? error.name : typeof error,
      stack: error instanceof Error ? error.stack : undefined,
      walletAddress,
    });
    return jsonError(
      502,
      "resolve_failed",
      "Failed to resolve the smart account for this wallet."
    );
  }

  try {
    const solanaEnv = getConfiguredSolanaEnv();
    const preparedDeposit = hydratePreparedEarnUsdcDeposit(
      fields.preparedDeposit
    );

    const confirmBody = buildEarnDepositConfirmRequestBody({
      preparedDeposit,
      signature: fields.depositSignature,
      confirmedSlot: fields.confirmedSlot,
      smartAccountAddress,
    });
    const principal = { walletAddress, smartAccountAddress, settingsPda };
    const input = await citeEarnDepositPolicies({
      input: parseEarnDepositConfirmRequestBody(confirmBody),
      principal,
    });

    const position = await verifyConfirmedEarnDeposit({ principal, input });

    await reportEarnDepositQuestCompletion(
      walletAddress,
      input.principalAmountRaw,
      {
        source: "mobile-earn-deposit-confirm",
        solanaEnv,
        depositUsdcRaw: input.principalAmountRaw.toString(),
      }
    );

    return NextResponse.json({ position });
  } catch (error) {
    if (error instanceof EarnDepositConfirmError) {
      return jsonError(error.status, error.code, error.message);
    }
    console.error("[mobile-earn-deposit-confirm] build/record failed", {
      depositSignature: fields.depositSignature,
      errorMessage:
        error instanceof Error ? error.message : "Unknown confirm error.",
      errorName: error instanceof Error ? error.name : typeof error,
      stack: error instanceof Error ? error.stack : undefined,
      walletAddress,
    });
    return jsonError(
      400,
      "confirm_failed",
      error instanceof Error ? error.message : "Failed to confirm Earn deposit."
    );
  }
}
