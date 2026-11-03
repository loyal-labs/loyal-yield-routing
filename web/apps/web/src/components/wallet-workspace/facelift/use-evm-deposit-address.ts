"use client";

import type {
  EvmDepositAddressResponse,
  EvmDepositNetwork,
  RouteError,
} from "@loyal-labs/api";
import { useAuthorizationSignature, usePrivy } from "@privy-io/react-auth";
import { useCallback, useEffect, useRef, useState } from "react";

import { usePublicEnv } from "@/contexts/public-env-context";

type EvmDepositInfo = EvmDepositAddressResponse & { address: string };

type DepositState = {
  wallet: string | null;
  info: EvmDepositInfo | null;
  networks: EvmDepositNetwork[];
  error: string | null;
  loading: boolean;
};

const emptyState = (wallet: string | null): DepositState => ({
  wallet,
  info: null,
  networks: [],
  error: null,
  loading: false,
});

async function readDepositResponse(
  res: Response
): Promise<EvmDepositAddressResponse> {
  const body = (await res.json().catch(() => null)) as
    | EvmDepositAddressResponse
    | Partial<RouteError>
    | null;
  if (res.ok && body && "address" in body) return body;
  throw new Error(
    (body && "error" in body && body.error?.message) ||
      `Could not get a deposit address (${res.status}).`
  );
}

/**
 * Cross-chain deposit address for the signed-in user's Privy-embedded wallet.
 * `eligible` is false for external wallets (Loyal extension, Phantom…); the
 * address is fetched lazily on `load()` because Privy provisions it on first
 * call and we only want that when the user opens the tab.
 */
export function useEvmDepositAddress(walletAddress: string | null) {
  const { privyAppId } = usePublicEnv();
  const { user } = usePrivy();
  const { generateAuthorizationSignature } = useAuthorizationSignature();
  const [state, setState] = useState<DepositState>(emptyState(walletAddress));
  const { info, networks, error, loading } =
    state.wallet === walletAddress ? state : emptyState(walletAddress);

  const eligible =
    Boolean(privyAppId) &&
    Boolean(walletAddress) &&
    (user?.linkedAccounts.some(
      (a) =>
        a.type === "wallet" &&
        a.chainType === "solana" &&
        a.walletClientType === "privy" &&
        a.address === walletAddress
    ) ??
      false);

  const attemptedRef = useRef(false);
  const walletRef = useRef(walletAddress);
  useEffect(() => {
    attemptedRef.current = false;
    walletRef.current = walletAddress;
  }, [walletAddress]);
  const retryOnNextLoad = useCallback(() => {
    if (!info) attemptedRef.current = false;
  }, [info]);

  const load = useCallback(async () => {
    if (!eligible || info || loading || attemptedRef.current) return;
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
      const read = await readDepositResponse(res);
      if (!current()) return;
      if (read.address) {
        patch({
          info: { ...read, address: read.address },
          networks: read.networks ?? [],
        });
        return;
      }
      if (read.networks) patch({ networks: read.networks });
      if (!read.toSign) {
        throw new Error(`Could not get a deposit address (${res.status}).`);
      }
      // First time: the wallet is user-owned, so the user signs the exact
      // Privy request with their key and the server forwards the signature.
      const { signature } = await generateAuthorizationSignature(
        read.toSign as Parameters<typeof generateAuthorizationSignature>[0]
      );
      const posted = await fetch("/api/funding/evm-deposit-address", {
        method: "POST",
        credentials: "include",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          signature,
          requestExpiry: read.requestExpiry,
        }),
      });
      const created = await readDepositResponse(posted);
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
  }, [eligible, generateAuthorizationSignature, info, loading, walletAddress]);

  return { eligible, info, networks, error, loading, load, retryOnNextLoad };
}
