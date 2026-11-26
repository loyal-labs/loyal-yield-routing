import { useWalletUsdcRaw } from "@/hooks/wallet/useWalletUsdcRaw";

export function useEarnMaxFundingUsdc(walletAddress: string | null): number | null {
  const raw = useWalletUsdcRaw(walletAddress, true);
  return raw === null ? null : Number(raw) / 1_000_000;
}
