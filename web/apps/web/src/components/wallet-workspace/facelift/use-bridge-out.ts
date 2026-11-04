"use client";

import type {
  BridgeDestination,
  BridgeDestinationsResponse,
  BridgeOutQuoteResponse,
  BridgeRecipient,
  BridgeRecipientsResponse,
  BridgeTransfer,
  BridgeTransfersResponse,
  RouteError,
} from "@loyal-labs/api";
import { useConnection, useWallet } from "@solana/wallet-adapter-react";
import { useCallback, useEffect, useReducer, useRef, useState } from "react";

import { runBridgeOutPreflight } from "./bridge-out-preflight";
import { settleWithin, toRaw } from "./use-bridge-move";

export type OutToken = BridgeDestination["tokens"] extends
  | (infer T)[]
  | null
  ? T
  : never;

export type OutStep =
  | { kind: "form" }
  | { kind: "sending" }
  | { kind: "bridging"; requestId: string }
  | { kind: "filled"; transfer: BridgeTransfer }
  | { kind: "failed"; message: string; requestId: string | null };

export type OutQuoteError = { code: string | null; message: string };

const USDC_DECIMALS = 6;
const QUOTE_DEBOUNCE_MS = 500;
const QUOTE_MAX_AGE_MS = 30_000;
const POLL_MS = 3000;
const SUBMIT_ATTEMPTS = 3;
const SLOT_SKEW_REASON = "simulation and pre state never shared a slot";

class BridgeApiError extends Error {
  constructor(message: string, readonly code: string | null) {
    super(message);
  }
}

async function readJSON<T>(res: Response, fallback: string): Promise<T> {
  const body = (await res.json().catch(() => null)) as
    | T
    | Partial<RouteError>
    | null;
  if (res.ok && body) return body as T;
  const detail =
    body && typeof body === "object" && "error" in body ? body.error : null;
  throw new BridgeApiError(
    detail?.message || `${fallback} (${res.status}).`,
    detail?.code ?? null
  );
}

const post = (path: string, body: unknown, signal?: AbortSignal) =>
  fetch(path, {
    method: "POST",
    credentials: "include",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
    signal,
  });

const errorText = (e: unknown) => (e instanceof Error ? e.message : String(e));

const readTransfers = async () =>
  (
    await readJSON<BridgeTransfersResponse>(
      await fetch("/api/funding/bridge/transfers?direction=out", {
        credentials: "include",
      }),
      "Could not read the bridge"
    )
  ).transfers ?? [];

export function useBridgeDestinations() {
  const [state, setState] = useState<{
    destinations: BridgeDestination[] | null;
    error: string | null;
  }>({ destinations: null, error: null });
  const [attempt, retry] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    let live = true;
    void fetch("/api/funding/bridge/destinations", { credentials: "include" })
      .then((res) =>
        readJSON<BridgeDestinationsResponse>(
          res,
          "Could not load the chains"
        )
      )
      .then((body) => {
        if (live)
          setState({ destinations: body.destinations ?? [], error: null });
      })
      .catch((e) => {
        if (live) setState({ destinations: null, error: errorText(e) });
      });
    return () => {
      live = false;
    };
  }, [attempt]);
  return { ...state, retry };
}

export function useBridgeRecipients(chain: string, enabled: boolean) {
  const [read, setRead] = useState<{
    chain: string;
    recipients: BridgeRecipient[];
  } | null>(null);
  useEffect(() => {
    if (!enabled) return;
    let live = true;
    void fetch(
      `/api/funding/bridge/out/recipients?chain=${encodeURIComponent(chain)}`,
      { credentials: "include" }
    )
      .then((res) =>
        readJSON<BridgeRecipientsResponse>(
          res,
          "Could not load recent recipients"
        )
      )
      .then((body) => {
        if (live) setRead({ chain, recipients: body.recipients ?? [] });
      })
      .catch(() => {
        if (live) setRead({ chain, recipients: [] });
      });
    return () => {
      live = false;
    };
  }, [chain, enabled]);
  return read?.chain === chain ? read.recipients : null;
}

type HeldQuote = {
  key: string;
  response: BridgeOutQuoteResponse;
  quotedAt: number;
};

