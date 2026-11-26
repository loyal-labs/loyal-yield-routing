"use client";

import type {
  BridgeBalance,
  BridgeTransfer,
  IncomingEvent,
  IncomingItem,
  IncomingRequest,
  MoveOutcome,
  RouteError,
} from "@loyal-labs/api";
import {
  emptyIncoming, executeIncomingQuote, incomingItems, lane, prepareIncomingMove,
  readIncomingBalances, readIncomingTransfers, reduceIncoming, settleWithin,
} from "@loyal-labs/api";
export { classifySendError, fromRaw, settleWithin, toRaw } from "@loyal-labs/api";
export type { IncomingItem } from "@loyal-labs/api";
import {
  useSendTransaction,
  useUser,
  useWallets,
} from "@privy-io/react-auth";
import {
  useCallback,
  useEffect,
  useMemo,
  useReducer,
  useRef,
} from "react";

import { useEvmDepositAddress } from "./use-evm-deposit-address";

const POLL_MS = 3000;
const ARRIVED_MS = 5000;
const FOCUS_READ_MS = 30_000;

async function readJSON<T>(res: Response, fallback: string): Promise<T> {
  const body = (await res.json().catch(() => null)) as
    | T
    | Partial<RouteError>
    | null;
  if (res.ok && body) return body as T;
  const detail =
    body && typeof body === "object" && "error" in body ? body.error : undefined;
  throw Object.assign(new Error(detail?.message || `${fallback} (${res.status}).`), {
    status: res.status,
    code: detail?.code,
    requestId: detail?.requestId,
  });
}

const request: IncomingRequest = async (path, { method, body, signal }, fallback) =>
  readJSON(await fetch(path, {
    method, credentials: "include", signal,
    ...(body === undefined ? {} : {
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    }),
  }), fallback);

const errorText = (e: unknown) => (e instanceof Error ? e.message : String(e));

type SendTransaction = ReturnType<typeof useSendTransaction>["sendTransaction"];
type EvmWallet = ReturnType<typeof useWallets>["wallets"][number];

type Move = {
  sender: string;
  chain: string;
  asset: string;
  symbol: string;
  wallet: EvmWallet | undefined;
  sendTransaction: SendTransaction;
  signal: AbortSignal;
  onRead: (balances: BridgeBalance[], transfers: BridgeTransfer[]) => void;
  onQuoted: (requestId: string, amountRaw: bigint) => void;
};

export async function moveIncoming(move: Move): Promise<MoveOutcome> {
  const lockName = `loyal.bridge.move:${move.sender.toLowerCase()}:${move.chain}`;
  try {
    return await (navigator.locks
      ? navigator.locks.request(lockName, { signal: move.signal }, () =>
          moveLocked(move)
        )
      : moveLocked(move));
  } catch (e) {
    if (move.signal.aborted) return { kind: "aborted" };
    return { kind: "failed", message: errorText(e), requestId: null };
  }
}

async function moveLocked(move: Move): Promise<MoveOutcome> {
  const prepared = await prepareIncomingMove({ ...move, request });
  if (prepared.kind !== "quoted") return prepared;
  move.onQuoted(prepared.quote.requestId, prepared.amountRaw);
  return executeIncomingQuote({
    ...move, request, quote: prepared.quote,
    async connect(chainId) {
      const { wallet } = move;
      if (!wallet) throw new Error("Your EVM wallet is not ready. Please try again.");
      await settleWithin(wallet.switchChain(chainId));
      const provider = await settleWithin(wallet.getEthereumProvider());
      return {
        provider,
        send: (call, index) => move.sendTransaction(
          { to: call.to, data: call.data, value: BigInt(call.value), chainId },
          { sponsor: true, address: wallet.address, uiOptions: { showWalletUIs: index === 0 } },
        ),
      };
    },
  });
}

const isExternalSolanaWallet = (
  user: ReturnType<typeof useUser>["user"],
  address: string | null
) =>
  user?.linkedAccounts.some(
    (a) =>
      a.type === "wallet" &&
      a.chainType === "solana" &&
      a.address === address &&
      a.walletClientType !== "privy" &&
      a.walletClientType !== "privy-v2"
  ) ?? false;

