import {
  AccountLayout,
  getAssociatedTokenAddressSync,
  TOKEN_PROGRAM_ID,
} from "@solana/spl-token";
import { type AccountInfo, PublicKey } from "@solana/web3.js";
import { useEffect, useState } from "react";
import { AppState } from "react-native";

import { SOLANA_USDC_MINT_MAINNET } from "@/lib/solana/constants";
import { getConnection } from "@/lib/solana/rpc/connection";

function decodeUsdcAmountRaw(
  account: AccountInfo<Buffer> | null,
  mint: PublicKey,
  owner: PublicKey
): bigint {
  if (!(account && account.owner.equals(TOKEN_PROGRAM_ID))) return BigInt(0);
  const decoded = AccountLayout.decode(account.data);
  if (!(decoded.mint.equals(mint) && decoded.owner.equals(owner))) {
    return BigInt(0);
  }
  return BigInt(decoded.amount.toString());
}

export function useEarnMaxFundingUsdc(walletAddress: string | null): number | null {
  const [balance, setBalance] = useState<{
    walletAddress: string;
    amountRaw: bigint;
  } | null>(null);
  useEffect(() => {
    if (!walletAddress) return;
    let owner: PublicKey;
    try {
      owner = new PublicKey(walletAddress);
    } catch {
      return;
    }
    const mint = new PublicKey(SOLANA_USDC_MINT_MAINNET);
    const ata = getAssociatedTokenAddressSync(mint, owner, false, TOKEN_PROGRAM_ID);
    const connection = getConnection();
    let active = true;
    let newestSlot = -1;
    const commit = (account: AccountInfo<Buffer> | null, slot: number) => {
      if (!active || slot < newestSlot) return;
      newestSlot = slot;
      setBalance({ walletAddress, amountRaw: decodeUsdcAmountRaw(account, mint, owner) });
    };
    const catchUp = () => {
      connection
        .getAccountInfoAndContext(ata, "confirmed")
        .then(({ context, value }) => commit(value, context.slot))
        .catch((error) =>
          console.warn("[earn-max] failed to load wallet USDC ATA", error)
        );
    };
    catchUp();
    const subscriptionId = connection.onAccountChange(
      ata,
      (account, context) => {
        try {
          commit(account, context.slot);
        } catch (error) {
          console.warn("[earn-max] failed to decode wallet USDC ATA", error);
        }
      },
      "confirmed"
    );
    const appState = AppState.addEventListener("change", (state) => {
      if (state === "active") catchUp();
    });
    return () => {
      active = false;
      appState.remove();
      void connection.removeAccountChangeListener(subscriptionId);
    };
  }, [walletAddress]);
  return balance && balance.walletAddress === walletAddress
    ? Number(balance.amountRaw) / 1_000_000
    : null;
}
