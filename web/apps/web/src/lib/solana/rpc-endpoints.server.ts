import "server-only";

import {
  getSolanaEndpoints,
  type SolanaEndpoints,
  type SolanaEnv,
} from "@loyal-labs/solana-rpc";

function readServerEndpointOverride(env: SolanaEnv): SolanaEndpoints | null {
  if (env === "localnet") {
    return null;
  }

  const envPrefix = env.toUpperCase();
  const rpcEndpoint =
    process.env[`SOLANA_${envPrefix}_RPC_URL`]?.trim() ||
    (env === "mainnet" ? process.env.SOLANA_RPC_URL?.trim() : undefined);
  if (!rpcEndpoint) {
    return null;
  }

  const sharedEndpoints = getSolanaEndpoints(env);
  return {
    rpcEndpoint,
    websocketEndpoint:
      process.env[`SOLANA_${envPrefix}_WEBSOCKET_URL`]?.trim() ||
      process.env.SOLANA_WEBSOCKET_URL?.trim() ||
      sharedEndpoints.websocketEndpoint,
  };
}

export function getServerSolanaEndpoints(env: SolanaEnv): SolanaEndpoints {
  return readServerEndpointOverride(env) ?? getSolanaEndpoints(env);
}