export function useIncomingTransfers(walletAddress: string | null) {
  const { user } = useUser();
  const { sendTransaction } = useSendTransaction();
  const { wallets } = useWallets();
  const external = isExternalSolanaWallet(user, walletAddress);
  const evm = useEvmDepositAddress(external ? walletAddress : null);
  const { info, load, networks, retryOnNextLoad } = evm;
  const [peeks, peek] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    void load(false);
  }, [load, peeks]);
  const sender =
    external && info?.kind === "relay_signed" ? info.address : null;
  const enabled = sender !== null && networks.length > 0;

  const [state, dispatch] = useReducer(reduceIncoming, null, () =>
    emptyIncoming(null)
  );
  const walletRef = useRef(walletAddress);
  walletRef.current = walletAddress;
  const apply = useCallback(
    (wallet: string, event: IncomingEvent) => {
      if (walletRef.current === wallet) dispatch({ ...event, wallet });
    },
    []
  );
  const readAt = useRef(0);
  const balancesReadAt = useRef(0);
  const read = useCallback(
    async (wallet: string, withBalances: boolean) => {
      const at = ++readAt.current;
      if (withBalances) balancesReadAt.current = Date.now();
      const [balances, transfers] = await Promise.allSettled([
        withBalances ? readIncomingBalances(request) : Promise.resolve(undefined),
        readIncomingTransfers(request),
      ]);
      apply(wallet, {
        type: "read",
        at,
        balances: balances.status === "fulfilled" ? balances.value : undefined,
        transfers: transfers.status === "fulfilled" ? transfers.value : undefined,
        error: !withBalances
          ? undefined
          : balances.status === "rejected"
          ? errorText(balances.reason)
          : null,
      });
    },
    [apply]
  );

  const [reads, reread] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    if (enabled && walletAddress) void read(walletAddress, true);
  }, [enabled, read, reads, walletAddress]);

  useEffect(() => {
    if (!enabled || !walletAddress) return;
    const wallet = walletAddress;
    const peekBalances = () => {
      if (document.visibilityState !== "visible") return;
      if (Date.now() - balancesReadAt.current < FOCUS_READ_MS) return;
      void read(wallet, true);
    };
    window.addEventListener("focus", peekBalances);
    document.addEventListener("visibilitychange", peekBalances);
    return () => {
      window.removeEventListener("focus", peekBalances);
      document.removeEventListener("visibilitychange", peekBalances);
    };
  }, [enabled, read, walletAddress]);

  const shown = enabled && state.wallet === walletAddress ? state : null;
  const items = useMemo(
    () => (shown ? incomingItems(shown, networks) : []),
    [networks, shown]
  );

  const polling = items.some(
    (i) => i.status.kind === "bridging" || i.transfer?.inFlight
  );
  useEffect(() => {
    if (!polling || !walletAddress) return;
    const timer = setInterval(() => void read(walletAddress, false), POLL_MS);
    return () => clearInterval(timer);
  }, [polling, read, walletAddress]);

  const arrived = items.some((i) => i.status.kind === "arrived");
  useEffect(() => {
    if (!arrived || !walletAddress) return;
    const timer = setTimeout(() => {
      apply(walletAddress, { type: "settle" });
      reread();
    }, ARRIVED_MS);
    return () => clearTimeout(timer);
  }, [apply, arrived, walletAddress]);

  const runSignal = useRef<AbortSignal | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    runSignal.current = controller.signal;
    return () => controller.abort();
  }, [walletAddress]);

  const active = useRef(new Set<string>());
  const confirm = useCallback(
    async (item: IncomingItem) => {
      const signal = runSignal.current;
      if (!sender || !walletAddress || !signal || signal.aborted) return;
      const wallet = walletAddress;
      const busy = `${wallet}:${lane(item.chain, item.asset)}`;
      if (active.current.has(busy)) return;
      active.current.add(busy);
      const { key } = item;
      apply(wallet, {
        type: "run",
        run: {
          key,
          chain: item.chain,
          asset: item.asset,
          amountRaw: item.amountRaw,
          requestId: null,
          running: true,
          failure: null,
        },
      });
      try {
        const outcome = await moveIncoming({
          sender,
          chain: item.chain,
          asset: item.asset,
          symbol: item.symbol,
          wallet: wallets.find(
            (w) => w.address.toLowerCase() === sender.toLowerCase()
          ),
          sendTransaction,
          signal,
          onRead: (balances, transfers) => {
            balancesReadAt.current = Date.now();
            apply(wallet, {
              type: "read",
              at: ++readAt.current,
              balances,
              transfers,
              error: null,
            });
          },
          onQuoted: (requestId, amountRaw) =>
            apply(wallet, { type: "patch", key, patch: { requestId, amountRaw } }),
        });
        if (outcome.kind === "aborted") return;
        if (outcome.kind === "nothing" || outcome.kind === "rejected") {
          apply(wallet, { type: "drop", key });
          return;
        }
        apply(wallet, {
          type: "patch",
          key,
          patch:
            outcome.kind === "failed"
              ? {
                  running: false,
                  failure: { message: outcome.message, requestId: outcome.requestId },
                }
              : { running: false, requestId: outcome.requestId },
        });
        void read(wallet, false);
      } finally {
        active.current.delete(busy);
      }
    },
    [apply, read, sendTransaction, sender, walletAddress, wallets]
  );

  const reload = useCallback(() => {
    retryOnNextLoad();
    peek();
    reread();
  }, [retryOnNextLoad]);

  const discoveryError = external && !info ? evm.error : null;

  return {
    items,
    error: discoveryError ?? shown?.error ?? null,
    confirm,
    reload,
  };
}

export type IncomingTransfersState = ReturnType<typeof useIncomingTransfers>;