export function useBridgeOut({
  chain,
  token,
  recipient,
  amountInput,
  balanceRaw,
}: {
  chain: string;
  token: OutToken | null;
  recipient: string;
  amountInput: string;
  balanceRaw: bigint;
}) {
  const { connection } = useConnection();
  const { publicKey, sendTransaction } = useWallet();
  const [held, setHeld] = useState<HeldQuote | null>(null);
  const [quoteError, setQuoteError] = useState<OutQuoteError | null>(null);
  const [quoting, setQuoting] = useState(false);
  const [step, setStep] = useState<OutStep>({ kind: "form" });
  const [lockedElsewhere, setLockedElsewhere] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [retries, retryQuote] = useReducer((n: number) => n + 1, 0);
  const sendingRef = useRef(false);
  const amountRaw = toRaw(amountInput, USDC_DECIMALS);
  const insufficient = amountRaw > balanceRaw;
  const minRaw = token
    ? BigInt(token.minUsdc) * BigInt(10) ** BigInt(USDC_DECIMALS)
    : BigInt(0);
  const belowMinimum = amountRaw > BigInt(0) && amountRaw < minRaw;
  const asset = token?.asset ?? "";
  const key = `${chain}:${asset}:${amountRaw}:${recipient}`;
  const keyRef = useRef(key);
  keyRef.current = key;
  const current = held && held.key === key ? held : null;
  const quotedKeyRef = useRef<string | null>(null);
  quotedKeyRef.current = current ? key : null;
  const onForm = step.kind === "form";
  const owner = publicKey?.toBase58() ?? "";
  const lockName = `loyal.bridge.out:${owner}`;
  const quotable =
    onForm &&
    Boolean(asset) &&
    Boolean(recipient) &&
    amountRaw > BigInt(0) &&
    !insufficient &&
    !belowMinimum;

  const fetchQuote = useCallback(
    (forKey: string, signal?: AbortSignal) =>
      post(
        "/api/funding/bridge/out/quote",
        { chain, asset, amountRaw: amountRaw.toString(), recipient },
        signal
      )
        .then((res) =>
          readJSON<BridgeOutQuoteResponse>(res, "Could not get a quote")
        )
        .then(
          (response): HeldQuote => ({
            key: forKey,
            response,
            quotedAt: Date.now(),
          })
        ),
    [amountRaw, asset, chain, recipient]
  );

  const settleQuoteError = useCallback((e: unknown) => {
    setQuoteError({
      code: e instanceof BridgeApiError ? e.code : null,
      message: errorText(e),
    });
  }, []);

  useEffect(() => {
    if (!quotable || (quotedKeyRef.current === key && retries === 0)) {
      setQuoting(false);
      setQuoteError(null);
      return;
    }
    const controller = new AbortController();
    setQuoting(true);
    setQuoteError(null);
    const timer = setTimeout(() => {
      fetchQuote(key, controller.signal)
        .then((quote) => {
          if (!controller.signal.aborted) setHeld(quote);
        })
        .catch((e) => {
          if (!controller.signal.aborted) settleQuoteError(e);
        })
        .finally(() => {
          if (!controller.signal.aborted) setQuoting(false);
        });
    }, QUOTE_DEBOUNCE_MS);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [fetchQuote, key, quotable, retries, settleQuoteError]);

  useEffect(() => setNotice(null), [key]);

  useEffect(() => {
    if (lockedElsewhere !== lockName) return;
    const waiting = new AbortController();
    void navigator.locks
      .request(lockName, { signal: waiting.signal }, () => {
        setLockedElsewhere(null);
        retryQuote();
      })
      .catch(() => undefined);
    return () => waiting.abort();
  }, [lockName, lockedElsewhere]);

  const requote = useCallback(async () => {
    const forKey = keyRef.current;
    setQuoting(true);
    setQuoteError(null);
    try {
      const quote = await fetchQuote(forKey);
      if (keyRef.current === forKey) setHeld(quote);
    } catch (e) {
      if (keyRef.current === forKey) settleQuoteError(e);
    } finally {
      setQuoting(false);
    }
  }, [fetchQuote, settleQuoteError]);

  const run = useCallback(
    async (quoted: BridgeOutQuoteResponse) => {
      const fail = (message: string, requestId: string | null = null) =>
        setStep({ kind: "failed", message, requestId });
      if (!publicKey || !sendTransaction) {
        fail("Your Solana wallet is not ready. Please try again.");
        return;
      }
      setNotice(null);
      setStep({ kind: "sending" });
      const requestPath = `/api/funding/bridge/transfers/${encodeURIComponent(
        quoted.requestId
      )}`;

      let verdict: Awaited<ReturnType<typeof runBridgeOutPreflight>>;
      try {
        verdict = await runBridgeOutPreflight({
          connection,
          owner: publicKey,
          quote: quoted,
          amountRaw,
        });
      } catch {
        verdict = { ok: false, reason: "preflight threw" };
      }
      if (!verdict.ok && verdict.reason === SLOT_SKEW_REASON) {
        setNotice("Could not verify this transfer yet. Please try again.");
        setStep({ kind: "form" });
        return;
      }
      if (!verdict.ok) {
        fail("This transfer could not be checked. Your USDC stays in your wallet.");
        return;
      }

      try {
        await settleWithin(
          post(`${requestPath}/confirm`, {}).then((res) =>
            readJSON<BridgeTransfer>(res, "Could not start the transfer")
          )
        );
      } catch (e) {
        fail(`${errorText(e)} Your USDC stays in your wallet.`);
        return;
      }

      let signature: string;
      try {
        signature = await settleWithin(
          sendTransaction(verdict.transaction, connection)
        );
      } catch (e) {
        if (isRejected(e)) {
          setStep({ kind: "form" });
          return;
        }
        fail("The transfer did not finish. Check your wallet before trying again.");
        return;
      }

      setStep({ kind: "bridging", requestId: quoted.requestId });
      for (let attempt = 0; attempt < SUBMIT_ATTEMPTS; attempt++) {
        try {
          await readJSON<BridgeTransfer>(
            await post(`${requestPath}/submit`, { originTx: signature }),
            "Could not record the transfer"
          );
          return;
        } catch {
          if (attempt < SUBMIT_ATTEMPTS - 1)
            await new Promise((r) => setTimeout(r, 2000));
        }
      }
    },
    [amountRaw, connection, publicKey, sendTransaction]
  );

  const send = useCallback(async () => {
    if (!current || sendingRef.current) return;
    sendingRef.current = true;
    try {
      if (Date.now() - current.quotedAt >= QUOTE_MAX_AGE_MS) {
        await requote();
        return;
      }
      const quoted = current.response;
      if (!navigator.locks) {
        await run(quoted);
        return;
      }
      await navigator.locks.request(
        lockName,
        { ifAvailable: true },
        async (lock: Lock | null) => {
          if (lock) await run(quoted);
          else setLockedElsewhere(lockName);
        }
      );
    } finally {
      sendingRef.current = false;
    }
  }, [current, lockName, requote, run]);

  const bridgingId = step.kind === "bridging" ? step.requestId : null;
  useEffect(() => {
    if (!bridgingId) return;
    let live = true;
    const poll = async () => {
      try {
        const transfer = (await readTransfers()).find(
          (t) => t.requestId === bridgingId
        );
        if (!live || !transfer) return;
        if (transfer.state === "filled") setStep({ kind: "filled", transfer });
        else if (transfer.state === "refunded" || transfer.state === "failed")
          setStep({
            kind: "failed",
            message:
              transfer.state === "refunded"
                ? "Refunded to your Solana wallet"
                : "The transfer failed.",
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
  }, [bridgingId]);

  return {
    amountRaw,
    insufficient,
    belowMinimum,
    quote: current?.response ?? null,
    ready: Boolean(current && !quoting),
    elsewhere: lockedElsewhere === lockName,
    quoting,
    quoteError: quotable ? quoteError : null,
    notice,
    step,
    send,
  };
}

function isRejected(e: unknown): boolean {
  for (
    let cause: unknown = e, depth = 0;
    cause && typeof cause === "object" && depth < 4;
    depth++
  ) {
    const { code, message, error } = cause as {
      code?: unknown;
      message?: unknown;
      error?: unknown;
    };
    if (code === 4001) return true;
    if (typeof message === "string" && /user rejected/i.test(message))
      return true;
    cause = error;
  }
  return false;
}
