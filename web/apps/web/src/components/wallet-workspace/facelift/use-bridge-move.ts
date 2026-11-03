"use client";

import type {
  BridgeBalancesResponse,
  BridgeQuoteResponse,
  BridgeTransfer,
  BridgeTransfersResponse,
  EvmDepositNetwork,
  RouteError,
} from "@loyal-labs/api";
import { useSendTransaction, useWallets } from "@privy-io/react-auth";
import { useCallback, useEffect, useRef, useState } from "react";

export type MoveBalance = {
  chain: string;
  chainName: string;
  chainIcon: string | null;
  asset: string;
  symbol: string;
  name: string;
  icon: string | null;
  decimals: number;
  amountRaw: bigint;
};

export type MoveSource = {
  sender: string;
  recipient: string;
  balances: MoveBalance[];
  start: MoveBalance;
};

export type MoveStep =
  | { kind: "form" }
  | { kind: "sending"; quote: BridgeQuoteResponse }
  | { kind: "bridging"; quote: BridgeQuoteResponse; originTx: string }
  | { kind: "filled"; transfer: BridgeTransfer }
  | { kind: "failed"; message: string; requestId: string | null };

const RECEIVE_DECIMALS = 6;
const QUOTE_DEBOUNCE_MS = 500;
const POLL_MS = 3000;
const RECEIPT_TIMEOUT_MS = 120_000;

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

export const receivedAmount = (raw: string) => fromRaw(raw, RECEIVE_DECIMALS);

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

const userRejected = (e: unknown) =>
  /exited|rejected|cancel/i.test(errorText(e));

