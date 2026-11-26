import type {
  EvmDepositAddressResponse,
  TronDepositAddressResponse,
} from "@loyal-labs/api";
import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { AppState } from "react-native";

import { track } from "@/lib/analytics/analytics";
import { BRIDGE_EVENTS } from "@/lib/analytics/bridge-events";
import { getSolanaEnv, onSolanaEnvChange } from "@/lib/solana/rpc/connection";
import {
  receiveNetworks,
  renewAt,
  tronAddress,
  validatedDeposit,
} from "./deposit-routes";
import { fetchFundingLaunch, fundingRequest } from "./funding-api";
import { useReceiveAuthorization } from "./ReceiveAuthorization";

const evmPath = "/api/funding/evm-deposit-address";
const tronPath = "/api/funding/tron-deposit-address";

type State = {
  wallet: string | null;
  deposit: EvmDepositAddressResponse | null;
  tron: TronDepositAddressResponse | null;
  loading: boolean;
  tronLoading: boolean;
  error: string | null;
  tronError: string | null;
};
const empty = (wallet: string | null): State => ({
  wallet,
  deposit: null,
  tron: null,
  loading: false,
  tronLoading: false,
  error: null,
  tronError: null,
});

type Attempt = "ready" | "loading" | "finished";
type Sheet = {
  wallet: string;
  signal: AbortSignal;
  evm: Attempt;
  tron: Attempt;
};

function errorMessage(error: unknown) {
  return error instanceof Error
    ? error.message
    : "Could not load the deposit address. Please try again.";
}

export type ReceiveTab = "Solana" | "Other chains";

