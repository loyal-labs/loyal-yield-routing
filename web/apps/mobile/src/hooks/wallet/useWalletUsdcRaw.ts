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

export function useWalletUsdcRaw(walletAddress: string | null, retainLastOnReadError = false): bigint | null {
  const [balance, setBalance] = useState<{
    walletAddress: string;
    amountRaw: bigint;
  } | null>(null);
  useEffect(() => {
    setBalance(null);
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
      try {
        setBalance({ walletAddress, amountRaw: decodeUsdcAmountRaw(account, mint, owner) });
      } catch {
        if (!retainLastOnReadError) setBalance(null);
      }
    };
    const catchUp = () => {
      const startedAt = newestSlot;
      connection
        .getAccountInfoAndContext(ata, "confirmed")
        .then(({ context, value }) => commit(value, context.slot))
        .catch(() => {
          if (active && newestSlot === startedAt && !retainLastOnReadError) setBalance(null);
        });
    };
    catchUp();
    const subscriptionId = connection.onAccountChange(
      ata,
      (account, context) => commit(account, context.slot),
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
  }, [walletAddress, retainLastOnReadError]);
  return balance && balance.walletAddress === walletAddress
    ? balance.amountRaw
    : null;
}
