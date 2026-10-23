import type { WalletTransfer } from "@/lib/solana/rpc/types";

export type TransactionType = "incoming" | "outgoing" | "pending";

export type TransactionStatus = "pending" | "completed" | "error";

export type Transaction = {
  id: string;
  type: TransactionType;
  amountLamports: number;
  status?: TransactionStatus;
  networkFeeLamports?: number;
  signature?: string;
  transferType?: WalletTransfer["type"];
  tokenMint?: string;
  tokenAmount?: string;
  tokenDecimals?: number;
  sender?: string;
  username?: string;
  recipient?: string;
  timestamp: number;
  swapFromMint?: string;
  swapToMint?: string;
  swapFromSymbol?: string;
  swapToSymbol?: string;
  swapToAmount?: number;
  swapToAmountUsd?: number;
};

export type IncomingTransaction = {
  id: string;
  amountLamports: number;
  sender: string;
  username: string;
};

export type TransactionDetailsData = {
  id: string;
  type: "incoming" | "outgoing";
  amountLamports: number;
  transferType?: WalletTransfer["type"];
  tokenMint?: string;
  tokenAmount?: string;
  tokenDecimals?: number;
  recipient?: string;
  recipientUsername?: string;
  recipientAvatar?: string;
  sender?: string;
  senderUsername?: string;
  senderAvatar?: string;
  status: TransactionStatus;
  timestamp: number;
  networkFeeLamports?: number;
  comment?: string;
  signature?: string;
  swapFromMint?: string;
  swapToMint?: string;
  swapFromSymbol?: string;
  swapToSymbol?: string;
  swapToAmount?: number;
  swapToAmountUsd?: number;
};
