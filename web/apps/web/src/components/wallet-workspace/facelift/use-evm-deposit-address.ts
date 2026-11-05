"use client";

import type {
  EvmDepositAddressResponse,
  EvmDepositNetwork,
  RouteError,
  TronDepositAddressResponse,
} from "@loyal-labs/api";
import {
  getIdentityToken,
  useAuthorizationSignature,
  useCreateWallet,
  usePrivy,
  useUser,
} from "@privy-io/react-auth";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { bridgeIconFile } from "./bridge-icons";

type EvmDepositInfo = EvmDepositAddressResponse & { address: string };

type DepositState = {
  wallet: string | null;
  info: EvmDepositInfo | null;
  networks: EvmDepositNetwork[];
  error: string | null;
  loading: boolean;
  tron: TronDepositAddressResponse | null;
  tronError: string | null;
  tronLoading: boolean;
};

const noNetworks: EvmDepositNetwork[] = [];

const emptyState = (wallet: string | null): DepositState => ({
  wallet,
  info: null,
  networks: noNetworks,
  error: null,
  loading: false,
  tron: null,
  tronError: null,
  tronLoading: false,
});

async function readDepositResponse<T extends { address: unknown }>(
  res: Response
): Promise<T> {
  const body = (await res.json().catch(() => null)) as
    | T
    | Partial<RouteError>
    | null;
  if (res.ok && body && "address" in body) return body;
  throw new Error(
    (body && "error" in body && body.error?.message) ||
      `Could not get a deposit address (${res.status}).`
  );
}

function withTron(
  network: EvmDepositNetwork,
  tron: TronDepositAddressResponse | null
): EvmDepositNetwork {
  if (network.route !== "relay_deposit_address" || !tron) return network;
  return {
    ...network,
    address: tron.address,
    tokens:
      network.tokens?.map((token) => ({
        ...token,
        feeUsd: tron.feeUsd ?? token.feeUsd,
        receiveUsdc: tron.receiveUsdc ?? token.receiveUsdc,
      })) ?? null,
  };
}

const hasPrivyEvmWallet = (user: ReturnType<typeof useUser>["user"]) =>
  user?.linkedAccounts.some(
    (a) =>
      a.type === "wallet" &&
      a.chainType === "ethereum" &&
      (a.walletClientType === "privy" || a.walletClientType === "privy-v2")
  ) ?? false;

/**
 * Cross-chain deposit address for the signed-in wallet. Loaded lazily on
 * `load()` because the first load may create the user's Privy EVM wallet, and
 * that should only happen once they open the tab.
 */
