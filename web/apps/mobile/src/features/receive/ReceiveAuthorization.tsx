import type {
  EvmDepositAddressRequest,
  EvmDepositAddressResponse,
} from "@loyal-labs/api";
import {
  getAllUserEmbeddedSolanaWallets,
  getUserEmbeddedEthereumWallet,
  useAuthorizationSignature,
  useIdentityToken,
  useLoginWithSiws,
  usePrivy,
  usePrivyClient,
} from "@privy-io/expo";
import { createContext, type ReactNode, useContext } from "react";

import { env } from "@/config/env";
import { getWalletSigner } from "@/lib/solana/wallet/wallet-details";
import { signInWithSigner } from "@/lib/wallet/privy-migration";
import { exchangePrivySession } from "@/lib/wallet/privy-session";
import { depositAuthorization } from "./deposit-authorization";

type ReceiveAuthorization = {
  claim(
    wallet: string,
    preview: EvmDepositAddressResponse,
    signal: AbortSignal
  ): Promise<EvmDepositAddressRequest>;
  bind(wallet: string, signal: AbortSignal): Promise<void>;
};

const Context = createContext<ReceiveAuthorization | null>(null);
export const useReceiveAuthorization = () => useContext(Context);

function checkActive(signal: AbortSignal) {
  if (signal.aborted) throw new Error("Receive was closed.");
}

export function ReceiveAuthorizationProvider({
  children,
}: {
  children: ReactNode;
}) {
  const { user, isReady } = usePrivy();
  const client = usePrivyClient();
  const siws = useLoginWithSiws();
  const { generateAuthorizationSignature } = useAuthorizationSignature();
  const { getIdentityToken } = useIdentityToken();

  async function owner(wallet: string, signal: AbortSignal) {
    const signer = await getWalletSigner();
    checkActive(signal);
    if (!isReady || signer.publicKey.toBase58() !== wallet) {
      throw new Error("Unlock your wallet and try again.");
    }
    const linked = user ?? (await signInWithSigner(signer, siws, signal));
    checkActive(signal);
    if (
      !linked.linked_accounts.some(
        (account) =>
          account.type === "wallet" &&
          account.chain_type === "solana" &&
          account.address === wallet
      )
    ) {
      throw new Error(
        "Sign in with the Solana wallet shown on the wallet screen."
      );
    }
    return { linked, signer };
  }

  async function identity(wallet: string, userId: string, signal: AbortSignal) {
    // Expo reads a cached identity token. Refresh it after linking or creating a wallet.
    const { user: fresh } = await client.user.refreshUser();
    checkActive(signal);
    if (
      fresh.id !== userId ||
      !fresh.linked_accounts.some(
        (account) =>
          account.type === "wallet" &&
          account.chain_type === "solana" &&
          account.address === wallet
      )
    )
      throw new Error("The active wallet changed. Reopen Receive.");
    const token = await getIdentityToken();
    checkActive(signal);
    if (!token)
      throw new Error("Please sign in again to receive from other chains.");
    return token;
  }

  const auth: ReceiveAuthorization = {
    async bind(wallet, signal) {
      const { linked, signer } = await owner(wallet, signal);
      await exchangePrivySession(
        await identity(wallet, linked.id, signal),
        signer
      );
    },
    async claim(wallet, preview, signal) {
      const { linked } = await owner(wallet, signal);
      const solana = getAllUserEmbeddedSolanaWallets(linked);
      if (preview.kind === "relay_signed") {
        if (!getUserEmbeddedEthereumWallet(linked)) {
          await client.embeddedWallet.create({ solanaAccount: solana[0] });
          checkActive(signal);
        }
        return {
          privyIdentityToken: await identity(wallet, linked.id, signal),
        };
      }
      const embedded = solana.find((account) => account.address === wallet);
      if (!embedded)
        throw new Error(
          "Sign in with the wallet that owns this deposit address."
        );
      const request = depositAuthorization(
        preview,
        env.privyAppId,
        embedded.id
      );
      const { signature } = await generateAuthorizationSignature(request);
      checkActive(signal);
      return { signature, requestExpiry: preview.requestExpiry };
    },
  };
  return <Context.Provider value={auth}>{children}</Context.Provider>;
}
