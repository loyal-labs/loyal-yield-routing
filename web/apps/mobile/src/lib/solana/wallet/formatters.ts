import { LAMPORTS_PER_SOL } from "@solana/web3.js";

import type { TransactionStatus } from "@/types/wallet";

export function truncateDecimals(num: number, decimals: number): string {
  const factor = Math.pow(10, decimals);
  const truncated = Math.floor(num * factor) / factor;
  return truncated.toFixed(decimals);
}

export function formatBalance(lamports: number | null): string {
  if (lamports === null) return "0.0000";
  const sol = lamports / LAMPORTS_PER_SOL;
  return truncateDecimals(sol, 4);
}

export function formatUsdValue(
  lamports: number | null,
  solPriceUsd: number | null
): string {
  if (lamports === null || solPriceUsd === null) return "\u2014";
  const sol = lamports / LAMPORTS_PER_SOL;
  const usd = sol * solPriceUsd;
  return truncateDecimals(usd, 2);
}

export function formatAddress(address: string | null): string {
  if (!address) return "Loading...";
  return `${address.slice(0, 4)}...${address.slice(-4)}`;
}

export function formatSenderAddress(address: string): string {
  return `${address.slice(0, 4)}...${address.slice(-4)}`;
}

export function formatTransactionAmount(lamports: number): string {
  const sol = lamports / LAMPORTS_PER_SOL;
  return parseFloat(truncateDecimals(sol, 4)).toString();
}

export function formatTransactionDate(timestamp: number): string {
  const date = new Date(timestamp);
  return (
    date.toLocaleDateString("en-US", { month: "short", day: "numeric" }) +
    ", " +
    date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
  );
}

export function getStatusText(
  status: TransactionStatus,
  isIncoming: boolean
): string {
  if (isIncoming && status === "pending") {
    return "Ready to claim";
  }
  switch (status) {
    case "completed":
      return "Completed";
    case "pending":
      return "Pending";
    case "error":
      return "Failed";
    default:
      return status;
  }
}
