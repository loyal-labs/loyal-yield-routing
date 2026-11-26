import {
  arbitrum,
  avalanche,
  base,
  bsc,
  mainnet,
  megaeth,
  monad,
  optimism,
  plasma,
  polygon,
  robinhood,
  tempo,
} from "viem/chains";

// Go uses these names in deposit balances and bridge quotes.
export const evmChains = {
  ethereum: mainnet,
  bsc,
  base,
  arbitrum,
  optimism,
  polygon,
  avalanche,
  plasma,
  monad,
  tempo,
  robinhood,
  megaeth,
};
