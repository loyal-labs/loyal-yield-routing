export type BridgeAsset = { symbol: string; name: string; icon: string };

// Figma fixtures only. Bridge has no backend, deposit address, or transaction flow yet.
export const CHAINS = [
  { id: "ethereum", name: "Ethereum (ERC20)", network: "Ethereum" },
  { id: "tron", name: "Tron (TRC20)", network: "Tron" },
  { id: "bnb", name: "BNB Smart Chain (BEP20)", network: "BNB Smart Chain" },
  { id: "base", name: "Base Mainnet", network: "Base" },
  { id: "arbitrum", name: "Arbitrum One", network: "Arbitrum" },
  { id: "polygon", name: "Polygon PoS", network: "Polygon" },
  { id: "avalanche", name: "Avalanche", network: "Avalanche" },
] as const;
export const SOLANA_ASSETS = [
  {
    symbol: "USDC",
    name: "USD Coin",
    icon: "usdc.png",
  },
  {
    symbol: "SOL",
    name: "Solana",
    icon: "sol.png",
  },
] as const;
export const BRIDGE_ASSETS = [
  { symbol: "USDT", name: "Tether USD", icon: "usdt.png" },
  { symbol: "USDG", name: "Global Dollar", icon: "usdg.png" },
  { symbol: "PYUSD", name: "PayPal USD", icon: "pyusd.png" },
  { symbol: "USDC", name: "USD Coin", icon: "usdc.png" },
  { symbol: "USDS", name: "USDS", icon: "usds.png" },
] as const;
