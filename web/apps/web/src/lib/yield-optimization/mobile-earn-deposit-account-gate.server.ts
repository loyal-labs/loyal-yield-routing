import { Connection, PublicKey } from "@solana/web3.js";
import { NextResponse } from "next/server";

import type { SolanaEnv } from "@loyal-labs/solana-rpc";

import { getOrCreateCurrentUser } from "@/features/chat/server/app-user";
import {
  ensureWalletUserSmartAccountTraced,
  findReadyCurrentUserSmartAccount,
  isSmartAccountProvisioningError,
} from "@/features/smart-accounts/server/service";
import { getServerSolanaEndpoints } from "@/lib/solana/rpc-endpoints.server";
import { getFrontendSolanaRpcFetch } from "@/lib/solana/rpc-rate-limit";
import { type EarnProductAsset } from "@/lib/yield-optimization/earn-product-mints.shared";

// Resolve (provisioning if needed) the canonical smart account for a mobile
// Earn deposit caller — the shared gate of `deposit/prepare` and
// `deposit/prepare-context`. The account is the same one the web flow uses, so
// Earn is one position everywhere. `ensureWalletUserSmartAccount` is
// idempotent: an account created on web is reused at no cost. The *first-ever*
// provisioning is sponsored (Loyal pays rent), so gate it behind real funds —
// the wallet must already hold the selected mint it is depositing. This makes
// free-account spam economically infeasible (each new account needs a distinct
// funded wallet) without depending on the captcha or external rate-limit
// infra.
export async function ensureMobileEarnDepositSmartAccount(args: {
  amountRaw: bigint;
  logTag: string;
  productMint: EarnProductAsset;
  request: Request;
  solanaEnv: SolanaEnv;
  walletAddress: string;
}): Promise<
  | { ok: true; settingsPda: string; smartAccountAddress: string }
  | { ok: false; response: NextResponse }
> {
  try {
    const user = await getOrCreateCurrentUser({
      provider: "solana",
      authMethod: "wallet",
      subjectAddress: args.walletAddress,
      walletAddress: args.walletAddress,
    });
    const existing = await findReadyCurrentUserSmartAccount({
      userId: user.id,
      walletAddress: args.walletAddress,
    });
    if (existing) {
      return {
        ok: true,
        settingsPda: existing.settingsPda,
        smartAccountAddress: existing.smartAccountAddress,
      };
    }
    const mintBalanceRaw = await getWalletMintBalanceRaw(
      getConnection(args.solanaEnv),
      new PublicKey(args.walletAddress),
      args.productMint.mint
    );
    if (mintBalanceRaw < args.amountRaw) {
      return {
        ok: false,
        response: NextResponse.json(
          {
            error: {
              code: "insufficient_balance",
              message:
                "Wallet must hold the selected mint before its Earn account can be created.",
            },
          },
          { status: 402 }
        ),
      };
    }
    const ensured = await ensureWalletUserSmartAccountTraced({
      userId: user.id,
      walletAddress: args.walletAddress,
      request: args.request,
    });
    return {
      ok: true,
      settingsPda: ensured.smartAccount.settingsPda,
      smartAccountAddress: ensured.smartAccount.smartAccountAddress,
    };
  } catch (error) {
    if (isSmartAccountProvisioningError(error)) {
      return {
        ok: false,
        response: NextResponse.json(
          { error: { code: error.code, message: error.message } },
          { status: error.status }
        ),
      };
    }
    console.error(`[${args.logTag}] provisioning failed`, {
      errorMessage:
        error instanceof Error ? error.message : "Unknown provisioning error.",
      errorName: error instanceof Error ? error.name : typeof error,
      stack: error instanceof Error ? error.stack : undefined,
      walletAddress: args.walletAddress,
    });
    return {
      ok: false,
      response: NextResponse.json(
        {
          error: {
            code: "provisioning_failed",
            message: "Failed to resolve the smart account for this wallet.",
          },
        },
        { status: 502 }
      ),
    };
  }
}

const connectionCache = new Map<SolanaEnv, Connection>();

function getConnection(cluster: SolanaEnv): Connection {
  const cached = connectionCache.get(cluster);
  if (cached) {
    return cached;
  }

  const { rpcEndpoint, websocketEndpoint } = getServerSolanaEndpoints(cluster);
  const connection = new Connection(rpcEndpoint, {
    commitment: "confirmed",
    disableRetryOnRateLimit: true,
    fetch: getFrontendSolanaRpcFetch(globalThis.fetch),
    wsEndpoint: websocketEndpoint,
  });
  connectionCache.set(cluster, connection);
  return connection;
}

// Sum the selected mint across the wallet's token accounts (parsed RPC; no
// spl-token dependency, mirroring the frontend asset provider).
async function getWalletMintBalanceRaw(
  connection: Connection,
  owner: PublicKey,
  mint: PublicKey
): Promise<bigint> {
  const { value } = await connection.getParsedTokenAccountsByOwner(owner, {
    mint,
  });
  let total = BigInt(0);
  for (const entry of value) {
    const parsed = entry.account.data.parsed as
      | { info?: { tokenAmount?: { amount?: unknown } } }
      | undefined;
    const amount = parsed?.info?.tokenAmount?.amount;
    if (typeof amount === "string") {
      total += BigInt(amount);
    }
  }
  return total;
}
