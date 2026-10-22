export type SolanaEnv = "mainnet" | "testnet" | "devnet" | "localnet";

export type WalletTransfer = {
  signature: string;
  slot: number;
  timestamp: number | null;
  direction: "in" | "out";
  type: "transfer" | "swap";
  amountLamports: number;
  netChangeLamports: number;
  feeLamports: number;
  status: "success" | "failed";
  counterparty?: string;
  tokenMint?: string;
  tokenAmount?: string; // human-readable (decimal-adjusted, truncated for display)
  tokenDecimals?: number;
  swapFromMint?: string;
  swapFromAmount?: string;
  swapFromDecimals?: number;
  swapToMint?: string;
  swapToAmount?: string;
  swapToDecimals?: number;
};

export type GetAccountTransactionHistoryOptions = {
  limit?: number;
  before?: string;
  until?: string;
  onlySystemTransfers?: boolean;
};
