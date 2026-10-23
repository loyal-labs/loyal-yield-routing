import "server-only";

import { PublicKey, type Connection } from "@solana/web3.js";

import { getOrCreateCurrentUser } from "@/features/chat/server/app-user";
import {
  ensureWalletUserSmartAccountTraced,
  findReadyCurrentUserSmartAccount,
} from "@/features/smart-accounts/server/service";

// Resolve (provisioning if needed) the canonical smart account for a wallet —
// the same account the web flow uses, so Earn is one position everywhere.
// `ensureWalletUserSmartAccount` is idempotent: an account created on web is
// reused at no cost. The *first-ever* provisioning is sponsored (Loyal pays
// rent), so gate it behind real funds — the wallet must already hold the
// selected mint it is depositing. This makes free-account spam economically
// infeasible (each new account needs a distinct funded wallet) without
// depending on the captcha or external rate-limit infra.
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

export async function resolveFundedSmartAccount(input: {
  connection: Connection;
  walletAddress: string;
  mint: PublicKey;
  amountRaw: bigint;
  request: Request;
}): Promise<
  | {
      outcome: "ready";
      settingsPda: string;
      smartAccountAddress: string;
    }
  | { outcome: "insufficient_balance" }
> {
  const user = await getOrCreateCurrentUser({
    provider: "solana",
    authMethod: "wallet",
    subjectAddress: input.walletAddress,
    walletAddress: input.walletAddress,
  });
  const existing = await findReadyCurrentUserSmartAccount({
    userId: user.id,
    walletAddress: input.walletAddress,
  });
  if (existing) {
    return {
      outcome: "ready",
      settingsPda: existing.settingsPda,
      smartAccountAddress: existing.smartAccountAddress,
    };
  }
  const mintBalanceRaw = await getWalletMintBalanceRaw(
    input.connection,
    new PublicKey(input.walletAddress),
    input.mint
  );
  if (mintBalanceRaw < input.amountRaw) {
    return { outcome: "insufficient_balance" };
  }
  const ensured = await ensureWalletUserSmartAccountTraced({
    userId: user.id,
    walletAddress: input.walletAddress,
    request: input.request,
  });
  return {
    outcome: "ready",
    settingsPda: ensured.smartAccount.settingsPda,
    smartAccountAddress: ensured.smartAccount.smartAccountAddress,
  };
}
