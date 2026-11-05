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

export type MoveTransfer = {
  requestId: string;
  status: string;
  amount: string;
  symbol: string;
  chainName: string;
  chainIcon: string | null;
  icon: string | null;
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
const SEND_TIMEOUT_MS = 120_000;
const SEND_TIMEOUT = "move_send_timeout";
const QUOTE_MAX_AGE_MS = 30_000;
const SETTLED_ROW_MS = 86_400_000;
const TRANSFER_ROWS = 3;

const SPONSOR_REFUSAL_CODES = new Set([
  "insufficient_funds",
  "policy_violation",
  "feature_not_enabled",
]);

const TRANSFER_STATUS: Record<BridgeTransfer["state"], string> = {
  quoted: "Bridging to Solana",
  submitted: "Bridging to Solana",
  filled: "Moved to Solana",
  refunded: "Refunded",
  failed: "Move failed",
};

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

function recentTransfers(transfers: BridgeTransfer[], now: number) {
  return transfers
    .filter(
      (t) =>
        t.state === "submitted" ||
        now - Date.parse(t.settledAt ?? t.quotedAt) < SETTLED_ROW_MS
    )
    .slice(0, TRANSFER_ROWS);
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
  error: string | null;
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
        if (live)
          setRead({
            wallet,
            balances,
            transfers: recentTransfers(transfers, Date.now()),
            error: null,
          });
      })
      .catch((e) => {
        if (live)
          setRead({ wallet, balances: [], transfers: [], error: errorText(e) });
      });
    return () => {
      live = false;
    };
  }, [enabled, hasNetworks, wallet, reads]);

  const shown = enabled && read?.wallet === wallet ? read : null;
  const balances = useMemo(
    () =>
      (shown?.balances ?? []).flatMap((b): MoveBalance[] => {
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
      }),
    [networks, shown]
  );
  const transfers = useMemo(
    () =>
      (shown?.transfers ?? []).flatMap((t): MoveTransfer[] => {
        const network = networks.find((n) => n.chain === t.chain);
        const token = network?.tokens?.find((k) => k.asset === t.asset);
        if (!network || !token) return [];
        return [
          {
            requestId: t.requestId,
            status: TRANSFER_STATUS[t.state],
            amount: fromRaw(t.amountInRaw, token.decimals),
            symbol: token.symbol,
            chainName: network.name,
            chainIcon: network.icon,
            icon: token.icon,
          },
        ];
      }),
    [networks, shown]
  );
  return { balances, transfers, error: shown?.error ?? null, reload };
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

type HeldQuote = {
  key: string;
  response: BridgeQuoteResponse;
  expiresAt: number;
  stale: boolean;
};

export function useBridgeMove(balance: MoveBalance, sender: string) {
  const { sendTransaction } = useSendTransaction();
  const { wallets } = useWallets();
  const [amountInput, setAmountInput] = useState("");
  const [held, setHeld] = useState<HeldQuote | null>(null);
  const [quoteError, setQuoteError] = useState<string | null>(null);
  const [quoting, setQuoting] = useState(false);
  const [refreshes, refresh] = useReducer((n: number) => n + 1, 0);
  const [lockedElsewhere, setLockedElsewhere] = useState<string | null>(null);
  const [step, setStep] = useState<MoveStep>({ kind: "form" });
  const sendingRef = useRef(false);
  const amountRaw = toRaw(amountInput, balance.decimals);
  const insufficient = amountRaw > balance.amountRaw;
  const key = `${balance.chain}:${balance.asset}:${amountRaw}`;
  const current = held && held.key === key ? held : null;
  const onForm = step.kind === "form";
  const lockName = `loyal.bridge.move:${sender.toLowerCase()}:${balance.chain}`;

  useEffect(() => {
    if (!onForm || amountRaw === BigInt(0) || insufficient) {
      setQuoting(false);
      setQuoteError(null);
      return;
    }
    let live = true;
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
          if (live)
            setHeld({
              key,
              response,
              expiresAt: Date.now() + QUOTE_MAX_AGE_MS,
              stale: false,
            });
        })
        .catch((e) => {
          if (live) setQuoteError(errorText(e));
        })
        .finally(() => {
          if (live) setQuoting(false);
        });
    }, QUOTE_DEBOUNCE_MS);
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [
    amountRaw,
    balance.asset,
    balance.chain,
    insufficient,
    key,
    onForm,
    refreshes,
  ]);

  const expire = useCallback(() => {
    setHeld((q) => q && { ...q, stale: true });
    if (!document.hidden) refresh();
  }, []);

  useEffect(() => {
    if (!onForm || !current) return;
    const timer = setTimeout(
      expire,
      current.stale ? QUOTE_MAX_AGE_MS : current.expiresAt - Date.now()
    );
    const onVisible = () => {
      if (!document.hidden && Date.now() >= current.expiresAt) expire();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [current, expire, onForm]);

  useEffect(() => {
    if (lockedElsewhere !== lockName) return;
    const waiting = new AbortController();
    void navigator.locks
      .request(lockName, { signal: waiting.signal }, () => {
        setLockedElsewhere(null);
        refresh();
      })
      .catch(() => undefined);
    return () => waiting.abort();
  }, [lockName, lockedElsewhere]);

  const move = useCallback(
    async (quoted: BridgeQuoteResponse) => {
      const calls = quoted.calls ?? [];
      setStep({ kind: "sending", quote: quoted });
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
          message: sendFailureCopy(failure, balance.symbol, sent),
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
        } catch {
          if (attempt < 2) await new Promise((r) => setTimeout(r, 2000));
        }
      }
    },
    [balance.symbol, sendTransaction, sender, wallets]
  );

  const send = useCallback(async () => {
    if (!current || sendingRef.current) return;
    if (Date.now() >= current.expiresAt) {
      expire();
      return;
    }
    const quoted = current.response;
    sendingRef.current = true;
    try {
      if (!navigator.locks) {
        await move(quoted);
        return;
      }
      await navigator.locks.request(
        lockName,
        { ifAvailable: true },
        async (lock: Lock | null) => {
          if (lock) await move(quoted);
          else setLockedElsewhere(lockName);
        }
      );
    } finally {
      sendingRef.current = false;
    }
  }, [current, expire, lockName, move]);

  const bridging = step.kind === "bridging" ? step : null;
  useEffect(() => {
    if (!bridging) return;
    let live = true;
    const poll = async () => {
      try {
        const transfer = (await readTransfers()).find(
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
    quote: current?.response ?? null,
    ready: Boolean(current && !current.stale && !quoting),
    elsewhere: lockedElsewhere === lockName,
    quoting,
    quoteError,
    step,
    send,
  };
}
