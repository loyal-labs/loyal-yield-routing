import { WalletReadyState } from "@solana/wallet-adapter-base";

export function isWalletInstalled(wallet: {
  readyState: WalletReadyState;
}): boolean {
  return wallet.readyState === WalletReadyState.Installed;
}

export function filterInstalledWallets<
  T extends { readyState: WalletReadyState }
>(wallets: readonly T[]): T[] {
  return wallets.filter(isWalletInstalled);
}

export function hasInstalledWalletOption(
  connected: boolean,
  wallets: readonly { readyState: WalletReadyState }[]
): boolean {
  return connected || wallets.some(isWalletInstalled);
}