export function useReceiveBridge(
  walletAddress: string | null,
  tab: ReceiveTab | null
) {
  const open = tab === "Other chains";
  const receiveOpen = tab !== null;
  const mainnet =
    useSyncExternalStore(onSolanaEnvChange, getSolanaEnv, getSolanaEnv) ===
    "mainnet";
  const authorization = useReceiveAuthorization();
  const latestAuth = useRef(authorization);
  useEffect(() => {
    latestAuth.current = authorization;
  }, [authorization]);
  const sheet = useRef<Sheet | null>(null);
  const [state, setState] = useState<State>(() => empty(null));
  const [launchedWallet, setLaunchedWallet] = useState<string | null>(null);
  const launched =
    mainnet && !!walletAddress && launchedWallet === walletAddress;

  useEffect(() => {
    setLaunchedWallet((previous) =>
      mainnet && previous === walletAddress ? previous : null
    );
    if (!walletAddress || !mainnet) return;
    const controller = new AbortController();
    void fetchFundingLaunch(walletAddress, controller.signal)
      .catch(() => false)
      .then((allowed) => {
        if (!controller.signal.aborted)
          setLaunchedWallet(allowed ? walletAddress : null);
      });
    return () => controller.abort();
  }, [walletAddress, mainnet, receiveOpen]);

  const patch = useCallback((active: Sheet, data: Partial<State>) => {
    if (active.signal.aborted) return;
    setState((previous) =>
      previous.wallet === active.wallet && !active.signal.aborted
        ? { ...previous, ...data }
        : previous
    );
  }, []);

  useLayoutEffect(() => {
    setState(empty(walletAddress));
    if (!open || !walletAddress || !launched) return;
    const controller = new AbortController();
    const active: Sheet = {
      wallet: walletAddress,
      signal: controller.signal,
      evm: "ready",
      tron: "ready",
    };
    sheet.current = active;
    return () => {
      controller.abort();
      sheet.current = null;
    };
  }, [open, walletAddress, launched]);

  const loadRoute = useCallback(
    async (route: "evm" | "tron", retry: boolean) => {
      const active = sheet.current;
      if (
        !active ||
        active.wallet !== walletAddress ||
        !launched ||
        !open ||
        getSolanaEnv() !== "mainnet" ||
        active[route] === "loading" ||
        (active[route] === "finished" && !retry)
      )
        return;
      active[route] = "loading";
      const { wallet, signal } = active;
      const auth = latestAuth.current;
      const options = { interactive: true, signal };
      patch(
        active,
        route === "evm"
          ? { loading: true, error: null }
          : { tronLoading: true, tronError: null }
      );

      // Older mobile accounts may have a valid bearer before their first Privy
      // identity exchange creates the app-user row required by funding.
      async function withOwner<T>(request: () => Promise<T>): Promise<T> {
        try {
          return await request();
        } catch (error) {
          if (
            (error as { code?: string })?.code !== "smart_account_required" ||
            !auth ||
            signal.aborted
          )
            throw error;
          await auth.bind(wallet, signal);
          if (signal.aborted) throw error;
          return request();
        }
      }
      let outcome: "ready" | "failed" = "ready";
      try {
        if (route === "tron") {
          const tron = await withOwner(() =>
            fundingRequest<TronDepositAddressResponse>(wallet, tronPath, {
              ...options,
              method: "POST",
            })
          );
          if (
            !tronAddress.test(tron.address) ||
            !(renewAt(tron) > Date.now())
          ) {
            throw new Error(
              "The TRON deposit address is unavailable. Please try again."
            );
          }
          patch(active, { tron });
        } else {
          const preview = validatedDeposit(
            await withOwner(() =>
              fundingRequest<EvmDepositAddressResponse>(
                wallet,
                evmPath,
                options
              )
            )
          );
          if (signal.aborted) return;
          patch(active, { deposit: preview });
          if (preview.address) return;
          if (!auth) throw new Error("Sign in to receive from EVM networks.");
          const body = await auth.claim(wallet, preview, signal);
          if (signal.aborted) return;
          const deposit = validatedDeposit(
            await withOwner(() =>
              fundingRequest<EvmDepositAddressResponse>(wallet, evmPath, {
                ...options,
                method: "POST",
                body,
              })
            )
          );
          if (!deposit.address)
            throw new Error(
              "Could not create the deposit address. Please try again."
            );
          patch(active, { deposit });
        }
      } catch (error) {
        outcome = "failed";
        patch(
          active,
          route === "evm"
            ? { error: errorMessage(error) }
            : { tron: null, tronError: errorMessage(error) }
        );
      } finally {
        if (!signal.aborted) track(BRIDGE_EVENTS.receiveAddressResult, { network_family: route, outcome });
        active[route] = "finished";
        patch(
          active,
          route === "evm" ? { loading: false } : { tronLoading: false }
        );
      }
    },
    [open, walletAddress, launched, patch]
  );

  useEffect(() => {
    const active = sheet.current;
    if (!active || !state.tron || state.wallet !== active.wallet) return;
    const deadline = renewAt(state.tron);
    let timer: ReturnType<typeof setTimeout>;
    const expire = () => {
      clearTimeout(timer);
      if (active.signal.aborted) return;
      const remaining = deadline - Date.now();
      if (remaining > 0) {
        timer = setTimeout(expire, Math.min(2_147_483_647, remaining));
      } else {
        active.tron = "ready";
        patch(active, {
          tron: null,
          tronError: "Renew the TRON deposit address before sending funds.",
        });
      }
    };
    expire();
    const listener = AppState.addEventListener("change", (status) => {
      if (status === "active") expire();
    });
    return () => {
      clearTimeout(timer);
      listener.remove();
    };
  }, [state.wallet, state.tron, patch]);

  const visible = launched && open && state.wallet === walletAddress;
  const deposit = visible ? state.deposit : null;
  const tron = visible ? state.tron : null;
  const networks = useMemo(
    () => receiveNetworks(deposit, tron),
    [deposit, tron]
  );

  return {
    launched,
    networks,
    kind: deposit?.kind ?? null,
    loading: visible && state.loading,
    error: visible ? state.error : null,
    tronLoading: visible && state.tronLoading,
    tronError: visible ? state.tronError : null,
    load: useCallback(() => loadRoute("evm", false), [loadRoute]),
    retry: useCallback(() => loadRoute("evm", true), [loadRoute]),
    loadTron: useCallback(() => loadRoute("tron", false), [loadRoute]),
    retryTron: useCallback(() => loadRoute("tron", true), [loadRoute]),
  };
}