export function useEvmDepositAddress(walletAddress: string | null) {
  const { ready: privyReady, authenticated } = usePrivy();
  const { user: privyUser } = useUser();
  const { createWallet } = useCreateWallet();
  const { generateAuthorizationSignature } = useAuthorizationSignature();
  const [state, setState] = useState<DepositState>(emptyState(walletAddress));
  const current =
    state.wallet === walletAddress ? state : emptyState(walletAddress);
  const { info, error, loading, tron, tronError, tronLoading } = current;
  const serverNetworks = current.networks;
  const networks = useMemo(
    () => serverNetworks.map((source) => {
      const network = withTron(source, tron);
      return {
        ...network,
        icon: bridgeIconFile(network.chain, network.icon),
        qrIcon: network.qrIcon || bridgeIconFile(network.chain, network.icon),
        tokens: network.tokens?.map((token) => ({
          ...token,
          icon: bridgeIconFile(token.asset, token.icon),
        })) ?? null,
      };
    }),
    [serverNetworks, tron]
  );

  const ready = privyReady && authenticated && Boolean(walletAddress);

  const attemptedRef = useRef(false);
  const tronAttemptedRef = useRef(false);
  const walletRef = useRef(walletAddress);
  const privyUserRef = useRef(privyUser);
  privyUserRef.current = privyUser;
  useEffect(() => {
    attemptedRef.current = false;
    tronAttemptedRef.current = false;
    walletRef.current = walletAddress;
  }, [walletAddress]);
  const retryOnNextLoad = useCallback(() => {
    if (!info) attemptedRef.current = false;
    if (!tron) tronAttemptedRef.current = false;
  }, [info, tron]);

  const load = useCallback(async (create = true) => {
    if (!ready || !walletAddress || info || loading || attemptedRef.current) {
      return;
    }
    attemptedRef.current = true;
    const forWallet = walletAddress;
    const current = () => walletRef.current === forWallet;
    const patch = (next: Partial<DepositState>) =>
      setState((prev) => ({
        ...(prev.wallet === forWallet ? prev : emptyState(forWallet)),
        ...next,
      }));
    patch({ loading: true, error: null });
    try {
      const res = await fetch("/api/funding/evm-deposit-address", {
        credentials: "include",
      });
      const read = await readDepositResponse<EvmDepositAddressResponse>(res);
      if (!current()) return;
      if (read.address) {
        patch({
          info: { ...read, address: read.address },
          networks: read.networks ?? [],
        });
        return;
      }
      if (read.networks) patch({ networks: read.networks });
      if (!create) return;
      let claim: object;
      if (read.kind === "relay_signed") {
        if (!hasPrivyEvmWallet(privyUserRef.current)) {
          try {
            await createWallet();
          } catch {
            // A stale in-memory user or a failed create falls through to the
            // server, which checks the token's linked wallets.
          }
          if (!current()) return;
        }
        // Re-issues the token, so it lists a wallet created above.
        const privyIdentityToken = await getIdentityToken();
        if (!privyIdentityToken) {
          throw new Error("Privy identity token unavailable.");
        }
        claim = { privyIdentityToken };
      } else {
        if (!read.toSign) {
          throw new Error(`Could not get a deposit address (${res.status}).`);
        }
        // First time: the wallet is user-owned, so the user signs the exact
        // Privy request with their key and the server forwards the signature.
        const { signature } = await generateAuthorizationSignature(
          read.toSign as Parameters<typeof generateAuthorizationSignature>[0]
        );
        claim = { signature, requestExpiry: read.requestExpiry };
      }
      if (!current()) return;
      const posted = await fetch("/api/funding/evm-deposit-address", {
        method: "POST",
        credentials: "include",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(claim),
      });
      const created =
        await readDepositResponse<EvmDepositAddressResponse>(posted);
      if (!created.address) {
        throw new Error(`Could not get a deposit address (${posted.status}).`);
      }
      if (!current()) return;
      patch({
        info: { ...created, address: created.address },
        networks: created.networks ?? [],
      });
    } catch (e) {
      if (current()) patch({ error: e instanceof Error ? e.message : String(e) });
    } finally {
      setState((prev) =>
        prev.wallet === forWallet ? { ...prev, loading: false } : prev
      );
    }
  }, [
    createWallet,
    generateAuthorizationSignature,
    info,
    loading,
    ready,
    walletAddress,
  ]);

  const retry = useCallback(() => {
    attemptedRef.current = false;
    return load();
  }, [load]);

  const mintTron = useCallback(async () => {
    if (!ready || !walletAddress || tron || tronLoading) return;
    if (tronAttemptedRef.current) return;
    tronAttemptedRef.current = true;
    const forWallet = walletAddress;
    const patch = (next: Partial<DepositState>) =>
      setState((prev) => ({
        ...(prev.wallet === forWallet ? prev : emptyState(forWallet)),
        ...next,
      }));
    patch({ tronLoading: true, tronError: null });
    try {
      const res = await fetch("/api/funding/tron-deposit-address", {
        method: "POST",
        credentials: "include",
      });
      const minted = await readDepositResponse<TronDepositAddressResponse>(res);
      if (walletRef.current === forWallet) patch({ tron: minted });
    } catch (e) {
      if (walletRef.current === forWallet) {
        patch({ tronError: e instanceof Error ? e.message : String(e) });
      }
    } finally {
      setState((prev) =>
        prev.wallet === forWallet ? { ...prev, tronLoading: false } : prev
      );
    }
  }, [ready, tron, tronLoading, walletAddress]);

  const retryTron = useCallback(() => {
    tronAttemptedRef.current = false;
    return mintTron();
  }, [mintTron]);

  return {
    ready,
    info,
    networks,
    error,
    loading,
    load,
    retry,
    retryOnNextLoad,
    tronError,
    mintTron,
    retryTron,
  };
}
