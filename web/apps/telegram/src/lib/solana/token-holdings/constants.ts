import {
  NATIVE_SOL_MINT,
  SOLANA_USDC_MINT_DEVNET,
  SOLANA_USDC_MINT_MAINNET,
  SOLANA_USDT_MINT_MAINNET,
} from "@/lib/constants";

export const CACHE_TTL_MS = 30_000;

export const KNOWN_TOKEN_ICONS: Record<string, string> = {
  [NATIVE_SOL_MINT]: "/tokens/solana-sol-logo.png",
  [SOLANA_USDT_MINT_MAINNET]: "/tokens/USDT.png",
  [SOLANA_USDC_MINT_MAINNET]: "/tokens/usd-coin-usdc-logo.png",
  [SOLANA_USDC_MINT_DEVNET]: "/tokens/usd-coin-usdc-logo.png",
};

export const DEFAULT_TOKEN_ICON = "/tokens/solana-sol-logo.png";
