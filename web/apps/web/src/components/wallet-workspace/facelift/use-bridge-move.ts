"use client";

import type {
  BridgeBalance,
  BridgeBalancesResponse,
  BridgeQuoteResponse,
  BridgeTransfer,
  BridgeTransfersResponse,
  EvmDepositNetwork,
  RouteError,
} from "@loyal-labs/api";
import { useSendTransaction, useWallets } from "@privy-io/react-auth";
import {
  useCallback,
  useEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
} from "react";

import { useEvmDepositAddress } from "./use-evm-deposit-address";

export type IncomingTransfer = {
  chain: string;
  shortName: string;
  chainIcon: string | null;
  asset: string;
  symbol: string;
  icon: string | null;
  decimals: number;
  amountRaw: bigint;
  minUsd: number;
  requestId: string | null;
};

export type MoveStep =
  | { kind: "form" }
  | { kind: "sending" }
  | { kind: "bridging"; requestId: string }
  | { kind: "filled"; transfer: BridgeTransfer }
  | { kind: "failed"; message: string; requestId: string | null };

const POLL_MS = 3000;
const RECEIPT_TIMEOUT_MS = 120_000;
const SEND_TIMEOUT_MS = 120_000;
const SEND_TIMEOUT = "move_send_timeout";

const SPONSOR_REFUSAL_CODES = new Set([
  "insufficient_funds",
  "policy_violation",
  "feature_not_enabled",
]);

export function toRaw(input: string, decimals: number): bigint {
  const [whole = "", fraction = ""] = input.replace(/,/g, "").split(".");
  const digits = `${whole}${fraction.slice(0, decimals).padEnd(decimals, "0")}`;
  return /^\d+$/.test(digits) ? BigInt(digits) : BigInt(0);
}

export function fromRaw(raw: bigint | string, decimals: number): string {
  const value = typeof raw === "bigint" ? raw : BigInt(raw);
  const base = BigInt(10) ** BigInt(decimals);
  const fraction = (value % base).toString().padStart(decimals, "0");
  const trimmed = fraction.slice(0, 6).replace(/0+$/, "");
  const whole = (value / base).toLocaleString("en-US");
  return trimmed ? `${whole}.${trimmed}` : whole;
}

async function readJSON<T>(res: Response, fallback: string): Promise<T> {
  const body = (await res.json().catch(() => null)) as
    | T
    | Partial<RouteError>
    | null;
  if (res.ok && body) return body as T;
  throw new Error(
    (body && typeof body === "object" && "error" in body && body.error?.message) ||
      `${fallback} (${res.status}).`
  );
}

const post = (path: string, body: unknown) =>
  fetch(path, {
    method: "POST",
    credentials: "include",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });

const errorText = (e: unknown) => (e instanceof Error ? e.message : String(e));

type SendFailure = "rejected" | "refused" | "timeout" | "other";

export function classifySendError(e: unknown): SendFailure {
  const { code, privyErrorCode } = (e ?? {}) as {
    code?: unknown;
    privyErrorCode?: unknown;
  };
  if (code === 4001) return "rejected";
  if (code === SEND_TIMEOUT) return "timeout";
  return [code, privyErrorCode].some(
    (c) => typeof c === "string" && SPONSOR_REFUSAL_CODES.has(c)
  )
    ? "refused"
    : "other";
}

export function settleWithin<T>(pending: Promise<T>, ms = SEND_TIMEOUT_MS) {
  let timer: ReturnType<typeof setTimeout>;
  const expired = new Promise<never>((_, reject) => {
    timer = setTimeout(
      () => reject(Object.assign(new Error("Timed out"), { code: SEND_TIMEOUT })),
      ms
    );
  });
  return Promise.race([pending, expired]).finally(() => clearTimeout(timer));
}

function sendFailureCopy(failure: SendFailure, symbol: string, sent: boolean) {
  if (failure === "timeout")
    return "This is taking too long. Check your wallet before trying again.";
  if (failure === "refused")
    return sent
      ? "Not sponsored. Please try again."
      : `Not sponsored. ${symbol} stays in your wallet.`;
  return sent
    ? "The move did not finish. Check your wallet before trying again."
    : "The move did not finish. Please try again.";
}

const readBalances = async () =>
  (
    await readJSON<BridgeBalancesResponse>(
      await fetch("/api/funding/bridge/balances", { credentials: "include" }),
      "Could not read your EVM balances"
    )
  ).balances ?? [];