export function useBridgeMoveBalances(
  enabled: boolean,
  networks: EvmDepositNetwork[]
) {
  const [balances, setBalances] = useState<MoveBalance[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [reads, setReads] = useState(0);
  const reload = useCallback(() => setReads((n) => n + 1), []);
  useEffect(() => {
    if (!enabled || networks.length === 0) return;
    let live = true;
    void fetch("/api/funding/bridge/balances", { credentials: "include" })
      .then((res) =>
        readJSON<BridgeBalancesResponse>(res, "Could not read your EVM balances")
      )
      .then((read) => {
        if (!live) return;
        setError(null);
        setBalances(
          (read.balances ?? []).flatMap((b) => {
            const network = networks.find((n) => n.chain === b.chain);
            const token = network?.tokens?.find((t) => t.asset === b.asset);
            const amountRaw = BigInt(b.amountRaw);
            if (!network || !token || amountRaw === BigInt(0)) return [];
            return [
              {
                chain: b.chain,
                chainName: network.name,
                chainIcon: network.icon,
                asset: b.asset,
                symbol: token.symbol,
                name: token.name,
                icon: token.icon,
                decimals: b.decimals,
                amountRaw,
              },
            ];
          })
        );
      })
      .catch((e) => live && setError(errorText(e)));
    return () => {
      live = false;
    };
  }, [enabled, networks, reads]);
  return { balances: enabled ? balances : [], error, reload };
}

async function waitForReceipt(
  provider: { request: (args: { method: string; params?: unknown[] }) => Promise<unknown> },
  hash: string
) {
  const deadline = Date.now() + RECEIPT_TIMEOUT_MS;
  while (Date.now() < deadline) {
    const receipt = (await provider.request({
      method: "eth_getTransactionReceipt",
      params: [hash],
    })) as { status?: string } | null;
    if (receipt?.status === "0x1") return;
    if (receipt?.status === "0x0") throw new Error("The approval transaction reverted.");
    await new Promise((resolve) => setTimeout(resolve, 1500));
  }
  throw new Error("Transaction confirmation timed out");
}

export function useBridgeMove(balance: MoveBalance, sender: string) {
  const { sendTransaction } = useSendTransaction();
  const { wallets } = useWallets();
  const [amountInput, setAmountInput] = useState("");
  const [quote, setQuote] = useState<{
    key: string;
    response: BridgeQuoteResponse;
  } | null>(null);
  const [quoteError, setQuoteError] = useState<string | null>(null);
  const [quoting, setQuoting] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);
  const [step, setStep] = useState<MoveStep>({ kind: "form" });
  const amountRaw = toRaw(amountInput, balance.decimals);
  const insufficient = amountRaw > balance.amountRaw;
  const key = `${balance.chain}:${balance.asset}:${amountRaw}`;
  const current = quote && quote.key === key ? quote.response : null;
  const latestKey = useRef(key);
  latestKey.current = key;

  useEffect(() => {
    if (amountRaw === BigInt(0) || insufficient) {
      setQuoting(false);
      setQuoteError(null);
      return;
    }
    setQuoting(true);
    setQuoteError(null);
    const timer = setTimeout(() => {
      void post("/api/funding/bridge/quote", {
        chain: balance.chain,
        asset: balance.asset,
        amountRaw: amountRaw.toString(),
      })
        .then((res) => readJSON<BridgeQuoteResponse>(res, "Could not get a quote"))
        .then((response) => {
          if (latestKey.current !== key) return;
          setQuote({ key, response });
        })
        .catch((e) => {
          if (latestKey.current === key) setQuoteError(errorText(e));
        })
        .finally(() => {
          if (latestKey.current === key) setQuoting(false);
        });
    }, QUOTE_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [amountRaw, balance.asset, balance.chain, insufficient, key]);

  const send = useCallback(async () => {
    if (!current) return;
    const quoted = current;
    const calls = quoted.calls ?? [];
    setSendError(null);
    setStep({ kind: "sending", quote: quoted });
    const wallet = wallets.find(
      (w) => w.address.toLowerCase() === sender.toLowerCase()
    );
    let originTx: string | null = null;
    let refused = null as string | null;
    try {
      if (!wallet || calls.length === 0) {
        throw new Error("Your EVM wallet is not ready. Please try again.");
      }
      await wallet.switchChain(quoted.chainId);
      const provider = await wallet.getEthereumProvider();
      for (const [index, call] of calls.entries()) {
        const { hash } = await sendTransaction(
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
        ).catch((e: unknown) => {
          if (!userRejected(e)) refused = errorText(e);
          throw e;
        });
        if (index < calls.length - 1) await waitForReceipt(provider, hash);
        else originTx = hash;
      }
    } catch (e) {
      if (userRejected(e)) {
        setStep({ kind: "form" });
        return;
      }
      if (refused)
        void post(
          `/api/funding/bridge/transfers/${encodeURIComponent(quoted.requestId)}/sponsor-refusal`,
          { message: refused.slice(0, 500) }
        ).catch(() => undefined);
      setStep({
        kind: "failed",
        message: refused
          ? `Not sponsored. ${balance.symbol} stays in your wallet.`
          : errorText(e),
        requestId: null,
      });
      return;
    }
    if (!originTx) return;
    setStep({ kind: "bridging", quote: quoted, originTx });
    for (let attempt = 0; attempt < 3; attempt++) {
      try {
        await readJSON<BridgeTransfer>(
          await post(
            `/api/funding/bridge/transfers/${encodeURIComponent(quoted.requestId)}/submit`,
            { originTx }
          ),
          "Could not record the bridge"
        );
        return;
      } catch (e) {
        if (attempt === 2) setSendError(errorText(e));
        else await new Promise((resolve) => setTimeout(resolve, 2000));
      }
    }
  }, [balance.symbol, current, sendTransaction, sender, wallets]);

  const bridging = step.kind === "bridging" ? step : null;
  useEffect(() => {
    if (!bridging) return;
    let live = true;
    const poll = async () => {
      try {
        const read = await readJSON<BridgeTransfersResponse>(
          await fetch("/api/funding/bridge/transfers", { credentials: "include" }),
          "Could not read the bridge"
        );
        const transfer = read.transfers?.find(
          (t) => t.requestId === bridging.quote.requestId
        );
        if (!live || !transfer) return;
        if (transfer.state === "filled") setStep({ kind: "filled", transfer });
        else if (transfer.state === "refunded" || transfer.state === "failed")
          setStep({
            kind: "failed",
            message:
              transfer.state === "refunded"
                ? `Relay refunded ${balance.symbol} to your ${balance.chainName} wallet.`
                : "The bridge failed.",
            requestId: transfer.requestId,
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
  }, [balance.chainName, balance.symbol, bridging]);

  return {
    amountInput,
    setAmountInput,
    amountRaw,
    insufficient,
    quote: current,
    quoting,
    quoteError,
    sendError,
    step,
    send,
  };
}
