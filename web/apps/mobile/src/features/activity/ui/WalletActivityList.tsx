import type { BottomSheetModal } from "@gorhom/bottom-sheet";
import { useCallback, useRef, useState } from "react";

import { ActivityFeed } from "@/components/wallet/ActivityFeed";
import { TransactionDetailsSheet } from "@/components/wallet/TransactionDetailsSheet";
import {
  useTokenDetailMints,
  useTokenDetails,
} from "@/hooks/wallet/useTokenDetails";
import { useTokenHoldings } from "@/hooks/wallet/useTokenHoldings";
import type { Transaction } from "@/types/wallet";

import { useActivity } from "../model/ActivityProvider";

export function WalletActivityList() {
  const { walletAddress, walletTransactions, isFetchingTransactions } =
    useActivity();
  const { tokenHoldings } = useTokenHoldings(walletAddress);

  const tokenDetailMints = useTokenDetailMints(tokenHoldings);
  const tokenDetailsByMint = useTokenDetails(tokenDetailMints);

  const [selectedTransaction, setSelectedTransaction] =
    useState<Transaction | null>(null);
  const txDetailsSheetRef = useRef<BottomSheetModal>(null);

  const handleTransactionPress = useCallback((transaction: Transaction) => {
    setSelectedTransaction(transaction);
    txDetailsSheetRef.current?.present();
  }, []);

  return (
    <>
      <ActivityFeed
        transactions={walletTransactions}
        tokenHoldings={tokenHoldings}
        tokenDetailsByMint={tokenDetailsByMint}
        isLoading={isFetchingTransactions}
        onTransactionPress={handleTransactionPress}
        onShowAll={() => {}}
        maxItems={walletTransactions.length}
        hideHeading
      />

      <TransactionDetailsSheet
        ref={txDetailsSheetRef}
        transaction={selectedTransaction}
        tokenHoldings={tokenHoldings}
        tokenDetailsByMint={tokenDetailsByMint}
      />
    </>
  );
}
