import {
  resolveLoyalClusterForSolanaEnv,
  Stablecoin,
} from "@loyal-labs/actions";
import { pda } from "@loyal-labs/loyal-smart-accounts";
import type { SolanaEnv } from "@loyal-labs/solana-rpc";
import { PublicKey } from "@solana/web3.js";
import { NextResponse } from "next/server";

import { authenticateMobileWalletRequest } from "@/features/identity/server/mobile-wallet-auth";
import { WalletAuthError } from "@/features/identity/server/wallet-auth-errors";
import { getServerEnv } from "@/lib/core/config/server";
import { resolveLoyalWebSolanaEnvFromEnv } from "@/lib/core/config/solana-env-override";
import { getDeploymentPolicySignerPublicKey } from "@/lib/yield-optimization/deployment-policy-signer.server";
import { ensureMobileEarnDepositSmartAccount } from "@/lib/yield-optimization/mobile-earn-deposit-account-gate.server";
import { hasLiveBalanceSweepTargetForWallet } from "@/lib/yield-optimization/earn-autodeposit-repository.server";
import { parseEarnDepositPrepareRequestBody } from "@/lib/yield-optimization/earn-deposit-prepare-contracts.shared";
import {
  type EarnProductAsset,
  resolveEarnProductAsset,
} from "@/lib/yield-optimization/earn-product-mints.shared";
import { resolveEarnDepositTarget } from "@/lib/yield-optimization/earn-reserve-target.server";
import {
  findActiveYieldRoutePolicyPair,
  findReconciledActiveYieldPositionForVault,
  type RoutePolicyRecord,
} from "@/lib/yield-optimization/yield-deposit-repository.server";
import { EARN_VAULT_INDEX } from "@/features/smart-accounts/derivation";

// Context twin of `../prepare` for ON-DEVICE deposit prepare: same auth,
// provisioning gate, and DB reads, but instead of building the deposit here
// (a ~16-RPC-call SDK prepare that contends for this server's shared,
// per-IP-rate-limited Solana RPC pipe) it returns the inputs so the device
// runs `prepareEarnUsdcDeposit` on its own RPC/IP allowance — mirroring the
// on-device autodeposit setup/close flows. `../prepare` stays for app
// versions that predate on-device prepare.

function jsonError(
  status: number,
  code: string,
  message: string
): NextResponse {
  return NextResponse.json({ error: { code, message } }, { status });
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
      purpose: "earn-deposit-prepare",
    }));
  } catch (error) {
    if (error instanceof WalletAuthError) {
      return jsonError(error.status, error.code, error.message);
    }
    return jsonError(401, "unauthenticated", "Mobile wallet auth failed.");
  }

  let amountRaw: bigint;
  let mint: string | null;
  try {
    ({ amountRaw, mint } = parseEarnDepositPrepareRequestBody(body));
  } catch (error) {
    return jsonError(
      400,
      "invalid_request",
      error instanceof Error ? error.message : "Invalid request body."
    );
  }

  const solanaEnv = getConfiguredSolanaEnv();
  const cluster = resolveLoyalClusterForSolanaEnv(solanaEnv);
  let productMint: EarnProductAsset;
  try {
    productMint = resolveEarnProductAsset({
      cluster,
      mint,
    });
  } catch (error) {
    return jsonError(
      400,
      "unsupported_mint",
      error instanceof Error ? error.message : "Unsupported Earn mint."
    );
  }
  // Legacy bodies omit the mint (ASK-2099); from here on use the resolved
  // product mint, which defaults those to USDC.
  mint = productMint.mint.toBase58();

  const gate = await ensureMobileEarnDepositSmartAccount({
    amountRaw,
    logTag: "mobile-earn-deposit-prepare-context",
    productMint,
    request,
    solanaEnv,
    walletAddress,
  });
  if (!gate.ok) {
    return gate.response;
  }
  const { settingsPda, smartAccountAddress } = gate;

  let policy: RoutePolicyRecord | null = null;

  try {
    const serverEnv = getServerEnv();
    const programId = new PublicKey(serverEnv.loyalSmartAccounts.programId);
    const settings = new PublicKey(settingsPda);
    const [earnVaultPda] = pda.getSmartAccountPda({
      accountIndex: EARN_VAULT_INDEX,
      programId,
      settingsPda: settings,
    });
    const [policyResult, activePosition] = await Promise.all([
      findActiveYieldRoutePolicyPair({
        authority: walletAddress,
        cluster,
        settings: settingsPda,
        vaultIndex: EARN_VAULT_INDEX,
        vaultPubkey: earnVaultPda.toBase58(),
      }),
      findReconciledActiveYieldPositionForVault({
        cluster,
        liquidityMint: mint,
        settings: settingsPda,
        vaultIndex: EARN_VAULT_INDEX,
        walletAddress,
      }),
    ]);
    policy = policyResult?.routePolicy ?? null;
    const policySigner = getDeploymentPolicySignerPublicKey();
    const target = await resolveEarnDepositTarget({
      activePosition,
      cluster,
      hasActiveRoutePolicy: policy !== null,
      liquidityMint: mint,
      logTag: "mobile-earn-deposit-prepare-context",
      productMint,
    });
    if (!target) {
      return jsonError(
        409,
        "no_eligible_reserve",
        "No eligible Safe Kamino reserve is available for this Earn mint."
      );
    }
    // Stray-approval heal, fail-closed: the SPL delegate is load-bearing for
    // sweeps, so the revoke rider is requested only when the wallet provably
    // has NO live autodeposit target (any settings, duplicates included). The
    // SDK re-checks on chain that the delegate is our subscription authority.
    let revokeStrayUsdcDelegate = false;
    try {
      revokeStrayUsdcDelegate =
        productMint.stablecoin === Stablecoin.USDC &&
        !(await hasLiveBalanceSweepTargetForWallet(walletAddress));
    } catch {
      revokeStrayUsdcDelegate = false;
    }
    return NextResponse.json({
      cluster,
      programId: serverEnv.loyalSmartAccounts.programId,
      settingsPda,
      smartAccountAddress,
      policySigner: policySigner.toBase58(),
      revokeStrayUsdcDelegate,
      yieldRoutingPolicy: policy
        ? {
            account: policy.policyAccount,
            seed: policy.policySeed.toString(),
            setupPolicy: policyResult?.setupPolicy
              ? {
                  account: policyResult.setupPolicy.policyAccount,
                  seed: policyResult.setupPolicy.policySeed.toString(),
                }
              : null,
          }
        : null,
      target: {
        reserve: target.reserve.toBase58(),
        market: target.market.toBase58(),
        liquidityMint: target.liquidityMint.toBase58(),
        liquidityTokenProgram: target.liquidityTokenProgram.toBase58(),
        supplyApyBps: target.supplyApyBps?.toString() ?? null,
      },
    });
  } catch (error) {
    console.error("[mobile-earn-deposit-prepare-context] context failed", {
      amountRaw: amountRaw.toString(),
      cluster,
      mint,
      errorMessage:
        error instanceof Error ? error.message : "Unknown context error.",
      errorName: error instanceof Error ? error.name : typeof error,
      policyAccount: policy?.policyAccount ?? null,
      settings: settingsPda,
      solanaEnv,
      stack: error instanceof Error ? error.stack : undefined,
      walletAddress,
    });
    return jsonError(
      500,
      "context_failed",
      error instanceof Error
        ? error.message
        : "Failed to resolve Earn deposit context."
    );
  }
}