const readTransfers = async () =>
  (
    await readJSON<BridgeTransfersResponse>(
      await fetch("/api/funding/bridge/transfers", { credentials: "include" }),
      "Could not read the bridge"
    )
  ).transfers ?? [];

type MoveRead = {
  wallet: string | null;
  balances: BridgeBalance[];
  transfers: BridgeTransfer[];
};

export function useBridgeMoveBalances(
  enabled: boolean,
  networks: EvmDepositNetwork[],
  wallet: string | null
) {
  const [read, setRead] = useState<MoveRead | null>(null);
  const [reads, reload] = useReducer((n: number) => n + 1, 0);
  const hasNetworks = networks.length > 0;
  useEffect(() => {
    if (!enabled || !hasNetworks) return;
    let live = true;
    void Promise.all([readBalances(), readTransfers().catch(() => [])])
      .then(([balances, transfers]) => {
        if (live) setRead({ wallet, balances, transfers });
      })
      .catch(() => {
        if (live) setRead({ wallet, balances: [], transfers: [] });
      });
    return () => {
      live = false;
    };
  }, [enabled, hasNetworks, wallet, reads]);

  const shown = enabled && read?.wallet === wallet ? read : null;
  const incoming = useMemo(() => {
    const row = (
      chain: string,
      asset: string,
      amountRaw: bigint,
      requestId: string | null
    ): IncomingTransfer[] => {
      const network = networks.find((n) => n.chain === chain);
      const token = network?.tokens?.find((t) => t.asset === asset);
      if (!network || !token || amountRaw === BigInt(0)) return [];
      return [
        {
          chain,
          shortName: network.shortName,
          chainIcon: network.icon,
          asset,
          symbol: token.symbol,
          icon: token.icon,
          decimals: token.decimals,
          amountRaw,
          minUsd: network.minUsd,
          requestId,
        },
      ];
    };
    const bridging = (shown?.transfers ?? []).filter(
      (t) => t.state === "submitted"
    );
    const moving = new Set(bridging.map((t) => `${t.chain}:${t.asset}`));
    return [
      ...bridging.flatMap((t) =>
        row(t.chain, t.asset, BigInt(t.amountInRaw), t.requestId)
      ),
      ...(shown?.balances ?? [])
        .filter((b) => !moving.has(`${b.chain}:${b.asset}`))
        .flatMap((b) => row(b.chain, b.asset, BigInt(b.amountRaw), null)),
    ];
  }, [networks, shown]);
  return { incoming, reload };
}

export function useIncomingTransfers(walletAddress: string | null) {
  const evm = useEvmDepositAddress(walletAddress);
  const { info, load, retryOnNextLoad } = evm;
  const [peeks, peek] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    void load(false);
  }, [load, peeks]);
  const sender = info?.kind === "relay_signed" ? info.address : null;
  const { incoming, reload } = useBridgeMoveBalances(
    sender !== null,
    evm.networks,
    walletAddress
  );
  const refresh = useCallback(() => {
    retryOnNextLoad();
    peek();
    reload();
  }, [reload, retryOnNextLoad]);
  return { sender, transfers: sender ? incoming : [], reload: refresh };
}

async function waitForReceipt(
  provider: { request: (args: { method: string; params?: unknown[] }) => Promise<unknown> },
  hash: string
) {
  const deadline = Date.now() + RECEIPT_TIMEOUT_MS;
  while (Date.now() < deadline) {
    const receipt = (await settleWithin(
      provider.request({
        method: "eth_getTransactionReceipt",
        params: [hash],
      })
    )) as { status?: string } | null;
    if (receipt?.status === "0x1") return;
    if (receipt?.status === "0x0") throw new Error("The approval transaction reverted.");
    await new Promise((resolve) => setTimeout(resolve, 1500));
  }
  throw new Error("Transaction confirmation timed out");
}

