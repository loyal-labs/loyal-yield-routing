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
  | { kind: "bridging"; requestId: string; unsubmittedBy: number | null }
  | { kind: "filled"; transfer: BridgeTransfer }
  | { kind: "failed"; message: string; requestId: string | null };

export type OutQuoteError = {
  code: string | null;
  message: string;
  retryable: boolean;
};

const USDC_DECIMALS = 6;
const QUOTE_DEBOUNCE_MS = 500;
const QUOTE_MAX_AGE_MS = 30_000;
const POLL_MS = 3000;
const SUBMIT_ATTEMPTS = 3;
const BLOCKHASH_WINDOW_MS = 120_000;
const READ_TIMEOUT_MS = 10_000;
const STILL_LANDING =
  "A transfer from this wallet may still be landing. Check your wallet and wait before sending again.";
const DID_NOT_FINISH =
  "The transfer did not finish. Check your wallet before trying again.";

class BridgeApiError extends Error {
  constructor(
    message: string,
    readonly code: string | null,
    readonly status: number
  ) {
    super(message);
  }
}

type Shape<T> = (body: unknown) => body is T;

const isRecord = (b: unknown): b is Record<string, unknown> =>
  typeof b === "object" && b !== null && !Array.isArray(b);

const isText = (v: unknown): v is string => typeof v === "string" && v !== "";

const isListOrNull = (v: unknown) => v === null || Array.isArray(v);

const TRANSFER_STATES = ["quoted", "submitted", "filled", "refunded", "failed"];

const TRANSFER_TEXT = [
  "requestId",
  "chain",
  "asset",
  "amountInRaw",
  "amountOutRaw",
  "quotedAt",
] as const;

const TRANSFER_TEXT_OR_NULL = [
  "originTx",
  "destinationTx",
  "refundTx",
  "confirmedAt",
  "submittedAt",
  "settledAt",
] as const;

const isTransfer: Shape<BridgeTransfer> = (b): b is BridgeTransfer =>
  isRecord(b) &&
  TRANSFER_TEXT.every((k) => isText(b[k])) &&
  TRANSFER_TEXT_OR_NULL.every((k) => b[k] === null || isText(b[k])) &&
  TRANSFER_STATES.includes(b.state as string) &&
  (b.direction === "in" || b.direction === "out") &&
  typeof b.inFlight === "boolean";

const isQuote: Shape<BridgeOutQuoteResponse> = (
  b
): b is BridgeOutQuoteResponse =>
  isRecord(b) &&
  isText(b.requestId) &&
  Array.isArray(b.instructions) &&
  isListOrNull(b.lookupTables) &&
  isText(b.amountOutRaw) &&
  isText(b.amountOutMinRaw) &&
  isText(b.loyalFeeRaw) &&
  typeof b.loyalFeeBps === "number" &&
  typeof b.slippageBps === "number";

const isTransfers: Shape<BridgeTransfersResponse> = (
  b
): b is BridgeTransfersResponse =>
  isRecord(b) &&
  isListOrNull(b.transfers) &&
  (b.transfers as unknown[] | null ?? []).every(isTransfer);

const isDestinations: Shape<BridgeDestinationsResponse> = (
  b
): b is BridgeDestinationsResponse => isRecord(b) && isListOrNull(b.destinations);

const isRecipients: Shape<BridgeRecipientsResponse> = (
  b
): b is BridgeRecipientsResponse => isRecord(b) && isListOrNull(b.recipients);

