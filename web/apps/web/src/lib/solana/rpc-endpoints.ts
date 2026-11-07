import {
  getSolanaEndpoints as getSharedSolanaEndpoints,
  type SolanaEndpoints,
  type SolanaEnv,
} from "@loyal-labs/solana-rpc";

const FRONTEND_SOLANA_ENDPOINTS_BY_ENV: Partial<
  Record<SolanaEnv, SolanaEndpoints>
> = {
  devnet: {
    rpcEndpoint: "https://karlotta-a6micy-fast-devnet.helius-rpc.com",
    websocketEndpoint: "wss://karlotta-a6micy-fast-devnet.helius-rpc.com",
  },
};

// loyal-web holds the mainnet upstream URL, so it never ships in the bundle.
// Connection and createSolanaRpc need absolute URLs and privy-provider.tsx
// calls this at module load, which also runs during prerender.
function getRoutedMainnetEndpoints(): SolanaEndpoints {
  const origin =
    typeof window === "undefined"
      ? "https://askloyal.com"
      : window.location.origin;
  const rpcEndpoint = `${origin}/api/solana/rpc`;
  return {
    rpcEndpoint,
    websocketEndpoint: rpcEndpoint.replace(/^http/, "ws"),
  };
}

export function getFrontendSolanaEndpoints(env: SolanaEnv): SolanaEndpoints {
  if (env === "mainnet") {
    return getRoutedMainnetEndpoints();
  }
  return FRONTEND_SOLANA_ENDPOINTS_BY_ENV[env] ?? getSharedSolanaEndpoints(env);
}