export function useBridgeMove(
  transfer: IncomingTransfer,
  sender: string,
  onSubmitted: () => void
) {
  const { sendTransaction } = useSendTransaction();
  const { wallets } = useWallets();
  const [step, setStep] = useState<MoveStep>(
    transfer.requestId
      ? { kind: "bridging", requestId: transfer.requestId }
      : { kind: "form" }
  );
  const busyRef = useRef(false);
  const lockName = `loyal.bridge.move:${sender.toLowerCase()}:${transfer.chain}`;

  const move = useCallback(
    async (quoted: BridgeQuoteResponse) => {
      const calls = quoted.calls ?? [];
      setStep({ kind: "sending" });
      const wallet = wallets.find(
        (w) => w.address.toLowerCase() === sender.toLowerCase()
      );
      let sent = false;
      let originTx: string | null = null;
      try {
        if (!wallet || calls.length === 0) {
          throw new Error("Your EVM wallet is not ready. Please try again.");
        }
        await settleWithin(wallet.switchChain(quoted.chainId));
        const provider = await settleWithin(wallet.getEthereumProvider());
        await settleWithin(
          post(
            `/api/funding/bridge/transfers/${encodeURIComponent(quoted.requestId)}/confirm`,
            {}
          ).then((res) => readJSON<BridgeTransfer>(res, "Could not start the move"))
        );
        for (const [index, call] of calls.entries()) {
          const { hash } = await settleWithin(
            sendTransaction(
              {
                to: call.to,
                data: call.data,
                value: BigInt(call.value),
                chainId: quoted.chainId,
              },
              {
                sponsor: true,
                address: wallet.address,
                uiOptions: { showWalletUIs: index === 0 },
              }
            )
          );
          sent = true;
          if (index < calls.length - 1) await waitForReceipt(provider, hash);
          else originTx = hash;
        }
      } catch (e) {
        const failure = classifySendError(e);
        if (failure === "rejected") {
          setStep({ kind: "form" });
          return;
        }
        if (failure === "refused")
          void post(
            `/api/funding/bridge/transfers/${encodeURIComponent(quoted.requestId)}/sponsor-refusal`,
            { message: errorText(e).slice(0, 500) }
          ).catch(() => undefined);
        setStep({
          kind: "failed",
          message: sendFailureCopy(failure, transfer.symbol, sent),
          requestId: null,
        });
        return;
      }
      if (!originTx) return;
      setStep({ kind: "bridging", requestId: quoted.requestId });
      for (let attempt = 0; attempt < 3; attempt++) {
        try {
          await readJSON<BridgeTransfer>(
            await post(
              `/api/funding/bridge/transfers/${encodeURIComponent(quoted.requestId)}/submit`,
              { originTx }
            ),
            "Could not record the bridge"
          );
          break;
        } catch {
          if (attempt < 2) await new Promise((r) => setTimeout(r, 2000));
        }
      }
      onSubmitted();
    },
    [onSubmitted, sendTransaction, sender, transfer.symbol, wallets]
  );

  const confirm = useCallback(async () => {
    if (busyRef.current) return;
    busyRef.current = true;
    setStep({ kind: "sending" });
    try {
      const quoted = await readJSON<BridgeQuoteResponse>(
        await post("/api/funding/bridge/quote", {
          chain: transfer.chain,
          asset: transfer.asset,
          amountRaw: transfer.amountRaw.toString(),
        }),
        "Could not get a quote"
      );
      if (!navigator.locks) {
        await move(quoted);
        return;
      }
      await navigator.locks.request(
        lockName,
        { ifAvailable: true },
        async (lock: Lock | null) => {
          if (lock) await move(quoted);
          else
            setStep({
              kind: "failed",
              message: "A move is already running in another tab.",
              requestId: null,
            });
        }
      );
    } catch (e) {
      setStep({ kind: "failed", message: errorText(e), requestId: null });
    } finally {
      busyRef.current = false;
    }
  }, [lockName, move, transfer.amountRaw, transfer.asset, transfer.chain]);

  const reset = useCallback(() => setStep({ kind: "form" }), []);

  const bridging = step.kind === "bridging" ? step : null;
  useEffect(() => {
    if (!bridging) return;
    let live = true;
    const poll = async () => {
      try {
        const settled = (await readTransfers()).find(
          (t) => t.requestId === bridging.requestId
        );
        if (!live || !settled) return;
        if (settled.state === "filled")
          setStep({ kind: "filled", transfer: settled });
        else if (settled.state === "refunded" || settled.state === "failed")
          setStep({
            kind: "failed",
            message:
              settled.state === "refunded"
                ? `Relay refunded ${transfer.symbol} to your ${transfer.shortName} wallet.`
                : "The bridge failed.",
            requestId: settled.requestId,
          });
      } catch {
        return;
      }
    };
    const timer = setInterval(() => void poll(), POLL_MS);
    void poll();
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [bridging, transfer.shortName, transfer.symbol]);

  return { step, confirm, reset };
}