async function readJSON<T>(
  res: Response,
  fallback: string,
  shape: Shape<T>
): Promise<T> {
  const body: unknown = await res.json().catch(() => null);
  if (res.ok && shape(body)) return body;
  const detail =
    isRecord(body) && isRecord(body.error)
      ? (body.error as Partial<RouteError["error"]>)
      : null;
  throw new BridgeApiError(
    detail?.message ||
      (res.ok
        ? `${fallback}: the reply was not what we expected.`
        : `${fallback} (${res.status}).`),
    detail?.code ?? null,
    res.status
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

const readTransfers = async (ms = READ_TIMEOUT_MS) =>
  (
    await readJSON<BridgeTransfersResponse>(
      await fetch("/api/funding/bridge/transfers?direction=out", {
        credentials: "include",
        signal: AbortSignal.timeout(ms),
      }),
      "Could not read the bridge",
      isTransfers
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
          "Could not load the chains",
          isDestinations
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
          "Could not load recent recipients",
          isRecipients
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
  const [landing, setLanding] = useState<"reading" | "landing" | "clear">(
    "reading"
  );
  const sendingRef = useRef(false);
  const rejectedRef = useRef(new Set<string>());
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);
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
  const currentIdRef = useRef<string | null>(null);
  currentIdRef.current = current?.response.requestId ?? null;
  const quotedKeyRef = useRef<string | null>(null);
  quotedKeyRef.current = current ? key : null;
  const onForm = step.kind === "form";
  const owner = publicKey?.toBase58() ?? "";
  const lockName = `loyal-bridge-out:${owner}`;
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
          readJSON<BridgeOutQuoteResponse>(res, "Could not get a quote", isQuote)
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
      retryable:
        e instanceof BridgeApiError && (e.status === 429 || e.status === 503),
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

  const settled = landing === "clear";
  const holding = lockedElsewhere === lockName || landing === "landing";
  useEffect(() => {
    if (settled) return;
    let live = true;
    const read = async () => {
      try {
        const rows = await readTransfers();
        if (live)
          setLanding(
            rows.some(
              (t) => t.inFlight && !rejectedRef.current.has(t.requestId)
            )
              ? "landing"
              : "clear"
          );
      } catch {
        return;
      }
    };
    const timer = setInterval(() => void read(), POLL_MS);
    void read();
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [settled]);

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
    async (
      quoted: BridgeOutQuoteResponse,
      quotedKey: string
    ): Promise<string | undefined> => {
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
        verdict = { ok: false, reason: "preflight threw", retry: false };
      }
      const inFlight = verdict.ok
        ? await readTransfers().then(
            (rows) =>
              rows.some(
                (t) => t.inFlight && !rejectedRef.current.has(t.requestId)
              ),
            () => true
          )
        : false;
      if (!mountedRef.current) return;
      if (keyRef.current !== quotedKey) {
        setNotice("The details changed. Check them and send again.");
        setStep({ kind: "form" });
        return;
      }
      if (!verdict.ok && verdict.retry) {
        setNotice("Could not verify this transfer yet. Please try again.");
        setStep({ kind: "form" });
        return;
      }
      if (!verdict.ok) {
        fail("This transfer could not be checked. Your USDC stays in your wallet.");
        return;
      }
      if (inFlight) {
        setLanding("landing");
        setStep({ kind: "form" });
        return;
      }

      try {
        const confirmed = await settleWithin(
          post(`${requestPath}/confirm`, {}).then((res) =>
            readJSON<BridgeTransfer>(
              res,
              "Could not start the transfer",
              isTransfer
            )
          )
        );
        if (
          confirmed.requestId !== quoted.requestId ||
          confirmed.direction !== "out" ||
          confirmed.state !== "quoted"
        ) {
          throw new Error("Could not start the transfer: wrong transfer.");
        }
      } catch (e) {
        fail(`${errorText(e)} Your USDC stays in your wallet.`);
        return;
      }

      if (!mountedRef.current) return;
      if (currentIdRef.current !== quoted.requestId) {
        setNotice("The details changed. Check them and send again.");
        setStep({ kind: "form" });
        return;
      }

      let signature: string;
      try {
        signature = await settleWithin(
          sendTransaction(verdict.transaction, connection)
        );
      } catch (e) {
        if (isRejected(e)) {
          rejectedRef.current.add(quoted.requestId);
          setStep({ kind: "form" });
          return;
        }
        setStep({
          kind: "bridging",
          requestId: quoted.requestId,
          unsubmittedBy: Date.now() + BLOCKHASH_WINDOW_MS,
        });
        return quoted.requestId;
      }

      setStep({
        kind: "bridging",
        requestId: quoted.requestId,
        unsubmittedBy: null,
      });
      for (let attempt = 0; attempt < SUBMIT_ATTEMPTS; attempt++) {
        try {
          await readJSON<BridgeTransfer>(
            await post(`${requestPath}/submit`, { originTx: signature }),
            "Could not record the transfer",
            isTransfer
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
    if (!current || !settled || sendingRef.current) return;
    sendingRef.current = true;
    try {
      if (Date.now() - current.quotedAt >= QUOTE_MAX_AGE_MS) {
        await requote();
        return;
      }
      const quoted = current.response;
      if (!navigator.locks) {
        await run(quoted, current.key);
        return;
      }
      await navigator.locks.request(
        lockName,
        { ifAvailable: true },
        async (lock: Lock | null) => {
          if (!lock) {
            setLockedElsewhere(lockName);
            return;
          }
          const landing = await run(quoted, current.key);
          if (landing) await holdUntilSettled(landing);
        }
      );
    } finally {
      sendingRef.current = false;
    }
  }, [current, lockName, requote, run, settled]);

  const bridgingId = step.kind === "bridging" ? step.requestId : null;
  const unsubmittedBy = step.kind === "bridging" ? step.unsubmittedBy : null;
  useEffect(() => {
    if (!bridgingId) return;
    let live = true;
    const poll = async () => {
      try {
        const transfer = (await readTransfers()).find(
          (t) => t.requestId === bridgingId
        );
        if (!live) return;
        if (!transfer && unsubmittedBy !== null && Date.now() >= unsubmittedBy) {
          setStep({
            kind: "failed",
            message: DID_NOT_FINISH,
            requestId: bridgingId,
          });
          return;
        }
        if (!transfer) return;
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
  }, [bridgingId, unsubmittedBy]);

  return {
    amountRaw,
    insufficient,
    belowMinimum,
    quote: current?.response ?? null,
    ready: Boolean(current && !quoting && settled),
    elsewhere: holding,
    quoting,
    quoteError: quotable ? quoteError : null,
    notice: holding ? STILL_LANDING : notice,
    step,
    send,
    retry: requote,
  };
}

async function holdUntilSettled(requestId: string) {
  const until = Date.now() + BLOCKHASH_WINDOW_MS;
  for (;;) {
    await new Promise((r) => setTimeout(r, POLL_MS));
    const left = until - Date.now();
    if (left <= 0) return;
    try {
      const row = (await readTransfers(Math.min(READ_TIMEOUT_MS, left))).find(
        (t) => t.requestId === requestId
      );
      if (row && ["filled", "refunded", "failed"].includes(row.state)) return;
    } catch {
      continue;
    }
  }
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
