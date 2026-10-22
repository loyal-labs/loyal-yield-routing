import type { SolanaEnv } from "@loyal-labs/solana-rpc";
import {
  createSolanaWalletDataClient,
  type SolanaWalletDataClient,
} from "@loyal-labs/solana-wallet";
import type { PublicKey } from "@solana/web3.js";
import { useMemo } from "react";

export function useSolanaWalletDataClient(
  solanaEnv: SolanaEnv,
  walletPublicKey: PublicKey | null
): SolanaWalletDataClient {
  return useMemo(() => {
    return createSolanaWalletDataClient({ env: solanaEnv });
  }, [solanaEnv, walletPublicKey]);
}
